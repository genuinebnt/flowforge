#!/usr/bin/env bash
# devpg.sh: start or stop a throwaway local PostgreSQL cluster for the
# integration tests (FF_TEST_PG_DSN). Not for production use.
#
#   scripts/devpg.sh start   # prints the DSN to export
#   scripts/devpg.sh stop
#   scripts/devpg.sh status
#
# Environment: FF_DEVPG_DIR (default: ${TMPDIR:-/tmp}/flowforge-devpg),
# FF_DEVPG_PORT (default 55432), PG_BIN (default /usr/lib/postgresql/16/bin).
set -euo pipefail

PG_BIN="${PG_BIN:-/usr/lib/postgresql/16/bin}"
DIR="${FF_DEVPG_DIR:-${TMPDIR:-/tmp}/flowforge-devpg}"
PORT="${FF_DEVPG_PORT:-55432}"
DSN="postgres://postgres@127.0.0.1:${PORT}/postgres?sslmode=disable"

pgctl() {
	# initdb and pg_ctl refuse to run as root; drop to the postgres user if needed.
	if [ "$(id -u)" = "0" ]; then
		chown -R postgres "$DIR"
		su postgres -s /bin/bash -c "$(printf '%q ' "$@")"
	else
		"$@"
	fi
}

case "${1:-}" in
start)
	mkdir -p "$DIR"
	if [ ! -f "$DIR/pg/PG_VERSION" ]; then
		pgctl "$PG_BIN/initdb" -D "$DIR/pg" -U postgres --auth=trust -E UTF8 >"$DIR/initdb.log"
	fi
	pgctl "$PG_BIN/pg_ctl" -D "$DIR/pg" -o "-p $PORT -k $DIR -c listen_addresses=127.0.0.1 -c max_connections=200" \
		-l "$DIR/pg.log" -w start >/dev/null
	echo "export FF_TEST_PG_DSN='$DSN'"
	;;
stop)
	if [ -f "$DIR/pg/postmaster.pid" ]; then
		pgctl "$PG_BIN/pg_ctl" -D "$DIR/pg" -m fast -w stop >/dev/null
	fi
	if [ "${2:-}" = "--purge" ]; then rm -rf "$DIR"; fi
	;;
status)
	pgctl "$PG_BIN/pg_ctl" -D "$DIR/pg" status || true
	;;
*)
	echo "usage: $0 start|stop [--purge]|status" >&2
	exit 2
	;;
esac
