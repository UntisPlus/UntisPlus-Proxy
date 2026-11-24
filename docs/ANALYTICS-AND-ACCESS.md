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
| `analytics.teacher.view` | Analyse approved teachers, approved rooms as a dashboard administrator, or rooms available through your own saved teacher credential |
| `analytics.student.view` | Analyse explicitly approved student timetables through an eligible saved teacher |
| `analytics.class_history.view` | Analyse an accessible own class, an approved class, or an approved class-history profile |
| `analytics.export` | Download JSON or CSV ZIP within the same data permissions |

Assign the capability and configure target approvals separately. In the
dashboard, select the school and account under **Permissions → Analytics data
scope configuration**, load the account's scope, then save the approved
targets. The same settings are available through:

`PUT /admin/access/analytics-access?school=NAME&user=USERNAME`

```json
{
  "teacherIds": [42],
  "studentIds": [70001],
  "classIds": [81],
  "roomSelectors": ["R101"],
  "profiles": [{
    "id": "my-class-history",
    "segments": [
      {"classId": 81, "from": "2023-08-01", "to": "2024-07-31"},
      {"classId": 91, "from": "2024-08-01", "to": "2025-07-31"}
    ]
  }]
}
```

GET on the same URL returns the configured scope with numeric `roomIds`.
On PUT, supply either `roomSelectors` (exact upstream names or numeric strings)
or `roomIds`. Use an empty array to clear an approval list. An omitted room,
student, or class list retains its saved value. Each target list accepts up to
2,000 IDs. Class IDs and exact date ranges identify historical classes. A profile may
contain multiple classes in the same date range for a cohort. Duplicate teacher
assignments with the same upstream period identity are counted once.

The administrator route `/admin/access/analytics-years?school=NAME` supports
GET to read saved boundaries, POST to preview upstream dates through a saved
teacher, and PUT to save reviewed dates:

```json
{"years":[{"name":"2024/25","from":"2024-08-01","to":"2025-07-31"}]}
```

Use the actual dates from upstream. Year names must be unique and ranges must
not overlap. An explicit date-range report works before school years are saved.
Lessons outside the saved boundaries are grouped as `unmapped`.

## JSON endpoint

`GET /api/analytics/options?school=NAME` lists the signed-in account's
permitted targets and sources and the school's configured year names and
dates. It reads configuration without fetching timetables. Use each target's
`mode` and ID or room name to build a report request. The report endpoint
rechecks access independently of discovery.

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

- `mode`: `personal` (default), `teacher`, `student`, `class`, `room`, or `class-history`.
- `teacherId`: required and approved for teacher mode; optional as a filter of
  teachers appearing in personal/class-history data. It never widens that scope.
- `room`: required in room mode. Use the exact upstream room name or numeric
  room ID. A teacher with `analytics.teacher.view` can use their own saved
  account and upstream access. A dashboard administrator with the same grant
  can use an eligible saved teacher in the same school for approved rooms.
- `studentId`: required for student mode. The target needs individual approval
  and the caller needs the student analytics grant.
- `classId`: required for class mode. Select your accessible class or a class
  approved for your account. Class mode requires the class-history analytics grant.
- `dataSource` is optional. Omit it to let the proxy select a permitted source.
  For teachers and rooms, the proxy prefers an eligible direct upstream
  timetable and uses reconstruction if direct access is unavailable and
  reconstruction is permitted. For classes, it tries your own timetable
  access, a shared class timetable, then an eligible saved teacher.
  Reconstruction uses only accessible shared classes.
  To select a specific source, send a value from the target's `dataSources`
  list. If that source is unavailable, the request is denied. The report's
  `dataSource` identifies the source actually used.
- `profileId`: required for class-history mode.
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
- `personalOtp`: optional six-digit WebUntis code for a personal report of up
  to 31 days. The proxy uses it for the same signed-in account's timetable read
  without saving or returning it. Normal session and capability checks still
  apply. Omit it to use saved source credentials.

Dates are inclusive. The current-day and future cutoff uses UTC. A request
covers at most 366 days. Personal, teacher, student, class, and room reports
use up to 53 upstream windows of at most seven days each. Reconstruction
counts each class window toward that limit, so a broad reconstruction may
need a shorter date range. For reports of at least a week, a short final window
uses a full week within the requested range. Only its assigned dates are
counted. Class-history reports use up to 32
upstream windows of at most 31 days each. A one-day personal query
reads up to seven nearby dates from the same timetable in a single upstream
request and reports only the requested date. Personal history has no
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

