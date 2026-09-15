#!/usr/bin/env bash
#
# Provision a REAL MySQL 8 / InnoDB instance for the integration test suite
# (service package, build tag "integration"). It downloads an official generic
# tarball from working redirect/mirror sources (with retries, resume support
# and SHA-256 verification), extracts libaio without root, initializes a
# persistent data directory and starts an isolated mysqld over TCP.
#
# This is deliberately NOT an in-memory/mock/single-connection setup: the
# tests need independent TCP connections exercising genuine InnoDB row locks,
# the unique index and real transaction isolation.
#
# Usage:
#   scripts/setup-integration-mysql.sh up     # download (once), init, start
#   scripts/setup-integration-mysql.sh status # ping the instance
#   scripts/setup-integration-mysql.sh dsn    # print the usable DSN
#   scripts/setup-integration-mysql.sh down   # stop the server (data kept)
#   scripts/setup-integration-mysql.sh clean  # stop and remove everything
#
# Environment overrides:
#   IT_HOME, IT_MYSQL_VERSION, IT_MYSQL_PORT (38109), IT_MYSQL_DB (gigmatch_it),
#   IT_MYSQL_USER (it), IT_MYSQL_PASS (it_pwd), IT_MYSQL_SHA256 (optional pin),
#   IT_SKIP_CHECKSUM=1 (not recommended)
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
MYSQL_VERSION="${IT_MYSQL_VERSION:-8.0.39}"
IT_HOME="${IT_HOME:-$HOME/.cache/gigmatch-it-mysql}"
PORT="${IT_MYSQL_PORT:-38109}"
DB_NAME="${IT_MYSQL_DB:-gigmatch_it}"
DB_USER="${IT_MYSQL_USER:-it}"
DB_PASS="${IT_MYSQL_PASS:-it_pwd}"

ARCH="$(uname -m)"
case "$ARCH" in
  aarch64|arm64) ARCH_DIR="aarch64" ;;
  x86_64|amd64)  ARCH_DIR="x86_64"  ;;
  *) die "不支持的 CPU 架构: $ARCH" ;;
esac

TARBALL="mysql-${MYSQL_VERSION}-linux-glibc2.28-${ARCH_DIR}.tar.xz"
BASE_DIR="$IT_HOME/mysql-${MYSQL_VERSION}-linux-glibc2.28-${ARCH_DIR}"
DATA_DIR="$IT_HOME/data"
RUN_DIR="$IT_HOME/run"
LIBAIO_DIR="$IT_HOME/libaio"
SOCKET="$RUN_DIR/mysql.sock"
PID_FILE="$RUN_DIR/mysqld.pid"
LOG_FILE="$IT_HOME/server.log"
TARBALL_PATH="$IT_HOME/$TARBALL"

# Official/working sources. The canonical dev.mysql.com endpoint 302-redirects
# to the versioned CDN archive; direct HEAD on the archive can return 404 even
# though GET works, so these are always fetched with GET (curl -fL).
MYSQL_URLS=(
  "https://dev.mysql.com/get/Downloads/MySQL-8.0/${TARBALL}"
  "https://cdn.mysql.com//Downloads/MySQL-8.0/${TARBALL}"
  "https://cdn.mysql.com//archives/mysql-8.0/${TARBALL}"
)

# SHA-256 of the official generic tarball (computed from the file served by
# dev.mysql.com/get, which 302-redirects to the versioned CDN archive). Override
# with IT_MYSQL_SHA256 if you pin a different 8.0.x build.
case "${ARCH_DIR}" in
  aarch64) DEFAULT_SHA="${IT_MYSQL_SHA256_ARM64:-11097531ad9955c4bb2f560ddaeeeadb8e55c6f12c775658ec3b89c074727b5d}" ;;
  x86_64)  DEFAULT_SHA="${IT_MYSQL_SHA256_AMD64:-36adbd6a91daea2df57e2b0b7110eec9f700ae5207f28e088dd69130bea463ae}" ;;
esac
EXPECTED_SHA="${IT_MYSQL_SHA256:-$DEFAULT_SHA}"

# libaio is a hard runtime dependency of mysqld on Linux.
LIBAIO_PKG_VERSION="0.3.113-4"
LIBAIO_SO_DIR="$LIBAIO_DIR/usr/lib/${ARCH_DIR}-linux-gnu"
LIBAIO_DEB_NAME="libaio1_${LIBAIO_PKG_VERSION}_${ARCH_DIR/deb64/amd64}.deb"
case "$ARCH_DIR" in
  aarch64) LIBAIO_DEB_NAME="libaio1_${LIBAIO_PKG_VERSION}_arm64.deb" ;;
  x86_64)  LIBAIO_DEB_NAME="libaio1_${LIBAIO_PKG_VERSION}_amd64.deb" ;;
esac
LIBAIO_URLS=(
  "https://deb.debian.org/debian/pool/main/liba/libaio/${LIBAIO_DEB_NAME}"
  "http://ftp.debian.org/debian/pool/main/liba/libaio/${LIBAIO_DEB_NAME}"
  "https://mirrors.tuna.tsinghua.edu.cn/debian/pool/main/liba/libaio/${LIBAIO_DEB_NAME}"
)

