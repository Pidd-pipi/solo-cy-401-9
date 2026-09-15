//go:build itlifecycle
// +build itlifecycle

// Package scripts contains black-box lifecycle tests for
// setup-integration-mysql.sh. They exercise the REAL provisioning chain end to
// end:
//
//   - a real local HTTP source serves the REAL vendor tarball/.deb (with RFC
//     7233 byte-range resume support) and dedicated routes that interrupt the
//     transfer mid-stream, return a corrupted payload or 404;
//   - the script itself is executed as a real subprocess on the real filesystem;
//   - a real mysqld process is initialized/started and probed over an
//     independent TCP connection with the bundled mysql client.
//
// Nothing here is mocked, substituted with an in-memory database, single
// connection-serialized, or reduced to grepping script source.
//
// Prerequisites: the vendor artifacts must already be cached once (produced by
// a successful run of the script against the live mirrors):
//
//	~/ .cache/gigmatch-it-mysql/mysql-8.0.39-linux-glibc2.28-<arch>.tar.xz
//	~/ .cache/gigmatch-it-mysql/libaio.deb
//
// Override their locations with IT_SEED_TARBALL / IT_SEED_LIBAIO.
//
// Run:
//
//	go test -tags itlifecycle -count=1 -v ./scripts/
package scripts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const mysqlVersion = "8.0.39"

// harness holds process/port/file handles shared by one test binary run.
type harness struct {
	t          *testing.T
	scriptPath string
	seedTar    string
	seedDeb    string
	wantSHA    string
	tarName    string
	debName    string
	archTrip   string
	httpAddr   string
	workRoot   string
	mu         sync.Mutex
	nextPort   int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	script, err := filepath.Abs("setup-integration-mysql.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("setup script not found (%v); run from backend/", err)
	}

	archDir := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	trip := map[string]string{"arm64": "aarch64-linux-gnu", "amd64": "x86_64-linux-gnu"}[runtime.GOARCH]
	if archDir == "" {
		t.Skipf("unsupported arch %s", runtime.GOARCH)
	}
	tarName := fmt.Sprintf("mysql-%s-linux-glibc2.28-%s.tar.xz", mysqlVersion, archDir)
	debName := map[string]string{
		"aarch64": "libaio1_0.3.113-4_arm64.deb",
		"x86_64":  "libaio1_0.3.113-4_amd64.deb",
	}[archDir]

	cache := filepath.Join(os.Getenv("HOME"), ".cache", "gigmatch-it-mysql")
	seedTar := envOr("IT_SEED_TARBALL", filepath.Join(cache, tarName))
	seedDeb := envOr("IT_SEED_LIBAIO", filepath.Join(cache, "libaio.deb"))
	for path, name := range map[string]string{seedTar: "seed tarball", seedDeb: "seed libaio.deb"} {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("%s missing at %s; run './setup-integration-mysql.sh up' once to seed it", name, path)
		}
	}
	sum, err := fileSHA256(seedTar)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupted variant: same length, several regions overwritten.
	workRoot := t.TempDir()
	if err := makeCorrupt(seedTar, filepath.Join(workRoot, "corrupt.tar.xz")); err != nil {
		t.Fatal(err)
	}

	h := &harness{
		t:          t,
		scriptPath: script,
		seedTar:    seedTar,
		seedDeb:    seedDeb,
		wantSHA:    sum,
		tarName:    tarName,
		debName:    debName,
		archTrip:   trip,
		workRoot:   workRoot,
	}
	h.startHTTPServer()
	t.Logf("seed tarball %s sha256=%s", seedTar, sum)
	return h
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// makeCorrupt writes a same-length file whose content differs from the seed,
// guaranteeing the SHA-256 check fails.
func makeCorrupt(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	for _, off := range []int{0, 1024 * 1024, len(in) / 2, len(in) - 4096} {
		if off >= 0 && off < len(in) {
			for j := 0; j < 256 && off+j < len(in); j++ {
				in[off+j] = 0x5A
			}
		}
	}
	return os.WriteFile(dst, in, 0o644)
}