The reader supports WebUntis responses using nested `timetable.periods`,
flat `periods`, or mobile `ttData`. It resolves teachers from typed timetable
elements or embedded teacher lists.

The JSON includes `version`, a report-local `reportId`, target type in
`source`, the actual read path in `dataSource`, dates,
`generatedAt`, requested filters, warnings, and observed/unavailable windows.
`complete` means all requested approved windows were read and the class-history
range has no gaps. It cannot establish that upstream retained every historical
lesson; the response states this limitation. Unreadable windows are partial data,
not successful zero-cancellation observations. Each unavailable window publishes
a bounded `reason`: `source_auth_unavailable` (replay authentication could
not be prepared), `upstream_rejected`, `upstream_unreachable`,
`upstream_http_error`, or `invalid_timetable_response`. The report does not
include raw upstream errors or source credentials. A rejected JSON-RPC reply
may include the numeric `upstreamCode` for diagnosis. Empty denominators have null rates.

Statistics include:

- `cancelledLessons` and `coveredLessons` count distinct periods. The existing
  `teacherAssignmentTotals.cancelled` still counts teacher assignments, so a
  team-taught cancellation can count once in the first field and twice in the
  second. Cancellation markers include mobile `is: ["CANCELLED"]`, legacy
  `code: "cancelled"`, and an explicit cancelled timetable status.
- `lessonChanges` lists each observed cancellation or confirmed cover, with
  date, time, original subjects and missed teachers where available. Each
  `covers` pair names the original teacher and the cover teacher. A cover is
  counted only when the upstream teacher element links two distinct IDs via
  `orgId`. A cancelled period is counted as cancelled even if it also carries
  an irregular flag. Cover details remain available for irregular periods
  excluded from the ordinary cancellation rates.
- `missedTeachers`, `missedSubjects`, and `coverTeachers` rank the affected
  people or subjects by distinct periods, including separate cancelled and
  covered counts. Teachers who were replaced are missed for that period even
  though the lesson itself was covered. Missing metadata leaves the teacher or
  subject list empty. The report still counts an unattributed cancellation
  when no specific teacher or minimum-count filter is requested.
- `clashingLessons` counts affected periods that overlap another non-cancelled
  lesson on the same personal timetable. The per-row `clash` value is
  `overlap_observed`, `no_overlap_observed`, or `unknown` for missing times or
  other scope modes. Periods whose start and end merely touch do not overlap.
  An overlap alone cannot establish the reason a period was cancelled.


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
and teacher IDs are omitted. Subjects and cover teachers use report-local
labels in all detailed and export fields. Exclusion prefixes are omitted from anonymous
metadata. Dates and statistical patterns remain, so this is not a guarantee of
irreversible anonymisation. No credentials, source-account identity, class IDs,
profile IDs or raw lesson bodies are returned.

## Smoke test from a terminal

Run `python3 scripts/test-teacher-cancellations.py USERNAME` for the last seven
completed days on beta. Enter the current WebUntis authenticator code when
prompted. The script keeps the resulting proxy session in memory and prints
pseudonymised totals. Use `--from YYYY-MM-DD --to YYYY-MM-DD` to choose dates or
`--json` to see the full report. Add `--show-names` when you want actual teacher
and subject names in your terminal output.

Personal mode needs the analytics switch and `analytics.personal.view`. For
queries of up to 31 days, the script uses the fresh code for sign-in and the
personal timetable request. Add `--saved-source` to use the account's saved
replay credential for the timetable read. Longer reports always use the saved
credential. The sign-in prompt still asks for a fresh code in either case.
Add `--teacher-id ID` for an approved teacher target. This mode needs
`analytics.teacher.view`, the explicit target ID, and an eligible saved teacher
source. The source account is used for timetable reads.

The script labels errors by stage. Use the stage to decide what to check:

