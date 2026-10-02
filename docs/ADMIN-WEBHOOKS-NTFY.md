# Admin Dashboard · Webhooks · ntfy · Multi-School

Status: implemented in v1.4.0. This doc covers the new surfaces only; the tiered
permission model stays as described in [`GOD-API-PLAN.md`](GOD-API-PLAN.md).

For the app-side contract see [`APP-INTEGRATION.md`](APP-INTEGRATION.md); for
running the thing, [`../DEPLOYMENT.md`](../DEPLOYMENT.md).

## Admin dashboard

A single-page UI, served **only to admin sessions** at:

```
GET /admin            → the dashboard (embedded HTML, dependency-free)
GET /admin/* …        → JSON API consumed by the dashboard
```

`/admin` itself (no trailing slash) serves the HTML page; everything under
`/admin/` is the JSON API. Both are gated by an admin **session** (JSESSIONID)
plus the `users.admin` flag.

### Bootstrapping the first admin

The DB has no admin until one is granted:

```sh
untisctl -db data/untis.db users admin --user evan          # on, default
untisctl -db data/untis.db users admin --user evan --off    # revoke
```

At server start the same can be done idempotently with the `-admin` flag:
`-admin evan,bschneider`. The flag **only** bootstraps admins that do not exist yet
(settings row `admin_bootstrap`); afterwards the `users.admin` column is the
source of truth and the dashboard can promote/demote.

An admin can do everything `untisctl` can, from the dashboard:

| Endpoint | Methods | Purpose |
|---|---|---|
| `/admin/status` | GET | counts: users, admins, pool, schools, tokens, webhooks, ntfy, recon, boosted, editors, global perms |
| `/admin/users` | GET/POST | list users; add/promote (`{username, admin}`) |
| `/admin/users/{name}` | POST/DELETE | set admin flag, set a feature (`feature`+`allowed`), revoke a permission, or delete |
| `/admin/perms` | GET | all permission rows |
| `/admin/pool[/{school}]` | GET | pooled classes + owners |
| `/admin/tokens[/{school}]` | GET | calendar tokens |
| `/admin/tokens/{token}` | DELETE | revoke a token |
| `/admin/schools` | GET | registered schools |
| `/admin/schools/{school}` | POST | register a school |
| `/admin/webhooks` | GET/POST | list / add (`school`, `url`, `secret` + target `classId` (legacy) or `elementType`/`elementId` for class/student/teacher/room/subject) |
| `/admin/webhooks/{id}` | DELETE | remove |
| `/admin/webhooks/{id}/test` | POST | send a test change to one hook (owner/admin) |
| `/admin/ntfy` | GET/POST | list / add (`school`, `topic`, `baseUrl` + target `classId` or `elementType`/`elementId`) |
| `/admin/ntfy/{id}` | DELETE | remove |
| `/admin/ntfy/{id}/test` | POST | publish a test change to one topic (owner/admin) |
| `/admin/recon[/{school}]` | GET | recon snapshot stats |

## Webhooks (change subscriptions)

On every detected timetable change the proxy delivers a JSON payload to every
**matching** webhook. A webhook can target **any element** or be school-wide:

- **school-wide** — no element target (`classId = 0`, empty `elementType`);
  fires for every class in that school,
- **per-class** — `{classId: 5000}` (legacy) or `{elementType:"CLASS",
  elementId:5000}`; fires for that class,
- **student** — `{elementType:"STUDENT", elementId:<personId>}`; fires when a
  change touches the student's class,
- **teacher / room / subject** — `{elementType:"TEACHER"|"ROOM"|"SUBJECT",
  elementId:<id>}`; fires when a changed row's `Teacher`/`Room`/`Subject` name
  resolves to that element.

