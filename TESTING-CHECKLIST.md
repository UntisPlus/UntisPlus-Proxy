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
- [ ] 2.11 **key + password retained** — after a `getAppSharedSecret` (or password `authenticate`) call, `dumpcreds <db>` prints both `key` and `password` for the account; a later TOTP-only login (`getUserData2017`) does not clear the stored password

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

## 15. Technik / custom events (v1.7.0)

Signed in as an admin. Pick a student who has real lessons on the chosen date.

### Writing

- [ ] 15.1 `/admin` → *Student events*: pick a school, pick a student from the
      list, enter a date, `14:00`–`15:00`, a title, pick a room and set a teacher
      → **add**
- [ ] 15.1a the student picker lists only students of the chosen school (no
      teacher accounts); the room picker lists **all** of that school's rooms
      (a school with hundreds of rooms still offers *Aula* and *CSS* — the list
      is not cut off at 60), and the room filter box narrows it
- [ ] 15.1b select **two or more students** and **two rooms**, add once → one
      event per student, each carrying both rooms; the table shows it for a
      picked student
- [ ] 15.1c type in the student filter *after* picking → the ticks stay on the
      picks (narrowing the filter must not drop a selection)
- [ ] 15.1d switch to a different school → picks and the room list follow the
      new school rather than keeping the old one's
- [ ] 15.1e a batch naming one student who is not in the school → the whole batch
      is refused and **nothing** is written for the others
- [ ] 15.1f the automated equivalent: `node scripts/check-admin-ui.js` passes
      (also run by `scripts/release.sh` when node is installed)
- [ ] 15.1g add the same student, date and time a second time → the add is
      reported as *already had that slot*, the table still shows **one** row, and
      that student's `eventVersion` does not move
- [ ] 15.1h add a batch where one student already has the slot → that student is
      reported as skipped and the others still get the event
- [ ] 15.1i leave the **subject** field empty and add → the event is stored with
      the default subject *Technik*; the events table's subject column shows it,
      and the app shows *Technik* as the lesson unless you typed another subject
- [ ] 15.1j the app **places** the event rather than dropping it: the served
      period names `SUBJECT`, `ROOM` and `TEACHER` elements, and `masterData`
      carries a catalogue entry for each name so the app can resolve them
- [ ] 15.1k paste the `technikkalender` JSON into the **Import Technik calendar**
      box and import → each appointment creates one row per named technician; the
      summary reports how many were added
- [ ] 15.1l import the same JSON again → every slot is reported as already
      present and nothing new is written
- [ ] 15.1m a code that names no technician (a teacher, *SISS Technik*) is listed
      under *skipped non-technicians* and creates no event
- [ ] 15.1n a technician who has not signed up is listed under *no account yet*;
      after that username first logs in, their appointments appear with no second
      import
- [ ] 15.1o choose the exported **CSV** file and import → it decodes the
      Windows-1252 umlauts (a room *Büro* shows as *Büro*, not mojibake) and
      creates the same events
- [ ] 15.1p an existing **teacher** code in the import (e.g. the CSV's teacher
      codes) is skipped rather than filed as a student event
- [ ] 15.1q `POST /api/admin/events/import` as a **non-admin** → 401/403
- [ ] 15.1r pick two rooms (*Aula* and *CSS*) → both are added to the one event as
      `Aula, CSS`; the same works by typing `Aula, CSS` in the *room not in the
      list* box
- [ ] 15.2 the table lists it with revision 1
- [ ] 15.3 the same student again after an edit → the revision reads 2 (each edit
      increments it)
- [ ] 15.4 **edit** → the row's values fill the form above (date, start/end,
      title, subject, room, teacher, description and the student); change the time
      to `16:00`–`17:00` and **save** → the row updates. Nothing was hidden behind
      a `prompt()` popup
- [ ] 15.4a in the edit form, click a **different student** and **save** → the
      event moves to that student: it leaves the old student's table and appears
      under the new one. Both students' `eventVersion` rises (the old client drops
      it, the new one picks it up)
- [ ] 15.4b clear the **description** and **save** → the description is actually
      removed (the form submits every field it shows, so a cleared field stays
      cleared)
- [ ] 15.5 **del** the row → it disappears; deleting again is not an error
- [ ] 15.6 add with the end before the start → refused, nothing written
- [ ] 15.7 add for a username that does not exist → refused with *no such user*
- [ ] 15.8 the events list is sorted by date, then start time

### Seeing them as the student

- [ ] 15.9 as the **student**, open the timetable in the app → the event appears in
      the week with the *Termin* marker and the time you entered
- [ ] 15.10 the entry is on the **correct day and time** — the app shows the
      school-clock time you typed, not a shifted one
- [ ] 15.10a put the event on a date **inside a school holiday** (e.g. the autumn
      break) → the app still shows it on that day: the proxy splits the holiday
      range in `masterData.holidays` around the event date so the day is not
      hidden, and raises `masterData.timeStamp` so a caching client notices
