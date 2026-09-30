# Manual QA checklist — untis-proxy beta

Tick every box. Anything that fails is a bug; note it under the section and
report it with the exact command + response.

- **Beta base URL:** `http://localhost:8509`
- **Server process:** `pgrep -a untis-server` · **log:** `tail -f bin/untis-server.log`
- **DB:** `data/untis.db` (live beta data — do not delete rows)
- **Backup first:** `go run ./cmd/untisctl -db data/untis.db backup`

Legend: `[ ]` untested · `[x]` passed · `[!]` failed (write what you saw)

---

## 0. Prerequisites

- [ ] `curl -s localhost:8509/status` → `{"mode":"prod","status":"ok",...,"version":"beta-2026-09-28"}`
- [ ] Pick a calendar token for the ICS/week tests: `go run ./cmd/untisctl -db data/untis.db calendar list`
- [ ] Write it into a shell var: `export TOK=<token>` (and `export CLASS=<classId>` for the same row)
- [ ] Admin session: open `http://localhost:8509/admin/login`, sign in with an admin username + 6-digit TOTP
      (get the code with `go run ./cmd/untisctl -db data/untis.db totp <adminuser>`)
- [ ] Copy the `JSESSIONID` cookie from the browser devtools into `export ADMINCOOKIE="JSESSIONID=..."`
- [ ] Student session: sign in through the app or `/admin/login` as a normal user, copy their cookie into `export USERCOOKIE="JSESSIONID=..."`

---

## 1. Liveness and operations (no auth)

These three are the only endpoints that need no auth, and two of them are
deliberately thin. The per-school view — school name, class count, scan progress,
poll counters — is not on them any more; it is on the gated listener, because it
was being published on whatever address a tunnel exposes.

- [ ] `GET /status` → 200, `status:"ok"`, uptime counting up, and `version` is the
      published tag rather than `dev` (a plain `go build` reports `dev`; that is
      the signal that the version was not compiled in)
- [ ] `GET /healthz` → **200**, and the body is the aggregate only:
      `status:"ok"`, `uptime_sec`, `schools` as a **count**, `degraded` as a count
- [ ] `GET /healthz` contains **no school name** and no `pollRuns` / `classes`
      keys. This is the regression check for the leak: if a name shows up here it
      is back on the public address
- [ ] `GET /metrics` → **404**. The endpoint is not served unless `-metrics-addr`
      is set, so a 404 is the passing result
- [ ] Break the upstream (or stop the school server), wait for a poll to fail →
      `GET /healthz` → **503** with `degraded` ≥ 1. The public probe must still
      reach the verdict after the detail was stripped, or a broken proxy would
      look healthy to a healthcheck
- [ ] With `-metrics-addr 127.0.0.1:9109` set, `GET 127.0.0.1:9109/healthz` → the
      full per-school view: `school`, `classes`, `scanScanned`, `scanStale`,
      `scanTarget`, `pollRuns`, `pollChanges`, `pollFails: 0`,
      `lastPollAgoSec` < ~90, `lastSuccessfulPollAgoSec` < ~90, `degraded: false`,
      no `reasons`
- [ ] `GET 127.0.0.1:9109/metrics` → 200, `Content-Type: text/plain`, and contains
      all of: `untis_up 1`, `untis_uptime_seconds`, `untis_school_degraded{school=…} 0`,
      `untis_pool_classes`, `untis_recon_classes_scanned`, `untis_recon_classes_stale`,
      `untis_poll_runs_total`, `untis_poll_changes_total`, `untis_poll_failures_total`,
      `untis_poll_last_age_seconds`, `untis_poll_last_success_age_seconds`
- [ ] The gated listener serves nothing but those two: `GET 127.0.0.1:9109/admin`,
      `/status`, `/`, `/metrics` twice → 404 for everything except `/metrics` and
      `/healthz`
- [ ] `GET 127.0.0.1:9109/healthz` twice, 65s apart → `pollRuns` increased by the
      number of pooled classes
