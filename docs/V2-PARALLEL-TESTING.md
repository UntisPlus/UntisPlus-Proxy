# UntisPlus-Proxy v2.0 Parallel Test Deployment

> Historical checklist/design reference. Checked boxes and implementation claims
> below are not evidence that the current beta passed those checks. For the
> 2026-11-23 assessment, see [Production readiness](PRODUCTION-READINESS.md).

This document describes how to run **v2.0 in parallel** with an existing v1
deployment (e.g. current NAS/production instance) for testing. v2 is a breaking
parallel deployment: it uses v2 access/permission semantics and should run as a
separate instance with its **own database, port, domain/public URL, and
container/network identity**. **Do not point v2 at the v1 production database and
do not modify the live v1/NAS deployment.**

## What v2 changes (high level)

- Per-school upstream-detected anonymous behavior (`school_anon_profiles`) with
  admin/CLI refresh/override; anonymous sessions remain non-persisted and cannot
  mint calendar tokens; anonymous access is stock/upstream-detected only (no
  proxy pooled/reconstructed/boosted-raw supplements).
- Fine-grained capability/group/role-default/grant/preference model with
  effective resolution (`server_enabled && admin_allowed && user_opted_in`).
- `/me` (authenticated real accounts) returns fine-grained effective state and
  accepts user preference updates within the admin-allowed/toggleable ceiling.
- Pooled/reconstructed/boosted-raw access requires authenticated real in-school
  account + effective permission (+ coverage where applicable).
- Admin/CLI v2 management surfaces (`/admin/v2/*`, `untisctl v2 ...`) for gates,
  groups/grants, user grants/groups, role defaults, person-type role mappings,
  anon profiles, and effective/"why" inspection.

See `docs/V2-PERMISSIONS-SPEC.md` for the design baseline and current status.

## Prerequisites

- A separate host port and (recommended) separate subdomain/domain for v2.
- A separate persistent data directory/volume so v2 uses its own SQLite DB
  (default `untis.db` inside the data dir).
- Access to the target Untis school login name(s) for testing.
- Go toolchain/build environment matching the repo if building from source, or a
  v2 image/tag built from branch `V2`/`dev`.

## Recommended parallel config

Use values distinct from v1 for all of the following:

- `UNTIS_PORT` (or published host port): e.g. `8510` instead of v1's `8509`
- Data/DB path: separate directory, e.g. `./data-v2:/data` or `UNTIS_DB=/data/untis-v2.db`
- `UNTIS_PUBLIC_BASE`: v2 test domain/base URL if used for notification/calendar links
- Container name / compose project name: e.g. `untis-proxy-v2`
- `UNTIS_ENV`: `dev`/`beta` for test as appropriate
- `UNTIS_SCHOOL`: same test school login name is fine if intentional, but data/state
  remains isolated in the v2 DB

Do **not** share the v1 DB file or v1 data volume.

## Example: Docker Compose (parallel v2)

Create a separate env file (e.g. `.env.v2`, gitignored) and a parallel compose
invocation. Example environment values:

```env
UNTIS_IMAGE_REPO=your-namespace/untisplus-proxy
UNTIS_PORT=8510
UNTIS_SCHOOL=your-school-login-name
UNTIS_ENV=beta
UNTIS_POLL_INTERVAL=60s
UNTIS_NTFY_BASE=https://ntfy.sh
UNTIS_PUBLIC_BASE=https://untis-v2.example.test
```

If using `compose.yaml`-style local build/run, point the volume to a separate
directory (example override idea; adjust to your setup):

```yaml
# compose.v2.yaml (example sketch)
services:
  untis-proxy-v2:
    build: .
    image: untis-proxy:v2-test
    container_name: untis-proxy-v2
    restart: unless-stopped
    ports:
      - "${UNTIS_PORT:-8510}:8509"
    environment:
      UNTIS_ADDR: ":8509"
      UNTIS_SERVER: "${UNTIS_SERVER:-}"
      UNTIS_SCHOOL: "${UNTIS_SCHOOL:?set UNTIS_SCHOOL}"
      UNTIS_DB: "/data/untis-v2.db"
      UNTIS_ENV: "${UNTIS_ENV:-beta}"
      UNTIS_POLL_INTERVAL: "${UNTIS_POLL_INTERVAL:-60s}"
      UNTIS_NTFY_BASE: "${UNTIS_NTFY_BASE:-https://ntfy.sh}"
      UNTIS_PUBLIC_BASE: "${UNTIS_PUBLIC_BASE:-}"
    volumes:
      - ./data-v2:/data
```

Run (example): `docker compose -f compose.v2.yaml --env-file .env.v2 up -d --build`

> Adjust image/build/network/reverse-proxy settings to match your environment.
> Keep v1 services/networks/volumes untouched.

## Running from source (local parallel test)

```bash
export PATH="/path/to/go/bin:$PATH"
mkdir -p ./data-v2
./bin/untis-server -addr :8510 -db ./data-v2/untis-v2.db -env dev -school your-school-login-name
# or: go run ./cmd/server -addr :8510 -db ./data-v2/untis-v2.db -env dev -school your-school-login-name
```

Use a different port and DB path than any v1 instance on the same host.

## First-time v2 test checklist

1. Start v2 instance and confirm `/health` and `/status`.
2. Log in with a real test account (not anonymous) to seed/migrate identity as needed.
3. Open `/admin` (admin session required) and/or use `untisctl` to inspect v2 state:
   - `untisctl v2 effective <school> <username>` (explain/why)
   - `untisctl v2 groups list [school]`, `untisctl v2 grants <school> <user>`
   - `untisctl v2 user-groups <school> <user>`
   - `untisctl v2 role-defaults list [school] [role]`
   - `untisctl v2 gates list`, `untisctl v2 person-type-roles list [scope]`
   - `untisctl v2 anon-profile list|get|refresh|override ...`
4. Configure/verify per-school anonymous profile detection/override and capability
   behavior for the test school.
5. Validate authenticated real-user flows: `/me` effective state and preference
   toggles (within ceiling), timetable/pool/recon/masterData access per grants,
   subscriptions/changes/streams, calendar mint/serve/re-authorization, notify
   delivery authorization, and technik/event surfaces as applicable.
6. Confirm "no accessible/exposed targets" paths fail appropriately where expected.
7. Promote to wider testing only after verifying behavior on the isolated v2
   instance; cut over by switching the test domain/users to v2 when ready.

## Notes

- Migration from v1 data is idempotent where implemented, but parallel testing
  should start from a clean v2 DB unless you explicitly test migration/import on
  a copied (never live) database.
- Anonymous `#anonymous#` is never persisted as a DB user row.
- Outage verification pings: `Options.OutageVerifyPings` / `-outage-verify-pings` /
  `UNTIS_OUTAGE_VERIFY_PINGS` (default 5, `0` disables extra verification).
- If you rename branch `V2` to `dev` for ongoing work, update remote tracking and
  CI/deploy references accordingly; keep v1 deployment untouched.
