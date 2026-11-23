#!/usr/bin/env sh
# Disposable, offline restore/startup check for the built release image.
set -eu
runtime="${CONTAINER_RUNTIME:-docker}"
image="${1:?usage: check-container.sh IMAGE}"
volume="untis-smoke-$(date +%s)-$$"
"$runtime" volume create "$volume" >/dev/null
trap '"$runtime" volume rm "$volume" >/dev/null' EXIT
mount="$volume:/data"
if [ "$runtime" = podman ]; then mount="$mount:U"; fi
"$runtime" run --rm --network=none --read-only --cap-drop=ALL \
  --security-opt=no-new-privileges --tmpfs /tmp:rw,size=64m,mode=1777 \
  -v "$mount" "$image" sh -ec '
    test "$(id -u)" = 10001
    untisctl -db /data/live.db users admin --user restore-test
    untisctl -db /data/live.db backup --out /data/restored.db
    untisctl -db /data/restored.db users list | grep restore-test
    test "$(stat -c %a /data/restored.db)" = 600
    untis-server -db /data/restored.db -school isolated-test -server 127.0.0.1:9 \
      -env dev -addr 127.0.0.1:8509 -poll-interval 24h -outage-verify-pings 0 \
      >/tmp/server.log 2>&1 &
    server_pid=$!
    trap "kill $server_pid" EXIT
    for attempt in 1 2 3 4 5; do
      if wget -q -O /tmp/health http://127.0.0.1:8509/healthz 2>/dev/null; then
        cat /tmp/health
        exit 0
      fi
      sleep 1
    done
    cat /tmp/server.log
    exit 1
  '
