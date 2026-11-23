# Production readiness review

Reviewed: **2026-11-23**. Decision: **security fixes deployed to beta; production acceptance still pending**.

## Subsequent feature update

The new [analytics and access contract](ANALYTICS-AND-ACCESS.md) covers on-demand
scoped JSON/CSV analytics, server master switches, permission preview, separate
Technik actions and account-type admission. Analytics starts disabled and login
rules start unrestricted. Live upstream administrator metadata and historical
retention are not universally certified. The original baseline assessment below
is historical; it does not describe these new features.

## Remediation update

The remediated implementation now binds saved-account access to a matching session or current
OTP, verifies codes against server time, rejects conflicting request methods,
and prevents failed enrollment from replacing stored credentials. Local event
and cross-school access have additional identity checks and regression tests.
Login cannot claim administrator access; operators must provision administrators
explicitly. Notification delivery checks all DNS answers and dials approved IPs,
rechecking new connections and redirects. Private Docker targets require an
explicit operator opt-in.

The image runs as UID/GID `10001`; sample Compose files enable a read-only root
filesystem, dropped capabilities and no-new-privileges. Database/backup files
are owner-only. A disposable offline container backup/restore/startup smoke test
passed with those restrictions. An isolated copy of the actual beta backup also
started and retained the same account count (five account rows). Networking was
disabled: `/status` was OK, while upstream health was predictably degraded.
Authenticated timetable recovery and the full operational rollback drill remain
acceptance checks.

In a single synthetic 5,000-account benchmark, pool-list allocations fell from
about 1.18 million to 78 thousand (about 93% fewer), and allocated bytes from
62.2 MB to 3.37 MB (about 95% less). Timing was 11.57 s versus 0.131 s under
concurrent verification load; this is directional evidence, not a production
latency guarantee.

Pool listing avoids redundant permission checks for already admitted classes,
without caching permission denials. Tests cover hidden donors and changing
permissions. Notification connections are reused and responses are drained;
dashboard refreshes are coalesced, with retry tests. The dashboard uses flatter
panels, clearer typography, keyboard focus and mobile permission controls.

CI now also runs on direct `dev` pushes. The Docker check includes the offline
hardened startup/restore smoke test. The original findings below describe the
old committed baseline; they must not be read as a claim that the new fixes are
already deployed or fully validated. Final formatting, vet, build and the complete Go test suite passed after adding
the first-admin key-login regression. The dashboard JavaScript checks also
passed. A dependency vulnerability scan reported no known vulnerabilities.
The store race suite passed (547 seconds). The first combined proxy run hit
its ten-minute limit without reporting a race. All **574 proxy tests** then passed
under race detection in four disjoint groups (111–126 seconds each), including
the added key-login setup regression. CI's race timeout is now 30 minutes.

The verified image was deployed to beta as `2.0.0-dev-security`, image ID
`a86519ca8d8ad70aec62f3bdf74ccc6417426f282389c10065c962a4744238eb`.
Public `/status` and `/healthz` succeeded, and unauthenticated `/admin/users`
returned HTTP 401. The running container uses UID/GID `10001`, a read-only root
filesystem, dropped capabilities and no-new-privileges. The original database,
configuration and image were backed up before replacement. Accounts were kept.

The beta Compose image is pinned to the host-local
`untisplus/untisplus-proxy:beta-security-20261123` tag. Preserve that image or
publish a reviewed registry artifact before moving this configuration to another
host; the tag was transferred through SSH, not pushed to the public registry.
Rollback: stop the beta service, restore `backups/compose-before-security-20261123.yaml`
as `compose.yaml`, and start using the retained
`untisplus/untisplus-proxy:rollback-security-20261123` image. The current schema
is unchanged; keep the pre-update database backup for recovery. Never overwrite
a running database.

Live teacher/student/guardian read-only acceptance, browser visual/mobile checks,
sustained-load testing and a complete recovery exercise remain before production
approval. The completed automated tests do not imply exhaustive coverage. Browser-based
visual verification was unavailable because the local preview could not be
reached; the automated UI checks do not establish pixel-level correctness.

Coverage and commands: [Security and reliability regressions](SECURITY-REGRESSION-COVERAGE.md).

## Original baseline assessment

## Scope and current status

The committed baseline reviewed was `ce647f2` on `dev`. The beta responded
successfully to `/status` and `/healthz` (one school, no degraded schools), with
version `2.0.0-dev`. A healthy service does not establish secure authorization.
This review did not modify upstream accounts or send teacher-account writes.

