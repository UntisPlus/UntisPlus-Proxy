# untis-proxy feature checklist (fresh-DB test run)

Baseline: fresh empty DB, server on `:8787`. Backup: `data/backup-20260905-105720`.
Credentials (for reference/re-seed): `data/backup-20260905-105720/credentials.txt`

Checkboxes are clickable in GitHub/editor task lists. Tick them as you test.

## 1. Server & health
- [x] `/status` returns 200 + JSON
- [x] `/` and unknown paths 404
- [x] private routes 401 anonymous

## 2. Auth & accounts (needs app login — your phone, TOTP)
- [x] keyLogin as a student → session cookie (`JSESSIONID` + `schoolname`)
- [x] user row created, class donated to pool (pool grows)
- [x] `/me` returns the session user + permission level
- [x] password-method login — pasting the base32 shared secret in the `otp` field logs in (proxy derives the TOTP, presents the secret as the replay key, stores it only after the upstream accepts it; a rejected paste never clobbers a stored key). Covered by `TestKeyLoginPastedSecret`, `TestKeyLoginPastedSecretRejectedDoesNotClobber`, `TestKeyLoginLiveOTPUnchanged` and verified live.

## 3. Multi-school auto-register
- [x] first login from a fresh school auto-registers it (`schools` table + `/admin/schools`) — auto-register on first use in `stateFor`; covered by `TestMultiSchoolAutoRegisterAndPoolIsolation`
- [x] per-school pool stays isolated (otherschool's class never leaks into testschool's pool; `school=''` legacy accounts match any school)

## 4. Timetables (JSON-RPC, after a pool exists)
- [x] getTimetable2017 own class (both STUDENT and CLASS views)
- [x] getTimetable2017 another pooled class
- [x] getTimetable (legacy) class

## 4b. App JSON-RPC methods (login session)
- [x] messages/conversation
- [x] events
- [x] absences
- [x] exams

## 5. Recon (teacher/room/subject reconstruction)
- [x] boot/background enumeration fills recon (counts in `/admin/status` or `/admin/recon`)
- [x] reconstructed teacher timetable
- [x] reconstructed room timetable
- [x] reconstructed subject timetable
- [x] recon gating: user without recon gets forbidden

## 6. Permissions tiers (untisctl perms)
- [x] defaults: Basic for everyone
- [x] grant recon per-user → teacher/room/subject unlocked
- [x] grant boosted per-user → raw forwarding via saved teacher account (source: `tmueller`)
- [x] XOR rule: boosted auto-revokes recon (and vice-versa)
- [x] editor → write methods unlocked (level `boosted+editor`; live write left to app test)

## 7. Calendar subscription tokens
- [x] `POST /api/calendar/token` class → token
- [x] `GET /api/calendar/{token}.ics` → valid iCal
- [x] token for teacher/room/subject (fuzzy lookup — names persisted from masterData)
- [x] token revoke (`untisctl calendar revoke`)
- [x] `/me` shows level

