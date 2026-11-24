# Basic access, inherited-group exceptions and compatibility diagnostics

Implemented 2026-11-24. These controls do not reset existing accounts automatically.
All administrative routes below require a dashboard administrator session and
same-origin browser requests for changes. No user-facing web client is included.

## Upstream-only Basic preset

The built-in `v2.upstream_basic` group grants stock upstream timetable reads,
own class/personal timetable capabilities, and own absence reads. It grants no
pool contribution or pooled reads, reconstruction, saved-teacher extensions,
notifications, calendar links, analytics, private notes, homework completion, or
proxy-created personal/aggregate events.

`Use my own upstream timetable access` (`timetable.upstream.view`) means timetable
requests go through the caller's own upstream identity, and upstream determines
which targets are visible. It never borrows another account. Its stock timetable
catalogue remains unchanged unless additional catalogue/timetable supplements
are explicitly granted. Existing upstream write/edit gates and read-only source
account restrictions remain in place: Basic is a read baseline, not a write grant.

Assigning Basic as an extra group is additive: it does not silently remove other
groups or automatic defaults. For an actual clean baseline, choose a person under
**Permissions → Choose groups → Reset to upstream-only Basic**. The confirmation
explains that this atomic reset:

1. Clears that person's direct group assignments, direct permission rules and
   preferences in the selected school and their global person-level scope.
2. Turns off automatic role/everyone-group inheritance for that person, including
   future defaults.
3. Assigns the Basic group directly.

Other users, other school-specific assignments, the person's dashboard-admin
flag, credentials, notes, events and upstream data are not changed. Existing
calendar/notification subscriptions are retained, but their capability checks
stop serving/delivering when permissions are removed. Add extra groups after the
reset to enable individual proxy features again. Global server disables still win.

API: `POST /admin/access/apply-basic?school=NAME&user=USERNAME`.
This is an explicit permission reset, not a preview. Dashboard-admin management
powers remain separate from the person's capability baseline.

Private additions now have independent capabilities:

| Capability | Proxy-local feature |
| --- | --- |
| `homework.own.manage` | Read/write personal homework completion flags |
| `absence.notes.own.manage` | Read/write private absence notes and their enrichment |
| `events.personal.view` | Add assigned personal events to the caller's own timetable |

Existing student/teacher/guardian base groups receive these grants to preserve
previous behaviour. Basic excludes the inherited base groups through its reset
policy, so these features remain off until explicitly added. Stored private data
is preserved. Unreadable account/rule storage leaves upstream responses undecorated.

## Per-person inherited groups

`GET|PUT /admin/access/default-group-policy?school=NAME&user=USERNAME`:

```json
{"inherit":true,"excludedGroupIds":[12,18]}
```

The `inherit` boolean is required on PUT. Exclusions remove a group's contribution
only when it arrives through role defaults or the everyone audience. They do not
create a blanket permission denial. Direct assignment of the same group still
works, and direct grants remain independent. A global inherited group can be
excluded for one person without changing it for anyone else. Foreign-school
custom groups cannot be entered in the exclusion list.

With `inherit:false`, only direct assignments/grants apply; future role/everyone
defaults are suppressed too. Re-enabling inheritance restores the current defaults,
subject to the saved exclusion list. The dashboard shows inherited, excluded and
automatic-groups-off states separately from directly assigned groups.

`GET /admin/access/effective` additionally returns `inheritedGroupIds`,
`excludedDefaultGroupIds`, and `inheritRoleDefaults`. Permission-group preview uses
the same filtered inheritance inputs as enforcement. It continues to preview
unsaved group changes, not the destructive Basic reset. These settings persist
across restart and are cleared when the account is deleted.

## Stored compatibility diagnostics

`GET /admin/access/compatibility?school=NAME&user=USERNAME` performs **no upstream
network request**. It reports:

- Stored account role/person type, guardian status and linked-person count.
- Whether a replay token/password is stored, without returning their values.
- Last authenticated upstream administrator flag, or null when not reported.
- Current local admission decision and configured school-year ranges.
- The last explicit probe, if any.

Stored identity is labelled as stored, not as a fresh compatibility result.
Configured school years do not prove that upstream retains those timetables.
An unknown administrator flag must not be treated as confirmed non-administrator.

## Explicit read-only probe

`POST /admin/access/compatibility-probe?school=NAME&user=USERNAME`:

```json
{"from":"2024-09-02","to":"2024-09-08"}
```

A guardian can additionally select `personId` for a student returned in that
account's freshly verified linked identities. Arbitrary person IDs and accounts
from another school are never queried. Without a verified active/linked identity,
the diagnostic does not fall back to a guessed stored student or teacher.

The probe creates a separate login using the selected account's own credentials,
reads upstream identity/configuration and one timetable date window, then logs
out that separate session. It never writes absences, lessons, events or other
upstream school records. It does not change the stored account's identity,
admission classification, group assignments or credentials. It can inspect a
locally blocked account for an authenticated administrator who is investigating
compatibility; this does not admit that account to the application.

A window is inclusive, past/current (UTC cutoff), and at most 31 days. Requests
share the bounded diagnostic/analytics worker slots and have a per-account probe
limit. Responses contain dates, status, lesson count and classification flags,
not raw lessons, private notes, passwords, tokens or upstream error bodies.
The last result is retained locally per school/account, and the probe is audited.

- `observed_nonempty`: upstream returned lessons for that window.
- `empty_or_unavailable`: upstream returned an empty list. This can mean holidays,
  no scheduled lessons, a visibility limit or missing history; it proves neither
  support nor lack of support for historical access.
- `upstream_rejected`: upstream returned an application-level rejection.
- `identity_selection_required`: no verified usable identity was selected.
- `unavailable`: credentials, network or response structure did not permit a result.

A successful window does not certify unlimited history, every account type, or
another school's upstream implementation. Probe other representative ranges only
when needed. No automatic broad history scan runs in the background.

Tests cover Basic isolation and stacking, inherited/global exceptions, preview
consistency, deletion cleanup, own-source forwarding/catalogues, private-feature
permissions, scoped API failures and bounded read-only probes. All upstream tests
use fake servers; live teacher writes are not used.

## Rollback note

Older builds do not understand per-person default exclusions. Once an operator
uses these controls, do not roll back to an older build with live access exposed
without restoring/reapplying the intended restrictions. A pre-deployment backup
and retained image support recovery, but restoring an older permission snapshot
can restore the older, broader access too. The deployment itself does not apply
Basic or exclusions to any real account automatically.
