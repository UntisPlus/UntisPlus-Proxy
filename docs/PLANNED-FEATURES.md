# Planned work — personal homework, absence notes, Technik events, delivery outbox

Status: **Phase 1 implemented, not yet released.** Phases 0 and 1 are done in the
tree; every other phase below is still only agreed. Decisions were made in
discussion, and the reasoning is recorded so a later reader can tell which parts
are load-bearing and which were arbitrary.

Phases ship as **one release each**, so each can be tested against real data
before the next is built. Phase 1 (the delivery outbox) goes first: it is a live
bug and independent of the features.

## Decisions

| Decision | Answer |
|---|---|
| Delivery model | Transparent enrichment of existing Untis responses |
| Homework "done" | **Re-confirmation required — see Phase 0** |
| Absence notes | Private to the student; admins see existence, not text |
| Absence class/day | Proxy-derived metadata, not typed by the student |
| Technik events | Manually configured, per student, specific dates, admin UI |
| Technik visibility | Everywhere a student's timetable is served, **including `.ics`** |
| Delivery-loss fix | Bundled into this plan, but released first and separately |
| Homework event recurrence | Not applicable — specific dates, one row each |
| App fetch style | `STUDENT` (personal), not `CLASS` |

## Phase 0 results (probe complete)

A throwaway read-only program (since deleted) called the real school with the
stored dev credentials and printed JSON *structure* only — key names, types,
digit counts, character patterns — so no personal data entered any log or commit.

Method note, because it invalidated two intermediate runs: the self-auth TOTP
expires every 30s, and upstream answers a **stale** TOTP with a silently *empty*
result rather than an error. Every meaningful call therefore recomputes the TOTP
immediately before the request. Anything reading "0 results" from those two runs
was an artefact, not a fact.

### Finding 1 — homework has a stable id; its `completed` flag is the teacher's

`homeWorks[]` entries are richer than assumed:

| field | shape | note |
|---|---|---|
| `id` | number, 5 digits | **stable, globally unique per assignment** |
| `completed` | bool | **the teacher's call, not the student's** |
| `remark` | string or null | empty/null in all samples |
| `text` | string | the assignment body |
| `startDate` / `endDate` | `YYYY-MM-DD` | due window |
| `lessonId` | number, 6 digits | links to the period |
| `attachments` | array | empty in all samples |

**Decision: the student's own "done" state is stored proxy-local.** `completed`
records the teacher's decision about the assignment itself — three classmates
sharing one class showed identical values (2 true / 12 false) across all 14
shared ids — so it cannot express "I finished mine". It stays untouched; we never
write it.

The stable upstream `id` is still the right storage key, which removes the
content-hash fallback and its "a teacher edits the text and the flag resets" wart.

Also discovered: **`getHomeWork2017`** is a dedicated method returning
`{homeWorks, lessonsById}` (3 entries over ±7d, 9 over ±30d). A cleaner read
source for Phase 2 than digging homework out of timetable periods.

### Finding 2 — students *can* read their own absences; the flags are the trap

`getStudentAbsences2017` accepts only `startDate`, `endDate`, `includeExcused`
and `includeUnExcused`. There is **no `id` and no `type`** — the student comes
from the auth block. Passing `id`/`type` changes nothing.

**Omitting the two `include` flags returns an empty list with no error.** An
account with three absences returns 0 without them and 3 with them, which is
indistinguishable from "no absences". This produced a whole round of wrong
conclusions before the parameter list was checked against the published Untis
clients; probe date range and account identity were never the problem.

Methods that do not exist on this school at all:

| method | result |
|---|---|
| `getAbsences` | `-32601 Method not found` |
| `getAbsences2017` | `-32601 Method not found` |
| `getOwnAbsence` | `-32601 Method not found` |
| `getPersonAbsence` | `-32601 Method not found` |

`README.md` advertised two of those; corrected.

**No teacher identity is needed** for a student to read their own absences. The
teacher-backed `getPeriodData2017` path remains relevant only for the class
register and editing, which is a separate concern.

### Finding 3 — the absence element shape, and the derived-metadata plan

Probe-verified against an account with three absences:

