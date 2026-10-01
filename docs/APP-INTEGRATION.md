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
  "Kind": "ADDED" | "CHANGED" | "REMOVED" | "UNCHANGED",
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
matching above uses them.

Two details worth getting right in a client:

- **`UNCHANGED` is reachable.** It is not in the union above by intent — it means
  "this period is still here, unchanged" and carries the mod version from when it
  last really changed. A full-history request (`since=0`, which is also what the
  SSE stream replays on connect) returns these, so a `switch` over `Kind` needs
  a default or an `UNCHANGED` arm. Notifications never include them: the digest
  is built from `PendingChanges` with the current version, which selects only
  rows written by the change that just happened.
- **A reinstated lesson arrives as `ADDED`, not `CHANGED`.** A period that was
  cancelled upstream and then restored with byte-identical data is reported as
  `ADDED`. It is not a modification, and treating it as one loses the fact that
  the lesson is back — which is the only thing a subscriber needs to know. The
  digest marks it `new:`.

For the full before/after period objects, diff `/api/timetable/changes` against
the class timetable fetched from the upstream JSON-RPC service.

## Homework done flags

Every `homeWorks[]` entry in a **`getHomeWork2017`** or **`getPeriodData2017`**
response carries two proxy-added fields:

```json
{
  "id": 1001,
  "text": "Mathe S.42",
  "completed": false,
  "done": true,
  "doneAt": "2026-10-01T12:34:56Z"
}
```

| Field | Meaning |
|---|---|
| `id` | upstream homework id; the stable key for writing a flag |
| `completed` | **teacher-owned**, passed through untouched |
| `done` | the student's own answer, stored by this proxy |
| `doneAt` | when the student marked it, RFC3339, or `null` |

`done` is always present on an entry the proxy could key — `false` means "not
marked done", and is distinct from a missing field, which means the entry had no
usable `id` and was left exactly as upstream sent it.

`completed` and `done` are different questions. `completed` is the teacher's mark
on the assignment; `done` is the student's private answer. The proxy never writes
`completed`, so marking your own work done can never overwrite a teacher.

The two methods differ in what they carry, and both are decorated so the app can
read whichever is convenient:

- `getHomeWork2017` → `{homeWorks, lessonsById}` — the whole list, one call.
- `getPeriodData2017` → per-period `homeWorks[]` — the path the `.ics` export
  already walks.

Homework does **not** appear in `getTimetable2017`; that response is left
byte-for-byte untouched.

### Reading and writing the flags

The enrichment above means the app may never need the standalone endpoints, but
they exist for a client that wants the flags without refetching homework (for
example on app start, to render badges offline):

```
GET  /api/homework/flags[?school=<name>]
200  {"school":"testschool","flags":[{"homeworkId":1001,"done":true,
                                       "doneAt":"2026-10-01T12:34:56Z"}]}
401  {"error":"not logged in"}

POST /api/homework/done[?school=<name>]   {"homeworkId":1001,"done":true}
200  {"homeworkId":1001,"done":true,"doneAt":"2026-10-01T12:34:56Z"}
200  {"homeworkId":1001,"done":false,"doneAt":null}
400  {"error":"homeworkId and done are required"}
401  {"error":"not logged in"}
```

`?school=` is optional and defaults to the server's configured school. Every error
on these two endpoints is a JSON body with a real status code.

Rules a client can rely on:

- **The session decides the user.** Both endpoints require the `JSESSIONID`
  cookie; a `username` in the body is ignored. There is no way to write another
  student's rows.
- **`done:false` clears** the flag. Both writes are idempotent, so retrying after
  a dropped response is safe.
- **Bad input is a real status code**, not a 200 with an error body: 400 for a
  malformed body, a missing or non-positive `homeworkId`, or a missing `done`.
- Flags are scoped per school *and* per user, so the same homework id in two
  schools is two independent flags.
- When an editor's request was rewritten upstream to run as a boosted teacher
  (the class-scoped methods), the response is the teacher's data and carries **no**
  `done` fields at all.

