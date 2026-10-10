# Security vulnerabilities, bugs, leaks and open feature wishes

Out-of-band review report. This file is gitignored on purpose: it contains
live-credential evidence (OTP values found in logs) and internal findings that
must not be committed. Anything here that is fixed, drop or update the entry.

Last review date: 2026-10-10.

## Table of contents

- [High](#high)
  - [C1 – Technik aggregate served on a self-asserted identity (no auth)](#c1--technik-aggregate-served-on-a-self-asserted-identity-no-auth)
  - [C2 – Request bodies logged verbatim: OTPs and app secrets leak to logs](#c2--request-bodies-logged-verbatim-otps-and-app-secrets-leak-to-logs)
  - [C3 – Data race on p.secrets -> process crash](#c3--data-race-on-psecrets---process-crash)
- [Medium](#medium)
  - [C4 – No HTTP timeouts and unbounded request bodies](#c4--no-http-timeouts-and-unbounded-request-bodies)
  - [M1 – Personal events reference a class non-techniker students cannot resolve](#m1--personal-events-reference-a-class-non-techniker-students-cannot-resolve)
  - [M2 – untisctl cannot grant/revoke the techniker permission](#m2--untisctl-cannot-grantrevoke-the-techniker-permission)
  - [M3 – notification_outbox dead rows never pruned (unbounded disk growth)](#m3--notification_outbox-dead-rows-never-pruned-unbounded-disk-growth)
  - [M4 – element_names grows forever and LookupElement full-table scans](#m4--element_names-grows-forever-and-lookupelement-full-table-scans)
- [Low](#low)
  - [L1 – Stale README: isCustom marker no longer emitted](#l1--stale-readme-iscustom-marker-no-longer-emitted)
  - [L2 – Dead Options.Admin field](#l2--dead-optionsadmin-field)
  - [L3 – technikClassID shares the synthetic-element id space](#l3--technikclassid-shares-the-synthetic-element-id-space)
  - [L4 – ttCache can exceed its 2000-entry cap](#l4--ttcache-can-exceed-its-2000-entry-cap)
- [Verified clean (checked, no action)](#verified-clean-checked-no-action)
- [Feature / UX wishlist](#feature--ux-wishlist)
- [Verification checklist](#verification-checklist)

Severity scale used here: High = privacy or credential breach; Medium =
availability/integrity risk or clear functional defect; Low = cosmetic, doc
drift, or negligible exposure.

## High

### C1 – Technik aggregate served on a self-asserted identity (no auth)

The synthetic "Technik" aggregate class answers `getTimetable2017` entirely from
the proxy. It is the one surface where every student's private events are served
at once, and the only thing standing between an unauthenticated client and all
of that data is a **username anyone can put in the request body**.

Evidence:

- `getTimetable2017` takes the requester from the JSON param:
  `requesterName := pr.Auth.User` (`internal/proxy/jsonrpc_intern.go:590`),
  falling back to the session cookie only when the body value is empty
  (`jsonrpc_intern.go:591-595`).
- The aggregate is answered before any upstream validation occurs
  (`jsonrpc_intern.go:606-613` -> `serveTechnikClass`,
  `internal/proxy/technik.go:131-174`). Every other serving branch forwards to
  the real server (`jsonrpc_intern.go:646-650`, `676-682`), which validates the
  `auth` block and rejects a forged identity. The aggregate does not.
- No route-level middleware guards `/WebUntis/jsonrpc_intern.do`
  (`internal/proxy/proxy.go:330-360`).
- The whole request handler just takes the username out of the body
  (`jsonrpc_intern.go:588-604`), and `serveTechnikClass` only gates on
  `hasTechniker(requester.Username)` (`technik.go:132-135`).

A working exploit request (username is discoverable — tests, README and the
logs below all name `techuser1`):

```
POST /WebUntis/jsonrpc_intern.do?school=example-school
{"id":1,"method":"getTimetable2017","params":[{
  "id":2000000000,"type":"CLASS",
  "startDate":"2026-09-28","endDate":"2026-10-04",
  "auth":{"user":"techuser1"}}]}
```

The live log confirms the app itself fetches the aggregate this way
(`bin/untis-server.log`), e.g.:

```
getTimetable2017","params":[{"id":2000000000,"type":"CLASS",...,"auth":{"user":"techuser1"}}]
```

Impact: any client that can reach `:8509` (default bind is all interfaces,
`cmd/server/main.go:30,141-145`; `compose.yaml:17-18` publishes the port) and
names a username holding the `techniker` permission receives every student's
private events: display names, dates, times, titles, descriptions, rooms. The
feature's whole privacy premise (events are keyed to one viewer only) is broken.

Why a trivial fix is not obvious: the app sends `auth.user` on some Technik
requests but **no `auth` block at all** on others (seen in the log), which means
the app does send the proxy `JSESSIONID` session cookie at least sometimes.
So binding identity to the session (options below) is viable, but the app flow
must be validated before forcing it. Re-validating a self-asserted `auth.user`
against upstream proves nothing — the attacker presents the same username, and
the proxy holds the secret to replay that user. Identity has to come from an
authenticator the caller actually holds (the login-created session cookie; a
session is only created after upstream accepts the OTP,
`internal/proxy/jsonrpc_intern.go:392-393`).

Recommended fix (pick one, verify against the app first):

1. For the aggregate branch only: require `p.sessionUser(r)` to be non-nil; take
   identity from the session; reject (or ignore, in favour of the session) a
   body `auth.user` that differs from the session user. This is the only
   server-side proof the caller really logged in as themselves.
2. Keep `auth.user` as a fallback but only when it does not grant anything: i.e.
   never trust a body-claimed identity for data a non-techniker cannot see.
   (This is just option 1 restated as a rule.)

Regression coverage to add (none exists today): the test suite only checks that
a user *without* the permission is refused (`internal/proxy/technik_test.go:
86-95`); there is no test for "an unauthenticated caller claiming a techniker".

### C2 – Request bodies logged verbatim: OTPs and app secrets leak to logs

Every request body is written to the log unchanged:

```
log.Printf("[intern] %s?%s body=%s", r.URL.Path, r.URL.RawQuery, string(body))
internal/proxy/jsonrpc_intern.go:24
```

Those bodies contain live credentials:

- `keyLogin` receives the current TOTP: `auth.otp`
  (`internal/proxy/jsonrpc_intern.go:280-306`).
- `getAppSharedSecret` receives the account password / shared secret
  (`internal/proxy/jsonrpc_intern.go:46-70`), which is exactly the long-lived
  replay key the proxy persists on first success
  (`store.UpsertSecret`, `internal/proxy/jsonrpc_intern.go:324-329`) and reuses
  to escalate sessions (`jsonrpc_intern.go:289`).

Evidence from the live log (`bin/untis-server.log`, gitignored but written to
disk / Docker):

- ~780 request bodies carry an OTP literal, e.g. `"otp":"114360"`,
  `"otp": 635449`.
- 5 bodies carry a `"password"` string.

Impact: the TOTP is short-lived (a ~30 s window), but the **shared secret is
permanent and replayable**. Any user who can read the log (disk, `docker
logs`, log shipment) can replay that student's/teacher's account indefinitely.

The proxy also retains the **account password** itself at rest now, next to the
key in `secrets.password` (captured at `getAppSharedSecret`, at password-method
`authenticate`, and via `untisctl`/admin). This is deliberate for the API
project, but it means a database or backup leak yields both the replay key and
the long-lived password — whatever the log leak gives, the DB gives permanently.

Recommended fix: redact before logging. Never print `auth`/`otp`/`password`/
`secret`/`key`; log method + params with sensitive fields stripped, or drop the
body log entirely (the change/absence paths already log a summary).

### C3 – Data race on p.secrets -> process crash

`Proxy.secrets` is written under `secretsMu` but read without the lock:

- Unlocked read: `replayKey = p.secrets[strings.ToLower(auth.User)]`
  (`internal/proxy/jsonrpc_intern.go:289`).
- Locked writes: `internal/proxy/jsonrpc_intern.go:67-69` and `325-327`.

Go maps are not safe for concurrent read/write; the runtime aborts the whole
process with `fatal error: concurrent map read and map write`. Trigger: two
logins racing — one escalating (persisting a secret) while another reads the
map. That is exactly what happens when several people log in around the same
time, a plausible event.

Recommended fix: read under `secretsMu` (small helper or inline lock). Verify
with `go run -race` / `go test -race`.

## Medium

### C4 – No HTTP timeouts and unbounded request bodies

- The HTTP server has no timeouts at all:
  `srv := &http.Server{Addr: *addr, Handler: p.Handler()}`
  (`cmd/server/main.go:141`) — no `ReadHeaderTimeout`, `ReadTimeout`,
  `WriteTimeout`, `IdleTimeout`.
- Request bodies are read with `io.ReadAll` and never bounded:
  `internal/proxy/jsonrpc_intern.go:19`, `internal/proxy/jsonrpc.go:14`,
  `internal/proxy/homework.go:24` (via `readBody`).

Combined on the exposed `:8509` listener (all interfaces, compose-mapped) this
allows slow-loris-style connection exhaustion and memory exhaustion from huge
bodies — and the body is read before any gating, so the unauthenticated
`/WebUntis/jsonrpc_intern.do` path is memory-DoS-able as well.

Recommended fix: set `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/
`IdleTimeout` on both servers, and wrap handlers with `http.MaxBytesReader`
(upstream response reads are fine — the upstream is trusted).

### M1 – Personal events reference a class non-techniker students cannot resolve

`decorateStudentEvents` tags every personal event with `CLASS=technikClassID`
(`internal/proxy/events.go:270` -> `tagClassElement`,
`internal/proxy/events_elements.go:148-157`), but the Technik class is only
added to masterData for holders of the `techniker` permission
(`injectTechnikClass`, `internal/proxy/technik.go:51-54`). A normal student's
own timetable therefore references a class that is not in their masterData: the
app may render a blank class, or drop the row entirely. The feature author is a
techniker, so this path was never observed.

Recommended fix: either tag with a class the viewer can resolve (their own
class, as real periods do), or inject the Technik class stub into the response's
masterData for everyone who sees an event referencing it (a label-only
injection; the aggregate itself stays permission-gated).

### M2 – untisctl cannot grant/revoke the techniker permission

`untisctl perms` knows only `recon`/`boosted`/`editor`
(`internal/proxy/...`, `cmd/untisctl/main.go:145-149`). `techniker` is settable
only through the admin UI/API (`internal/proxy/admin.go:960-961`). An operator
who manages permissions from the CLI cannot provision the Technik feature.

### M3 – notification_outbox dead rows never pruned (unbounded disk growth)

`FailOutbox` marks an exhausted delivery `state='dead'` so an operator can
inspect it (`internal/store/store.go:2251-2263`; exhaustion decision at
`internal/proxy/outbox.go:108-110`). Nothing ever deletes those rows, and each
holds the full change payload (`payload TEXT`, up to a full timetable-change
JSON). Over time — permanent webhook failures, deleted destinations — the table
grows without limit.

Recommended fix: a retention sweep (e.g. drop `dead` rows older than N days)
while keeping `RecentDeadOutbox` (`store.go:2311`) as the recent-inspection
surface. Consider also capping the record to its summary instead of the payload.

### M4 – element_names grows forever and LookupElement full-table scans

- `SaveMasterNames` only upserts; retired elements are never removed
  (`internal/store/store.go:1751-1770`).
- `element_names` has no index on (`school`, `el_type`), and `LookupElement`
  scans the whole table per call
  (`store.Exec`…) (`store.go:1788`).

Latency on admin lookups grows with the table. Minor today, unbounded in the
long run. Recommended fix: index + pruning of names no longer present in the
latest masterData snapshot (and the same for the persisted `element_names`).

## Low

### L1 – Stale README: isCustom marker no longer emitted

`README.md:392` documents events served with `isCustom: true` and a negative id.
The wire format now uses `customEventId` and detection keys off that
(`internal/proxy/events.go:118`, `events.go:539-540`). The README's "In
progress" pointer (`README.md:673-677`) also contradicts `docs/PLANNED-
FEATURES.md`, which lists everything as released.

### L2 – Dead Options.Admin field

Declared (`internal/proxy/proxy.go:26`) but never read.

### L3 – technikClassID shares the synthetic-element id space

`technikClassID = syntheticElementBase` (`internal/proxy/technik.go:36`).
Synthesized *elements* (rooms/teachers/subjects) hash into
`base + hash % 1e9`, so the fixed class id 2,000,000,000 collides with an
element only if that 64-bit hash mod 1e9 is exactly 0 — ~1 in 1e9. Accepted
risk; nothing to do.

### L4 – ttCache can exceed its 2000-entry cap

`ttCache.Put` evicts only when `len(m) > 2000` and only deletes expired entries
(`internal/proxy/timetable.go:137-148`). A burst of distinct keys can exceed the
cap until the TTL elapses. Each entry is a timetable slice; bounded by request
volume × TTL. Minor.

## Verified clean (checked, no action)

- SSE subscriber channels are unsubscribed and closed on disconnect
  (`defer unsub()`, `unsubEvents`, `r.Context().Done()`;
  `internal/proxy/notify.go:839-846,862-866`); publishes are non-blocking.
- HTTP timeouts for outbox delivery are set (10 s webhook/ntfy,
  `internal/proxy/notify.go:508,540,614`; 25 s upstream,
  `internal/untis/client.go:63`) — no permanently stuck worker.
- Session manager GC sweeps expired sessions every 10 min
  (`internal/session/session.go:60-66`); sessions carry a 24 h TTL
  (`cmd/server/main.go:112`).
- `timetable_changes` is rewritten per class on each snapshot, so it stays
  bounded by pool classes × periods (`internal/store/store.go:2043`).
- Recon element registry is replaced (not merged) on a full sweep
  (`internal/proxy/reconstruct.go:672-705`).
- `attachEventElements`/`elementNameIndex` build per-response indexes and mutate
  only the response's masterData — nothing leaks server-side
  (`internal/proxy/events_elements.go:32-71`).
- Admin event UI escapes user text (`esc()`, `textContent`) — no obvious XSS
  (`internal/proxy/static/admin.html`).
- `notification_outbox` is deduplicated per version/change for a destination
  (one row per enqueued change-frame).

## Feature / UX wishlist

Collected from the code discussions and the docs. None of these are defects;
they are the "what people might want" list.

1. **Room mapping as server options.** The user asked for Aula→169 / CSS→238 to
   be reference-able (done dynamically from live masterData, active entries win,
   `internal/proxy/events_elements.go:83-107`) *and* be configurable via
   `Options` rather than resolved implicitly. No such `Options` field exists
   yet (`internal/proxy/proxy.go:26-36`).
2. **Technik aggregate: return the entire history** regardless of the requested
   window (offered in discussion, declined, still pending decision —
   `opencode-chat.md`).
3. **Calendar tokens: expiry and rotation.** Tokens never expire
   (`docs/MANUAL-QA-CHECKLIST.md` §16).
4. **ntfy de-duplication** when a topic is subscribed twice
   (`README.md:286-290`).
5. **Login rate limiting / lockout** (documented as a known gap; upstream OTP
   invalidates after 3 tries, but the proxy itself has no throttle).
6. **Admin audit log** of admin actions (event create/update/delete, import,
   permission changes).
7. **`/week` page:** month view, export, and an `.ics` download link (currently
   none of these).
8. **Done-flag / absence-note retention policy.** No cleanup of
   `homework_done`/`absence_notes`; decided to revisit only if it becomes a
   measurable cost (`docs/PLANNED-FEATURES.md:135-136,439-444`).
9. **Stale docs:** fix README "In progress" pointer and the `isCustom` text
   (see L1).

## Verification checklist

Run before trusting any of the above as fixed:

- `go test ./...` and `go test -race ./...` — the `-race` build should surface
  C3 (a safe repro is two goroutines calling `keyLogin` concurrently).
- `go vet ./...`.
- C1: from a host that is not logged in, POST the exploit request above with a
  techniker username; the correct result after the fix is `-8520`/`-8509`, not
  a populated timetable. Then confirm the real app's Technik class still loads
  (it sends `auth.user` on some requests and a session cookie on others).
- C2: after the redaction change, the `[intern]` log must not contain any OTP or
  password literal; grep `bin/untis-server.log` for `"otp"` and `"password"`.
- C4: `curl --max-time 60 -H 'Content-Length: 1000000000' …` with a halting
  body should time out server-side instead of occupying a worker indefinitely.
- MANUAL-QA-CHECKLIST §15.33-15.37 (Technik aggregate manual QA) once C1's fix
  is in place.