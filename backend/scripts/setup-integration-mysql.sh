#!/usr/bin/env bash
#
# Provision a real MySQL 8 / InnoDB instance for the integration test suite
# (service package, build tag "integration"). It downloads an official generic
# tarball into a user-writable directory (no root / Docker required), extracts
# libaio, initializes a persistent data directory and starts mysqld listening on
# 127.0.0.1:${IT_MYSQL_PORT}.
#
# Usage:
#   scripts/setup-integration-mysql.sh up     # download (once), init, start
#   scripts/setup-integration-mysql.sh down   # stop the server (data is kept)
#   scripts/setup-integration-mysql.sh clean  # stop and delete all data
#
# Then run:
#   TEST_MYSQL_DSN="it:it_pwd@tcp(127.0.0.1:38109)/gigmatch_it?charset=utf8mb4&parseTime=true&loc=Local" \
#     go test -tags integration -count=1 ./internal/service/
set -euo pipefail

MYSQL_VERSION="${IT_MYSQL_VERSION:-8.0.39}"
IT_HOME="${IT_HOME:-$HOME/.cache/gigmatch-it-mysql}"
PORT="${IT_MYSQL_PORT:-38109}"
DB_NAME="${IT_MYSQL_DB:-gigmatch_it}"
DB_USER="${IT_MYSQL_USER:-it}"
DB_PASS="${IT_MYSQL_PASS:-it_pwd}"

ARCH="$(uname -m)"
case "$ARCH" in
  aarch64|arm64) ARCH_DIR="aarch64"; LIBAIO_DEB="libaio1_0.3.113-4_arm64.deb" ;;
  x86_64|amd64)  ARCH_DIR="x86_64";  LIBAIO_DEB="libaio1_0.3.113-4_amd64.deb" ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac
BASE_DIR="$IT_HOME/mysql-$MYSQL_VERSION-linux-glibc2.28-$ARCH_DIR"
DATA_DIR="$IT_HOME/data"
RUN_DIR="$IT_HOME/run"
LIBAIO_DIR="$IT_HOME/libaio/usr/lib/$ARCH_DIR-linux-gnu"
SOCKET="$RUN_DIR/mysql.sock"
PID_FILE="$RUN_DIR/mysqld.pid"
LOG_FILE="$IT_HOME/server.log"

export LD_LIBRARY_PATH="$LIBAIO_DIR${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

download() {
  if [[ -x "$BASE_DIR/bin/mysqld" ]]; then return; fi
  mkdir -p "$IT_HOME"
  local url="https://cdn.mysql.com//archives/mysql-$MYSQL_VERSION/mysql-$MYSQL_VERSION-linux-glibc2.28-$ARCH_DIR.tar.xz"
  echo "downloading $url"
  curl -fsSL "$url" -o "$IT_HOME/mysql.tar.xz"
  tar -xf "$IT_HOME/mysql.tar.xz" -C "$IT_HOME"
  rm -f "$IT_HOME/mysql.tar.xz"

  echo "extracting libaio"
  mkdir -p "$IT_HOME/libaio"
  curl -fsSL "http://ftp.debian.org/debian/pool/main/liba/libaio/$LIBAIO_DEB" -o "$IT_HOME/libaio.deb"
  (cd "$IT_HOME/libaio" && ar x ../libaio.deb && tar xf data.tar.*)
  rm -f "$IT_HOME/libaio.deb"
}

initdb() {
  if [[ -d "$DATA_DIR/mysql" ]]; then return; fi
  mkdir -p "$DATA_DIR" "$RUN_DIR"
  "$BASE_DIR/bin/mysqld" --no-defaults --initialize-insecure \
    --basedir="$BASE_DIR" --datadir="$DATA_DIR" \
    --pid-file="$PID_FILE" --socket="$SOCKET"
}

start() {
  download
  initdb
  if "$BASE_DIR/bin/mysqladmin" --no-defaults --socket="$SOCKET" -uroot ping >/dev/null 2>&1; then
    echo "already running"
    return
  fi
  mkdir -p "$RUN_DIR"
  nohup "$BASE_DIR/bin/mysqld" --no-defaults \
    --basedir="$BASE_DIR" --datadir="$DATA_DIR" \
    --socket="$SOCKET" --port="$PORT" --pid-file="$PID_FILE" \
    --bind-address=127.0.0.1 --mysqlx=OFF --skip-name-resolve \
    --innodb-flush-log-at-trx-commit=0 \
    >"$LOG_FILE" 2>&1 &
  for _ in $(seq 1 60); do
    if "$BASE_DIR/bin/mysqladmin" --no-defaults --socket="$SOCKET" -uroot ping >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  "$BASE_DIR/bin/mysql" --no-defaults --socket="$SOCKET" -uroot <<SQL
CREATE DATABASE IF NOT EXISTS \`$DB_NAME\` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS '$DB_USER'@'127.0.0.1' IDENTIFIED BY '$DB_PASS';
CREATE USER IF NOT EXISTS '$DB_USER'@'localhost' IDENTIFIED BY '$DB_PASS';
GRANT ALL PRIVILEGES ON \`$DB_NAME\`.* TO '$DB_USER'@'127.0.0.1';
GRANT ALL PRIVILEGES ON \`$DB_NAME\`.* TO '$DB_USER'@'localhost';
FLUSH PRIVILEGES;
SQL
  echo "MySQL $MYSQL_VERSION ready on 127.0.0.1:$PORT (db=$DB_NAME)"
  echo "DSN: $DB_USER:$DB_PASS@tcp(127.0.0.1:$PORT)/$DB_NAME?charset=utf8mb4&parseTime=true&loc=Local"
}

stop() {
  if [[ -f "$PID_FILE" ]]; then
    "$BASE_DIR/bin/mysqladmin" --no-defaults --socket="$SOCKET" -uroot shutdown 2>/dev/null || true
  fi
}

case "${1:-up}" in
  up) start ;;
  down) stop ;;
  clean) stop; rm -rf "$DATA_DIR" "$RUN_DIR" "$LOG_FILE" ;;
  *) echo "usage: $0 up|down|clean" >&2; exit 2 ;;
esac