# ---------------------------------------------------------------------------
# Logging / helpers
# ---------------------------------------------------------------------------
log()  { printf '\033[1;32m[mysql-it]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[mysql-it]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m[mysql-it][失败]\033[0m %s\n' "$*" >&2; exit 1; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else die "缺少 sha256sum/shasum，无法校验"; fi
}

# fetch_resumable <dest> <url...>  — retry + resume across several sources.
fetch_resumable() {
  local dest="$1"; shift
  local urls=("$@")
  mkdir -p "$(dirname "$dest")"
  local attempt
  for attempt in 1 2 3; do
    for url in "${urls[@]}"; do
      log "下载（第 ${attempt} 次）: $url"
      if curl -fL --retry 3 --retry-delay 2 -C - --connect-timeout 20 \
              --speed-time 30 --speed-limit 1024 \
              -o "$dest" "$url"; then
        log "下载完成: $(du -h "$dest" | awk '{print $1}')"
        return 0
      fi
      warn "该源失败（支持断点续传，将尝试下一个源）: $url"
    done
    warn "第 ${attempt} 轮全部源失败，3 秒后重试（已下载部分保留，可直接重跑本脚本恢复）"
    sleep 3
  done
  return 1
}

# ---------------------------------------------------------------------------
# Download & verify
# ---------------------------------------------------------------------------
download_mysql() {
  [[ -x "$BASE_DIR/bin/mysqld" ]] && { log "MySQL 已解压，跳过下载"; return; }

  if [[ ! -f "$TARBALL_PATH" ]] || ! tar tJf "$TARBALL_PATH" >/dev/null 2>&1; then
    fetch_resumable "$TARBALL_PATH" "${MYSQL_URLS[@]}" \
      || die "阶段[下载 MySQL]失败。已保留断点文件：$TARBALL_PATH
  可恢复操作：
    1) 检查网络/代理后重跑本脚本（curl -C - 会从断点续传）；
    2) 手动下载 ${TARBALL} 放到 $TARBALL_PATH；
    3) 用 IT_MYSQL_VERSION 指定其他 8.0.x 版本，或设置 IT_MYSQL_SHA256 校验值。"
  else
    log "使用已有压缩包: $TARBALL_PATH"
  fi

  # Checksum verification
  if [[ "${IT_SKIP_CHECKSUM:-0}" == "1" ]]; then
    warn "IT_SKIP_CHECKSUM=1，已跳过 SHA-256 校验（不推荐）"
  elif [[ "$EXPECTED_SHA" =~ ^[0-9a-f]{64}$ && "$EXPECTED_SHA" != "$(printf '0%.0s' {1..64})" ]]; then
    local actual
    actual="$(sha256_of "$TARBALL_PATH")"
    [[ "$actual" == "$EXPECTED_SHA" ]] \
      || die "阶段[校验 MySQL]失败：SHA-256 不匹配
  期望: $EXPECTED_SHA
  实际: $actual
  文件可能损坏或被篡改。删除后重跑即可重新下载：rm -f $TARBALL_PATH"
    log "SHA-256 校验通过"
  else
    local actual
    actual="$(sha256_of "$TARBALL_PATH")"
    warn "未内置 ${MYSQL_VERSION}/${ARCH_DIR} 的 SHA-256 固定值。"
    warn "实际 SHA-256 = $actual"
    warn "请与官方 https://dev.mysql.com/downloads/mysql/ 公布值核对；确认后可用 IT_MYSQL_SHA256=$actual 重新运行以启用强校验。"
  fi

  log "解压 MySQL ..."
  tar -xJf "$TARBALL_PATH" -C "$IT_HOME" \
    || die "阶段[解压 MySQL]失败：压缩包可能不完整。删除后重跑：rm -f $TARBALL_PATH"
  [[ -x "$BASE_DIR/bin/mysqld" ]] || die "解压后未找到 $BASE_DIR/bin/mysqld"
}

download_libaio() {
  [[ -f "$LIBAIO_SO_DIR/libaio.so.1" ]] && return
  local deb="$IT_HOME/libaio.deb"
  fetch_resumable "$deb" "${LIBAIO_URLS[@]}" \
    || die "阶段[下载 libaio]失败。可手动下载 ${LIBAIO_DEB_NAME} 放到 $deb；或 apt-get install libaio1 后设置 IT_SKIP_CHECKSUM 重试。"
  rm -rf "$LIBAIO_DIR"
  mkdir -p "$LIBAIO_DIR"
  (cd "$LIBAIO_DIR" && ar x "$deb" && tar xf data.tar.*) \
    || die "阶段[解压 libaio]失败：$deb 不是有效的 .deb"
  [[ -f "$LIBAIO_SO_DIR/libaio.so.1" ]] \
    || die "阶段[解压 libaio]失败：未在 .deb 中找到 lib/$ARCH_DIR 下的 libaio.so.1"
  log "libaio 就绪"
}

