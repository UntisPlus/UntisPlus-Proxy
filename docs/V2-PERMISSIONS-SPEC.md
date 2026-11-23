# v2.0 Permissions, Groups, and Anonymous Detection Spec (Draft)

> Historical checklist/design reference. Checked boxes and implementation claims
> below are not evidence that the current beta passed those checks. For the
> 2026-11-23 assessment, see [Production readiness](PRODUCTION-READINESS.md).

Status: **draft / implementation in progress on branch `V2` (parallel to v1)**.
This document defines the v2.0 direction described in the migration checkpoint:
a parallel v2 deployment that replaces v1's coarse/global anonymous toggles and
coarse permission flags with per-school upstream-detected anonymous behavior and
a maximally flexible, fine-grained, admin-scopable permission/group system.
v1/NAS remains untouched until v2 is tested and cut over.

## 1. Goals

- **Per-school upstream-detected anonymous behavior.** Anonymous capabilities are
  derived from what upstream `#anonymous#` actually exposes for that school, not
  from a fixed global profile.
- **Fine-grained, admin-scopable permissions.** Admin can grant/deny specific
  capabilities per user and via admin-defined groups/bundles, and can assign
  default group(s) by login/user type (e.g. parent/student, teacher) with
  admin-defined meaning.
- **Three-level control per capability:** server/admin enablement, admin-allowed
  ceiling/default for the user, and user opt-in/out in-app. Effective access is
  the intersection: `server_enabled && admin_allowed && user_opted_in` (with
  sane defaults so existing behavior is preservable where intended).
- **Authenticated real-account access to pooled/reconstructed data** is governed
  by fine-grained permissions and requires being logged in with a real account
  in that school (not anonymous), subject to the effective permission.
- **Speed is a hard requirement.** Detection/caching/effective resolution must be
  cheap on hot paths (reuse in-memory state, cache DB lookups, avoid per-request
  heavy queries). Beat the official WebUntis app.
- **CLI-first** for new features (`untisctl`), with matching admin API/UI.
- **Safety/compat constraints carried forward:** anonymous sessions are never
  persisted as DB user rows; anonymous calendar-link minting is forbidden; no
  real school/user identifiers or secrets committed; tests/CI green.

## 2. Non-goals (v2.0)

- APNs/FCM push. ntfy is the push path.
- Automatic retention/cleanup and calendar-token rotation stay **off by default**
  (operators may enable explicitly, as today).
- In-place upgrade of the v1 NAS deployment. v2 runs as a **parallel instance**.
- Exposing proxy-pooled/reconstructed supplements to plain anonymous sessions
  (see §4). Anonymous remains "as close to upstream as detected".

## 3. Current v1 Baseline (to replace/map)

- **Anonymous:** global capabilities `classes|ntfy|webhooks|changes` (default
  off), gated by per-school anonymous login. `classes` fallback can
  serve/promote pooled, recon-covered classes to anonymous sessions; anonymous
  cannot mint calendar tokens; anonymous never persisted as a user row.
- **Permissions:** coarse per-user/global flags `recon|boosted|editor|techniker`
  plus default-perms for newly seen users (applied once). Recon/boosted mutually
  exclusive in many flows. `/me` returns a coarse level and booleans.
- **Subscriptions/calendar:** ntfy/webhook self-service gated by coarse checks;
  calendar tokens minted only for stored accounts, target resolution mirrors
  recon/boosted/staff visibility.

## 4. Anonymous Model (per-school, upstream-detected)

### 4.1 Principle

For each school, detect and cache the **upstream anonymous profile**: what the
school's own `#anonymous#` login/session can see and do upstream (e.g. which
class/element visibility masterData exposes, and which operations succeed).
Proxy behavior for anonymous requests must not exceed this detected upstream
profile, except where explicitly impossible by construction (calendar minting is
always forbidden).

**Decision (resolves open point):** plain `#anonymous#` does **not** receive
proxy-pooled supplements or proxy-reconstructed timetables. Pooled classes and
reconstructions are proxy-derived data and require an authenticated **real
account in that school** plus the corresponding fine-grained permissions. v1's
anonymous `classes` fallback is removed/replaced by detection of real upstream
exposure.

Rationale: aligns with "access to proxy pooled classes/reconstructed data
requires being logged in with a real account in that school", keeps anonymous
stock-like per school, and avoids leaking proxy-aggregated data to unauthenticated
sessions.

