#!/usr/bin/env bash
# Release gate + ship: format check, vet, build, full test run, admin UI
# runtime check, then push one release tag for GitHub to test and publish.
#
# Usage:
#   ./scripts/release.sh                 # test everything, then push
#   VERSION=v1.4.1 ./scripts/release.sh  # explicit image/version tag
#   PUSH=0 ./scripts/release.sh          # just the local gate (compile+test)
#
# Env overrides:
#   The release workflow owns the image repository and multi-platform publishing.
#   VERSION     release tag (default: exact git tag, else "dev" for PUSH=0).
#               Compiled into the binary, so /status reports it without anyone
#               having to set an environment variable that can disagree.
#   PUSH        set to 0 to skip release-tag submission
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

VERSION="${VERSION:-$(git describe --tags --exact-match 2>/dev/null || echo dev)}"
PUSH="${PUSH:-1}"

if [ "$PUSH" = "1" ]; then
  if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$ ]]; then
    echo "error: shipping requires an explicit release tag such as VERSION=v2.0.0" >&2
    exit 2
  fi
  if [ -n "$(git status --porcelain)" ]; then
    echo "error: commit changes before creating a release" >&2
    exit 2
  fi
  if git rev-parse --verify "refs/tags/$VERSION" >/dev/null 2>&1 &&
     [ "$(git rev-parse "refs/tags/$VERSION^{commit}")" != "$(git rev-parse HEAD)" ]; then
    echo "error: release tag already names a different commit" >&2
    exit 2
  fi
fi

echo "==> [1/6] go vet ./..."
go vet ./...

echo "==> [2/6] go build ./..."
go build ./...

echo "==> [3/6] go test -count=1 ./..."
go test -count=1 ./...

# The Go tests cannot execute the admin page's JavaScript, so a bug there only
# shows up in a browser. This runs admin.html's script against a small DOM shim
# and drives the event pickers. It is not a hard requirement of the build, so a
# machine without node warns and continues rather than blocking the release.
if command -v node >/dev/null 2>&1; then
  echo "==> [4/6] node scripts/check-admin-ui.js"
  node scripts/check-admin-ui.js
else
  echo "==> [4/6] admin UI check skipped (node not installed)"
fi

# The database holds every account secret, calendar token and notification
# subscription, and the next image start migrates its schema. Take the copy
# before anything is pushed, not after.
if [ -n "${BACKUP_DB:-}" ]; then
  echo "==> [5/6] backup ${BACKUP_DB}"
  go run ./cmd/untisctl -db "$BACKUP_DB" backup --keep "${BACKUP_KEEP:-7}"
else
  echo "==> [5/6] backup skipped (BACKUP_DB unset — set it to protect the live data)"
fi

if [ "$PUSH" != "1" ]; then
  echo "==> [6/6] push skipped (PUSH=0) — local gate passed"
  exit 0
fi

echo "==> [6/6] submit ${VERSION} to the tested multi-platform release workflow"
if ! git rev-parse --verify "refs/tags/$VERSION" >/dev/null 2>&1; then
  git tag "$VERSION"
fi
git push origin "refs/tags/$VERSION"
echo "submitted ${VERSION}; GitHub CI must pass before publishing images and the release"