- [ ] `tail -20 bin/untis-server.log` → no panic, no repeated errors

## 2. Admin dashboard

- [ ] `GET /admin` **without** cookie → 401
- [ ] `GET /admin` with non-admin cookie → 403
- [ ] `GET /admin` with `ADMINCOOKIE` → 200, dashboard HTML renders (no JS console errors)
- [ ] Dashboard sections all load: status, users, perms, pool, tokens, schools, search, webhooks, ntfy, recon
- [ ] Every element picker (class / student / teacher / room / subject) fuzzy-searches and selects
- [ ] Logout (or clear cookie) → `/admin` 401 again

## 3. Admin JSON API (with `ADMINCOOKIE`)

- [ ] `GET /admin/status` → counts for users, admins, pool, schools, tokens, webhooks, ntfy, recon
- [ ] `GET /admin/users` → list; `POST /admin/users` add `{username, admin:false}`; appears in list
- [ ] `POST /admin/users/<name>` set admin true → then `/admin/status` admin count +1
- [ ] `POST /admin/users/<name>` grant feature `{feature:"recon", allowed:true}`; `{feature:"editor"}`; `{feature:"boosted"}`; revoke again
- [ ] `DELETE /admin/users/<throwaway>` → user gone
- [ ] `GET /admin/perms` → rows match `untisctl perms list`
- [ ] `GET /admin/pool` and `GET /admin/pool/testschool` → `{school, pool:[{id,name,owner}]}`; compare with `untisctl pool list`
- [ ] `GET /admin/tokens` → all tokens; `DELETE /admin/tokens/<throwaway>` revokes (see §5 for 404 after)
- [ ] `GET /admin/schools` → list; `POST /admin/schools/<newscho>` → `{"registered":true}`
- [ ] `GET /admin/search?q=<partial name>` → hits; add `&type=TEACHER` (CLASS/TEACHER/ROOM/SUBJECT/STUDENT) to filter
- [ ] `GET /admin/recon` → counts; `scan.target` set; `scan.perClass` has one row per pooled class
- [ ] The teacher/room/subject counts here match what the app offers: with `recon`, the app's element
      pickers must list exactly these elements (e.g. ~90 teachers / ~71 rooms at testschool, not all 526/484).
      An element that is selectable but loads an empty week is a bug — cross-check the two lists.
- [ ] `POST /admin/recon/rescan` → 200, then within ~30s the log shows a full sweep and
      `GET /admin/recon` shows a new `scanTarget` reached
- [ ] `POST /admin/recon/rescan` with a non-admin cookie → 403

## 4. Calendar tokens (session required)

- [ ] `GET /me` with `USERCOOKIE` → user info + permission level (`basic`/`recon`/`boosted`/`admin`)
- [ ] `GET /me` without cookie → 401
- [ ] `POST /api/calendar/token` `{"classId":$CLASS}` → token + `.ics` url (own class)
- [ ] Same request again → **same token** (idempotent)
- [ ] `{"personId":<other student>}` → 403
- [ ] `{"personal":true}` → own personal token
- [ ] `{"teacherId":<id>}` / `{"roomId":<id>}` / `{"subjectId":<id>}` → 403 for plain user, 200 for recon/boosted/admin
- [ ] `{"timezone":"Asia/Tokyo"}` honoured: `DTSTART;TZID=Asia/Tokyo` in the feed and shifted wall-clock times
- [ ] `{"classId":0}` / two targets in one body → 400
- [ ] `POST /api/calendar/token` without cookie → 401

## 5. iCal feed (`$TOK`)

- [ ] `curl -sD- localhost:8509/api/calendar/$TOK.ics -o /tmp/feed.ics` → 200, `Content-Type: text/calendar`
- [ ] Header block: `BEGIN:VCALENDAR`, `VERSION:2.0`, `PRODID`, `CALSCALE:GREGORIAN`,
      `X-WR-CALNAME`, `X-WR-CALDESC`, `END:VCALENDAR` (there is no `X-WR-TIMEZONE`; the zone is on every event)