payload (marshaled with Go's default field names):

```json
{
  "event": "change",
  "school": "testschool",
  "classId": 5000,
  "version": 4,
  "changes": [
    { "PeriodID": 123456, "Kind": "ADDED", "Start": "2026-09-07 08:00", "End": "2026-09-07 08:45", "Subject": "M", "Room": "101", "Teacher": "Willner", "Description": "…", "ModVer": 4 }
  ]
}
```

`Kind` is `ADDED`, `CHANGED` or `REMOVED`.

Every delivery also carries a **digest**: the changed lessons written out in
language a parent would use, e.g.

```
class 5000 - 2 changes: 1 added - 1 changed
  Mon 1. Std 08:00-08:45  Mathematik 3 (A. Hartley, R204)  new
  Tue 3. Std 10:00-10:45  Mathematik 3 (A. Hartley, R112)  room R204 -> R112
  ... and 6 more
```

The one-line version (transliterated to plain ASCII, so it survives a push
title) goes into the `X-Untis-Summary` header, which names the target element for
element-scoped hooks (e.g. "Timetable change — room 01-Aula"); the full
multi-line digest is the ntfy message body. `GET /api/timetable/changes` returns
it as `summary` / `message` next to the raw `changes` array. Long days are
truncated to 8 lessons plus a "... and N more" footer, so a schedule reshuffle
cannot produce a 300-line notification.

Headers on every delivery:

- `X-Untis-Event: timetable-change`
- `X-Untis-Summary` — human-readable one-line change summary
- `X-Untis-Signature: sha256=<HMAC-SHA256(body, secret)>` — only if a `secret`
  was configured on the webhook; receivers should verify it.

Deliveries are queued durably and sent by a background worker — see
[Delivery reliability](#delivery-reliability) below.

### Configuring webhooks

Admins: `POST /admin/webhooks` or the dashboard (Webhooks section — type
dropdown + searchable element picker, same one calendar tokens use).

Self-service (any logged-in session) at `/api/webhooks`:

| Method | Purpose | Access |
|---|---|---|
| GET | list hooks you may see | your own class always; all with `editor`/`boosted`/admin |
| POST `{classId, url, secret}` | create | your own class always; any pooled class + school-wide (`classId 0`) with `editor`/`boosted`/admin |
| POST `/api/webhooks/{id}/test` | fire a test delivery | the hook's creator, or `editor`/`boosted`/admin |
| DELETE `/api/webhooks/{id}` | remove | your own hooks; any with `editor`/`boosted`/admin |

## Push notifications via ntfy

Change events are also fanned out to ntfy topics. Topics use exactly the same
element targeting as webhooks: **school-wide**, or any of **class / student /
teacher / room / subject** (the dashboard ntfy section has the same type
dropdown + picker). A matched topic is posted via `POST https://ntfy.sh/{topic}`
or its own `baseUrl` when a per-topic ntfy server is configured; the Android app
(BetterPlus-Android) subscribes to the topic(s) the user has access to.

The pushed message body is the same human-readable digest that webhooks carry in
their summary header; element-scoped topics prepend the element name to the
title. The full JSON change payload is available in the message.

`POST /admin/ntfy` (or dashboard "ntfy topics" section) manages topics.
Self-service `POST /api/ntfy/{id}/test` fires a test publish (creator or
privileged). The `GET /api/ntfy` listing is **element-aware**: a plain user sees
topics for their own class and (if set) their own student element; school-wide
and teacher/room/subject topics are only listed for `editor`/`boosted`/admin.

### Topic naming recommendation

Use per-school prefixes so different schools never collide on the public ntfy
server, e.g. `schl{schule}-klasse{ID}` and `schl{schule}-all`. The proxy itself
only enforces per-row `school`, not topic naming.

## Delivery reliability

Every webhook and ntfy delivery goes through a durable outbox, so a change that
the poller detects is never silently dropped.

- A change is queued in the **same database transaction** that records the new
  timetable version. Either both happen or neither does.
- One queued row per **destination**. A broken endpoint cannot cause deliveries
  that other endpoints already received to be replayed.
- A worker retries failed deliveries with backoff (30s, doubling to a 15-minute
  cap, up to 8 attempts), and a slow or unreachable destination never blocks the
  poll loop.
- After 8 failed attempts a delivery is marked **dead** and stops retrying. It
  stays in the table on purpose: that is the signal that a human needs to look.
- Restarting reclaims deliveries that were in flight when the process stopped,
  and retries them.
- Deleting a subscription drops its queued backlog rather than retrying a
  destination you removed on purpose.

Inspecting failures (admin session required):

| Method | Purpose |
|---|---|
| `GET /admin/outbox` | counts by state, plus the most recent dead deliveries with their error |
| `GET /admin/outbox?state=dead` | only the exhausted deliveries |
| `GET /admin/status` | includes an `outbox` object with `pending`/`sending`/`dead` counts |

A dead row names the school, class, version, destination and error, which is
usually enough to tell whether the fix belongs in the URL or in the receiver.
`pending` growing steadily means the worker cannot keep up or a destination is
failing; `dead` above zero means a destination has given up.

The public `/healthz` and `/status` endpoints stay aggregate-only and never
expose which school or destination is failing. Outbox detail requires an admin
session, or the separate loopback `-metrics-addr` listener.

## Streaming API (SSE)

`GET /api/timetable/stream?school=<s>` emits `event: change`
messages on every detected change; `GET /api/timetable/changes?since=N` is the
pollable diff. Android apps can use either the SSE stream or the per-class ntfy
topic as the push trigger and reconcile via `/api/timetable/changes`.

## Student events (Technik) — and why they bypass the outbox

The *Student events* section of the dashboard puts an entry on **one student's**
timetable. Those events are **not** class changes, so they deliberately do not go
through the outbox, the webhooks or the ntfy topics.

Every webhook and ntfy destination is configured for an element or a whole class,
and everyone who can read that class can read those messages. A student's private
appointment announced there would reach every classmate — a Technik slot with a
room and a time is exactly the kind of thing that should not be. There is no
per-student variant of these subscriptions, so the events take a separate route:

| Mechanism | Scope |
|---|---|
| `GET /api/timetable/changes?sinceEvents=N` | `eventVersion`, private to the session user |
| `GET /api/timetable/stream` → `event: student-events` | same counter, for that user only |

Both move on create, edit **and delete** — the delete is the case a counter
derived from the events could not catch, since no row is left to derive from. The
signal carries the counter and a reason (`created`/`updated`/`deleted`), never the
event's content; the client refetches through a surface it is already
authenticated for.

If you want the whole school to see something, that is a different thing: write it
into your own teacher account's timetable, or set a webhook/ntfy topic yourself.

## Admin API for events

Admin session required. A non-admin gets `401`/`403` before the method is even
dispatched, and a list without `username` is refused rather than guessing which
student was meant.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/admin/events?username=<name>[&school=<s>]` | one student's events, sorted by date then start time |
| `POST` | `/api/admin/events` | create |
| `PATCH` | `/api/admin/events/{id}` | partial edit; the merged result is validated |
| `DELETE` | `/api/admin/events/{id}` | remove, idempotent |

```json
POST {"username":"dee","date":"2026-10-01","startTime":"14:00","endTime":"15:00",
      "title":"Technik","subject":"Mathe","room":"R12","teacher":"Mr Smith",
      "description":"Arbeitsblatt mitbringen"}
```

`username` must exist in that school (`404` if not) — an event filed against a
typo would never appear on anyone's timetable, which is the kind of mistake that
gets made and forgotten. `date` must be `YYYY-MM-DD`, the times `HH:MM`, and
`endTime` strictly after `startTime`, so a malformed entry is refused at write
time rather than rendered as a collapsed or misplaced block later. A `PATCH` is
validated against the *merged* row, so changing only the start time onto a value
after the stored end is caught.

Every write records `createdBy`, `createdAt`, `updatedAt` and a `revision` that
increments per edit. The revision is what an `.ics` client sees as `SEQUENCE`.

## Multi-school

A single proxy process can serve many WebUntis schools:

- The **school name** identifies every dimension: pool, recon, master data,
  tt-cache, permissions, tokens, webhooks, ntfy topics.
- On the **first login from a school the proxy has never seen**, the school is
  auto-registered (`schools` table) and its initial recon enumeration starts in
  the background (`ensureReconScan`). No restart needed.
- The change-detection poll loop periodically re-reads the `schools` table, so
  auto-registered schools are picked up and polled automatically.
- All in-memory caches (klasses, masterData, recon element store, timetable
  cache) are per-school; cache keys carry the school prefix.
- Clients select a school with `?school=` on the login/JSON-RPC/REST requests
  (and it is persisted on the user row); a `schoolname` cookie remembers it.

Exit condition / accounting: an admin can remove a school row from the Schools
section; its data remains keyed by school name and becomes orphaned.

## Files

- `internal/proxy/admin.go` — `/admin` dashboard embed + JSON API.
- `internal/proxy/static/admin.html` — dashboard page.
- `internal/proxy/subs.go` — self-service `/api/webhooks`, `/api/ntfy`.
- `internal/proxy/notify.go` — `deliverChange`, `postWebhook`, `publishNtfy`,
  `StartPollLoop` (multi-school), changes/stream API.
- `internal/proxy/events_admin.go` — `/api/admin/events` CRUD + the private
  change signal.
- `internal/store/store.go` — `users.admin`, `schools`, `webhooks`, `ntfy_topics`,
  `student_events`, `student_event_versions` tables + school-scoped queries.