| Output | What to check |
| --- | --- |
| `Sign-in failed` | Use the login for the selected school and a fresh code. Use `--school` if it differs from the server default. |
| `Sign-in was blocked by Cloudflare (error 1010)` | Update to the current script, which identifies itself as the UntisPlus analytics client. If it recurs, check the Cloudflare settings for the beta host. |
| `Analytics returned HTTP 403` | Sign-in succeeded. Check the Analytics switch, the capability for the target type, the account's approved targets, and whether the requested source is available. |
| Partial or empty report, exit code 2 | Use `--json` to inspect the coverage windows and try a known teaching week. Check saved source credentials for unavailable windows. |

A one-time-code-only sign-in can establish a session even when no reusable
credential is saved. Use **Account warnings** to check the account's credential
status. Administrator status alone does not grant access to reports.

### Save a school-year-to-date report

Run from the UntisPlus Proxy checkout. The example starts on 1 August 2026,
the proxy's default school-year boundary, and ends yesterday in Europe/Berlin.
Replace the start date with the school's actual upstream boundary if it differs.
Enter a fresh WebUntis code at the prompt. The timetable reads use the saved
account credential for this multi-month query.

```bash
(
  umask 077
  set -o pipefail
  python3 scripts/test-teacher-cancellations.py YOUR_USERNAME \
    --from 2026-08-01 --to "$(TZ=Europe/Berlin date -d yesterday +%F)" \
    --saved-source --json | tee /tmp/untisplus-analytics-2026-27.json
)
```

The file contains JSON only. Names use report-local labels unless you add
`--show-names`. Check `complete` and `windows` before interpreting totals.
Exit status 2 can leave a valid partial or empty JSON report in the file.
The explicit date range works without configured analytics years. Until the
school's year boundaries are configured, lessons can appear in the `unmapped`
year bucket and still count in the totals. This command uses ordinary JSON
report access. The dedicated export formats require the separate export grant.

### Save a room report

Use your enrolled teacher account to read cancellations for one room. For
example, to save room 14-05 over an explicit date range:

```bash
(
  umask 077
  set -o pipefail
  python3 scripts/test-teacher-cancellations.py YOUR_TEACHER_USERNAME \
    --room '14-05' --from 2026-08-01 --to 2026-11-23 --json --show-names \
    | tee /tmp/untisplus-room-14-05-2026-27.json
)
```

You can use `--school-year` instead of the date flags once the school year
has been saved. Use a numeric room ID if the room name is ambiguous.
Room reports describe cancellations on the room's upstream timetable.

To run both reports from a dashboard administrator account, select that
account under **Permissions → Analytics data scope configuration**. Approve
the teacher ID and add the room under **Approved rooms for this admin**. The
admin needs `analytics.teacher.view`. The admin's one-time code signs into the
proxy. Direct timetable reads use an eligible saved teacher. Reconstruction
requires permission to use shared class timetables and reconstruct the
selected teacher or room.

```bash
python3 scripts/test-teacher-cancellations.py YOUR_ADMIN_USERNAME \
  --teacher-id 42 --room 'R101' --from 2026-08-01 --to 2026-11-23 \
  --json --show-names
```

The command checks your available sources and asks once per target when you
can choose between multiple sources. Press Enter to let the proxy choose.
Use `--data-source` to select a specific source without a prompt. Prompts go
to the terminal so `--json` can be piped straight to a file. The JSON output
contains one report per target under `reports`, each with its own
`dataSource` field. Text output names the source as well.

Run `python3 scripts/test-teacher-cancellations.py YOUR_USERNAME --options`
to list targets and school years. Use `--student-id`, `--class-id`,
`--teacher-id`, or `--room` to select a target. For a slice of one school
year, use `--school-year` and supply `--from` or `--to` within its dates.

For a large catalog, use `jq` to show only the relevant entries and their
permitted sources:

```bash
python3 scripts/test-teacher-cancellations.py YOUR_USERNAME --options |
  jq '.targets[] | select((.mode == "teacher" and .id == 42) or (.mode == "room" and .name == "R101"))'
```

### Compare teachers and school years

In **Permissions → Analytics data scope configuration**, choose the school
and select **Load dates from upstream**. The proxy first requests
`getSchoolyears` and falls back to `getCurrentSchoolyear` if the school
doesn't supply a list. Review the returned names and inclusive dates, then
select **Save school years**. Loading is a preview. If upstream only returns
the current year, add any earlier date ranges separately after confirming
them with the school.

Approve each teacher ID for the signed-in account and grant
`analytics.teacher.view`. Use the exact year names in the saved list.

