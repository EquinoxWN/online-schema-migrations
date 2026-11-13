#!/usr/bin/env bash
# Local PostgreSQL 17 without Docker: zonky embedded binaries unpacked into .tmp/pg (git-ignored).
# usage: scripts/local-postgres.sh start|stop   (Windows x86-64 or Linux x86-64)
# After start: export OSM_TEST_DATABASE_URL=postgres://postgres@127.0.0.1:54329/postgres?sslmode=disable
set -euo pipefail
cd "$(dirname "$0")/.."
VER=17.11.0
PORT=${OSM_PG_PORT:-54329}
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) ART=windows-amd64; TXZ=postgres-windows-x86_64.txz; EXE=.exe
    SHA=98040fae18dd9633ff95932125b0cecf0a45a1a9312e216e2a33ad03a31d4251 ;;
  Linux) ART=linux-amd64; TXZ=postgres-linux-x86_64.txz; EXE=""; SHA="" ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 2 ;;
esac
BIN=.tmp/pg/bin

fetch() {
  [ -x "$BIN/postgres$EXE" ] && return 0
  local url="https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-$ART/$VER/embedded-postgres-binaries-$ART-$VER.jar"
  mkdir -p .tmp/pgdl
  curl -fsS --max-time 300 --retry 3 -o .tmp/pgdl/pg.jar "$url"
  [ -n "$SHA" ] || SHA=$(curl -fsS --max-time 30 "$url.sha256")
  echo "$SHA  .tmp/pgdl/pg.jar" | sha256sum -c -
  (cd .tmp/pgdl && unzip -o -q pg.jar "$TXZ")
  mkdir -p .tmp/pg && tar -xJf ".tmp/pgdl/$TXZ" -C .tmp/pg
}

case "${1:-}" in
  start)
    fetch
    if [ ! -f .tmp/pgdata/PG_VERSION ]; then
      "$BIN/initdb$EXE" -D .tmp/pgdata -U postgres -A trust -E UTF8 --no-locale > .tmp/initdb.log
    fi
    "$BIN/pg_ctl$EXE" -D .tmp/pgdata -o "-p $PORT -c listen_addresses=127.0.0.1" -l .tmp/pg.log -w -t 60 start > .tmp/pg_ctl.log 2>&1 < /dev/null
    echo "OSM_TEST_DATABASE_URL=postgres://postgres@127.0.0.1:$PORT/postgres?sslmode=disable"
    ;;
  stop)
    if [ -f .tmp/pgdata/postmaster.pid ]; then
      "$BIN/pg_ctl$EXE" -D .tmp/pgdata -m fast -w -t 60 stop
    else
      echo "not running"
    fi
    ;;
  *) echo "usage: $0 start|stop" >&2; exit 2 ;;
esac