- [ ] 15.10b a holiday with no event on it is left untouched — its range is
      unchanged. (`masterData.timeStamp` may still rise, because the synthesized
      subject/room/teacher catalogue is a masterData change a caching client has
      to see; that is expected.)
- [ ] 15.11 the entry is visually distinct from a real lesson (marker); its cell
      shows the event's **description** (what the appointment needs), the
      assigned student(s) on the substitution line, and the event's **title** on
      the **Lesson topic** line of the detail view — the subject ("Technik") is
      not in the cell
- [ ] 15.12 subject, room and teacher show exactly as typed, with the subject
      defaulting to *Technik* when left empty
- [ ] 15.12a grant a student the **boosted** flag, add them an event, and open
      their own timetable in the app → the event still appears. The boosted path
      forwards the timetable raw from a teacher account, so before the fix the
      events were skipped entirely; a *different* class or student's timetable the
      boosted user can read must still carry **no** event
- [ ] 15.13 open `/week/{your student token}` → the event appears there too
- [ ] 15.14 subscribe `/api/calendar/{your student token}.ics` and refresh the
      calendar → one new all-day-or-timed entry, with `SUMMARY` carrying
      title · subject · teacher
- [ ] 15.15 the `.ics` `UID` starts with `custom-` and `SEQUENCE` is 1
- [ ] 15.16 **edit** the event, then refresh the subscribed calendar → the entry
      updates in place (the UID is unchanged, `SEQUENCE` rose to 2). A client that
      keeps the old text means `SEQUENCE` is not moving
- [ ] 15.17 **delete** the event, refresh the calendar → the entry is gone after a
      full re-sync. An entry that survives a *second* refresh is stale client cache
      (documented, not a bug)

### Privacy

- [ ] 15.18 as the **student**, fetch the `CLASS` timetable for your class → the
      response contains no `isCustom` period
- [ ] 15.19 subscribe a **class** token's `.ics` → it contains no event
- [ ] 15.20 subscribe a **teacher**, **room** and **subject** token → none carries
      an event
- [ ] 15.21 as the **second student**, your own timetable and `/week/{your token}`
      show none of the first student's events
- [ ] 15.22 as an **editor** or **teacher** account, no event appears on any
      response you can fetch
- [ ] 15.23 `/api/admin/events` as a **non-admin** → 401/403 on GET, POST, PATCH
      and DELETE
- [ ] 15.24 `/api/admin/events` without `username` → 400, not everyone's events

### Being told about a change

- [ ] 15.25 note your current `eventVersion` from `/api/timetable/changes`
- [ ] 15.26 as the admin, edit the student's event → the student's next
      `/api/timetable/changes?sinceEvents=<noted>` returns 200 with a **higher**
      `eventVersion`, not 304
- [ ] 15.27 repeat with an unchanged `eventVersion` → 304
- [ ] 15.28 as the admin, **delete** the event → `eventVersion` rises again
- [ ] 15.29 with `/api/timetable/stream` open as the student → an
      `event: student-events` frame arrives, with a `reason` and **no event text**
- [ ] 15.30 as the **second student**, the same admin edit produces no
      `student-events` frame and no `eventVersion` change
- [ ] 15.31 no event change appears on the shared **ntfy topic** or **webhook**
      destinations configured for the class

### The Technik aggregate class

Events stay per-student on every surface above. A user holding the **techniker**
permission additionally sees one synthetic class, *Technik*, whose timetable is
the union of every student's events in the requested range. The permission is
independent of admin/boosted/editor/recon and is checked on every request.

- [ ] 15.32 `/admin` → *Users*: the **techniker** button toggles the permission;
      granting or revoking takes effect on the user's next request, without a
      re-login (the class list and the aggregate both follow)
- [ ] 15.33 with the permission **off**, the *Technik* class is absent from the
      app's class list, and a `getTimetable2017` for `type=CLASS` naming the
      aggregate id returns `-8509` (no right) rather than an empty timetable
- [ ] 15.34 with the permission **on**, the *Technik* class appears in the class
      list next to the real classes; opening it shows **every** student's events
      in the window, each row carrying the event's description on the lesson line
      and every assigned student on the substitution line (the subject is not in
      the cell)
- [ ] 15.35 a second student **without** the permission sees no *Technik* class
      and cannot fetch it
- [ ] 15.36 a **boosted** user who also holds techniker still gets the aggregate:
      it is answered by the proxy itself, never forwarded to the teacher account
- [ ] 15.37 the aggregate is not offered to a **class/teacher/room/subject**
      calendar token (`/api/calendar/...ics`, `/week/...`): those feeds remain
      event-free
