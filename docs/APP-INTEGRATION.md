# App Integration — Change Detection / Push / Subscribe

Status: implemented in v1.4.0. This is the contract for the Android app
(BetterPlus-Android) and any other client that wants live timetable changes.

Admin and webhook/ntfy surfaces are in
[`ADMIN-WEBHOOKS-NTFY.md`](ADMIN-WEBHOOKS-NTFY.md); the permission tiers are in
[`GOD-API-PLAN.md`](GOD-API-PLAN.md). The endpoints that need no auth are
`/status`, `/healthz` and — only with `-metrics-addr` set — `/metrics`; see
[`../DEPLOYMENT.md`](../DEPLOYMENT.md#health-and-monitoring).

## Login note (v1.4.0)

The proxy login (`getUserData2017` / `keyLogin`) accepts the **base32 shared
secret itself** in the `otp` field instead of a live 6-digit TOTP — useful when
the app shows the authenticator secret at setup. The proxy derives the live
code, validates it upstream and remembers the secret for replay; a rejected
paste never overwrites a stored key. Six-digit codes keep working exactly as
before.

## Two event sources

| Source | Type | Latency | Purpose |
|---|---|---|---|
| SSE stream | `GET /api/timetable/stream?school=S` | instant (within one poll interval) | live in-app updates |
| ntfy topic | `https://ntfy.sh/{topic}` | instant (within one poll interval) | push notification trigger |
| polling | `GET /api/timetable/changes?school=S&classId=C&since=N` | on demand | reconcile/offline catch-up |

All three are driven by the same change detection: the proxy polls each pooled
class every `-poll-interval`, diffs against the previous snapshot and bumps the
version.

## SSE stream

Requires a logged-in **session** (`JSESSIONID` cookie).

```
GET /api/timetable/stream?school=testschool
```

The stream always serves the **session user's own class** (`classId` is taken
from the session); the `school` query only picks the school and defaults to the
`schoolname` cookie's school. Fire-and-forget heartbeats every 30s
(`: heartbeat`). Events:

```
event: snapshot
data: {"event":"snapshot","school":"testschool","classId":5000,"current":4,"changes":[...]}

event: change
data: {"event":"change","school":"testschool","classId":5000,"version":5,"changes":[...]}
```

`socket.io`-style fallback is not needed — plain `EventSource` works. The
`school` is optional and defaults to the school on the session's `schoolname`
cookie.

## Polling diff API

```
GET /api/timetable/changes?school=testschool&classId=5000&since=4
```

- Requires a session whose class matches (or the `classId` param omitted → own
  class).
- `since` is the last seen version (`0` = full history).
- Returns `{school, classId, since, current, changes}`.
- `304 Not Modified` when nothing changed since `since`.

The app should drive its cache by version: keep the played-back version,
increment as `change` events arrive, and reconcile with `/changes` after
reconnect or push wake-up.

## ntfy push topics

Topics are configured server-side (admin dashboard, or self-service per-class
for ordinary users). Each topic targets **any element** — class, student,
teacher, room, subject — or is **school-wide**. On a change, the proxy POSTs to
`ntfy.sh/{topic}` (or the topic's own `baseUrl` if a per-topic ntfy server is
configured) only when the element is involved: the changed class id, the
student's class, or the teacher/room/subject name in the changed rows. Change
titles name the element, e.g. "Timetable change — room 01-Aula". The Android
app:

1. On login, fetch the user's topics: `GET /api/ntfy?school=S` (session
   required) → `{topics:[{id,school,classId,elementType,elementId,topic,baseUrl}]}`.
   `elementType` is `""` (school-wide), `CLASS`, `STUDENT`, `TEACHER`, `ROOM` or
   `SUBJECT`; `baseUrl` is the per-topic ntfy server (empty = the global one).
   School-wide topics and topics for other classes/elements are only listed for
   editor/boosted/admin.
2. Subscribe via ntfy (`https://ntfy.sh/{topic}/json` SSE, or the ntfy Android
   app).
3. On a new message, reconcile with `/api/timetable/changes`.

Topic payload: the message body is the human `X-Untis-Summary` text; the full
JSON event is delivered alongside it.

## Change row shape

Each `changes[]` entry is a `store.PeriodRow` (marshaled with Go's default
field names):

```json
{
  "PeriodID": 123456,
  "Kind": "ADDED" | "CHANGED" | "REMOVED",
  "Start": "2026-09-07 08:00",
  "End":   "2026-09-07 08:45",
  "Subject": "M",
  "Room": "101",
  "Teacher": "Willner",
  "Description": "…",
  "ModVer": 4
}
```

`Kind` describes how the period changed relative to the previous snapshot.
`Teacher` (and `Subject`/`Room`) name the involved elements — the element
matching above uses them. For the full before/after period objects, diff
`/api/timetable/changes` against the class timetable fetched via the JSON-RPC
ttservice.

## Self-service subscriptions (app-integrated config)

`/api/webhooks` and `/api/ntfy` (session required):

- `GET` — list your visible subscriptions (own class; everything with
  editor/boosted/admin).
- `POST` — create one for your class, or (privileged) for any pooled class,
  **student, teacher, room or subject**, or school-wide.
- `DELETE /api/webhooks/{id}` | `/api/ntfy/{id}` — remove your own (privileged
  users can remove any).

This lets the app offer in-app "notify me about changes" without admin
credentials — for example a student subscribing to a room's changes, or a
teacher to their own lessons.

## Webhook receivers

The webhook payload equals the SSE `change` data; receivers verify integrity via
`X-Untis-Signature: sha256=<HMAC-SHA256(raw body, secret)>` when a secret is
configured. See `docs/ADMIN-WEBHOOKS-NTFY.md`.