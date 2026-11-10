# Admin Dashboard · Webhooks · ntfy · Multi-School

Status: implemented. This doc covers the admin, webhook, ntfy and multi-school
surfaces; the tiered permission model is described in [`GOD-API-PLAN.md`](GOD-API-PLAN.md).

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
| `/admin/schools` | GET | registered schools, their `blocked` flag, and `allowNew` |
| `/admin/schools/{school}` | POST | register a school |
| `/admin/schools/{school}/block` | POST | refuse every public request for that school |
| `/admin/schools/{school}/unblock` | POST | restore access to that school |
| `/admin/schools/{school}` | DELETE | remove the registration row (data is kept) |
| `/admin/schools/settings` | POST | `{allowNew: bool}` — whether an unknown school may auto-register |
| `/admin/webhooks` | GET/POST | list / add (`school`, `url`, `secret` + target `classId` (legacy) or `elementType`/`elementId` for class/student/teacher/room/subject) |
| `/admin/webhooks/{id}` | DELETE | remove |
| `/admin/webhooks/{id}/test` | POST | send a test change to one hook (owner/admin) |
| `/admin/ntfy` | GET/POST | list / add (`school`, `topic`, `baseUrl` + target `classId` or `elementType`/`elementId`) |
| `/admin/ntfy/{id}` | DELETE | remove |
| `/admin/ntfy/{id}/test` | POST | publish a test change to one topic (owner/admin) |
| `/admin/outage-ntfy` | GET/POST | list / add (`school` empty = all schools, `topic`, `baseUrl`) |
| `/admin/outage-ntfy/{id}` | DELETE | remove an outage subscription |
| `/admin/outage-ntfy/{id}/test` | POST | publish a test outage alert to one subscription |
| `/admin/recon[/{school}]` | GET | recon snapshot stats |
| `/admin/rooms[/{school}]` | GET/POST | event-room-text → Untis room-id map (overrides the school catalogue) |
| `/admin/outbox` | GET | notification-delivery backlog (`?state=dead` for exhausted deliveries) |
| `/admin/events/absences[/{school}]` | GET | event-absence register, one row per recorded absence |
| `/admin/events/absences` | PATCH | correct an event absence (append-only changelog) |

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
| `GET /api/notifications/options` | list the targets you may watch | same catalog as `GET /api/calendar/options`, minus Technik |
| GET | list **your own** hooks | always (own rows only) |
| POST `{classId, url, secret}` | create | your own class always; any pooled class + school-wide (`classId 0`) with `editor`/`boosted`/admin |
| POST `{teacherId, url, secret}` / `{roomId,…}` / `{subjectId,…}` / `{personal:true,…}` | create | teacher/room/subject need recon or `boosted`/staff; `personal` is your own person only |
| POST `/api/webhooks/{id}/test` | fire a test delivery | the hook's creator, or `editor`/`boosted`/admin |
| DELETE `/api/webhooks/{id}` | remove | your own hooks; any with `editor`/`boosted`/admin |

The target keys are the calendar mint's (`classId` / `personal` / `personId` /
`teacherId` / `roomId` / `subjectId`); at most one may be set, and none means
school-wide (privileged only).

Creating a webhook is idempotent for its destination: school, owner, element
target and URL. POSTing the same destination twice — including two concurrent
requests — returns the existing id and leaves one row, so a retry cannot
double-deliver. The secret is mutable configuration, not identity: re-posting
with a new secret rotates the signature on the existing row instead of adding a
second hook that delivers twice. The admin route behaves the same way.

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
Self-service `POST /api/ntfy` takes the same target keys as `/api/webhooks`
(`teacherId`/`roomId`/`subjectId`/`personal`, gated by recon/boosted/staff), and
`GET /api/ntfy` lists only the caller's own topics. Discovery is via
`GET /api/notifications/options`.

Topic creation is idempotent for the exact identity (school, owner, element,
topic and server), exactly like webhooks: a retried or double-tapped save returns
the existing row instead of queueing a second notification. A different
`baseUrl` or topic is a separate subscription.

### Topic naming recommendation

Use per-school prefixes so different schools never collide on the public ntfy
server, e.g. `schl{schule}-klasse{ID}` and `schl{schule}-all`. The proxy itself
only enforces per-row `school`, not topic naming.

### Outage alerts

The dashboard's **Outage alerts (ntfy)** section (and `/admin/outage-ntfy`)
configures a different kind of push: a notification when the class poller stops
reaching Untis, and another when it recovers. These rows are kept in their own
`outage_ntfy` table, so they never take part in timetable-change matching.

- **Scope `all schools`** (`school` empty) fires whenever *any* registered,
  unblocked school is down, and the message names every affected school.
- **Scope `<school>`** fires only for that school.
- The alert is based on **poll success**, not recon progress. A school with no
  pooled classes is ignored. A school is down when its latest poll failed, when
  no poll has ever succeeded, or when the last successful poll is older than the
  10-minute health window.