Existing local authentication changes in `internal/proxy/jsonrpc_intern.go`,
`internal/proxy/rest.go`, and `internal/proxy/impersonation_regression_test.go`
were assessed separately. They were uncommitted and undeployed at review time.
The normal suite below describes the committed baseline, not those local fixes.

## Release blockers

1. **Caller impersonation in saved-account timetable paths.** Additional tests
   against fake upstream servers showed that a forged teacher identity could
   trigger saved-credential use, and an unauthenticated shared-timetable caller
   could impersonate another account. Authentication must bind the requesting
   identity to its session or a verified current code before using saved sources.
2. **Historical login codes accepted.** A targeted test showed that trusting a
   caller-provided timestamp allowed an old OTP to validate. Verification must
   use the server clock and a bounded tolerance.
3. **Security fixes not released.** The pending changes pass seven targeted
   regression tests, but need review, full validation and deployment followed by
   beta verification before a production decision.
4. **First-administrator setup needs controlled exposure.** The first successful
   non-anonymous login can claim administrator status. `maybeClaimSetupAdmin`
   performs separate checks and assignment; atomic first-admin claiming was not
   verified. Explicitly provision the intended administrator before exposing a
   fresh deployment, and review the setup flow for races.

These findings establish a release stop; this is not a complete penetration test.

## Verification evidence

| Check | Result | Scope |
| --- | --- | --- |
| Admin JavaScript runtime check | Passed | Committed baseline |
| `go vet ./...` | Passed | Committed baseline, Go 1.26 container |
| `go build ./...` | Passed | Committed baseline |
| `go test ./... -count=1 -timeout=300s` | Passed | All packages on committed baseline |
| Additional authentication probes | **Three failures** | Fake upstreams; saved-teacher identity, shared-timetable identity, historical OTP |
| Pending authentication changes | Seven targeted tests passed | Local working copy only; not full release validation |
| Current race-detector run | Not performed in this review | CI defines store/proxy race tests |
| Live beta health | Passed | Availability only |
| Backup restore drill | Not verified | Required before promotion |

Passing ordinary tests alongside failing adversarial tests means coverage needs
to include the new regressions in the release gate. CI runs on `main` pushes,
pull requests and reusable workflow calls; direct `dev` pushes do not trigger it.
The tagged release workflow invokes CI before image publication.

## Operational work before promotion

- Review and commit the authentication fixes with their regression coverage.
  Run the full suite, UI check, formatting, vet/build and race tests on that exact
  revision. Deploy to beta and check authenticated read-only user flows.
- Protect stored replay passwords, shared secrets and tokens. Replay credentials
  are stored in plaintext; database and backup access are security boundaries.
- Test a hardened container configuration. The observed beta runs with default
  root user and writable root filesystem; non-root/read-only operation was not
  validated here.
- Restore a backup into an isolated instance and record rollback steps. Test
  without replaying notification deliveries or permitting upstream writes.
- Review outbound destination enforcement, including DNS changes between URL
  validation and connection. Dial-time protections were not verified; this is an
  audit gap, not a demonstrated exploit in this review.

## Feature limits to communicate

- Selected class/room/teacher allowlists and reliable upstream administrator
  account-type admission blocking are not complete.
- A fully upstream-only “Basic” preset is not delivered; existing baselines can
  enable proxy features. Removing an inherited default group for one account is
  not implemented; direct permission exceptions are the current alternative.
- Guardian linked-child timetable support exists. Full child selection across
  calendar and absence workflows has not been certified.
- Pool-sharing opt-out and account authentication/warning indicators exist.
- Finer Technik creation/edit/attendance permission separation remains pending.
- In-app changes use SSE; native APNs/FCM delivery is not implemented.

## Documentation status

The main operating documents were corrected during this review: [README](../README.md),
[deployment](../DEPLOYMENT.md), [dashboard and notifications](ADMIN-WEBHOOKS-NTFY.md)
and [app integration](APP-INTEGRATION.md). Corrections cover anonymous access,
custom permission groups, pool sharing, persisted sessions, setup exposure,
absence routes and release checks.

Older feature/testing/manual-QA checklists and V2/planned/tier design documents
are now explicitly marked historical. Their checked boxes and design statements
are not current acceptance results. They remain useful context, not a promise
that every described feature exists. Documentation cannot be considered a
complete current security specification; use this review and source-backed
operating documentation for the present release decision.