| field | shape | use |
|---|---|---|
| `id` | number, 6 digits | **stable → the `absence_key`** |
| `startDateTime` / `endDateTime` | `YYYY-MM-DDTHH:MM` | **day** via weekday; window for subject matching |
| `klasseId` | number, 2 digits | **class**, resolved to a name from `users.class_name` |
| `absenceReasonId` / `absenceReason` | number, 2 digits / short code | reason, resolvable via `masterData.absenceReasons` |
| `text` | string | the **teacher's** comment — already upstream, not our note |
| `excused` | bool | excused state |
| `excuse` | object `{date, excuseStatusId, id, number, text}` | the excusing workflow; `excuse.text` is upstream's own text slot |
| `studentId` | number, 4 digits | identity check when enriching |
| `owner`, `studentOfAge` | bool | unused |

There is **no student-authored note field**, which confirms the notes store must
be proxy-local. Note that `text` is the teacher's comment and `excuse.text` is
upstream's excuse text — the private student note must be a distinct field, not
either of these.

Subject is not present on an absence, but the plan already called for deriving
metadata rather than typing it: match `startDateTime`/`endDateTime` against the
same student's timetable periods to recover the subject. That is now feasible,
since both halves are available from the student's own identity.

## Decisions settled by the probe

1. **The student's "done" state is stored proxy-local.** Upstream `completed` is
   the teacher's decision about the assignment, not the student's, so it cannot
   carry the feature and is never written. Keyed on the stable upstream `id`.
2. **Students read their own absences with their own account — no teacher
   identity.** The earlier worry about needing a teacher-backed fetch is
   withdrawn; it came from probing the method without its `include` flags.
3. **`absence_key` is the upstream absence `id`.** 6 digits, stable.
4. **Absence notes stay proxy-local and private.** No upstream student-note field
   exists; `text` is the teacher's comment and `excuse.text` is upstream's excuse
   text, so neither may be reused.

Still open, and worth deciding before the matching phase rather than during:

- How long a done-flag or a note is kept, given both reference records that age
  out of the snapshot.
- Whether the app renders homework from the timetable or needs `getHomeWork2017`
  exposed as a read endpoint of its own.



## What exists today

The starting point is emptier than the feature list suggests — but the probe
corrected it in two places. Per the findings above:

- **Absences** are pure byte-for-byte passthrough, with no table, no parser and
  no reason catalogue on our side. `masterData.absenceReasons` arrives in
  responses we already handle and is the natural source for reason text.
- **Homework** is consumed at `calendar.go:531`, which appends a
  "Hausaufgabe (bis …)" line to iCal descriptions from `homeWorks[]`. That array
  turned out to carry `id` and `completed` — see Finding 1.
- **Per-student timetable injection does not exist.** Every period the proxy
  serves is decoded from an upstream response. There is no synthetic period, no
  overlay, no user-authored timetable storage anywhere.
- **There is no method allowlist.** Gating is default-allow plus a verb-prefix
  rule (`isWriteMethod`, `proxy.go:424`). Method *naming* silently decides
  security: anything starting with `set`/`add`/`update` is `editor`-only, anything
  else is open to any logged-in session. This matters for any decision to write
  `completed` upstream.

## The shared enrichment seam

All three read-side features cut the same three places, which currently write
upstream bytes straight to the response:

- `jsonrpc_intern.go:588` — a student's own `STUDENT` timetable, raw-forwarded
- `jsonrpc_intern.go:112` — the info-center default branch, where
  `getStudentAbsences2017` lands
- `calendar.go` / `reconstruct.go:331` — the token paths behind `/week/{token}`
  and `/api/calendar/{token}.ics`

One helper, reused:

```go
enrichJSON(raw []byte, mutate func(doc any) error) []byte
```

Four properties it must have. Each of these is a bug I would otherwise ship:

1. **`dec.UseNumber()`.** Decoding into `any` turns every number into `float64`.
   Untis period ids are 10-digit, inside float64's exact range, so it probably
   survives — but "probably" is how ids get silently mangled later.
   `json.Number` preserves the original text.