- A failure must persist for **3 minutes** before the down alert is sent. This
  keeps a single timeout or upstream restart from paging anyone. The recovery
  alert is sent once when the condition clears; a sustained outage does not
  repeat every tick.
- Each subscription can set its own `baseUrl`, exactly like a timetable topic.
  `POST /admin/outage-ntfy/{id}/test` sends a sample alert through the same path.
- A blocked school is not polled and never appears in an outage alert.

Example:

```sh
# global outage topic
curl -X POST /admin/outage-ntfy \
  -d '{"topic":"untis-outages","baseUrl":"https://ntfy.example.org"}'

# only one school
curl -X POST /admin/outage-ntfy \
  -d '{"school":"myschool","topic":"untis-myschool"}'
```

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
  Dead rows are pruned after 7 days so the table cannot grow without bound; the
  recent ones stay visible in `/admin/outbox`.
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

`GET /api/timetable/stream?school=<s>` emits `event: snapshot` on connect,
`event: change` on every detected change, and `event: student-events` for the
caller's private Technik counter. `GET /api/timetable/changes?since=N` is the
pollable diff. Android apps can use either the SSE stream or the per-class ntfy
topic as the push trigger and reconcile via `/api/timetable/changes`.

The stream accepts the same target keys as notifications:

| Query | Target |
|---|---|
| *(none)* | personal, or the caller's class when the account has no person id |
| `classId=5000` | that class |
| `personal=true` / `personId=N` | the caller's own timetable |
| `teacherId=N` / `roomId=N` / `subjectId=N` | changes whose rows name that element |
| `schoolWide=true` | every class in the school (editor/boosted/admin) |

`schoolWide` must be explicit: no parameters means the legacy personal default,
never a silent escalation. Combining `schoolWide` with an element target is
rejected. Element streams fire only for elements mentioned by the changed rows
(once per element, not once per period) and include the element a changed or
removed lesson is leaving. Every stream, whatever its target, also carries the
session user's own `student-events` counter.

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

### Who sees a student's events

By default, only the owning student. A user holding the **`techniker`** permission
also sees:

- a synthetic *Technik* class whose timetable is the union of every student's
  events in the requested range,
- an event on the room and organiser (teacher) timetable it names,
- a proxy-local absence register on the event (nothing is written to Untis).

The permission is independent of admin/`editor`/`boosted`/`recon` and is granted
per user from the dashboard's *Users* section.

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

### Access control

Auto-registration is convenient, but public: anyone who knows a WebUntis school
login can be the first user from that school. The Schools section therefore has
two controls:

- **allow new schools to auto-register** (`/admin/schools/settings`,
  `{allowNew: bool}`). Default **on**, preserving the behaviour above. With it
  off, an unknown `?school=` is refused before any handler runs; an admin can
  still register the school explicitly with `POST /admin/schools/{school}`.
- **block / unblock** a registered school. Blocking refuses every public request
  for that school — new logins and existing sessions alike — on JSON-RPC, REST,
  calendar tokens and the `.ics`/`/week` feeds. The row and its data stay, so
  unblocking is a single flag flip. The configured default school cannot be
  blocked, which prevents locking the deployment out of itself.

`DELETE /admin/schools/{school}` removes the registration row only; the pool,
users, tokens and other per-school data remain. If auto-registration is still
on, that school can register again on its next login — use **block** to keep it
out. Deleting is for cleaning up a school that should never have been listed.

An admin can also pre-block a school name that has not logged in yet: `POST
/admin/schools/{school}/block` creates the row with the block set. That is the
simplest way to stop a school without waiting to see its login name in the list.

## Files

- `internal/proxy/admin.go` — `/admin` dashboard embed + JSON API.
- `internal/proxy/static/admin.html` — dashboard page.
- `internal/proxy/subs.go` — self-service `/api/webhooks`, `/api/ntfy`.
- `internal/proxy/notify.go` — `deliverChange`, `postWebhook`, `publishNtfy`,
  `StartPollLoop` (multi-school), changes/stream API.
- `internal/proxy/outage.go` — poll-outage ntfy alerts (`StartOutageAlertLoop`,
  `/admin/outage-ntfy`).
- `internal/proxy/events_admin.go` — `/api/admin/events` CRUD + the private
  change signal.
- `internal/proxy/roommap.go` — event-room-text → Untis room-id overrides.
- `internal/proxy/event_absences.go` — the event-absence register + changelog.
- `internal/proxy/events_import.go` — the Technik CSV/JSON import fan-out.
- `internal/store/store.go` — `users.admin`, `schools` (incl. `blocked`),
  `webhooks`, `ntfy_topics`, `outage_ntfy`, `student_events`,
  `student_event_versions` tables + school-scoped queries.