## Absence notes and derived metadata

Every entry in a **`getStudentAbsences2017`** response gains a private `note` and
a `derived` block:

```json
{
  "id": 300001,
  "startDateTime": "2026-10-01T10:00",
  "endDateTime": "2026-10-01T10:45",
  "klasseId": 5000,
  "absenceReasonId": 3,
  "text": "Doctor",
  "excuse": {"date": "2026-09-30", "text": "see doctor"},
  "studentId": 7,
  "note": "bring workbook",
  "noteUpdatedAt": "2026-10-01T12:34:56Z",
  "derived": {
    "classId": 5000,
    "className": "10b",
    "date": "2026-10-01",
    "weekday": "Thursday",
    "subject": "Mathematik 3",
    "reason": "Doctor's appointment"
  }
}
```

Three upstream fields exist and are none of them the student's:

| Field | Whose it is |
|---|---|
| `text` | the **teacher's** comment — passed through untouched |
| `excuse.text` | **upstream's** own excuse text |
| `note` | the **student's** private note, stored by this proxy |

### The `derived` block

Everything here is computed by the proxy from data already present, and **omitted
when it cannot be known** — a missing key means "not derivable", never "empty".

| Key | Source | When it can be missing |
|---|---|---|
| `classId` | the absence's own `klasseId` | absent upstream |
| `className` | `masterData.klassen` | no cached master data |
| `date`, `weekday` | `startDateTime` | unparseable timestamp |
| `reason` | `masterData.absenceReasons` by `absenceReasonId` | no cached master data |
| `subject` | the single timetable period the absence overlaps | see below |

`weekday` is an English weekday name; the raw timestamp is always present, so a
client that needs a localised name should derive it from `startDateTime` rather
than from this field.

### `subject` appears only when it is unambiguous

The subject is reported **only when the absence window overlaps exactly one
lesson** in the pooled timetable. That means:

- a lesson-scoped absence (10:00–10:45) reports that lesson's subject;
- a **whole-day absence has no subject**, because it covers several lessons and
  naming the first one would be a guess;
- an absence older than the polling window has no subject, because the snapshot
  no longer holds those periods.

This is deliberate. An absence with no subject is recoverable; an absence with
the *wrong* subject is not, and there is no way for a client to tell the two apart
after the fact.

### Privacy

Notes are private to the student, and that is structural rather than a check:

- A note is looked up by `(school, session user, absence id)`.
- It is attached only to an absence whose own `studentId` matches the session
  user. An entry with no `studentId`, or another student's, is passed through
  **completely undecorated** — no note, no derived block.
- `getStudentAbsences2017` is not in `classScopedMethods`, so it is never
  replayed as a boosted teacher. No editor or teacher response has a code path to
  a note lookup at all.
- The derived block is subject to the same ownership check, so a mismatch never
  attaches metadata resolved for the wrong person either.

### Reading and writing notes

```
GET  /api/absence/notes[?school=<name>]
200  {"school":"testschool","notes":[{"absenceKey":300001,
                                     "note":"bring workbook",
                                     "updatedAt":"2026-10-01T12:34:56Z"}]}

POST /api/absence/notes[?school=<name>]  {"absenceKey":300001,"note":"bring workbook"}
200  {"absenceKey":300001,"note":"bring workbook","updatedAt":"2026-10-01T12:34:56Z"}
200  {"absenceKey":300001,"note":null}
400  {"error":"absenceKey is required"}
400  {"error":"note is too long"}
401  {"error":"not logged in"}
```

- **The session decides the user.** No username parameter exists; a `username` in
  the body is ignored.
- **An empty or whitespace-only `note` clears** the note, so the response
  `note: null` and a stored note cannot disagree. Idempotent.
- A note is trimmed and capped at 2000 characters; over that is a 400 rather than
  a silent truncation.
- `updatedAt` is the same value the write returns and the read reports.

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