- [ ] Every `VEVENT` has `UID`, `DTSTAMP`, `SEQUENCE`, `DTSTART;TZID=`, `DTEND;TZID=`, `SUMMARY`
- [ ] `UID` values are unique; `grep -c 'BEGIN:VEVENT' = grep -c 'END:VEVENT'`
- [ ] `SEQUENCE` is an integer and **changes** after a timetable change (re-run and compare)
- [ ] Commas/semicolons/newlines in subject names are escaped (`\,` `\;` `\n`) — inspect a lesson with a long subject
- [ ] Lines are CRLF-terminated, and no line exceeds 75 octets **before** folding (`awk 'length($0)>75 && $0 !~ /^ /' /tmp/feed.ics | head`)
- [ ] Import into a calendar app (Google/Outlook/Apple): events appear with correct times and timezone
- [ ] `GET /api/calendar/does-not-exist.ics` → 404
- [ ] Revoke a token (`DELETE /admin/tokens/$TOK`), then the feed → 404 (revoke a throwaway, then re-create)

## 6. Week page (`$TOK`)

- [ ] `GET /week/$TOK` → 200, `text/html`, no `Cache-Control: public`
- [ ] Class name as `<h1>`, weekday sections Mon..Sun, **today** highlighted with the word `heute`
- [ ] Each lesson row: time range, subject (long name), teacher, room
- [ ] Cancellation/substitution/exam markers appear when the school server reports them (`entfällt`, `Vertretung`, `Klausur`)
- [ ] Window really is today..today+6: a lesson on the 7th day shows, a lesson from last Monday does not
- [ ] Parallel lessons (two subjects at the same time in different rooms) render as two rows
- [ ] Token timezone honoured (the `America/New_York` token shows shifted times + names its zone)
- [ ] Long day truncation not needed here, but check no lesson is missing when >8 lessons exist in a day
- [ ] Upstream HTML in a subject/room name is escaped, not rendered (ask the school to push `<script>`; or trust the automated test)
- [ ] `GET /week/does-not-exist` → 404 plain text, no HTML
- [ ] Page is readable in dark **and** light mode, and usable at 360px width
- [ ] `/week/$TOK.ics`-style suffix is tolerated (`/week/$TOK.html` also renders)

## 7. Change detection, diff API and digest

Getting a real change: the poller diffs every 60s, so the surest way is to watch until the school
publishes something. `GET /api/timetable/changes?since=0` replays everything still stored, which is
the fast way to inspect the digest.

- [ ] `GET /api/timetable/changes?since=0` with `USERCOOKIE` → JSON with `version`, `changes[]`, `summary`, `message`
- [ ] `?since=<current version>` → **304 Not Modified**
- [ ] `?classId=<someone else's class>` → 403
- [ ] without cookie → 401
- [ ] `summary` names the target and counts: e.g. `class 5000 - 2 changes: 1 added - 1 changed`
- [ ] `message` spells lessons out in parent language: `Mon 1. Std 08:00-08:45  Mathematik 3 (A. Hartley, R204)  new`
- [ ] Room move renders as `room R204 -> R112`; time move shows both old and new time
- [ ] `summary` is pure ASCII (no `·`, `ä`, `→`); `… and N more` footer on big reshuffles
- [ ] `GET /api/timetable/stream?school=testschool` (SSE) → connects, `event: change` arrives on a real change, heartbeat present, clean disconnect on `Ctrl-C`
- [ ] Stream with a non-matching class cookie does not leak other classes' changes

## 8. Webhooks

- [ ] `POST /admin/webhooks` school-wide `{school, url:"http://<receiver>"}` → appears in `GET /admin/webhooks`
- [ ] `POST /admin/webhooks` per-class `{elementType:"CLASS", elementId:$CLASS}`
- [ ] `POST /admin/webhooks` per-teacher/room/subject
- [ ] `POST /admin/webhooks/<id>/test` → `{"ok":true,"status":200}` and the receiver logs a POST with
      `X-Untis-Event: test`, `X-Untis-Summary`, and (if a secret is set) `X-Untis-Signature: sha256=…`
