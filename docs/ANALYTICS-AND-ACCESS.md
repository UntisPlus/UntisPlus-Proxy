# Cancellation analytics and access controls

Implemented backend contract, 2026-11-23. No user-facing web client or graph
rendering is included. Analytics is calculated on demand, is not collected in the
background, and is disabled by default. No automatic historical archive or
report cache is created.

## Operator feature switches

Dashboard: **Permissions → Feature switches & login rules**. Select server-wide
controls to edit the master switches, or select a school for stricter school
settings. Use Load controls to refresh their state.

`GET /admin/access/features?school=NAME` lists effective features.
`PUT /admin/access/features?school=NAME` accepts:

```json
{"feature":"analytics","enabled":true}
```

An empty school selects the server master switch. The feature keys are
`analytics`, `technik`, `calendar`, `ntfy`, `webhooks`, `changes`,
`reconstruction`, `homework`, and `absence_notes`. A school can restrict a
server-enabled feature, but cannot override a server disable. Analytics needs
both an enabled server switch and account capability grants. Dashboard admin
status does not bypass analytics scope or export grants. Existing per-capability
server gates also treat global denies as hard ceilings.

## Analytics permissions and approved sources

| Capability | Meaning |
| --- | --- |
| `analytics.personal.view` | Analyse the caller's own upstream timetable |
| `analytics.personal.history` | Permit personal dates older than the last 366 days; personal.view is still required |
| `analytics.teacher.view` | Analyse explicitly approved teacher IDs through a saved teacher source |
| `analytics.class_history.view` | Analyse an explicitly approved class-history profile |
| `analytics.export` | Download JSON or CSV ZIP within the same data permissions |

Teacher/class-history grants alone do not approve targets. Configure a person's
scope in the dashboard's analytics configuration, or use:

`PUT /admin/access/analytics-access?school=NAME&user=USERNAME`

```json
{
  "teacherIds": [42],
  "profiles": [{
    "id": "my-class-history",
    "segments": [
      {"classId": 81, "from": "2023-08-01", "to": "2024-07-31"},
      {"classId": 91, "from": "2024-08-01", "to": "2025-07-31"}
    ]
  }]
}
```

GET on the same URL returns the configured scope. Class IDs and exact date
ranges, rather than names alone, identify historical classes. A profile may
contain multiple classes in the same date range for a cohort. Duplicate teacher
assignments with the same upstream period identity are counted once.

Configure the school's actual year boundaries using
`PUT /admin/access/analytics-years?school=NAME`:

```json
{"years":[{"name":"2024/25","from":"2024-08-01","to":"2025-07-31"}]}
```

Use the actual dates from upstream; August dates above are examples. Year names
must be unique and ranges must not overlap. GET reads the configuration. Lessons
outside configured years are explicitly grouped as `unmapped`, not assigned an
invented school year. This is configuration, not an automatic upstream-year
importer.

## JSON endpoint

`POST /api/analytics/teacher-cancellations?school=NAME` requires a proxy
`JSESSIONID` session for an admitted account of that school. The body accepts:

```json
{
  "mode":"personal",
  "from":"2024-08-01",
  "to":"2025-07-31",
  "minLessons":26,
  "anonymise":true,
  "format":"json"
}
```

- `mode`: `personal` (default), `teacher`, or `class-history`.
- `teacherId`: required and approved for teacher mode; optional as a filter of
  teachers appearing in personal/class-history data. It never widens that scope.
- `profileId`: required for class-history mode; ignored profiles cannot be
  supplied in personal/teacher mode.
- `schoolYear`: optional configured year name; fills omitted from/to and rejects
  explicit dates outside that year.
- `minLessons`: minimum teacher assignments after date/source filtering,
  default 0. Use 26 to match the original app's initial slider value.
- `includeIrregular`: false by default; irregular/substitution-coded lessons
  are excluded unless explicitly requested.
- `excludeTeacherPrefixes`: default `["TA_","U "]`; an explicit empty array
  disables these name-prefix exclusions. Lessons without usable teacher IDs are
  excluded and counted separately.