```bash
python3 scripts/test-teacher-cancellations.py YOUR_USERNAME \
  --teacher-id 42 --teacher-id 43 \
  --school-year '2024/2025' --school-year '2025/2026' --json
```

The script signs in once and requests a separate JSON report for each selected
teacher and year, up to six combinations. Its `reports` array lets the same
chart show one series per teacher and one category per year. Compare
`teacherAssignmentTotals` per entry. Check each report's `windows` and
`complete` fields before comparing rates. An HTTP error is recorded against
its own selection. Omit `--show-names` to use report-local labels.

### Example chart dashboard

Use a saved personal, teacher, or room report to build a dashboard with a
timing view and separate tabs for teachers, subjects, and covers. Show the
target, date range, `dataSource`, and coverage beside the charts. A teacher
breakdown from a personal report represents that teacher's assignments on the
caller’s timetable. A teacher report covers the selected teacher's upstream
timetable. A room report covers periods on that room's timetable. Use the same rows for the
bars and an expandable data table under each chart, so readers can see exact
counts alongside the visual ranking.

| Chart | Category | Measure and table details |
| --- | --- | --- |
| Cancellations by week | ISO year and week of `lessonChanges[].date` | Count rows with `cancelled: true`. Count `clash: "overlap_observed"` separately. |
| Cancellations by weekday | Weekday of `lessonChanges[].date` | Count cancelled rows in weekday order. |
| Cancellations by original teacher | `missedTeachers[].name` | Sort bars by `cancelled`. Include `covered` in the table to show how many lessons another teacher took. |
| Cancellations by subject | `missedSubjects[].name` | Sort bars by `cancelled`. Include `covered` in the table. |
| Most frequent cover teachers | `coverTeachers[].name` | Sort by `covered`. Read `lessonChanges[].covers` alongside `subjects` for the original teacher and subject. |

Use `cancelledLessons` and `coveredLessons` for the summary totals. A lesson
can count against multiple teachers or subjects in a ranking. For a chart of
teacher-assignment rates instead, use the `scheduled`, `cancelled`, and
`cancellationRate` fields in `byWeek` or `byWeekday`. Label that chart
**Teacher assignments** so the denominator is clear.

This example turns the saved export into rows for a weekly bar chart and its
data table. Use `week` for the horizontal axis and `cancelled` for the bar
height. Include `overlaps` in the tooltip and table.

```bash
python3 - /tmp/untisplus-analytics-2026-27.json <<'PY'
import json
import sys
from collections import defaultdict
from datetime import date, timedelta
from pathlib import Path

report = json.loads(Path(sys.argv[1]).read_text())
if not report["complete"]:
    raise SystemExit("Report is partial. Check unavailable windows before charting.")

counts = defaultdict(lambda: {"cancelled": 0, "overlaps": 0})
day = date.fromisoformat(report["from"])
end = date.fromisoformat(report["to"])
while day <= end:
    year, week, _ = day.isocalendar()
    counts[f"{year}-W{week:02}"]
    day += timedelta(days=1)

for lesson in report["lessonChanges"]:
    if lesson["cancelled"]:
        year, week, _ = date.fromisoformat(lesson["date"]).isocalendar()
        row = counts[f"{year}-W{week:02}"]
        row["cancelled"] += 1
        row["overlaps"] += lesson["clash"] == "overlap_observed"

print(json.dumps([{"week": week, **row} for week, row in counts.items()], indent=2))
PY
```

For a dashboard that accepts partial reports, show unavailable windows next to
the charts and leave incomplete weeks as gaps. An observed overlap shows
concurrent timetable entries. Its presence gives no cancellation reason.
Keep report-local labels in examples you intend to share.

## Exports

`export-json` downloads the identical report as JSON.
`csv-zip` contains:

- `teachers.csv`: totals, rank and rates;
- `weekday.csv`, `week.csv`, `school-year.csv`: aggregate and teacher breakdowns;
- `missed-teachers.csv`, `missed-subjects.csv`, `cover-teachers.csv`: ranked
  breakdowns by affected period;
- `lesson-changes.csv` and `covers.csv`: period details and cover pairs;
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
`internal/proxy/analytics_options_test.go`, `internal/proxy/analytics_years_test.go`,
`scripts/check-analytics-cli.py`,
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