- [ ] Verify the HMAC yourself: `sha256(secret, body)` equals the header
- [ ] A real change to a matching target delivers: body `event: change`, `changes[].Kind` ∈ `ADDED|CHANGED|REMOVED`,
      headers `X-Untis-Event: timetable-change` and `X-Untis-Summary: <one-line digest>` (there is no
      multi-line header — the full digest text is only in the ntfy message, see §7/§9)
- [ ] A change to a **non-matching** class does not fire that hook
- [ ] Unreachable URL (point at a closed port) → poll loop keeps running, `pollFails`/`poll_runs` unaffected,
      retries visible in the log, no panic
- [ ] `DELETE /admin/webhooks/<id>` → gone from the list
- [ ] `POST /api/webhooks` self-service with `USERCOOKIE` for own class → 200; for another class → 403
- [ ] `DELETE /api/webhooks/<other user's id>` → 403

## 9. ntfy

- [ ] `POST /admin/ntfy` `{school, topic:"<scratch topic>"}` (per-topic `baseUrl` honoured)
- [ ] `POST /admin/ntfy/<id>/test` → `{"ok":true,"status":200,...}`; the phone receives the test push
- [ ] Real change → push with a parent-readable body (the full multi-line digest) and a title naming the element
- [ ] Element-scoped topic fires only for its element; school-wide fires for everything
- [ ] Wrong `baseUrl` → error reported, no crash, other topics still work
- [ ] `GET /api/ntfy` as a plain user lists own-class topics only; as admin lists all
- [ ] `DELETE /admin/ntfy/<id>` → gone
- [ ] Subscribe with the ntfy Android app and confirm a change reaches the phone

## 10. App-facing proxy (what the Untis app does)

- [ ] `POST /WebUntis/jsonrpc.do` `authenticate` with password → session cookie, `personId`
- [ ] `authenticate` with a 6-digit TOTP (key account) → 200
- [ ] `authenticate` with a pasted base32 **secret** → 200 (app setup flow fallback)
- [ ] Wrong password / wrong OTP → upstream-style error, no session, log shows the reason
- [ ] `POST /WebUntis/jsonrpc_intern.do` `getUserData2017` → user, class, master data, and the
      `displayable` flags for classes/teachers/rooms/subjects
- [ ] Same call with a key account + OTP → works (this is what the admin login uses)
- [ ] `getTimetable2017` (intern) → today's periods for the session's class
- [ ] `getAppSharedSecret` → key material for a key account
- [ ] `POST /WebUntis/jsonrpc.do` `logout` → subsequent calls 401
- [ ] `GET /WebUntis/api/public/timetable/weekly/data?...type=1` (class) → the week
- [ ] `type=2` teacher / `type=3` subject / `type=4` room → reconstructed week from pooled data
- [ ] Same four types as a user **without** recon → 403/empty, and non-pooled elements never resolve
- [ ] Weekly data for a class outside the pool → 403
- [ ] The pool is derived from users: `untisctl users add --user <u> --secret <s>`, then
      `untisctl users check --user <u>` learns their class → the class shows up in
      `untisctl pool list` / `GET /admin/pool` and becomes viewable with no restart
- [ ] `?school=<other>` on a login/REST call → that school's data, not this one's
- [ ] App reconnects after a proxy restart: login again, data returns (no stuck sessions)

## 11. Multi-school

- [ ] `POST /admin/schools/<newscho>` → registered, appears in `GET /admin/schools` and in `/healthz`
- [ ] New school gets its own pool/recon/counters in `/healthz` and `/metrics`
- [ ] Data of school A never appears under school B (pool, tokens, timetables, webhooks)
- [ ] First login from an unknown school auto-registers it (log: `auto-registered school "…"`)
- [ ] `schoolname` cookie is honoured and survives a restart