## 8. Change detection & push
- [x] `/api/timetable/changes?since=N` returns diff (version bump + CHANGED rows; poll verified end-to-end)
- [x] SSE `/api/timetable/stream` emits snapshot + `event: change` (heartbeat every 30s)
- [x] webhook delivery (school-wide + per-class), HMAC-SHA256 `X-Untis-Signature` header
- [x] webhook + ntfy topics can target **any element** (class/student/teacher/room/subject) via the same searchable picker as calendar tokens; deliveries match a changed class by element (`elementMatch`: class id, student's class, or teacher/room/subject name in the changed rows; teacher names captured in the change snapshot)
- [x] ntfy topic publish on change (`-ntfy-base` for self-hosted)
- [x] per-topic ntfy server override (`baseUrl`, users pick their own ntfy server)
- [x] webhook + ntfy **test button** (`POST /api/webhooks/{id}/test`, `/api/ntfy/{id}/test`, and `/admin/...` equivalents; owner/admin-gated)
- [x] calendar tokens track who created them (`createdBy`) and what element/class (`elementName`, `createdAt` in `/admin/tokens`)
- [x] self-service `/api/webhooks` + `/api/ntfy` gating (own class vs privileged)

## 9. REST API
- [x] `/WebUntis/api/public/timetable/weekly/data` for class
- [x] same for teacher/room/subject (recon) + boosted raw
- [x] recon gating on REST (Basic → forbidden)
- [x] own absence read (REST GET + JSON-RPC info-center forwarded; upstream lacks these REST paths on this school, gate verified via other GETs)
- [x] editor-gated absence write (Basic REST POST -> proxy 403; editor -> forwarded upstream; JSON-RPC write methods gate -32601 for Basic, pass for editor)

## 10. Admin dashboard
- [x] bootstrap admin via `untisctl users admin`
- [x] `GET /admin` serves embedded HTML to admin session (401 anon, 403 non-admin)
- [x] `/admin/status` counts correct on fresh+populated state
- [x] `/admin/users` list + create + delete (promote/demote via POST/untisctl)
- [x] `/admin/perms`, `/admin/pool`, `/admin/tokens` (list+revoke)
- [x] `/admin/tokens` create + edit (lookahead days), per-element, idempotent
- [x] `/admin/search` fuzzy element picker (classes/teachers/rooms/subjects/**students**) — no raw ids in the UI
- [x] token create type dropdown includes **student** (search-backed user picker, person-id lookup)
- [x] `/admin/schools` (list + register)
- [x] `/admin/webhooks` + `/admin/ntfy` (add/list/delete)
- [x] `/admin/recon` stats
- [x] FIXED: keyLogin no longer wipes the admin flag / created_at on login

## 11. CLI (untisctl)
- [x] `users list` / `add` / `remove` / `admin` / `check`
- [x] `perms list` / `grant` / `revoke` / `clear` / `reset`
- [x] `pool list` / `owners`
- [x] `calendar create` / `student` / `list` / `revoke`, `tokens list` / `revoke`
- [x] `totp USER [--scan]` (current 6-digit code + seconds left, or `otpauth://` URI + ANSI QR via `qrencode` if installed; flags accepted in any position)
- [x] `status`

## 12. Packaging
- [x] `go build ./...`, `go vet`, `go test ./...` green
- [x] docker build of v1.4.0 — <your-namespace>/untis-proxy:v1.4.0` (also tagged `latest`); container smoke-tested (`/status` 200, empty fresh DB). Both tags **pushed to Docker Hub** (`docker push`, digest `84d5e1bd…`). NAS upgrade (see README): `docker compose -f compose.nas.yaml pull && up -d`.

---

### Notes
- `untisctl` binary available at `/tmp/untisctl` (rebuild with `go build -o /tmp/untisctl ./cmd/untisctl`), run against the live DB with `-db data/untis.db`.
- Fixed during this run: `users admin --off` flag now exists; a keyless session-only student can now load their own STUDENT and CLASS timetables (previously errored `-8509`).
- Fixed this run: stale upstream-cookie retries; keyLogin identity wipe (upstream returns cookie+`-8504` on wrong-OTP, old code upserted a zeroed row); displayAllowed/displayable now stamped explicitly and the recon registry is replaced (not merged) after scan so only pooled-reachable elements are selectable; SQLite switched to WAL (`busy_timeout=10000`) — a stale rollback journal + un-released tx could wedge the whole DB; `recon_elements` is now an upsert-only name store (login masterData populates it) for CLI fuzzy lookup.
- Seeded this run for the boosted tier: teacher account `tmueller` (secret `<REDACTED-TOTP-SEED>`).
- **Flag order gotcha:** the listen addr is the trailing positional — put ALL flags before it (`server -db x -addr :8787 -ntfy-base URL`, not `server -db x :8787 -flag`), because Go's flag package stops parsing at the first non-flag arg (a positional `:8787` silently swallowed `-poll-interval`/`-ntfy-base` during testing).
- Pool features (another class, change tracking, notifications) still need at least one account with a provisioned key: `untisctl users add --user X --secret <KEY>`.