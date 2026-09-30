# Tiered Permission Model — Basic / Reconstruction / Boosted / Editor

Status: implemented and committed. Decline/stale notes removed.

## Goal

Tiered permission model for the untis-proxy backend:

1. **Basic** (default, no flag) — every user gets the pool of classes plus their
   own personal student timetable, and can **read their own absences**.
   **Everyone** (student and non-student) donates their class to the pool on
   login.
2. **Reconstruction** (flag `recon`) — teacher / room / subject timetables
   reconstructed 100% from pooled class data. Available as a global switch or a
   per-user override.
3. **Boosted** (flag `boosted`) — class/teacher/room/subject timetables
   raw-forwarded through ANY saved teacher account (`BoostedSourceAccounts`),
   INCLUDING the user's own personal (STUDENT) timetable — teacher-grade future
   horizon. Info center uses the user's own account, never a teacher account. Per-user
   only. No editing powers on its own.
4. **Editor** (flag `editor`) — unlocks absence + lesson/subject write (editing)
   methods. Independent per-user flag.

## Permission table

| Feature | Meaning | Global? |
|---|---|---|
| `recon` | Teacher/room/subject reconstruction from pooled classes | yes (switch + per-user override) |
| `boosted` | Raw teacher-account forwarding (no editing) | no (per-user only) |
| `editor` | Boosted *plus* the absence + lesson/subject write (editing) methods | no (per-user only) |

Enforced rule: **Boosted XOR Recon** — the flags are mutually exclusive per user.
Granting one side auto-revokes the other. `editor` implies `boosted` (editoring
is boosted-with-editing), so it never triggers the recon exclusion.

## Tier behavior

### Basic (default)
- Personal timetable: own stock Untis data (own account).
- Class pool: all pooled classes visible, served via the pooled owner account.
- Teacher/Room/Subject: not visible.
- Absence reads (own data): **allowed** (default). Lesson/subject writes: blocked.
- Donate to pool: everyone (class donated at login).

### Recon (`recon`)
- Grants teacher/room/subject reconstruced timetables from pooled class data.
- Absence reads: still allowed (default). Writes: still blocked unless editor.
- Caveat: only elements present in pooled class data are servable.

### Boosted (`boosted`)
- Class/teacher/room/subject timetables: ALL served raw, forwarded through the
  first available saved teacher account.
- Own personal (STUDENT) timetable: served the same way (through the teacher
  account), giving it teacher-grade future horizon instead of the student
  account's ~1-week limit. If nobody has saved a teacher account yet, boosted
  falls back to Basic for the personal timetable and errors on other elements.
- MasterData: full upstream element lists (displayAllowed set for recon/boosted).
- **No** editing powers: write methods still blocked unless the user also holds
  `editor`.

### Editor (`editor`)
- Grants the absence + lesson/subject **write (editing) methods**
  (`set`/`add`/`update`/`delete`/`change`) for the user's own requests.
- Does **not** grant raw forward-timetable access on its own — pair with
  `boosted` for the full experience.
- Absence reads are already default-allowed for everyone.

## Enforcement rules

- Mutual exclusion (Boosted XOR Recon) in the store + CLI; `editor` unaffected.
- Donation: everyone donates their class at login.
- Write gate: mutation methods need `editor`; enforced in every forwarding path
  (JSON-RPC self-auth + session, legacy JSON-RPC passthrough, REST). Absence
  reads pass through for all users.
- Data source: recon → reconstruction from pooled data; boosted → raw from saved
  teacher accounts. Non-overlapping.

## Calendar subscriptions

Token-based `.ics` feeds for all element types:

- **class** → pool members only
- **personal/student** → session user only
- **teacher/room/subject** → requires recon OR boosted

POST body accepts `classId`, `personal`, `teacherId`, `roomId`, `subjectId`
(exactly one). Optional `timezone` (default `Europe/Berlin`).

`GET /me` returns `{username, level, permissions}` for the current session.

CLI: `untisctl calendar create|list|revoke` with fuzzy name lookup.

## Files (implementation notes)

- `internal/store/store.go` — `FeatureRecon`/`FeatureBoosted`/`FeatureEditor`,
  `BoostFeatures`, `ReconAccess`/`BoostedAccess`/`EditorAccess`, `setPerm`
  mutual exclusion (recon↔boosted only), `BoostedSourceAccounts()`,
  `SetPerm`/`SetBoostedFlag`.
- `internal/proxy/jsonrpc_intern.go` — everyone-donates at keyLogin; boosted raw
  branch (skips STUDENT) in getTimetable2017; write gating (editor) in the
  default and self-auth paths; single-recon displayAllowed.
- `internal/proxy/jsonrpc.go`, `internal/proxy/rest.go` — write gating (editor)
  in passthrough/REST (absence reads default-allowed); boosted raw for weekly
  REST elements (skips STUDENT).
- `cmd/untisctl/main.go`, `cmd/perm/main.go` — recon|boosted|editor grantable
  features + auto-revoke (XOR) + perms list display. Calendar create|list|revoke
  with fuzzy name lookup via `LookupElement`.
- `internal/proxy/calendar.go` — `handleCalendarToken` (all element types),
  `handleCalendarICS` (branches on element type, configurable timezone).
- `internal/store/store.go` — `LookupElement` (fuzzy name resolution),
  `SaveMasterNames`, `ClassToken` struct with `ElementType`/`ElementID`/`Timezone`.
- `README.md` — documents the tiers.