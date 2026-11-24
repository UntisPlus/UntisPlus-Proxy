# Security and reliability regression coverage

Updated 2026-11-24. Tests use temporary databases and fake upstream servers.
They must not use real teacher accounts for write requests.

| Boundary | Tests and important cases |
| --- | --- |
| School ownership | `internal/store/account_school_test.go`: foreign-school credential reads/writes, preserved ownership and globally unique username rejection |
| Session lifecycle | Same file, `internal/proxy/audit_regression_test.go` and `internal/session/*test.go`: deleted/recreated accounts, legacy unbound cookies, durable revocation ordering, independent session copies and shutdown |
| Deletion scope | `account_school_test.go`: owned private data/subscriptions removed while another school's calendar token survives a colliding person ID |
| REST pool authorization | `audit_regression_test.go`: denied viewer cannot invoke a pooled donor |
| Invalid replay keys | `internal/untis/login_test.go`: malformed/empty keys produce no login code |
| Saved account identity | `internal/proxy/impersonation_regression_test.go`: missing credentials, forged teacher/editor/Technik names, another user's session, expired sessions, wrong school, valid session and cookie-free current OTP |
| Login codes | Same file: historical client timestamp cannot revive an old code; current codes work; upstream key login uses the server clock; local OTP checks share login throttling |
| Enrollment | Same file: failed, empty, malformed or conflicting upstream responses cannot overwrite saved credentials; password guessing is throttled |
| Request parsing | Same file: malformed bodies and query/body method disagreement never reach upstream |
| Administrator setup | `setup_test.go` and the key-login regression: password/key login cannot claim administrator access; explicit provisioning is preserved |
| Source scope | `editor_gate_test.go`: read-only teacher sources reject delegated writes while permitting reads; personal extension does not grant teacher REST access |
| Notification destinations | `target_url_test.go`, `target_dial_test.go`: private/mixed DNS, mapped IPs, reserved ranges, credentials in URLs, redirects, changing DNS, DNS errors, empty answers and explicit private-network override |
| Delivery reuse | `notify_delivery_test.go`, `notify_ntfy_test.go`: reusable connections and delivery behaviour; subscriptions still require capability and target access |
| Pool sharing | `internal/store/pool_sharing_test.go`: hidden donors, all donors hidden, duplicate classes, class-name selection, changed permissions and preserved personal identity |
| Guardian identities | Same file and guardian handler tests: linked children remain separate from independently enrolled accounts, cannot cross schools and are removed with the guardian |
| Backup and storage | `internal/store/backup_test.go`: restorable copy, refusal to overwrite, empty paths, owner-only database/WAL/backup modes |
| Dashboard | `scripts/check-admin-ui.js`: picker state, permission defaults, refresh coalescing, failed refresh retry and existing workflow regressions |
| Hardened image | `scripts/check-container.sh`: non-root UID, read-only filesystem, dropped capabilities, network-disabled temporary backup/restore/startup |

The JSON analytics/access extension adds tests in `internal/analytics/report_test.go`,
`internal/proxy/analytics_test.go`, `internal/proxy/permission_controls_test.go`,
`internal/store/feature_controls_test.go` and `internal/untis/admission_metadata_test.go`.
They cover filter consistency, deduplication, ISO week 53, weighted versus teacher
means, zero denominators, report-local pseudonyms, JSON/CSV export leakage,
source-scope enforcement, history gaps, partial failures, mid-report revocation,
master ceilings, preview/save parity, independent Technik actions, account-type
admission, upstream admin metadata and deletion cleanup. The UI check also verifies
preview uses its read-only endpoint and exposes the new capabilities.

## Running the checks

Use the Go version declared in `go.mod`:

```sh
test -z "$(gofmt -l cmd internal)"
go vet ./...
go build ./...
go test -count=1 ./...
go test -race -count=1 -timeout=30m ./internal/store ./internal/proxy ./internal/session
node scripts/check-admin-ui.js
scripts/check-container.sh IMAGE EXPECTED_VERSION
```

CI runs formatting, vet, build, ordinary tests, UI checks and store/proxy race
checks on pull requests and `main`/`dev` pushes. The Docker workflow builds and
runs the image smoke test. The optional multiarch dispatch and release gate
check both amd64 and arm64. The session package also has a local race check.
A local successful command is not a successful remote
CI run.

## Limits

This inventory maps important boundaries; it does not claim exhaustive edge-case
coverage. Full live account-type compatibility, real-backup recovery, sustained
load, browser accessibility and visual layouts require separate acceptance
checks. Synthetic restore success does not certify an operator's actual backup.
Race detection is expensive with instrumented SQLite; distinguish timeout or
resource failure from a reported race and keep the complete output.

See [production readiness](PRODUCTION-READINESS.md) for the current validation
and deployment state. Older checklists are historical, not current test results.