# ---------------------------------------------------------------------------
# Lifecycle
# ---------------------------------------------------------------------------
mysql_bin() {
  # Forward every argument after the binary name (the --datadir / --socket /
  # -h flags etc. must reach the invoked mysql tool).
  local bin="$1"; shift
  client_env
  "$BASE_DIR/bin/$bin" "$@"
}
client_env() { export LD_LIBRARY_PATH="$LIBAIO_SO_DIR${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"; }

initdb() {
  if [[ -d "$DATA_DIR/mysql" ]]; then
    log "数据目录已初始化，跳过"
    return
  fi
  mkdir -p "$DATA_DIR" "$RUN_DIR"
  client_env
  log "初始化数据目录 ..."
  mysql_bin mysqld --no-defaults --initialize-insecure \
    --basedir="$BASE_DIR" --datadir="$DATA_DIR" \
    --pid-file="$PID_FILE" --socket="$SOCKET" \
    || die "阶段[初始化数据库]失败，见上方 mysqld 输出。可清理后重来：$0 clean && $0 up"
}

is_up() {
  [[ -S "$SOCKET" ]] || return 1
  client_env
  mysql_bin mysqladmin --no-defaults --socket="$SOCKET" -uroot ping >/dev/null 2>&1
}

start() {
  mkdir -p "$IT_HOME" "$RUN_DIR"
  download_mysql
  download_libaio
  initdb

  if is_up; then
    log "mysqld 已在运行（端口 $PORT）"
  else
    mkdir -p "$RUN_DIR"
    client_env
    log "启动独立 mysqld，监听 127.0.0.1:$PORT ..."
    nohup "$BASE_DIR/bin/mysqld" --no-defaults \
      --basedir="$BASE_DIR" --datadir="$DATA_DIR" \
      --socket="$SOCKET" --port="$PORT" --pid-file="$PID_FILE" \
      --bind-address=127.0.0.1 --mysqlx=OFF --skip-name-resolve \
      --innodb-flush-log-at-trx-commit=0 \
      >"$LOG_FILE" 2>&1 &
    local i
    for i in $(seq 1 90); do
      if is_up; then break; fi
      # Fail fast if mysqld exited.
      if [[ -f "$PID_FILE" ]] && ! kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
        die "阶段[启动数据库]失败：mysqld 进程已退出，请查看日志 $LOG_FILE"
      fi
      sleep 1
    done
    is_up || die "阶段[启动数据库]失败：90 秒内未就绪，请查看日志 $LOG_FILE"
  fi

  log "创建测试库与账号 ..."
  client_env
  mysql_bin mysql --no-defaults --socket="$SOCKET" -uroot <<SQL \
    || die "阶段[建库/授权]失败，请检查 $LOG_FILE"
CREATE DATABASE IF NOT EXISTS \`$DB_NAME\` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS '$DB_USER'@'127.0.0.1' IDENTIFIED BY '$DB_PASS';
CREATE USER IF NOT EXISTS '$DB_USER'@'localhost' IDENTIFIED BY '$DB_PASS';
GRANT ALL PRIVILEGES ON \`$DB_NAME\`.* TO '$DB_USER'@'127.0.0.1';
GRANT ALL PRIVILEGES ON \`$DB_NAME\`.* TO '$DB_USER'@'localhost';
FLUSH PRIVILEGES;
SQL

  # Verify a real TCP login (what the tests use).
  if mysql_bin mysql --no-defaults -h127.0.0.1 -P"$PORT" -u"$DB_USER" -p"$DB_PASS" -e "SELECT 1" >/dev/null 2>&1; then
    log "就绪：MySQL $MYSQL_VERSION 真实 TCP 多连接实例 @ 127.0.0.1:$PORT 库=$DB_NAME"
  else
    die "阶段[TCP 连通性]失败：socket 可用但 TCP 登录失败，请查看 $LOG_FILE"
  fi
  echo
  echo "运行集成测试："
  echo "  TEST_MYSQL_DSN=\"$(print_dsn)\" go test -tags integration -count=1 ./internal/service/"
}

print_dsn() {
  printf '%s:%s@tcp(127.0.0.1:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local' \
    "$DB_USER" "$DB_PASS" "$PORT" "$DB_NAME"
}

status() {
  if is_up; then
    log "running, dsn: $(print_dsn)"
    return 0
  fi
  warn "not running"
  return 1
}

stop() {
  if [[ -S "$SOCKET" ]]; then
    client_env
    mysql_bin mysqladmin --no-defaults --socket="$SOCKET" -uroot shutdown 2>/dev/null || true
    log "已停止（数据保留在 $DATA_DIR）"
  fi
}

clean() {
  stop
  rm -rf "$DATA_DIR" "$RUN_DIR" "$LOG_FILE"
  warn "已删除数据目录（压缩包与 MySQL 程序保留在 $IT_HOME，重跑 up 无需重新下载）"
}

case "${1:-up}" in
  up)     start ;;
  status) status ;;
  dsn)    print_dsn; echo ;;
  down)   stop ;;
  clean)  clean ;;
  *)      echo "用法: $0 up|status|dsn|down|clean" >&2; exit 2 ;;
esac