## 12. Recon

- [ ] `GET /admin/recon` → `scan.scanned == scan.classes`, `scan.stale == 0`, target in the future
- [ ] Restart the server → log says classes already scanned, **no** full re-fetch (`classes already scanned through …, nothing to do`)
- [ ] `POST /admin/recon/rescan` → full sweep, and element counts stay correct afterwards (no elements lost)
- [ ] Reconstruct a teacher/room/subject feed after a restart (ICS + weekly REST) → same data as before
- [ ] Boot with `-recon-refresh 1` and a stale horizon → only the stale classes are re-scanned (log line counts)
- [ ] Element that no longer appears in any pooled class disappears after a **full** rescan (pickers stay honest)
- [ ] Recon student opens a pooled class they do **not** attend → timetable shows, and the teacher/room/subject
      pickers stay populated afterwards (a pooled class view must not replace the catalog with an all-false one)
- [ ] Lesson topic shows for a recon user on class, teacher, room and subject views (every period carries
      `READ_LESSONTOPIC`; without it the app hides the topic line even though the text is in the payload)
- [ ] `boosted` user: periods carry **no** write/delete capability and the app shows no editing affordances,
      while teacher/room/class data stays complete (still forwarded through a teacher account)
- [ ] `boosted` user attempting a write (`setAbsencesList`, `putLessonInfo`, …) → `method not allowed`, nothing
      forwarded upstream
- [ ] Grant `editor` (implies `boosted`): the write capabilities come back in `can` and the write is forwarded
- [ ] Proxy **admin** without `editor` still gets no write capabilities (admin ≠ editor)
- [ ] Revoke `editor` → write capabilities disappear again without a re-login
- [ ] Same pooled class viewed by the boosted owner vs the recon student → identical periods, same lessons/times/rooms

## 13. Backup and restore drill

- [ ] `go run ./cmd/untisctl -db data/untis.db backup` → new `.db.untis-backup-*.db` next to the live file
- [ ] `--keep 2` prunes the oldest automatic backups and never touches other files
- [ ] `go run ./cmd/untisctl -db <backup> status` → same user/pool/token counts as the live DB
- [ ] Restore drill: stop the server, copy the backup over `data/untis.db`, remove stale `-wal`/`-shm`,
      start again, log in → everything intact
- [ ] `backup --out /some/existing/file` → refuses, does not overwrite
- [ ] `BACKUP_DB=data/untis.db PUSH=0 ./scripts/release.sh` → gate passes and writes a backup

## 14. Failure drills (the important ones)

- [ ] Block upstream (wrong `-server`) → polls fail, snapshots **unchanged** (no mass "removed" notifications), log shows timeouts
- [ ] `/healthz` flips to 503 with `the most recent class poll failed` (or `no class poll has ever succeeded` on a fresh DB)
- [ ] Unblock upstream → `/healthz` returns to 200 within a poll cycle, no duplicate/stale notification burst
- [ ] Kill the server mid-poll → restart → resumes cleanly, no duplicate notifications
- [ ] `untis.db` read-only → server logs the error, keeps serving cached reads, `/healthz` reports it
- [ ] Disk filling up → writes fail with a clear error instead of corrupting the DB
- [ ] Massive timetable change (school-wide reshuffle) → one digest with a `… and N more` footer, not 300 lines

## 15. Security

- [ ] Calendar token is a password: anyone with `/week/$TOK` or the `.ics` URL can read that timetable
- [ ] `/healthz`, `/metrics`, `/status` leak no secrets (no usernames, no tokens, no upstream creds)
- [ ] Session cookie is `HttpOnly` + `SameSite`; session expires after `-ttl` (5m default)
- [ ] `../` and encoded traversal in `/week/…`, `/api/calendar/…` → 404
- [ ] XSS: subject/room/teacher names with `<script>`, `onerror=`, `javascript:` are escaped everywhere
      (week page, ICS escaping, admin UI, ntfy/webhook payloads)
