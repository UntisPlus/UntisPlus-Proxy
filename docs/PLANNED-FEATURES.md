# Planned work — personal homework, absence notes, Technik events, delivery outbox

Status: **agreed, not started.** Every decision below was made in discussion; the
reasoning is recorded so a later reader can tell which parts are load-bearing
and which were arbitrary.

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

### Finding 1 — homework already has a done flag and a stable id

`homeWorks[]` entries are richer than assumed:

| field | shape | note |
|---|---|---|
| `id` | number, 5 digits | **stable, globally unique per assignment** |
| `completed` | bool | **the done flag already exists upstream** |
| `remark` | string or null | per-entry free text, empty/null in all samples |
| `text` | string | the assignment body |
| `startDate` / `endDate` | `YYYY-MM-DD` | due window |
| `lessonId` | number, 6 digits | links to the period |
| `attachments` | array | empty in all samples |

Across three classmates in the same class, all 14 homework ids were shared, and
`completed` was uniform (2 true / 12 false) for all three. The same field is
present in the teacher and pooled-class views. This is **consistent with a
global per-assignment flag** and does not prove per-student state — but a
read-only probe cannot separate the two. Settling it needs either an upstream
write call (a side effect, needs consent) or a student with a genuinely
different completion state.

This is why the homework decision is re-opened below.

### Finding 2 — student absences are not available to students

| method | result |
|---|---|
| `getStudentAbsences2017` | exists, authenticates, returns `{absences: []}` — **0 for all 7 student accounts over ±180 days**, and 0 for `id=0`/own, for a `CLASS` id, and without a date range |
| `getAbsences` | `-32601 Method not found` |
| `getAbsences2017` | `-32601 Method not found` |
| `getOwnAbsence` | `-32601 Method not found` |
| `getPersonAbsence` | `-32601 Method not found` |
| `getStudentAbsences2017` with `type: "TEACHER"` | `-8526 invalid user role` |

So the three absence methods this README advertised are not just unused — two of
them **do not exist on this school at all**. `README.md` was corrected.

The actual absence data path is `getPeriodData2017`, and upstream withholds its
`absences` field from student identities, returning it only for a **teacher**
identity (already documented in `README.md` under "The absence/lesson editor
needs a teacher identity"). That is the whole reason the student-level list is
empty.

Consequence: **a student cannot see their own absences through any
self-authenticating upstream call.** Surfacing them to a student requires the
proxy to fetch `getPeriodData2017` through a teacher account on their behalf.
That is a permission-model decision, not an implementation detail.

### Finding 3 — the reason catalogue is available

`masterData.absenceReasons` carries 9 entries of
`{id, name, longName, active, automaticNotificationEnabled}`. This is the
catalogue needed to render an absence reason code as human text, and it is
already in responses the proxy handles.

## Decisions needing re-confirmation

Raised by the probe, not answered unilaterally:

1. **Homework done — proxy-local or upstream `completed`?** The original
   "never written upstream" decision was made when no done flag was known to
   exist. Now that `completed` does, writing it upstream would make the state
   visible in the official WebUntis app too. Against that: the proxy gates
   `set*`-style methods to `editor`, so a student write would need both a
   permission carve-out and an unverified upstream write method. Proxy-local
   stays fully under our control and still syncs across the user's devices,
   because the proxy is the app's only source. Either way, **precedence** must
   be defined: what shows when local and upstream disagree (they already do —
   two assignments are `completed: true` upstream today).
2. **Absence visibility for students — is teacher-backed fetching acceptable?**
   Without it, students get their private notes but no absences to attach them
   to.
3. **Absence element shape is still unknown**, because no account has an absence
   to inspect. Needs a test absence created through the teacher UI, or a known
   absent student.



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

The difficulty is atomicity. The digest is currently built from `PendingChanges`
*after* commit, so it cannot be enqueued in the same transaction without
restructuring. Plan: change `ReplaceClassSnapshot` to return the changed rows
instead of just a count, let the caller build the digest before commit, and
insert the outbox row in that same transaction. This needs a callback parameter
on the store method.

```sql
CREATE TABLE notification_outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  school TEXT NOT NULL, class_id INTEGER NOT NULL, version INTEGER NOT NULL,
  payload TEXT NOT NULL, state TEXT NOT NULL,      -- pending | sent | dead
  attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL, sent_at DATETIME
);
```

A background worker drains it with capped backoff, and the dashboard surfaces
failures instead of burying them in the log. Both ntfy and webhooks route through
it, which replaces the bespoke webhook retry with a shared one.

**Risk:** this refactor touches the poll hot path that both the flood fix
(v1.4.4) and the reinstate fix (v1.4.5) depend on. Run the full suite plus those
two regression tests specifically after this phase.

## Phase 2 — personal homework done

**Blocked on re-confirmation.** The stable key question is answered — the
upstream `id` is globally unique per assignment and is the obvious
`homeWorks[].id` to key on, which also removes the content-hash fallback and its
"teacher edits the text and the flag resets" wart. What is *not* answered is
whether to store the done state locally or write `completed` upstream. Both
variants use the same key and the same enrichment; they differ in where the
write lands.

```sql
CREATE TABLE homework_done (
  school TEXT NOT NULL, username TEXT NOT NULL, hw_id INTEGER NOT NULL,
  done_at DATETIME NOT NULL,
  PRIMARY KEY (school, username, hw_id)
);
```

Enrichment adds `done` and `doneAt` onto each `homeWorks[]` entry by looking up
`hw_id` for the resolved viewer. One new session-authenticated write endpoint,
scoped to the session user, rejecting any attempt to write another user's rows.

Precedence, whichever storage wins, must be explicit: two assignments are
`completed: true` upstream today, so a local-only store starts out disagreeing
with upstream unless upstream is treated as the seed.

## Phase 3 — absence enrichment + private notes

**Partly blocked.** The notes half is unaffected by the probe and is safe to
build: there is no upstream note field anywhere, so a proxy-local note store is
pure gain. The *display* half is blocked on Finding 2 — a student cannot read
their own absences upstream, and `absence_key` cannot be derived because no
account has an absence to inspect.

```sql
CREATE TABLE absence_notes (
  school TEXT NOT NULL, username TEXT NOT NULL, absence_key TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '', updated_at DATETIME NOT NULL,
  PRIMARY KEY (school, username, absence_key)
);
```

Unblocks, in order:

1. Decide whether the proxy may fetch `getPeriodData2017` through a teacher
   account on a student's behalf. If no, students get notes attached to absence
   keys they supply themselves, which is not a feature.
2. Obtain one real absence to fix the element shape and `absence_key`. Cheapest
   route: create a test absence through the teacher's own WebUntis UI, then
   re-probe. A known-absent student account would also do.
3. Then: intercept `getStudentAbsences2017` and/or the `getPeriodData2017`
   response, add the derived `class` (from `users.class_name`) and `day`/date,
   resolve the reason text from `masterData.absenceReasons`, and merge the
   student's note.

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

Not to be guessed. Re-ask against the Phase 0 output:

- **Does the app render homework from the timetable response, or need a dedicated
  list?** If it needs a list, Phase 2 grows a read endpoint, not just a write one.
- **How long should a done-flag or a note live?** Homework keys reference periods
  that age out of the snapshot. Without a cleanup rule the tables grow forever,
  and a reinstated lesson's key may or may not still resolve.
- **Deleting a Technik event in the admin UI — does it disappear from subscribed
  calendars?** ICS clients cache aggressively; this is a product question, not a
  technical one.
