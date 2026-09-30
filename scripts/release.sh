#!/usr/bin/env bash
# Release gate + ship: format check, vet, build, full test run, then Docker
# Hub push and GitHub push.
#
# Usage:
#   ./scripts/release.sh                 # test everything, then push
#   VERSION=v1.4.1 ./scripts/release.sh  # explicit image/version tag
#   PUSH=0 ./scripts/release.sh          # just the local gate (compile+test)
#
# Env overrides:
#   IMAGE       Docker Hub repository, e.g. <your-namespace>/untisplus-proxy.
#               Required when PUSH=1: there is deliberately no default, so a
#               release cannot be pushed to whichever account happens to be
#               logged in.
#   VERSION     tag pushed to Docker Hub (default: exact git tag, else "dev").
#               Compiled into the binary, so /status reports it without anyone
#               having to set an environment variable that can disagree.
#   PUSH        set to 0 to skip docker push / git push
#   BACKUP_DB   path to the live database; set it to take a VACUUM INTO backup
#               before shipping (upgrades run migrations that cannot be undone)
#   BACKUP_KEEP how many automatic backups to keep (default 7)
#
# The script is additive on purpose: DO NOT pre-patch the tag or README here,
# so it fails loudly and pinpoints exactly which step is broken.
#
# Note: the format check is intentionally NOT a gofmt gate — some Go toolchain
# builds rewrite `''` in comments to typographic quotes, which would produce
# spurious diffs. vet/build/test are the real gates.

set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE="${IMAGE:-}"
VERSION="${VERSION:-$(git describe --tags --exact-match 2>/dev/null || echo dev)}"
PUSH="${PUSH:-1}"

if [ "$PUSH" = "1" ] && [ -z "$IMAGE" ]; then
  echo "error: IMAGE is required to push, e.g. IMAGE=<your-namespace>/untisplus-proxy $0" >&2
  echo "       (there is no default: a release must not land in whichever account" >&2
  echo "        happens to be logged in). Use PUSH=0 for the local gate only." >&2
  exit 2
fi

echo "==> [1/5] go vet ./..."
go vet ./...

echo "==> [2/5] go build ./..."
go build ./...

echo "==> [3/5] go test -count=1 ./..."
go test -count=1 ./...

# The database holds every account secret, calendar token and notification
# subscription, and the next image start migrates its schema. Take the copy
# before anything is pushed, not after.
if [ -n "${BACKUP_DB:-}" ]; then
  echo "==> [4/5] backup ${BACKUP_DB}"
  go run ./cmd/untisctl -db "$BACKUP_DB" backup --keep "${BACKUP_KEEP:-7}"
else
  echo "==> [4/5] backup skipped (BACKUP_DB unset — set it to protect the live data)"
fi

if [ "$PUSH" != "1" ]; then
  echo "==> [5/5] push skipped (PUSH=0) — local gate passed"
  exit 0
fi

echo "==> [5/5] ship ${IMAGE}:${VERSION}"
echo "==> docker build"
docker build --build-arg VERSION="${VERSION}" -t "${IMAGE}:${VERSION}" -t "${IMAGE}:latest" .
echo "==> docker push"
docker push "${IMAGE}:${VERSION}"
docker push "${IMAGE}:latest"

branch="$(git branch --show-current)"
echo "==> git push origin $branch"
git push origin "$branch"
git push origin --tags

echo "done: ${IMAGE}:${VERSION} pushed (docker latest + ${VERSION}, branch ${branch})"