// freePort reserves a free TCP port, then releases it for mysqld to bind.
func (h *harness) freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// startHTTPServer serves real files over loopback with routes that the tests
// point IT_MYSQL_URLS / IT_LIBAIO_URLS at.
func (h *harness) startHTTPServer() {
	mux := http.NewServeMux()
	serveFile := func(path string, w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path) // honors Range/206 itself
	}
	mux.HandleFunc("/good/", func(w http.ResponseWriter, r *http.Request) {
		serveFile(h.seedTar, w, r)
	})
	mux.HandleFunc("/corrupt/", func(w http.ResponseWriter, r *http.Request) {
		serveFile(filepath.Join(h.workRoot, "corrupt.tar.xz"), w, r)
	})
	mux.HandleFunc("/libaio.deb", func(w http.ResponseWriter, r *http.Request) {
		serveFile(h.seedDeb, w, r)
	})
	mux.HandleFunc("/404", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "simulated unavailable version", http.StatusNotFound)
	})
	// /drop/ simulates a transfer that dies mid-stream. On a fresh GET it
	// delivers the first 1 MiB then closes the TCP connection early (no complete
	// Content-Length), so curl exits 18 with a genuine partial file. On a
	// Range/resume request it serves 1 MiB starting at the requested offset and
	// aborts again, which keeps every retry one block short of a complete file:
	// the run can only stop at the download stage, while the retained prefix is
	// a real resumable prefix completed by /good/ on the next invocation.
	mux.HandleFunc("/drop/", func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(h.seedTar)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		const chunk = 1 << 20
		start := int64(0)
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			rng := strings.TrimPrefix(rg, "bytes=")
			if i := strings.IndexByte(rng, '-'); i >= 0 {
				if n, perr := strconv.ParseInt(rng[:i], 10, 64); perr == nil {
					start = n
				}
			}
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if start > 0 {
			st, _ := f.Stat()
			total := st.Size()
			// Claim the full remainder and use chunked encoding, then abort after
			// one block: the declared range is never completed, so curl reports
			// a truncated transfer (exit 18) after appending the real bytes.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			// Lie about the length and die early: the client sees an aborted
			// transfer rather than a fully-delivered small object.
			w.Header().Set("Content-Length", "999999999")
			w.WriteHeader(http.StatusOK)
		}
		fl, _ := w.(http.Flusher)
		buf := make([]byte, 32*1024)
		var sent int
		for sent < chunk {
			n, rerr := f.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if fl != nil {
					fl.Flush()
				}
				sent += n
			}
			if rerr != nil {
				break
			}
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	h.t.Cleanup(func() { _ = srv.Close() })
	h.httpAddr = ln.Addr().String()
}

func (h *harness) url(route string) string { return "http://" + h.httpAddr + route }

// envFor builds the environment for one script invocation.
func (h *harness) envFor(home string, port int, mysqlURLs, libaioURLs string) []string {
	env := append(os.Environ(),
		"IT_HOME="+home,
		"IT_MYSQL_PORT="+strconv.Itoa(port),
		"IT_MYSQL_VERSION="+mysqlVersion,
		"IT_MYSQL_DB=gigmatch_it",
		"IT_MYSQL_USER=it",
		"IT_MYSQL_PASS=it_pwd",
		"IT_MYSQL_SHA256="+h.wantSHA,
	)
	if mysqlURLs != "" {
		env = append(env, "IT_MYSQL_URLS="+mysqlURLs)
	}
	if libaioURLs != "" {
		env = append(env, "IT_LIBAIO_URLS="+libaioURLs)
	}
	return env
}

// runScript executes the provisioning script and returns exit code + output.
func (h *harness) runScript(env []string, args ...string) (int, string) {
	cmd := exec.Command(h.scriptPath, args...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			h.t.Fatalf("failed to start script: %v", err)
		}
	}
	return exit, out.String()
}

func (h *harness) mustUp(env []string, stage string) string {
	h.t.Helper()
	exit, out := h.runScript(env, "up")
	if exit != 0 {
		h.t.Fatalf("%s: up exit=%d, output:\n%s", stage, exit, out)
	}
	return out
}

func (h *harness) assertFailStage(env []string, wantStage, wantHint string) {
	h.t.Helper()
	exit, out := h.runScript(env, "up")
	if exit == 0 {
		h.t.Fatalf("expected up to fail at %s, but it succeeded:\n%s", wantStage, out)
	}
	plain := stripANSI(out)
	if !strings.Contains(plain, "阶段["+wantStage+"]失败") {
		h.t.Fatalf("expected failure marker 阶段[%s]失败, got output:\n%s", wantStage, plain)
	}
	if wantHint != "" && !strings.Contains(plain, wantHint) {
		h.t.Fatalf("expected recovery hint %q, got output:\n%s", wantHint, plain)
	}
}

// mysqlClient runs the bundled mysql client over an independent TCP connection
// with the extracted libaio on the loader path.
func (h *harness) mysqlClient(home string, port int, sql string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(home, "mysql-*", "bin", "mysql"))
	if len(matches) == 0 {
		return "", fmt.Errorf("mysql client not found under %s", home)
	}
	cmd := exec.Command(matches[0], "--no-defaults",
		"-h127.0.0.1", "-P"+strconv.Itoa(port), "-uit", "-pit_pwd", "gigmatch_it", "-N", "-B", "-e", sql)
	ld := filepath.Join(home, "libaio", "usr", "lib", h.archTrip)
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+ld)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil // drop the non-fatal "Using a password on the command line" warning
	err := cmd.Run()
	return out.String(), err
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// newITHome creates a fresh provisioning home and guarantees its mysqld is
// stopped when the test ends.
func (h *harness) newITHome(name string) (string, int) {
	home := filepath.Join(h.workRoot, name)
	if err := os.MkdirAll(home, 0o755); err != nil {
		h.t.Fatal(err)
	}
	port := h.freePort()
	h.t.Cleanup(func() {
		env := h.envFor(home, port, "", "")
		_, _ = h.runScript(env, "down")
	})
	return home, port
}