### 4.2 Detection and caching

- **Probe:** when a school is (re)registered or on admin/periodic refresh, attempt
  an upstream anonymous login for that school (respecting per-school
  `anonymous_login_enabled`). Capture:
  - login success/failure;
  - masterData visibility snapshot (which element types/ids/classes are
    displayable/displayAllowed to anonymous);
  - optional capability probes for operations the proxy can mediate (read-only),
    recorded conservatively on failure/ambiguity.
- **Cache:** persist and keep in memory a `school_anonymous_profile` (versioned,
  with `detected_at`, `expires_at`/TTL, `source=detected|override`,
  `login_ok`, and a compact capability/visibility summary). Reads are lock-free
  or RW-locked snapshots on hot paths.
- **Refresh:** lazy refresh on expiry + explicit admin refresh action + refresh on
  relevant config changes. Detection must be bounded (timeouts, no unbounded
  enumeration) and must not block normal request serving (stale-while-revalidate
  or background refresh with last-known-good).
- **Admin override:** admin may force `disabled` for anonymous login/capabilities
  for a school, or pin/override specific detected aspects where safe. Overrides
  are explicit and audited/logged; "override" wins over stale detection but never
  grants more than the server allows.

### 4.3 Anonymous session handling

- Never create/persist a `users` row for `#anonymous#` (`PersonType`/identity
  synthetic as today). Anonymous subscriptions/state, if any, remain scoped to the
  anonymous principal per school and limited to what the detected profile allows.
- No calendar token minting/rotation for anonymous.
- Personal/teacher/room/subject targets remain unavailable unless upstream
  explicitly exposes them to anonymous and the profile records it (expected rare).

## 5. Fine-Grained Permission Model

### 5.1 Taxonomy (capability keys)

Capabilities are identified by stable string keys so they can be stored, grouped,
granted, audited, and extended. A maximally flexible taxonomy covers the
examples given and is decomposable along **resource/surface, element/target
family, data source/mode, and action**.

Proposed key shape (illustrative, to be finalized in implementation):

`<surface>.<target>.<mode>.<action>` with wildcards/groups for bundles.

**Surfaces / targets**
- `timetable.class|teacher|room|subject|student.personal`
- `pool.class` (view/use pooled class data/index)
- `recon.teacher|room|subject` (reconstructed views)
- `changes.class|teacher|room|subject|student|school` (poll/stream)
- `notify.ntfy|webhook` (+ target families as above)
- `calendar.class|teacher|room|subject|student.personal` (mint/use/manage tokens)
- `absence.read|write` (own vs class-scoped/editor flows as applicable)
- `events.admin` / `events.view` (student/Technik/event surfaces, techniker-like)
- `admin.*` (admin-only operations stay admin-gated separately)

**Modes / data sources** (critical for the stated example)
- `upstream` — raw-forwarded / stock upstream view.
- `pooled` — served from proxy class pool.
- `reconstructed` — derived from pooled data (recon).
- `boosted_raw` — forwarded via saved teacher/boosted account (teacher-grade/raw),
  distinct from `reconstructed`.

**Actions**
- `view` / `read`, `list`/`discover`, `subscribe`, `stream`, `create`/`mint`,
  `edit`/`write`, `manage` (own resources) as applicable per surface.

**Example mappings (requirement):**
- "allow room timetables via teacher accounts while allowing only standard
  reconstruction (not teacher-account/raw) for teacher timetables" →
  grant `timetable.room.boosted_raw.view` (and/or pooled/reconstructed as needed)
  but grant `timetable.teacher.reconstructed.view` without
  `timetable.teacher.boosted_raw.view` for that user/group.
- "standard reconstruction vs teacher-account/raw vs pooled" are distinct modes.
- Class/teacher/room/subject × reconstructed vs teacher-account/raw vs pooled ×
  view/read/subscribe/changes/calendar/edit are all expressible.

Implementation may normalize a curated canonical set (documented constants) while
allowing the group/grant engine to evaluate keys generically so new keys can be
added without schema churn.

### 5.2 Principals and role/user types

- **User (stored, authenticated):** real account in a school; identified by
  `(school, username)` as today.
