#!/usr/bin/env bash
# Build and (re)start the proxy detached from the calling terminal.
# The DB path is explicit on purpose: the server's own default is ./untis.db,
# which silently starts a fresh empty database when started from the repo root.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# Local deployment config (school name, public base URL, ntfy server, …) lives
# in a gitignored .env, so the repository carries no real school's identity and
# no personal hostname. The server reads these as ordinary UNTIS_* env vars.
if [[ -f .env ]]; then
	set -a
	# shellcheck disable=SC1091
	source .env
	set +a
fi

db="${UNTIS_DB:-data/untis.db}"
log="bin/untis-server.log"
bin="bin/untis-server"

if [[ -z "${UNTIS_SCHOOL:-}" ]]; then
	echo "UNTIS_SCHOOL is not set. Copy .env.example to .env and set it to your" >&2
	echo "school's WebUntis login name (it is the key all data is stored under)." >&2
	exit 1
fi

mkdir -p bin
go build -o "$bin" ./cmd/server

pids() { pgrep -x "$(basename "$bin")" || true; }

if [[ -n "$(pids)" ]]; then
	kill $(pids)
	for _ in $(seq 1 20); do
		[[ -z "$(pids)" ]] && break
		sleep 0.25
	done
	[[ -n "$(pids)" ]] && kill -9 $(pids) || true
fi

setsid "$bin" -db "$db" >>"$log" 2>&1 </dev/null &

for _ in $(seq 1 40); do
	code="$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8509/healthz || true)"
	[[ "$code" == "200" ]] && break
	sleep 0.25
done

echo "pid=$(pids | head -1) db=$db school=$UNTIS_SCHOOL health=${code:-000} log=$log"