// seedExtractedBase symlinks a once-extracted MySQL tree + libaio into home so
// tests that target lifecycle (not the download path) skip the large extract.
func (h *harness) seedExtractedBase(home string) {
	shared := filepath.Join(h.workRoot, "shared-base")
	base := filepath.Join(shared, fmt.Sprintf("mysql-%s-linux-glibc2.28-%s", mysqlVersion, archDir()))
	if _, err := os.Stat(filepath.Join(base, "bin", "mysqld")); err != nil {
		if err := os.MkdirAll(shared, 0o755); err != nil {
			h.t.Fatal(err)
		}
		cmd := exec.Command("tar", "-xJf", h.seedTar, "-C", shared)
		if out, err := cmd.CombinedOutput(); err != nil {
			h.t.Fatalf("seed extract: %v\n%s", err, out)
		}
	}
	target := filepath.Join(home, filepath.Base(base))
	if err := os.Symlink(base, target); err != nil {
		h.t.Fatal(err)
	}
	// Share the already-extracted libaio tree too.
	srcLibaio := filepath.Join(os.Getenv("HOME"), ".cache", "gigmatch-it-mysql", "libaio")
	dstLibaio := filepath.Join(home, "libaio")
	if _, err := os.Stat(srcLibaio + "/usr/lib/" + h.archTrip + "/libaio.so.1"); err == nil {
		_ = os.Symlink(srcLibaio, dstLibaio)
	}
}

func archDir() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestLifecycle_InterruptedDownloadResumes: an interrupted transfer must stop
// at the download stage with a recovery hint, keep the partial prefix, and
// complete on the next invocation via HTTP Range resume — then the real server
// starts and answers TCP queries.
func TestLifecycle_InterruptedDownloadResumes(t *testing.T) {
	h := newHarness(t)
	home, port := h.newITHome("interrupt")

	// Phase 1: source always cuts after 1 MiB -> the run cannot finish and must
	// stop at the download stage rather than extracting/checksuming/starting.
	badEnv := h.envFor(home, port, h.url("/drop/"+h.tarName), h.url("/libaio.deb"))
	h.assertFailStage(badEnv, "下载 MySQL", "重跑")

	partial := filepath.Join(home, h.tarName)
	st, err := os.Stat(partial)
	if err != nil {
		t.Fatalf("partial download not retained: %v", err)
	}
	if st.Size() == 0 {
		t.Fatal("interrupted download left no resumable bytes")
	}
	if _, err := os.Stat(filepath.Join(home, "data", "mysql")); !os.IsNotExist(err) {
		t.Fatalf("data dir must not be initialized after a download failure, err=%v", err)
	}

	// Phase 2: switch to a complete, range-capable source; curl -C - resumes the
	// exact retained prefix, checksum passes and the instance comes up.
	goodEnv := h.envFor(home, port, h.url("/good/"+h.tarName), h.url("/libaio.deb"))
	out := h.mustUp(goodEnv, "resume after interrupt")
	if !strings.Contains(stripANSI(out), "就绪") {
		t.Fatalf("resumed up did not report readiness:\n%s", out)
	}
	if out2, err := h.mysqlClient(home, port, "SELECT 1"); err != nil || !strings.Contains(out2, "1") {
		t.Fatalf("real TCP query after resume failed: %v out=%s", err, out2)
	}
}

// TestLifecycle_CorruptTarballFailsChecksumAndRecovers: a corrupted payload
// must be rejected at the checksum stage; following the printed recovery hint
// (delete and re-download) yields a working instance.
func TestLifecycle_CorruptTarballFailsChecksumAndRecovers(t *testing.T) {
	h := newHarness(t)
	home, port := h.newITHome("corrupt")

	corruptEnv := h.envFor(home, port, h.url("/corrupt/"+h.tarName), h.url("/libaio.deb"))
	h.assertFailStage(corruptEnv, "校验 MySQL", "SHA-256")

	if _, err := os.Stat(filepath.Join(home, "data", "mysql")); !os.IsNotExist(err) {
		t.Fatal("checksum failure must not initialize the data directory")
	}

	// Follow the recovery hint: remove the bad tarball, point at a good source.
	if err := os.Remove(filepath.Join(home, h.tarName)); err != nil {
		t.Fatal(err)
	}
	goodEnv := h.envFor(home, port, h.url("/good/"+h.tarName), h.url("/libaio.deb"))
	h.mustUp(goodEnv, "recover after corrupt")
	if _, err := h.mysqlClient(home, port, "SELECT 1"); err != nil {
		t.Fatalf("query after corrupt-recovery failed: %v", err)
	}
}