- **Login/user type:** derived from upstream/stored identity (e.g. `person_type`
  / role such as student, teacher/staff, other). The mapping from concrete
  `person_type` values to admin-defined **role labels** (e.g. `student`,
  `parent` if distinguishable, `teacher`) is configurable so "default group by
  login/user type (admin-defined meaning)" is satisfied. If the upstream type
  cannot distinguish parent vs student, the default mapping treats them as the
  configured role and admin can override per user.

### 5.3 Groups (admin-defined bundles)

- A **group** is an admin-defined named bundle of capability grants/denies and/or
  ceilings, scoped globally or per school (design choice: prefer **per-school or
  global-with-school-scope** to match multi-school; final choice in schema phase,
  but model must support scoping). Groups can be composed/ordered if useful; at
  minimum, assignment is explicit.
- Groups are the unit for "default group(s) based on login/user type".

### 5.4 Grants, ceilings, defaults, and user preferences

Three-level effective control per capability:

1. **Server/admin enablement (feature gate):** whether the deployment/school
   allows this capability category at all (supersedes everything). Replaces
   coarse global switches with finer server gates where needed.
2. **Admin-allowed ceiling/default for the user:** resolved from (in deterministic
   precedence, to be fixed explicitly) direct per-user grants/overrides, assigned
   groups, and default group(s) for the user's role/type. This defines what the
   user *may* use and the admin-set default state.
3. **User opt-in/out preference:** the user may enable/disable capabilities for
   themselves **only within** what (1)+(2) allow, and only for capabilities
   marked user-toggleable. Stored as per-user preferences.

**Effective:** `enabled_by_server && allowed_by_admin_ceiling && user_opted_in`.

- Deny/absence at ceiling means not allowed even if user opts in.
- Admin can scope "whatever they want": direct per-user assignment/override wins
  over group/default as specified by precedence; groups and defaults provide
  convenience.
- Resolution must be deterministic, documented, and cheap (precompute/cache
  effective set per session/user, invalidate on change).

## 6. `/me` Contract (read + user preference changes)

`/me` (authenticated, real accounts; anonymous gets a minimal/denied view or 401
as appropriate) must expose fine-grained effective state:

- identity/school/role/type;
- assigned groups and resolved defaults/ceiling sources (enough for UI/debug);
- per-capability (or per-family) **effective** state and its components:
  `server_enabled`, `admin_allowed` (ceiling/default), `user_opted_in`,
  `effective`;
- which capabilities are user-toggleable.

`/me` must support **user preference changes** (opt in/out) for toggleable
capabilities within the admin-allowed ceiling and server enablement. Writes are
validated server-side against ceiling/enabled; no escalation possible.

Response shape is versioned/documented; clients consume effective booleans/sets
rather than inferring from coarse flags. Back-compat shims (e.g. mapping old
`recon/boosted/editor/techniker` views) are considered only if needed for the v2
client, but v2 is a breaking/parallel deployment.

## 7. Pooled/Reconstructed Access and Serving

- **Pool/recon scanning scope:** continue deriving the class pool and
  reconstruction element coverage from stored real-user donations/data as today,
  so the proxy knows what is reconstructable (coverage/horizons). Scanning/index
  maintenance is an internal capability; it is not itself user-visible data
  exposure.
- **Serving/exposure scope:** a request for pooled or reconstructed (or
  boosted-raw) data is served only when the requester is an authenticated real
  account in that school **and** the effective permission allows that specific
  surface/target/mode/action (and coverage exists where required, e.g.
  recon-covered class). Anonymous never receives pooled/recon supplements (§4.1).
- **MasterData/display filtering:** derived per effective permissions (and per
  detected anonymous profile for anonymous), so clients see/select only what they
  may access.
- **Calendar:** remains forbidden for anonymous; for authenticated real accounts,
  mint/list/rotate/revoke and feed resolution require effective calendar
  permissions and accessible targets. Fail when no accessible/exposed targets
  exist.
- **Subscriptions/changes/notify:** target resolution and delivery gated by
  effective fine-grained perms (e.g. which targets can be watched/streamed,
  ntfy/webhook create vs manage-own vs admin).

## 8. Admin API, UI, and CLI (`untisctl`)

CLI-first. Extend/replace v1 surfaces:

- **Groups:** `untisctl group list|create|delete|show|grant|revoke|set` (manage
  bundles and capability grants/ceilings).
- **Assignments/defaults:** assign groups to users; set default group(s) by
  role/user type (`untisctl perms default`/role-mapping equivalents); per-user
  direct grants/overrides/deny.