- `format`: `json`, `export-json` (attachment), or `csv-zip` (attachment).

Dates are inclusive; the current-day/future cutoff uses UTC. A request covers at most 366 days and 32 source windows;
upstream requests are split into at most 31-day chunks. Personal history has no
proxy retention cutoff back to practical historical dates (1900 or later), but
requires the extra history grant for older dates. Request multiple ranges to
cover multiple years. Upstream may not retain or expose old data. Future days
are excluded; an entirely future range is rejected. Guardian personal identity
is not treated as the first child: use separately approved class history until
child-scoped analytics is implemented.

Two reports may execute concurrently per process. Each account is limited to
six accepted report attempts per minute. Busy/limited requests return HTTP 429;
invalid filters return 400, missing sessions 401, and denied scopes/features 403.
Reports recheck grants, admission, targets and master enablement before returning.

## Report data and definitions

The JSON includes `version`, a report-local `reportId`, source mode, dates,
`generatedAt`, requested filters, warnings, and observed/unavailable windows.
`complete` means all requested approved windows were read and the class-history
range has no gaps. It cannot establish that upstream retained every historical
lesson; the response states this limitation. Unreadable windows are partial data,
not successful zero-cancellation observations. Empty denominators have null rates.

Statistics include:

- Each teacher's scheduled/cancelled/not-cancelled counts, percentage rates,
  cancellation-count rank and all tied worst weekdays.
- Teacher-assignment totals, unique lesson count, mean of teacher percentages,
  and the lesson-weighted rate. A team-taught lesson counts once per teacher;
  `uniqueLessons` is a separate measure.
- Global and per-teacher breakdowns by weekday, dated ISO week (including week
  53), and configured school year. Filters apply consistently to every series.
- Counts of excluded irregular lessons, unknown-teacher lessons, teachers below
  the threshold and lessons with unmapped school years.

These are **timetable cancellation statistics**, not verified physical teacher
attendance or reasons for absence. A personal report covers lessons visible in
that caller's timetable; an approved teacher-source report covers only what the
saved source exposes. Rank is a numerical ordering, not a performance judgement.

Anonymisation is report-local pseudonymisation: teacher names become `Teacher N`
and teacher IDs are omitted. Exclusion prefixes are omitted from anonymous
metadata. Dates and statistical patterns remain, so this is not a guarantee of
irreversible anonymisation. No credentials, source-account identity, class IDs,
profile IDs or raw lesson bodies are returned.

## Exports

`export-json` downloads the identical report as JSON.
`csv-zip` contains:

- `teachers.csv`: totals, rank and rates;
- `weekday.csv`, `week.csv`, `school-year.csv`: aggregate and teacher breakdowns;
- `report.json`: complete report, filters, definitions, warnings and coverage.

Exports require `analytics.export` in addition to the source capability and
approved targets. They preserve anonymisation and do not unlock any extra data.
CSV fields are protected against spreadsheet formula injection. No HTML, chart
configuration, frontend component schema or private credentials are exported.

## Permission preview

`POST /admin/access/preview` accepts the unsaved group `id` (0 for new), `school`,
`grants` (capability → -1/0/1), and optional `assignTo` audience. It returns
per-account before/after decisions with their sources, `saved:false`, and
`existingAccountsOnly:true`. The dashboard's Preview changes button calls this
endpoint. Preview writes nothing and uses the actual permission resolver,
including compatibility expansion, role defaults, direct overrides and gates.
Future accounts are not simulated. Concurrent configuration changes can make a
preview stale. Dashboard administrative powers remain separate from capability
changes; preview does not promote/demote dashboard administrators.

## Separate Technik actions

`events.technik.view`, `.create`, `.edit`, `.delete`, `.import`,
`.attendance.read` and `.attendance.write` control separate operations.
The old `.manage` grant remains compatible where no explicit operation rule
exists. An explicit operation denial wins over that compatibility fallback.
Dashboard administrators retain management access subject to master/gate ceilings.