2. **Never fail the request.** If decode, mutate or encode fails, write the
   original bytes and log. A bug in enrichment must not turn a working timetable
   into an error response.
3. **Preserve unknown fields.** Mutate `map[string]any` in place; never re-model
   Untis objects as typed structs. Fields we don't know about survive untouched,
   and Untis adding one later needs no change here.
4. **Identity must be independently established.** The info-center path learns
   the username from the *client-supplied* auth block
   (`jsonrpc_intern.go:88`). Keying private notes off a claim the client controls
   is a leak waiting to happen. Rule: enrich only when the viewer is known from a
   session or a calendar token, or when the response itself carries a person id
   matching the requester. Otherwise pass through untouched.

## Phase 0 — probe (complete)

Executed and discarded. Results are the findings at the top of this document;
the program and its database snapshot have been deleted. The probe is redone
only if a needed answer is still missing (currently: the absence element shape).

## Phase 1 — delivery outbox (released first)

Root cause: the version bump commits at `store.go:1860` *before* the send at
`notify.go:210`. A failed post is logged and never retried, and the next poll
sees no change. Same class of bug as the re-notification flood, opposite
direction — one change sent too many times vs. one change sent zero times.

The difficulty is atomicity. The digest is built from the changed rows, which
only exist inside the snapshot transaction, so it cannot be rendered after
commit without losing the guarantee. The store method now takes an enqueue
callback: it collects the changed rows, hands them to the callback, and commits
only if the callback succeeds.

Two consequences worth keeping in mind for later phases:

- Everything the callback needs is resolved **before** the transaction opens.
  The store pool is a single connection, so any read inside that transaction
  deadlocks against the connection it already holds. That covers the class
  display name for the digest, and the identity of every subscription target
  (a STUDENT target needs a class lookup, a TEACHER/ROOM/SUBJECT target needs a
  name lookup). `resolveTargets` exists solely for this. A destination created in
  the gap between that read and the commit misses that one change, which is the
  right trade against a deadlock.
- One row per **destination**, not per change. A single unreachable endpoint must
  not replay deliveries that other endpoints already received successfully.