- **Server/school enablement & ceilings:** configure server-enabled gates and
  admin ceilings/defaults (global/per-school as defined).
- **Anonymous:** per-school detected profile view/refresh/override and login
  enable/disable (`untisctl anonymous ...` evolves to per-school/profile-aware).
- **Inspection:** effective permission resolution for a user (`why`/explain) to
  make admin scoping debuggable.
- **Admin API/UI:** mirror the above (dashboard panels for profiles, groups,
  assignments, role defaults, per-user effective view, ceilings). Admin actions
  audited.

Exact command/endpoint names are finalized during implementation but must cover
all model operations.

## 9. Storage and Migration (parallel v2)

- **Parallel deployment:** separate instance (and DB/path) or clearly versioned
  schema; v1 DB is not modified in place on NAS.
- **New/extended schema (conceptual):**
  - `permission_groups` (id, name, scope/school, definition/metadata)
  - `group_capabilities` (group_id, capability_key, effect: allow/deny/ceiling,
    value/default)
  - `user_groups` (school, username, group_id)
  - `user_capability_grants` (school, username, capability_key, effect, source/precedence)
  - `role_default_groups` (scope/school, role/person_type mapping, group_id/priority)
  - `user_preferences` (school, username, capability_key, opted_in)
  - `school_anonymous_profiles` (school, detected snapshot/capabilities,
    detected_at, expires_at, source/override)
  - `server_capability_gates` / settings for enablement & ceilings
  - keep `users`, `secrets`, pool/recon/tokens/subscriptions/outbox/event tables;
    evolve FKs/keys as needed.
- **Migration/mapping from v1 (import tool or one-shot migrator):**
  - `recon/boosted/editor/techniker` flags and `default_perm_*` → equivalent
    groups/grants/capabilities and role defaults (recon-on default preserved
    unless operator changes).
  - global anonymous capabilities → server gates/profile overrides as appropriate,
    then rely on detection per school.
  - existing calendar tokens/subscriptions/webhooks/ntfy rows preserved and
    attributed to real users; validate target access under new effective perms.
  - anonymous-owned subscription remnants handled explicitly (drop or reassign per
    policy) since anonymous has no stored user.
- **Performance:** index by (school, username), (group_id), capability lookups;
  cache effective sets; avoid N+1 in request paths.

## 10. Rollout / Phases

1. **Schema + model + CLI foundations:** tables, capability registry/keys,
   group/grant/role-default/preference storage, effective-resolution engine with
   tests, migration/import tooling skeleton.
2. **Detection + access/serving:** upstream anonymous detection/caching/override;
   enforce effective perms on timetable/pool/recon/masterData serving and calendar
   minting for real accounts.
3. **Subscriptions/calendar/edit parity:** ntfy/webhook/changes/stream targeting
   and delivery + calendar/token lifecycle + absence/editor/technik surfaces gated
   by fine-grained perms.
4. **`/me` + admin API/UI:** fine-grained `/me` read/write, admin endpoints and
   dashboard, audit.
5. **Tests/docs/gates + parallel cutover:** full `gofmt`, `go vet`, `go build`,
   `go test -count=1 ./...`, `go test -race ./internal/store ./internal/proxy`,
   UI check; run v2 in parallel, verify, then migrate.

## 11. Open Items to Confirm During Implementation

- Exact canonical capability key list and mode names (final constants) and whether
  wildcard inheritance is supported in storage or expanded at resolution time.
- Group scoping model (global vs per-school vs both) and precedence/ordering when
  multiple groups and direct grants conflict (recommend: explicit deny > direct
  grant > group grant > role default; document and test).
- Which capabilities are user-toggleable by default vs admin-marked.
- Concrete upstream anonymous probe set per school (bounded, cacheable), probe
  failure/fallback policy (transient vs authoritative). **Current behavior:** probe
  failures are stored as non-definitive `probe_error` while preserving last-known-good
  hints; definitive detected/override profiles are authoritative for stock class
  exposure. Proxy-managed anonymous features (`ntfy`/`webhooks`/`changes`) are
  policy-configured (not upstream-detected): they use global `anonymous_*` toggles as
  the baseline/configured state and may be explicitly overridden per school via anon
  profile payload capability keys (e.g. in admin overrides/profiles). Upstream
  detection remains authoritative for upstream-exposed stock surfaces such as class
  visibility.