// TestLifecycle_UpDownUpStatusDSN runs up, down, up, status, dsn and asserts
// process state, stable output and on-disk data persistence across restarts.
func TestLifecycle_UpDownUpStatusDSN(t *testing.T) {
	h := newHarness(t)
	home, port := h.newITHome("cycle")
	h.seedExtractedBase(home) // this case targets lifecycle, not downloading

	env := h.envFor(home, port, h.url("/good/"+h.tarName), h.url("/libaio.deb"))
	h.mustUp(env, "first up")
	if _, err := h.mysqlClient(home, port,
		"CREATE TABLE IF NOT EXISTS it_marker (id INT PRIMARY KEY); INSERT INTO it_marker VALUES (42);"); err != nil {
		t.Fatalf("seed marker row: %v", err)
	}

	// status: running, exit 0; dsn: exact expected string.
	if exit, out := h.runScript(env, "status"); exit != 0 || !strings.Contains(stripANSI(out), "running") {
		t.Fatalf("status while up: exit=%d out=%s", exit, out)
	}
	wantDSN := fmt.Sprintf("it:it_pwd@tcp(127.0.0.1:%d)/gigmatch_it?charset=utf8mb4&parseTime=true&loc=Local", port)
	if exit, out := h.runScript(env, "dsn"); exit != 0 || strings.TrimSpace(out) != wantDSN {
		t.Fatalf("dsn mismatch: exit=%d got=%q want=%q", exit, strings.TrimSpace(out), wantDSN)
	}

	// down stops the process; status then reports non-running with non-zero exit.
	if exit, out := h.runScript(env, "down"); exit != 0 {
		t.Fatalf("down exit=%d out=%s", exit, out)
	}
	if err := waitPortClosed(port, 15*time.Second); err != nil {
		t.Fatalf("mysqld still accepting TCP after down: %v", err)
	}
	if exit, _ := h.runScript(env, "status"); exit == 0 {
		t.Fatal("status must exit non-zero after down")
	}
	if _, err := h.mysqlClient(home, port, "SELECT 1"); err == nil {
		t.Fatal("TCP query unexpectedly succeeded while instance was down")
	}

	// up again: data directory is reused; marker row must survive the restart.
	h.mustUp(env, "second up")
	out, err := h.mysqlClient(home, port, "SELECT id FROM it_marker;")
	if err != nil {
		t.Fatalf("query after restart: %v", err)
	}
	if strings.TrimSpace(out) != "42" {
		t.Fatalf("data did not persist across down/up, marker=%q", out)
	}
}

// TestLifecycle_UnavailableVersionThenRecovery: pointing at a version with no
// downloadable source must fail at the download stage with a recovery hint;
// rotating back to the available version then provisions successfully.
func TestLifecycle_UnavailableVersionThenRecovery(t *testing.T) {
	h := newHarness(t)
	home, port := h.newITHome("rotation")

	// Simulate an unavailable version (all sources 404).
	unavailable := append(os.Environ(),
		"IT_HOME="+home,
		"IT_MYSQL_PORT="+strconv.Itoa(port),
		"IT_MYSQL_VERSION=8.0.999",
		"IT_MYSQL_DB=gigmatch_it", "IT_MYSQL_USER=it", "IT_MYSQL_PASS=it_pwd",
		"IT_MYSQL_SHA256="+h.wantSHA,
		"IT_MYSQL_URLS="+h.url("/404"),
		"IT_LIBAIO_URLS="+h.url("/libaio.deb"),
	)
	h.assertFailStage(unavailable, "下载 MySQL", "重跑")
	if _, err := os.Stat(filepath.Join(home, "data", "mysql")); !os.IsNotExist(err) {
		t.Fatal("failed version download must not initialize data")
	}

	// Rotate to the available version (tarball served locally, extracted base
	// pre-seeded) and recover.
	h.seedExtractedBase(home)
	available := h.envFor(home, port, h.url("/good/"+h.tarName), h.url("/libaio.deb"))
	h.mustUp(available, "recover with available version")
	if _, err := h.mysqlClient(home, port, "SELECT VERSION()"); err != nil {
		t.Fatalf("query after version recovery failed: %v", err)
	}
}

// waitPortClosed polls until nothing accepts TCP on the port, or times out.
func waitPortClosed(port int, within time.Duration) error {
	deadline := time.Now().Add(within)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return nil
		}
		_ = c.Close()
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("port %s still open after %s", addr, within)
}
