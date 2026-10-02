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
- `since` is the last seen class version (`0` = full history).
- `sinceEvents` is the last seen count of the session user's **own** custom
  events. Omit it to be sent the current value.
- Returns `{school, classId, since, current, eventVersion, sinceEvents, changes}`.
- `304 Not Modified` when neither the class version nor the event version moved.

The app should drive its cache by version: keep the played-back version,
increment as `change` events arrive, and reconcile with `/changes` after
reconnect or push wake-up.

`current` and `eventVersion` are two independent counters. `current` is
class-wide and moves for anyone's timetable change; `eventVersion` is private to
the session user and moves only when an admin edits *their* custom events (see
[Technik / custom events](#technik--custom-events)). Refetch when either moves.

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

## Technik / custom events

An admin can put an entry on **one student's** timetable: a Technik slot, a
meeting, anything with a date and a time. It appears wherever that student's own
timetable is served, and nowhere else.

### Where they appear

| Surface | Carries the student's events |
|---|---|
| `getTimetable2017`, `params[0].type = "STUDENT"`, your own `id` | yes |
| `getTimetable2017`, `params[0].type = "CLASS"`, **your own** `classId` | yes |
| `getTimetable2017`, `CLASS` for any other class, or `TEACHER` / `ROOM` / `SUBJECT` | **never** |
| `/api/calendar/{student-token}.ics` | yes |
| `/api/calendar/{class|teacher|room|subject-token}.ics` | **never** |
| `/week/{student-token}` | yes |

Both self paths are covered, because which one a client uses to ask for its own
timetable is a fact about the client, not something the proxy can assume: it
depends on the app version, and a client that asked for `STUDENT` while the proxy
only decorated `CLASS` would get a timetable with the events silently missing.
Both are keyed by the session user, so a classmate asking for the same class
receives *their own* events and never anyone else's — the response differs per
viewer, while the underlying class data does not.

Events are *appended to* `result.timetable.periods` — every upstream field,
including ones added after this proxy was written, is passed through untouched.

### The shape

```json
{
  "id": -1000000000,
  "startDateTime": "2026-10-01T14:00+02:00",
  "endDateTime": "2026-10-01T15:00+02:00",
  "isCustom": true,
  "customEventId": 12,
  "customRevision": 1,
  "customTitle": "Technik",
  "subject": "Mathe",
  "room": "R12",
  "teacher": "Mr Smith",
  "description": "Arbeitsblatt mitbringen",
  "date": 20261001,
  "startTime": 840,
  "endTime": 900,
  "lessonText": "Technik",
  "is": {"standard": false, "event": true},
  "cellState": "CUSTOM",
  "hasInfo": true,
  "elements": []
}
```

Read `isCustom` to tell an event from a real lesson — the app cannot infer it, and
guessing from `elements` being empty would misread a real lesson with no elements.

Two things to know about `id`:

- It is **negative**. Real period ids are small positive numbers, so the two
  cannot collide and an event cannot overwrite a lesson in a client's map.
- It **does not change when the event is edited**. It identifies the event, and
  the revision is reported separately in `customRevision` — which is also what the
  `.ics` `SEQUENCE` is built from. A client that replaces periods on every fetch
  needs a stable key to do that; an id that moved on every edit would leave the
  client unsure whether it had one event or two.

`elements` is empty because an event carries the text an admin typed, not ids that
resolve through master data. Render `customTitle`, `subject`, `teacher`, `room`
and `description` directly.

### Times and the school clock

`startDateTime` carries the school's UTC offset, exactly as upstream's own
periods do — the event is 14:00 on the school clock, not 14:00 UTC. A client that
converts using the school timezone gets the right instant.

### In the .ics feed

An event becomes one `VEVENT`:

```
BEGIN:VEVENT
UID:custom--1000000000@untis-api
SEQUENCE:1
DTSTART;TZID=Europe/Berlin:20261001T140000
DTEND;TZID=Europe/Berlin:20261001T150000
SUMMARY:Technik · Mathe · Mr Smith
LOCATION:R12
DESCRIPTION:Raum: R12\nArbeitsblatt mitbringen
TRANSP:TRANSPARENT
END:VEVENT
```

- `UID` is prefixed `custom-`, so it can never be mistaken for a real lesson's UID
  (which is the bare period id). It identifies the event and stays the same
  across edits, as RFC 5545 requires — a UID that moved would leave the old entry
  cached and add a second one.
- `SEQUENCE` is the event's revision, so **editing an event raises it** and a
  client replaces its cached copy.
- `TRANSP:TRANSPARENT` marks it as not occupying the time. A real lesson is
  `OPAQUE`.

### Deletion: stale copies are expected

**Deleting an event does not retract it from calendars that already have it.**
The event is gone from every future fetch, so a client that syncs drops it. A
client that has not synced keeps showing a cached copy until it does.

No cancellation is pushed. RFC 5545 `METHOD:PUBLISH` is a full-state push, so a
removal would have to arrive as a `CANCEL` override keyed on the UID the client
already stored, and clients disagree about how to resolve that against a
`PUBLISH` feed — including some that would delete the whole series. The reliable
fix is to keep the subscription URL short-lived so clients re-fetch often.

### Knowing an event changed

An admin edit bumps a **per-student counter**, separate from the class one:

- `GET /api/timetable/changes` carries `eventVersion` (and echoes `sinceEvents`).
  Pass `?sinceEvents=<last value>` alongside `since`; the endpoint answers `304`
  only when *both* the class version and the event version are unchanged.
- `GET /api/timetable/stream` emits an extra frame:

```
event: student-events
data: {"event":"student-events","school":"…","eventVersion":4,"reason":"updated"}
```

`reason` is `created`, `updated` or `deleted`. The frame carries **no event
content** — only the counter — so treat it as "your schedule changed, refetch"
and re-read through whichever surface you are already authenticated for.

Deleting an event bumps this counter too. That is why it is a stored counter and
not something derived from the events: a delete leaves no row behind to derive
from.

Event changes are **not** delivered to the shared webhook and ntfy topics. Those
are read by everyone with access to the class, so a student's private appointment
announced there would reach every classmate.

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