- Mapping of all observed `person_type` values to configurable role labels
  (configurable storage/API/CLI added; defaults/coverage to confirm per deployment).
- Compatibility expectations for existing mobile/web clients during parallel v2
  period (v2 may be a breaking API for clients that adopt fine-grained `/me`).
- Admin UI scope for v2 management/explain panels (API/CLI parity vs full dashboard UI).

## 12. Implementation status (branch `V2`, current)

Implemented/integrated (partial, iterating):
- v2 schema/migration and effective-resolution core (`internal/access`, `internal/store/store_v2.go`) with builtin groups, role defaults, gates, grants, preferences, anon profile storage, person-type→role mappings, and v1→v2 compat migration helpers.
- Per-school anonymous profile probe/cache/refresh/override scaffolding (bounded `#anonymous#` login probe, detected `login_ok` + displayable classes + explicit capability hints, stale-while-revalidate background refresh, last-known-good hint preservation on transient probe failure, admin/school endpoints and CLI inspection).
- Anonymous enforcement hardening: transient probe failures are stored as non-definitive (`probe_error`) so they do not masquerade as authoritative detection; explicit admin `override` with `login_ok=false` forces anonymous login disabled; definitive detected/override profiles are authoritative for anonymous class target visibility (no proxy-pooled fallback on definitive profiles). Detection refresh now avoids clobbering concurrent admin overrides or newer profile state (fixes probe/override race).
- Profile-aware anonymous feature resolution with reduced legacy fallback for detected class exposure; anonymous class subscription/changes/timetable targets require upstream-detected/displayable stock exposure (hints honored for non-definitive probe states, no proxy-pooled fallback/inference for anonymous targets), and anonymous raw `getTimetable2017` class forwarding is scoped to allowed stock CLASS targets when classes feature/login permit. Proxy-managed anon features (`ntfy`/`webhooks`/`changes`) remain policy-configured via global toggles with optional per-school profile capability overrides.
- Fine-grained `/me` effective-state read and user preference updates (within ceiling/toggleable constraints); v2-aware admission/gating in calendar, notifications/subscriptions, changes/stream, timetable/masterData and related proxy paths.
- Enforcement hardening: ICS token re-authorization on serve, webhook/ntfy delivery authorization filtering at enqueue/send, v2-aware boosted/editor/class-scoped checks in key paths, calendar/notification options deny when no accessible/exposed targets exist, changes view/stream endpoints enforce fine-grained changes capabilities (`view`/`stream`) with admin bypass for school-wide, builtin student/teacher base grants include class changes view/stream, and technik local event-absence mutations plus synthetic-period write absence caps are gated on `events.technik.manage` (view remains sufficient for read surfaces).
- Admin HTTP API and `untisctl` v2 management/inspection coverage for core flows (effective/why decisions with context, groups list/show/create/delete/grant, user grants, user-groups, role-defaults list/set/delete, gates, anon profiles, person-type→role mappings), plus legacy compat surfaces where needed. Admin dashboard includes an initial v2 access management/explain panel.

Remaining/needs follow-up:
- Further validate/expand per-school upstream anonymous detection mapping and proxy-managed anon policy surfaces (global toggles vs per-school profile capability overrides) against real school behavior; keep semantics explicit.
- Continue enforcement audit: per-capability write/edit method scoping and teacher-source/class-scoped behavior, end-to-end changes/notify/subscription target+delivery alignment, technik/event surfaces/admin-API parity, token lifecycle/permission-validity edge cases, and comprehensive "no accessible/exposed targets" failures across login/session admission, timetable/masterData, subscriptions/changes/streams, notify delivery, technik/event surfaces, and calendar/token flows.
- Refine admin UI/explain UX and close any remaining CLI/API parity gaps found during testing.
- Resolve remaining spec items (final capability taxonomy/wildcards/toggleability, precedence/scoping details, role mapping completeness, client compatibility) and complete parallel validation/cutover prep.

Verification (local, this session): `gofmt -l .` clean, `go vet ./...` clean, `go build ./...` passing, `go test -count=1 ./...` passing, `go test -race -count=1 ./internal/store ./internal/proxy` passing (after anon profile race/clobber fix). v1/NAS deployment untouched.

---
This draft is intended to be committed as the v2 design baseline and refined as
implementation proceeds.
