# Admin Dashboard · Webhooks · ntfy · Multi-School

Status: implemented in v1.4.0. This doc covers the new surfaces only; the tiered
permission model stays as described in `GOD-API-PLAN.md`.

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

Delivery retries 3× with backoff; a slow/unreachable webhook never blocks the
poll loop.

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

## Streaming API (SSE) — unchanged

`GET /api/timetable/stream?school=<s>` emits `event: change`
messages on every detected change; `GET /api/timetable/changes?since=N` is the
pollable diff. Android apps can use either the SSE stream or the per-class ntfy
topic as the push trigger and reconcile via `/api/timetable/changes`.

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
- `internal/store/store.go` — `users.admin`, `schools`, `webhooks`, `ntfy_topics`
  tables + school-scoped queries.