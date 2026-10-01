# TESTING-CHECKLIST — full API surface walk-through (v1.6.0)

Interactive runbook. Every box gets pushed through live; tick it once the
expected result holds off this machine. Report anything that deviates.

Target: `http://127.0.0.1:8787` (the tunnel hostname, set `UNTIS_PUBLIC_BASE`). DB has
WAL+sessions in-memory — a server restart drops sessions, re-login is fine.

## Reference data (this DB)

| What | Value |
|---|---|
| School | `testschool` |
| Account | `jdoe` (key, person 5005, class 5000 10aR, **admin+boosted**) |
| Students | `kkoehler` 5007/5001, `bschneider` 5008/5002, `asmith` 5006/5003 |
| Teacher | `tmueller` = Willner Stefanie (person 5009, class 5004) |
| Pool classes | 5002 09cGym · 5003 09eGym · 5000 10aR · 5004 10bGym · 5001 10bSISS |
| Room | 169 "01-Aula" |
| Subject | id 1 "M" |
| Calendar tokens (exist) | STUDENT 5009 `327219fe…`, STUDENT 5005 `c12ca702…`, ROOM 169 `2d5805e9…` |

Helper to mint a live OTP for a user (`jdoe` admin):
```sh
OTP=$(python3 /tmp/opencode/otp.py jdoe | head -c 6)
```
or just read a code off your authenticator.

Login (stores cookies in `jar`):
```sh
curl -s -c jar -H 'Content-Type: application/json' -X POST \
  'http://127.0.0.1:8787/WebUntis/jsonrpc_intern.do?m=getUserData2017&school=testschool' \
  -d '{"id":"t","jsonrpc":"2.0","method":"getUserData2017","params":[{"auth":{"user":"jdoe","otp":"<OTP or base32 secret>","clientTime":'"$(date +%s%3N)"'}}]}' -o /tmp/opencode/login.json
head -c 120 /tmp/opencode/login.json
```

---

## 0. Environment & git readiness
- [x] 0.1 `git status` clean and `git log --oneline -3` shows the last commit
- [x] 0.2 New binary running: `curl -s http://127.0.0.1:8787/status` → `"version":"v1.4.0"`

## 1. Server basics
- [x] 1.1 `GET /status` → 200 JSON `{mode,status:"ok",uptime_sec,version:"v1.4.0"}`
- [x] 1.2 `GET /does-not-exist` → 404
- [x] 1.3 `GET /` → 404
- [x] 1.4 `GET /me` **without** session → error (401 / JSON error), not data
- [x] 1.5 `GET /admin` without session → 401 (not the dashboard HTML)

## 2. Auth & accounts (`/WebUntis/jsonrpc_intern.do`)
All: `?m=getUserData2017&school=testschool`, body auth `{user, otp, clientTime}`.