- [ ] A revoked token stops working immediately for ICS, week page and app
- [ ] A user cannot read another user's personal timetable, tokens, or hooks
- [ ] Logins are not rate-limited (known gap, §16) — confirm at least that failures are logged

---

## 16. Known gaps — do not spend time looking for these

- No login rate limiting / lockout (brute force is only slowed by upstream).
- No delivery log: a failed webhook/ntfy push is not recorded, only logged.
- The admin **test** buttons send a synthetic payload **without** the digest
  (`X-Untis-Summary: test notification`), so they do not exercise §7/§8 digest rendering.
- No audit log of admin actions.
- Calendar tokens have no expiry and no rotation (revoke + re-create is the only way).
- No push (APNs/FCM) from the proxy itself — ntfy is the push path.
- `/week` shows one rolling week; there is no month view, no export, no ICS file download link in the UI.

## 17. Recommended additional automated tests

Ranked by value; each is cheap to add and covers a real gap.

1. **Digest delivery test** — assert a *real* delivery (not `/{id}/test`) carries the digest
   (today only the one-line `X-Untis-Summary` reaches webhooks) and that ntfy's message body
   equals `digest.Message`. Blocks the gap in §16.
2. **Make `/{id}/test` include a digest** — the test button should exercise the same
   rendering as production, otherwise every manual test of notifications is misleading.
3. **ICS validator test** — parse the generated feed with a real iCal parser
   (e.g. `github.com/rickb777/ical`) instead of substring checks: catches unfolding and
   escaping mistakes no `grep` will.
4. **Week-page golden test** — render a fixed week and diff against a golden HTML file
   (timezone, truncation, parallel lessons, escaping all in one).
5. **Auth-failure table test** — one table-driven test over `authenticate`: wrong password,
   wrong OTP, pasted secret, unknown user, missing user, revoked key — asserting the exact
   upstream error code for each.
6. **Weekly REST matrix** — all 4 element types × 4 permission tiers (basic/recon/boosted/admin)
   × pooled/unpooled, asserting status and payload shape. Currently only a few cells are covered.
7. **Multi-school isolation test** — two schools in one store, assert no cross-school leakage in
   pool, recon, tokens, weekly data and webhooks.
8. **Restart/resume integration test** — run `StartRecon` twice against one DB and assert the
   second run makes zero upstream calls (today only the async completion is awaited).
9. **Outage matrix** — upstream 500/502, empty body, JSON-RPC error body, slow response: each must
   leave the snapshot byte-identical and send zero notifications.
10. **Long-change truncation test** — 200 changed periods produce a digest with a
    `… and N more` footer and a bounded line count.
11. **Backup restore round-trip** — write data, back up, open the copy, compare table checksums
    (the current test only checks a couple of rows).
12. **Session/cookie test** — assert `HttpOnly`, `SameSite`, and expiry after `-ttl` (currently untested).
13. **`/metrics` format test** — parse the output with a Prometheus text parser to catch
    duplicate `HELP`/`TYPE` lines or unescaped label values (a school name with a quote would break it).
14. **Property test for the digest** — random period sets produce a digest whose counted kinds
    always match the changes array.

## 18. Today's changes — regression focus

- [ ] ICS `DTSTAMP` + `SEQUENCE` (feed imported by a real calendar app)
- [ ] `/week` page renders with real data and correct timezone
- [ ] Subject long names (`longName`, e.g. "Mathematik 3") show in ICS, week page, admin pickers
- [ ] Upstream outage no longer wipes a class snapshot or fires mass "removed" notifications
- [ ] Recon resumes instead of restarting; `/admin/recon` progress is accurate; `rescan` still forces a full sweep
- [ ] `/healthz` + `/metrics` reflect successful polls only
- [ ] `untisctl backup` produces a restorable copy; `scripts/release.sh` gate still green