```sql
CREATE TABLE notification_outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  school TEXT NOT NULL, class_id INTEGER NOT NULL, version INTEGER NOT NULL,
  dest TEXT NOT NULL, dest_id INTEGER NOT NULL,   -- webhook | ntfy + subscription id
  payload TEXT NOT NULL,
  state TEXT NOT NULL,        -- pending | sending | dead
  attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

Delivered rows are deleted rather than kept, so the table stays small; exhausted
rows are kept as `dead` so a broken destination stays visible. A worker drains it
with capped backoff (30s doubling to 15m, 8 attempts), reclaiming rows abandoned
in `sending` at startup. Deleting or disabling a subscription drops its backlog
instead of retrying it forever.

Both ntfy and webhooks route through it, which replaces the bespoke webhook
retry with a shared one. `GET /admin/outbox` (admin auth) reports counts and the
recent dead rows with their failure reason; `/admin/status` carries the counts.

**Risk:** this refactor touches the poll hot path that both the flood fix
(v1.4.4) and the reinstate fix (v1.4.5) depend on. Run the full suite plus those
two regression tests specifically after this phase.

## Phase 2 — personal homework done

Ready. `homeWorks[].id` is the storage key, and the state is proxy-local per
Finding 1.

```sql
CREATE TABLE homework_done (
  school TEXT NOT NULL, username TEXT NOT NULL, hw_id INTEGER NOT NULL,
  done_at DATETIME NOT NULL,
  PRIMARY KEY (school, username, hw_id)
);
```

Enrichment adds `done` and `doneAt` onto each `homeWorks[]` entry by looking up
`hw_id` for the resolved viewer, alongside the untouched teacher-owned
`completed`. One new session-authenticated write endpoint, scoped to the session
user, rejecting any attempt to write another user's rows.

Prefer reading homework from **`getHomeWork2017`** if the app can use it — it
returns `{homeWorks, lessonsById}` directly instead of requiring every timetable
period to be walked. That is a question for the app side, not a blocker.

## Phase 3 — absence enrichment + private notes

Ready. `absence_key` is the upstream absence `id`, and no teacher identity is
involved.

```sql
CREATE TABLE absence_notes (
  school TEXT NOT NULL, username TEXT NOT NULL, absence_key INTEGER NOT NULL,
  note TEXT NOT NULL DEFAULT '', updated_at DATETIME NOT NULL,
  PRIMARY KEY (school, username, absence_key)
);
```

Intercept `getStudentAbsences2017` in the info-center default branch — taking
care to pass `includeExcused`/`includeUnExcused` through or upstream returns an
empty list — and for each absence add the derived metadata:

- **class** from `klasseId`, resolved to a name via `users.class_name`
- **day** as a weekday name from `startDateTime`
- **subject** by matching `startDateTime`/`endDateTime` against the same
  student's own timetable periods
- **reason** text resolved from `masterData.absenceReasons` by `absenceReasonId`

then merge the student's note into a field distinct from `text` (teacher's
comment) and `excuse.text` (upstream's excuse text).

Identity is verifiable from the response itself: `studentId` is present on every
entry, which satisfies the "enrich only when identity is independently
established" rule without relying on the client's auth claim.

Notes appear on no editor or teacher surface — enforced by the absence of a code
path rather than by a check, which is the cheapest form of that guarantee.

## Phase 4 — Technik events

```sql
CREATE TABLE student_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  school TEXT NOT NULL, username TEXT NOT NULL,
  date TEXT NOT NULL, start_time TEXT NOT NULL, end_time TEXT NOT NULL,
  title TEXT NOT NULL, subject TEXT, room TEXT, teacher TEXT, description TEXT,
  created_by TEXT NOT NULL, created_at DATETIME NOT NULL
);
```

Specific dates only, so no `RRULE` and no recurrence maths. Injected as synthetic
periods into the student `getTimetable2017` path and the token paths feeding
`/week/{token}` and `/api/calendar/{token}.ics`. Each carries `isCustom: true` so
the app and the ICS feed can tell a hand-authored event from a real lesson.

Admin UI in the existing dashboard: pick student → date/time/title → add, with a
delete per row.

## Security invariants

These are the design, not an afterthought:

1. **Personal overlays are keyed by resolved viewer identity, never by class.**
   Each student sees their own done-flags, events and notes on any path they can
   reach; nobody can ever see another's. This is why the overlay can safely sit
   on shared paths like a pooled class timetable — the overlay differs per viewer,
   the underlying data does not.
2. **No enrichment without established identity** — pass through untouched instead.
3. **New write endpoints are session-scoped**, and must not be reachable by
   supplying another username in the body.
4. **Technik admin routes are admin-gated**, not editor.
5. **Notes appear on no teacher/editor surface.**

## Docs to update, per phase

- `APP-INTEGRATION.md` — the enriched absence shape, the homework `done` field,
  the Technik `isCustom` marker, the new write endpoints
- `ADMIN-WEBHOOKS-NTFY.md` — the event manager, and outbox delivery visibility
- `TESTING-CHECKLIST.md` — coverage per phase
- `README.md` — the flag/endpoint tables

## Open questions

Not to be guessed. Decide before the matching phase, not during it:

- **Does the app render homework from the timetable response, or need a dedicated
  list?** `getHomeWork2017` exists and would serve as that list directly; if the
  app can use it, Phase 2 grows a read endpoint rather than a write-only one.
- **How long should a done-flag or a note live?** Both key on ids of records that
  age out of the snapshot. Without a cleanup rule the tables grow forever, and a
  reinstated lesson's key may or may not still resolve.
- **Should subject recovery for absences be best-effort?** Matching an absence's
  time window against timetable periods will miss cancelled or moved lessons. A
  miss should degrade to no subject rather than a wrong one.
- **Deleting a Technik event in the admin UI — does it disappear from subscribed
  calendars?** ICS clients cache aggressively; this is a product question, not a
  technical one.
- **Should Technik edits raise timetable-change events?** Phase 1 now delivers
  changes through a durable outbox, so emitting an SSE/ntfy change when an admin
  edits an event is nearly free — but whether a student should be told about an
  admin edit at all is a product call.