- [ ] 15.38 `/admin` → *Room map*: entries are keyed by the room text an event
      carries (the import's `Ort` column), not by a catalogue room name. Type an
      imported name (e.g. `Aula`) into the *event room text* field or pick one of
      the suggestions, choose the real *Untis room* with the picker, then *add /
      update* → the pair is saved (event room text → Untis room id) and the text
      drops out of the suggestions; *refresh* / reload and it persists per school.
      An entry here wins over the school catalogue; a text in neither is still shown

### Events on a room or organiser timetable

The techniker permission also unlocks the events where they happen: an event
whose room resolves to the requested room id, or whose teacher resolves to the
requested teacher id, is added to that room's / teacher's timetable. A viewer
without the permission gets exactly the old response — the events never leak.

- [ ] 15.39 as a **techniker**, open a room that an event names: the event
      appears on the room timetable even when recon never enabled that room, in
      the school's timezone, with the description and the assigned student; a
      room no event names stays as it was
- [ ] 15.40 as a **techniker**, open the organiser's (teacher's) timetable: the
      event appears there too, matched by the resolved teacher id
- [ ] 15.41 the same room request from a user **without** the permission returns
      the former `-8509` (recon off) and never the events
- [ ] 15.42 a **boosted** techniker (the common case) sees the events on the raw
      teacher-forwarded room/teacher timetable as well, not only on the
      reconstructed one

- [ ] 15.43 an event staffed by several students (the import writes one row per
      assigned person) shows **once** on the Technik class and on its room /
      organiser timetable, with all assigned students listed together, not once
      per person

### Absence register on an event (technikers)

A hand-authored event is not a real Untis lesson, so it has no upstream absence to
write. For a **techniker** the proxy still offers the app's absence register on the
event, and records every tick in its own store. Nothing here ever reaches Untis.
Grant *techniker* to a student account (15.32) before testing.

- [ ] 15.44 as the **techniker**, open an event on the Technik class (or their own
      timetable if they are an assigned student) → the absence row is present: the
      period's `can` carries `READ_STUD_ABSENCE` and `WRITE_STUD_ABSENCE`
- [ ] 15.45 open the register on a **real lesson** → it carries neither capability.
      The same account, viewing a real period, must not be offered the local
      register (the absence there would be a real one)
- [ ] 15.46 a user **without** the techniker permission, opening an event → no
      `READ/WRITE_STUD_ABSENCE`; the register never appears
- [ ] 15.47 mark an assigned student **absent** and submit → the tick persists:
      reopening the event (or reloading) still shows them absent
- [ ] 15.48 an event staffed by **two students**: mark one absent → only that
      student is ticked; the other is untouched. (`getPeriodData2017` for the
      synthetic id lists both, `absences` lists one)
- [ ] 15.49 **undo** the absence (delete) → it disappears and stays gone after a
      reload; deleting again is not an error
- [ ] 15.50 the app remembers the register was **submitted**: reopening the event
      shows `absenceChecked` true after `submitAbsencesChecked2017`
- [ ] 15.51 as the **admin**, `/admin` → *Event absences*: pick the school → the
      appointment appears, one row per absent student, naming the student and who
      recorded it
- [ ] 15.52 the *Event absences* date window narrows the list; leaving it empty
      shows the whole register
- [ ] 15.53 `/api/admin/events/absences` as a **non-admin** → 401/403; without a
      session → 401
- [ ] 15.54 confirm nothing was written to Untis: as the affected student, fetch
      their real `getStudentAbsences2017` → the event absence is **not** among them
- [ ] 15.55 a **real** period from the same techniker account (`getPeriodData2017`
      with a real `ttId`, or `deleteAbsence2017` with a real `absenceId`) is
      forwarded to Untis unchanged — the proxy answers only ids in its synthetic
      ranges

#### Absence register dashboard, changelog and correction

The admin page shows, per appointment, who was **absent** and who was
**present**, whether the register was **submitted**, and an append-only log of
every change. The log cannot be edited or deleted; the only correction path is
the admin **edit**, which is itself logged.

- [ ] 15.56 `/admin` → *Event absences*: each appointment lists its assigned
      students, marking exactly the absent ones (and their window) and the
      present ones; the *checked* column reads *yes* only after
      `submitAbsencesChecked2017` (15.50)
- [ ] 15.57 the *Change log* table lists every mark, undo, edit and submit,
      **newest first**, naming the student, the action and who did it; the date
      window (15.52) narrows both tables
- [ ] 15.58 there is **no** control to edit or delete a log line, and
      `DELETE /api/admin/events/absences/log` is not served (405) — the history
      is the point
- [ ] 15.59 click **edit** next to an absent student → change the window and
      **save** → the register shows the new window; reload the page and it holds
- [ ] 15.60 edit an absence and move it to **another assigned student** → the
      first is now present and the second absent; the app shows the same after a
      reload (`getPeriodData2017`)
- [ ] 15.61 each edit adds a new *edited* line to the changelog, with the old
      window/student in the detail — the earlier lines are untouched
- [ ] 15.62 `PATCH /api/admin/events/absences` as a **non-admin** → 401/403;
      without a session → 401; a move to a student **not** assigned to the
      appointment is rejected and nothing changes

## 16. Packaging
- [ ] 16.1 `/status` reports the published tag, and **not** `dev` — a plain
      `go build` reports `dev`, so `dev` means the version was never compiled in
- [ ] 16.2 `docker images <your-namespace>/untisplus-proxy` shows the release tag +
      `latest` (pushed)