Capability-controlled APIs are also available outside the dashboard:

| Route | Operation |
| --- | --- |
| `/api/technik/events` | GET list by username; POST create |
| `/api/technik/events/{id}` | PATCH edit; DELETE remove |
| `POST /api/technik/import` | Import local appointments |
| `GET /api/technik/attendance` | Read register |
| `PATCH /api/technik/attendance` | Correct register |
| `GET /api/technik/attendance/log` | Read history |

The existing `/api/admin/events...` routes remain compatible and enforce the
same operation permissions. Non-admin operations are restricted to the caller's
school. The existing aggregate-event app read surface keeps its view-compatible
register read behaviour; dedicated register API access uses attendance.read.
Attendance mutation uses attendance.write. All Technik changes remain proxy-local.

## Account-type login policy and recovery

`GET|PUT /admin/access/admission?school=NAME` reads/replaces:

```json
{"blockedRoles":["student","unknown"]}
```

Roles are `student`, `teacher`, `parent`, `staff`, `other`, `unknown`, and
`upstream_admin`. Empty school sets the server policy. A block at either level
wins; default is no restrictions. Classification comes from authenticated
upstream identity and configured person-type mappings. Guardian linked people
are handled separately from the login identity.

Upstream administrator status is used only when upstream explicitly supplies
`loginServiceConfig.user.isAdmin` as a boolean. It is not inferred from a teacher
person type or our local dashboard-admin flag. Blocking upstream_admin denies
unknown administrator status too, so do not enable that rule before checking
school compatibility. Classification is not universally certified across schools.

Rules apply at password/key login before account credentials are persisted and
on subsequent session/OTP use. A blocked existing session is refused on its next
request; it is not permanently destroyed and can work again if the rule is
removed. Unidentifiable bearer-only REST calls are denied when restrictions are
active. Enrollment is not an admission grant. Policies do not erase stored data
or change upstream accounts. Existing noninteractive calendar tokens and queued
notifications follow their own feature/capability controls, not login-only policy.

If you lock yourself out, use local operator access:

```sh
untisctl -db /data/untis.db -school YOUR_SCHOOL admission-reset
untisctl -db /data/untis.db -school '' admission-reset
```

The first clears that school; the second clears the server policy. Clear both
when both scopes restrict access. No upstream account changes are involved.

## Tests

Coverage is in `internal/analytics/report_test.go`,
`internal/store/feature_controls_test.go`, `internal/proxy/analytics_test.go`,
`internal/proxy/permission_controls_test.go`,
`internal/untis/admission_metadata_test.go`, and `scripts/check-admin-ui.js`.
Real teacher writes are never used for tests. Live school-year retention and
upstream account metadata availability still require read-only acceptance checks.

## Release update detector

`GET /admin/updates` checks the latest published stable release of
`UntisPlus/UntisPlus-Proxy` through GitHub's releases API. It returns the running
version, latest tag/release URL, comparison status, nullable `matchesLatest` and
`updateAvailable`, check times, staleness and any error category. Only dashboard
administrators can access it. The dashboard overview displays the result.

Checks run on demand when requested or when the overview refreshes. Successful
results are cached in memory for one hour; concurrent requests share the same
check. Failures retry after two minutes; rate limits/no published release wait
one hour. Requests time out after five seconds, response parsing is bounded,
and API redirects are not followed. No background polling or automatic update
installation is performed. No account credentials or school data are sent.

Statuses distinguish `current`, `update_available`, `ahead`, `development`,
`unknown`, and `disabled`. Development/prerelease builds are not declared current
from their version prefix. Stable semantic versions are compared numerically;
nonstandard unequal tags return unknown. Failed checks never claim current and
mark retained last-known release information stale.

The server-wide `update_check` master switch disables outbound checks and the
endpoint reports disabled. This switch is edited at server scope, not per school.
The detector does not compare image digests, branch commits, or install updates.
Tests are in `internal/proxy/updates_test.go`.