- [x] 2.1 **OTP login** `jdoe` with live 6-digit code → 200, `result.masterData`, cookies `JSESSIONID` + `schoolname` set
- [x] 2.2 **Pasted-secret login** `jdoe` with `<REDACTED-TOTP-SEED>` in `otp` → same success (proxy derives the code)
- [x] 2.3 **Wrong OTP** (e.g. `000000`) → `error.code -8504 bad credentials`
- [x] 2.4 **Wrong pasted secret** (`BBBB…`×16) → upstream error; afterwards `untisctl users list` still shows jdoe's original secret (not clobbered)
- [x] 2.5 **Unknown user** login → error, no user row created
- [x] 2.6 **getAppSharedSecret** (app flow) forwards upstream result (needs the account's real Untis password; with TOTP secret as password the upstream rejects `-899` — pass-through correct)
- [x] 2.7 `GET /me` (with jar) → `{"username":"jdoe","level":"boosted","permissions":{…}}`
- [x] 2.8 **logout** (`/WebUntis/jsonrpc.do` `logout`) → session invalid: `/me` after → error
- [x] 2.9 **anonymous login** `#anonymous#` → session works, but **no** user row / pool row appears in `untisctl users list`
- [ ] 2.10 **password-method** `m=authenticate` on `/WebUntis/jsonrpc.do` — needs an account whose Untis password we know (none seeded; skip/register one if you have creds)

## 3. Timetables — JSON-RPC intern (`?m=getTimetable2017`)
Session required (reuse `jar` from §2.1). Param shape:
`params[0] = {type, id, personId, classId, date, startDate, endDate, auth:{user,clientTime}}`.

> **26-Sep-26 results:** all `getTimetable2017` requests return HTTP 200 + a JSONRPC
> result and the proxy rewrites/auth-forwards faithfully (direct-vs-proxy identical,
> incl. the upstream's OTP requirement in the auth block). **Upstream is currently
> serving EMPTY timetables for everyone** (even tmueller' own TEACHER view, and even for
> a virgin auth account) — same result direct to `testschool.webuntis.com` — so "real
> periods" could not be eyeballed today; the request/forward/rewrite mechanics pass.

- [x] 3.1 own **STUDENT** timetable (personId 5005) → 200 + result forwarded (boosted→tmueller)
- [x] 3.2 own **CLASS** 5000 → same (upstream empty today; direct comparison identical)
- [x] 3.3 another **pooled class** 5002 (owner bschneider) → same
- [x] 3.4 another **student** personId 5006 (asmith) → same
- [x] 3.5 **non-pooled class** 99999 as Basic → `-8509 no right for timetable`
- [x] 3.6 **TEACHER** 5009 as **Basic** student → gated `-8509`; nothing leaks
- [x] 3.7 **ROOM** 169 as **Basic** student → gated `-8509`
- [x] 3.8 **SUBJECT** id 1 as **Basic** student → gated `-8509`
- [x] 3.9 **date range** `startDate`/`endDate` → request passes with the range intact (result today empty)
- [x] 3.10 **missing/invalid param** (no `params`) → `-8502 no username specified`, not a panic/5xx
- [x] 3.11 **legacy** `?m=getTimetable` on `/WebUntis/jsonrpc.do` class 5000 → **forwarded faithfully**; upstream rejects current dates (`startDate` binds to an `int` that overflows — legacy quirk, MS dates also rejected). Real timetables come from the REST weekly endpoint (§5), which the app uses.
- [x] 3.12 `getOwnData` / `getLatestImportTime` on public `/WebUntis/jsonrpc.do` → `-32601 Method not found` (intentional)

> **Fixed this pass (walk-through bugs):**
> - self-auth forwards now send the target-school `schoolname` cookie → info-center
>   methods return the upstream verdict (`-32601 Method not found`) instead of the
>   wrong `-8500 invalid schoolname` (`Client.SchoolCookie` + `RawIntern` call sites).
> - `escalatedREST` now also refreshes a dead cached session when upstream replies
>   with the flat REST body `{"errorCode":"FORBIDDEN","errorMessage":"…anonymous…"}`
>   (extended `untis.IsAuthFailure`); boosted/teacher REST forwards were 403-ing until
>   the 8-min session cache expired.

## 4. Info-center / self-auth methods (auth block per request)
These are forwarded when the body carries an `auth` block; otherwise need a session.

- [x] 4.1 own **absences** (e.g. `getAbsences`) with auth block → forwarded with schoolname cookie; upstream: `-32601 Method not found` (method not implemented at this endpoint)
- [x] 4.2 **events / messages / exams** with auth block → forwarded identically (same upstream verdict)
- [x] 4.3 **no auth block, no session** → `-8520 not logged in`
- [x] 4.4 **write method** (`setAbsence`) as non-editor (jdoe) → `-32601 method not allowed`
- [ ] 4.5 write method as **editor** (after §6.4) → forwarded upstream (no change made)

## 5. REST API (`/WebUntis/api/…`)
Weekly data needs a `date` (upstream 500s without it). All `…&date=2026-09-21` got **real periods** (class 5000 → 87).
- [x] 5.1 class 5000 via session → 200 JSON with real periods (needs `date`, else upstream 500 — app always sends it)
- [x] 5.2 teacher 5009 as **Basic** (asmith, no recon) → proxy 403 `{"error":"forbidden"}`; as **boosted** (jdoe) → 200 real periods
- [x] 5.3 room 169 / subject 1 (boosted) → 200 with real element data
- [x] 5.4 own **absence GET** (`/api/absence/1`) → forwarded, upstream 404 JSON (real answer, not gated)
- [x] 5.5 **absence/write POST** as Basic AND as non-editor → 403 gate; as editor → forwarded (after §6.4)
- [x] 5.6 **Bearer token** self-auth: forwarded as-is (bogus token → upstream 200 login HTML; GET only, write verbs 403'd by proxy)
- [x] 5.7 anon on weekly + other REST paths → 403 `{"error":"forbidden"}`

## 6. Permission tiers (untisctl against `data/untis.db`)
Use a throwaway user (`testtier`) — create, then remove at the end.
- [ ] 6.1 fresh user = **Basic**: teacher/room/subject timetables forbidden (§3.6 repeat with this user)
- [ ] 6.2 grant `recon` → teacher/room/subject now served (with pool present)
- [ ] 6.3 grant `boosted` → **recon auto-revoked** (XOR) and teacher/room/subject raw-served
- [ ] 6.4 grant `editor` → write methods unlocked, `/me` shows `editor:true`
- [ ] 6.5 `perms clear` → back to Basic; `perms reset` → all global overrides off
- [ ] 6.6 `/me` level reflects each change (basic→recon→boosted)

## 7. Calendar subscriptions
- [ ] 7.1 POST `/api/calendar/token` `{"classId":5000}` → token URL
- [ ] 7.2 POST `{"personId":5005}` (own) → student token; **another person** (5006) → 403
- [ ] 7.3 POST `{"teacherId":5009}`, `{"roomId":169}`, `{"subjectId":1}` when recon/boosted → tokens; Basic → 403
- [ ] 7.4 `GET /api/calendar/{token}.ics` → 200, `text/calendar`, starts `BEGIN:VCALENDAR`
- [ ] 7.5 `{"timezone":"America/New_York"}` → ICS carries that TZ
- [ ] 7.6 `{"days":7}` → shorter feed
- [ ] 7.7 **re-POST same target** → same token (idempotent), not a new one
- [ ] 7.8 revoke via admin/untisctl → subsequent ICS GET → 401/404
- [ ] 7.9 tokens list shows `elementType`/`elementName`/`createdBy`/`createdAt` in `/admin/tokens`

## 8. Change detection & push
- [ ] 8.1 `GET /api/timetable/changes?since=0` → JSON with versions + full history
- [ ] 8.2 `?since=<current version>` → `304 Not Modified`
- [ ] 8.3 `GET /api/timetable/stream` → `event: snapshot` then `: heartbeat` every 30s
- [ ] 8.4 (needs a real edit in Untis) `event: change` fires within a poll interval and `/changes?since=N` reconciles

## 9. Webhooks & ntfy (admin + self-service)
- [ ] 9.1 `POST /admin/webhooks` `{"classId":5000,"url":"https://e/h","secret":"s"}` → id; listed in dashboard with class name
- [ ] 9.2 add **element-targeted** hook `{"elementType":"ROOM","elementId":169,…}` → listed with name "01-Aula"; same for `STUDENT`/`TEACHER`/`SUBJECT`
- [ ] 9.3 `POST /api/webhooks/{id}/test` → delivery arrives with `X-Untis-Signature: sha256=…` + `X-Untis-Summary`
- [ ] 9.4 `POST /admin/ntfy` `{"topic":"untis","elementType":"STUDENT","elementId":5005}` → listed w/ element name; `/api/ntfy/{id}/test` publishes to the topic
- [ ] 9.5 **self-service** `GET /api/ntfy` as plain student → only own class/student topics; school-wide/room topics hidden
- [ ] 9.6 **legacy** `classId`-only rows still work (dashboard shows them as class targets)
- [ ] 9.7 delete hook/topic → gone from list, test 404s
- [ ] 9.8 **durable delivery**: with a webhook pointed at an unreachable URL, let
      the poller detect a change → `GET /admin/outbox` shows `pending ≥ 1` with
      the failing destination, and `/admin/status` agrees; a *healthy* webhook
      subscribed to the same change still receives it on the first attempt and
      does **not** get it again when the broken one retries
- [ ] 9.9 **retry then dead**: leave the broken URL alone → `pending` drops and
      `dead` becomes 1 after 8 attempts (~15m+ with backoff); `GET /admin/outbox`
      names the school/class/destination and the error; the healthy destination
      is unaffected
- [ ] 9.10 **survives restart**: queue a delivery to a broken URL, restart the
      process → the delivery is retried, not stranded
- [ ] 9.11 delete the broken webhook → its queued backlog clears instead of
      retrying forever
- [ ] 9.12 `/healthz` and public `/status` still expose **no** per-school or
      per-destination detail

## 10. Admin dashboard (browser: `<public-host>/admin`)
- [ ] 10.1 anon → 401; bschneider (non-admin) session → 403
- [ ] 10.2 admin (jdoe) → dashboard renders, all sections load
- [ ] 10.3 `/admin/status` counts match `untisctl status`
- [ ] 10.4 `/admin/search?q=lin` → shows student `Oth Linus` (STUDENT 5006), **not** Willner Stefanie
- [ ] 10.5 tokens tab: create with element dropdown incl **student**, edit lookahead days
- [ ] 10.6 webhooks + ntfy tabs: type dropdown + element picker; add/delete round-trips
- [ ] 10.7 `/admin/schools` lists testschool; `/admin/recon` shows 1747 elements
- [ ] 10.8 create a token then revoke it from the UI

## 11. untisctl CLI (against `data/untis.db`)
- [ ] 11.1 `status` → 5 users / 5 classes / 3 tokens etc.
- [ ] 11.2 `users list`, `users check jdoe` (asks real API for the class)
- [ ] 11.3 `perms grant/revoke/clear` round-trips
- [ ] 11.4 `pool list`, `pool owners`
- [ ] 11.5 `calendar create --class 5004`; `calendar list`; `calendar revoke <token>`
- [ ] 11.6 `calendar student --user asmith` → interactive picker lists DB students
- [ ] 11.7 `totp tmueller` → current code + seconds left; `totp tmueller --scan` → `otpauth://` + QR (if qrencode)
- [ ] 11.8 `users admin asmith --on` then `--off` round-trips via dashboard status (`/admin/users`)

## 12. Multi-school
- [ ] 12.1 `POST /admin/schools/otherschool` → added; `/admin/schools` lists both schools
- [ ] 12.2 `/admin/pool/otherschool` → isolated (empty ≠ testschool's 5)
- [ ] 12.3 auto-register-on-first-login is unit-covered (`TestMultiSchoolAutoRegisterAndPoolIsolation`) — no live second school to trigger

## 13. Homework done flags (v1.5.0)

Set `COOKIE=<your JSESSIONID>` and a homework `id` taken from a real
`getHomeWork2017` response.

- [ ] 13.1 `POST /WebUntis/jsonrpc_intern.do?school=<school>&m=getHomeWork2017`
      with the session cookie → every `homeWorks[]` entry has `done` and `doneAt`
- [ ] 13.2 pick an entry with `completed: true` → `done` is `false` and `doneAt`
      is `null`. The two fields are independent; `completed` is the teacher's
- [ ] 13.3 `POST /api/homework/done {"homeworkId":<id>,"done":true}` → 200; refetch
      13.1 → that entry now has `done: true` and a `doneAt`
- [ ] 13.4 the same entry's `completed` is **unchanged** by 13.3 — the proxy must
      never write the teacher's field
- [ ] 13.5 `GET /api/homework/flags` → lists only your flags, sorted by id
- [ ] 13.6 `POST /api/homework/done {"homeworkId":<id>,"done":false}` → flag
      disappears from `/api/homework/flags`; the homework entry reads
      `done: false, doneAt: null`
- [ ] 13.7 repeat 13.3 and 13.6 twice each → still exactly one row, no duplicates
- [ ] 13.8 `POST /api/homework/done` with `{"username":"<someone-else>",
      "homeworkId":<id>,"done":true}` → your own flag is written; the named user's
      `/api/homework/flags` is unchanged. The body `username` is ignored
- [ ] 13.9 both endpoints without the cookie → 401
- [ ] 13.10 `POST /api/homework/done` with `{"done":true}` (no id) and with
      `{"homeworkId":<id>}` (no `done`) → 400, and nothing is written
- [ ] 13.11 log in as a second student → their `done` is `false` for homework you
      marked done (no cross-user leak)
- [ ] 13.12 `getPeriodData2017` for a period that has homework → its `homeWorks[]`
      entries carry the same `done`/`doneAt`
- [ ] 13.13 as an **editor** with a boosted teacher source, open the lesson editor
      (`getPeriodData2017`) → response contains no `done` fields at all, because
      that data is the teacher's
- [ ] 13.14 `GET /api/timetable/stream` and a `getTimetable2017` fetch → byte-identical
      to upstream; homework flags must not appear there

## 14. Absence notes (v1.6.0)

Set `COOKIE=<your JSESSIONID>` and an `absenceKey` (the `id` of a real entry) from
a `getStudentAbsences2017` response. Remember the method needs
`includeExcused`/`includeUnExcused`, or it returns an empty list.

- [ ] 14.1 `getStudentAbsences2017` with the include flags → every entry has a
      `derived` block with `date` and `weekday`
- [ ] 14.2 a lesson-scoped absence (e.g. 10:00–10:45) → `derived.subject` names
      that lesson
- [ ] 14.3 a **whole-day** absence → `derived` has **no** `subject` key at all
      (it covers several lessons; a guess would be wrong and undetectable)
- [ ] 14.4 an absence from before the polling window → still has `date`, still no
      `subject`
- [ ] 14.5 `derived.className` and `derived.reason` are readable text, not the raw
      numeric id / 2-digit code
- [ ] 14.6 the upstream `text` is the **teacher's** comment and is unchanged;
      `excuse.text` is unchanged too
- [ ] 14.7 `POST /api/absence/notes {"absenceKey":<id>,"note":"bring workbook"}`
      → 200; refetch 14.1 → that entry has `note` + `noteUpdatedAt`
- [ ] 14.8 `GET /api/absence/notes` → lists only your notes, sorted by key
- [ ] 14.9 the same POST with `"note":""` → clears it; the absence entry loses the
      `note` field and `/api/absence/notes` no longer lists it
- [ ] 14.10 repeat the write twice → still one note, no duplicates
- [ ] 14.11 POST with a `"username":"<someone-else>"` → **your** note is written and
      the named student's notes are unchanged
- [ ] 14.12 both endpoints without the cookie → 401
- [ ] 14.13 POST with `{"note":"x"}` (no key) → 400 and nothing written
- [ ] 14.14 as a **second student**, fetch your own absences → `note` is absent;
      the other student's note must not appear
- [ ] 14.15 as an **editor** with a boosted teacher, open the class register
      (`getPeriodData2017`) → the response contains **no** `note` and no
      `noteUpdatedAt`, even though you have notes on your own absences

## 15. Packaging
- [ ] 15.1 `/status` reports the published tag, and **not** `dev` — a plain
      `go build` reports `dev`, so `dev` means the version was never compiled in
- [ ] 15.2 `docker images <your-namespace>/untisplus-proxy` shows the release tag +
      `latest` (pushed)