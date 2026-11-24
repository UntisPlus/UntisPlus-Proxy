#!/usr/bin/env sh
# Smoke-test a local image using a disposable volume and isolated networking.
set -eu
runtime="${CONTAINER_RUNTIME:-docker}"
image="${1:?usage: check-container.sh IMAGE [EXPECTED_VERSION]}"
expected_version="${2:-}"
volume="untis-smoke-$(date +%s)-$$"
"$runtime" volume create "$volume" >/dev/null
container=
cleanup() {
  if [ -n "$container" ]; then "$runtime" rm -f "$container" >/dev/null; fi
  "$runtime" volume rm "$volume" >/dev/null
}
trap cleanup EXIT
mount="$volume:/data"
if [ "$runtime" = podman ]; then mount="$mount:U"; fi
# Keep both containers under the same restrictions operators use.
set -- --network=none --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --tmpfs /tmp:rw,size=64m,mode=1777 -v "$mount"
"$runtime" run --rm "$@" "$image" sh -ec '
  test "$(id -u):$(id -g)" = 10001:10001
  untis-seed -db /data/live.db -creds /dev/null | grep -F "seeding 0 accounts"
  untisctl -db /data/live.db users admin --user restore-test
  untis-perm -db /data/live.db -user restore-test -type recon -grant | grep -F "granted"
  untisctl -db /data/live.db -school isolated-test admission-reset
  untisctl -db /data/live.db backup --out /data/restored.db
  untisctl -db /data/restored.db users list | grep -F restore-test
  test "$(stat -c %a /data/restored.db)" = 600
'
# Use the default image command and real environment parsing on the restored DB.
container=$("$runtime" run -d "$@" \
  -e UNTIS_DB=/data/restored.db -e UNTIS_SCHOOL=isolated-test \
  -e UNTIS_SERVER=127.0.0.1:9 -e UNTIS_ADDR=127.0.0.1:8509 \
  -e UNTIS_ENV=dev -e UNTIS_POLL_INTERVAL=24h -e UNTIS_OUTAGE_VERIFY_PINGS=0 "$image")
for attempt in 1 2 3 4 5; do
  if "$runtime" exec "$container" wget -qO- http://127.0.0.1:8509/healthz; then
    if [ -n "$expected_version" ]; then
      "$runtime" exec "$container" wget -qO- http://127.0.0.1:8509/status |
        grep -F "\"version\":\"$expected_version\""
    fi
    exit 0
  fi
  sleep 1
done
"$runtime" logs "$container"
exit 1
