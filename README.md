# untis-proxy

A Go reverse-proxy in front of WebUntis. It lets the **BetterPlus-Android** app
log in once and then:

- pool all users' classes so **every class timetable is available to everyone**
  (no per-user WebUntis grant needed),
- **reconstruct teacher / room / subject timetables** from the pooled class data,
- produce **iCal calendar subscription URLs for any element** — class,
  personal/student, teacher, room, subject,
- detect **timetable changes** and push them to the app via a live SSE stream,
- push the same changes to **ntfy** and **webhooks** so nothing depends on the
  app being open,
- accept the **base32 shared secret itself** at login (no need to type a live
  6-digit TOTP).

It is multi-school: the first login from a school it has never seen registers
that school — own pool, recon, caches, change polling — with no restart.

> **Hosting, Docker, the tunnel, backups and troubleshooting live in
> [`DEPLOYMENT.md`](DEPLOYMENT.md).** This file is about what the proxy does and
> how to use it.

---

## Table of contents

- [How it works](#how-it-works)
- [Configuration](#configuration)
- [Permissions](#permissions)
- [Admin dashboard](#admin-dashboard)
- [Change detection](#change-detection)
- [Webhooks](#webhooks)
- [ntfy push](#ntfy-push)
- [API endpoints](#api-endpoints)
- [Calendar subscriptions](#calendar-subscriptions)
- [`untisctl` — operator CLI](#untisctl--operator-cli)
- [Provisioning new students](#provisioning-new-students)
- [Internals worth knowing](#internals-worth-knowing)
- [Docs](#docs)

---

## How it works

WebUntis is a per-user system: a student can only see their own timetable, and a
teacher only theirs. The proxy works around that by **pooling** instead of
escalating:

1. **Pooling.** Every user automatically donates their class to the pool on
   login — student and non-student alike. The pool is the union of all donated
   classes, and each pooled class is fetched with its owner's student key.
2. **Reconstruction.** A teacher, room or subject timetable is not stored
   anywhere by WebUntis. It is *reconstructed* by scanning the pooled classes
   and collecting every period where that element appears. This is what the
   `recon` permission grants.
3. **Forwarding (boosted).** Where reconstruction is not wanted, a request can
   instead be re-authenticated as a *saved teacher account* and forwarded raw.
   That is what the `boosted` permission grants — and it is also the identity
   used to unlock the app's editing UI (see
   [the editor section](#the-absencelesson-editor-needs-a-teacher-identity)).
4. **Change detection.** Every `-poll-interval` (60s by default) the proxy
   re-fetches each pooled class, diffs it against the last stored snapshot, and
   bumps a per-class version. Diffs are persisted and fanned out.

The school-wide **name catalog** used by the admin pickers and `untisctl`
lookups is kept in a separate table from the reconstructed element set, so it
never leaks into the list of elements a user is offered.

---

## Configuration

Every flag is also settable as an `UNTIS_*` env var (`-recon-refresh` →
`UNTIS_RECON_REFRESH`), which is how the compose files configure the container.
Precedence is **command-line flag > environment > built-in default**. A
malformed value is logged and falls back to the default rather than taking the
process down at boot.

| Flag | Default | Meaning |
|------|---------|---------|
| `-addr` | `:8509` | listen address |
| `-db` | `untis.db` | sqlite database path — **relative to the working directory** |
| `-server` | *(empty)* | fallback upstream host — normally left empty, since the host is auto-resolved per school via WebUntis school search |
| `-school` | **required** | school login name (the key data is stored under); no default — the proxy refuses to start without it |
| `-env` | `dev` | deployment mode: `dev` / `beta` / `prod` |
| `-version` | build version | reported build version; compiled in by the release build, so the image needs no version variable |
| `-ttl` | `5m` | timetable cache TTL |
| `-poll-interval` | `60s` | how often the change-detector polls each class |
| `-ntfy-base` | `https://ntfy.sh` | ntfy server for push delivery (any ntfy, incl. self-hosted) |
| `-public-base` | — | externally reachable base URL (scheme+host) for the click-through link on notifications |
| `-metrics-addr` | *(empty)* | bind address for the Prometheus endpoint; empty means `/metrics` is not served at all |
| `-admin` | — | comma-separated usernames bootstrapped as admins once |
| `-year-start` / `-year-end` | auto | school-year bounds |
| `-recon-refresh` | `21` | days a class's recon scan horizon may lag before it is re-enumerated |
| `-recon-rescan` | off | re-enumerate every pooled class from scratch at startup instead of resuming |

> **`-db` is the one flag that bites people.** The default is `untis.db`
> *relative to the working directory*, so starting the server from the repo root
> without `-db` silently creates and uses a brand-new empty database. Nothing
> then looks like a permission problem — the admin, boost and editor flags are
> simply absent, and the dashboard reports "not an admin" for everyone. Use
> `-db data/untis.db` or `scripts/run.sh`. See
> [`DEPLOYMENT.md`](DEPLOYMENT.md#the-database-path-trap).
The upstream host for a school is **auto-resolved** via WebUntis'
`searchSchool` API, so `-server` is only a fallback and is normally left empty.
The **school name** is the identifier for sessions, the class pool, recon, perms
and calendar tokens, so keep it consistent with the database you migrated — it is
also the one flag with no default, because guessing it would serve the wrong
school.

---

## Permissions

Access follows a two-flag tier model on top of a Basic default:

| Tier | Flag | Scope | What you get |
|---|---|---|---|
| **Basic** | *(none)* | everyone by default | Pool of classes + your own personal student timetable + **reading your own absences**. |
| **Reconstruction** | `recon` | global switch **or** per-user override | Teacher / room / subject timetables **reconstructed 100% from pooled class data**. |
| **Boosted** | `boosted` | per-user only | Class/teacher/room/subject timetables **raw-forwarded through any saved teacher account** — no reconstruction. Your own personal (STUDENT) timetable is served the same way, giving it teacher-grade visibility (e.g. unlimited future weeks). **No** write / absence-editing powers on its own. If no teacher account is saved, boosted falls back to Basic. |
| **Editor** | `editor` | per-user only | **Boosted plus editing**: unlocks the absence + lesson/subject **write (editing) methods** (`set`/`add`/`update`/`delete`/`change`/`put`/`remove`/`save`/…) for the user's own requests, **and** runs the class-scoped editor data calls through the boosted teacher account. Holding `editor` alone is enough — it implies `boosted`. |

**Boosted XOR Recon**: the flags are mutually exclusive per user — granting one
auto-revokes the other.

```sh
untisctl perms list                              # global switches + overrides
untisctl perms grant --global recon              # enable recon for everyone
untisctl perms grant --user Jdoe recon         # per-user override
untisctl perms grant --user Jdoe boosted       # per-user boosted (raw forwarding)
untisctl perms grant --user Jdoe editor        # + absence/lesson/subject editing
untisctl perms clear --user Jdoe               # drop overrides -> fall back to global
untisctl perms reset                             # wipe all -> Basic for everyone
```

> Flags come **before** the positional type: `perms grant --user X recon`
> (Go's `flag` package does not intersperse flags after positionals).

`boosted` and `editor` are **per-user only** (no `--global`).

Absence **reads** (`getStudentAbsences2017`) are available to **everyone** by
default; only the **write/mutation** methods need `editor`. Being an **admin of
this proxy does not imply editing**: admins manage accounts here, they do not
write to Untis unless they hold `editor`.

> `getStudentAbsences2017` takes only `startDate`, `endDate`,
> `includeExcused` and `includeUnExcused` — **no `id` or `type`**; the student is
> identified by the auth block. **Omitting the two `include` flags makes it
> return an empty list with no error**, which reads exactly like "this student has
> no absences". Probe-verified 2026-10-01: an account with three absences returns
> 0 without the flags and 3 with them. `getAbsences`, `getAbsences2017`,
> `getOwnAbsence` and `getPersonAbsence` all return `-32601 Method not found` on
> this school — an earlier version of this README wrongly listed two of them.

Ask the server what you currently have with `GET /me`:

```json
{"username":"Jdoe","level":"recon",
 "permissions":{"recon":true,"boosted":false,"editor":false}}
```

### Recon scans resume, they do not restart

After a class is scanned, the reached date is persisted per class, so a restart
(or a school added later) only continues where the last sweep stopped. A class is
re-scanned once its stored horizon is more than `-recon-refresh` days behind the
target, which is what picks up newly added timetable. Partial sweeps merge into
the known element set; a full sweep replaces it, so elements that disappear from
every pooled class disappear from the pickers too.

`GET /admin/recon` (or the dashboard's recon section) reports scan progress per
school, and `POST /admin/recon/rescan` forces a complete sweep of every pooled
class without waiting for a restart. Boot with `-recon-rescan` to do the same
once at startup.

---

## Admin dashboard

Embedded single-page UI at **`/admin`**, gated by an admin session
(`users.admin`). It grants full DB/`untisctl` capability: users, perms, pool,
tokens, schools, webhooks, ntfy topics, recon.

`GET /admin/outbox` reports the notification-delivery backlog — counts by state
plus the most recent destinations that gave up, with their error. `/admin/status`
carries the same counts as an `outbox` object. The public `/healthz` and
`/status` never include this detail.

- All element pickers (class / **student** / teacher / room / subject) are
  fuzzy-searchable — no raw ids in the UI.
- Bootstrap the first admin with `untisctl users admin --user evan` (or
  `-admin evan` at boot).
- The ntfy table has a **test** button per topic that publishes a real test
  notification and reports the receiver's HTTP status. The test carries the same
  `title` / `tags` / `click` fields a real change does, so a green test means the
  notification will actually *render* — not just that the socket is open.

Users can self-service **their own** class subscriptions at `/api/webhooks` and
`/api/ntfy`; other classes or elements require editor/boosted/admin.

Full reference: [`docs/ADMIN-WEBHOOKS-NTFY.md`](docs/ADMIN-WEBHOOKS-NTFY.md).

---

## Change detection

```
GET /api/timetable/changes?since=<version>   # pollable diff
GET /api/timetable/stream                    # SSE live stream
```

The server polls each pooled class every `-poll-interval`, diffs it against the
last stored snapshot, bumps the class version, and persists the change rows.

Both endpoints require a session and are scoped to the caller's own class unless
they hold the access to see another (`classId=` otherwise). Payload:

```json
{"event":"change","school":"testschool","classId":5000,"version":7,
 "summary":"1 added, 1 removed",
 "digest":{"summary":"…","message":"…"},
 "changes":[…]}
```

The stream emits `event: snapshot` on connect, then `event: change` per detected
change — this is what the Android app subscribes to.

**The digest** is the human-readable rendering of the same diff
(`Thu 01.10. 09:50–10:35 · Physik · room 12-14 → 01-Aula · TchrA · Substitution:
…`), used as the `message` of every ntfy notification and the
`X-Untis-Summary` header of every webhook.

---

## Webhooks

On every detected change, JSON is POSTed to configured URLs with:

- an optional HMAC-SHA256 signature in `X-Untis-Signature`,
- a single-line human summary in `X-Untis-Summary`,
- the full change payload as the body.

Each webhook targets **any element** (class / student / teacher / room / subject)
or is **school-wide**. It fires when the matching element is involved: the class
id, the student's class, or the teacher/room/subject name appearing in the
changed rows.

Deliveries are **durable**: each one is queued in the same transaction that
records the new timetable version, so a detected change is never silently
dropped, and it survives a restart. A failing destination retries with backoff
(30s doubling to a 15m cap, 8 attempts) and is then marked dead — visible via
`GET /admin/outbox` — while other destinations for the same change are unaffected.
See [docs/ADMIN-WEBHOOKS-NTFY.md](docs/ADMIN-WEBHOOKS-NTFY.md#delivery-reliability).

---

## ntfy push

Change events are fanned out to ntfy topics using the same per-element targeting
as webhooks; each notification names its element in the title
(`Timetable change — student Test Student`).

**The JSON envelope is posted to the ntfy server *root*, not to `/<topic>`.**
This is an ntfy requirement, and getting it wrong fails silently: ntfy accepts a
POST to `/<topic>` with HTTP 200 and then stores the entire body as the message
*text*, so the reader sees raw JSON and the `title`/`tags`/`click` fields are
discarded. Posting to the root with the topic in the body is what makes the
notification render. See [the ntfy docs](https://docs.ntfy.sh/publish/#publish-as-json).

| Field | Value |
|---|---|
| `topic` | the topic name, as configured |
| `title` | names the target element |
| `message` | the human-readable digest |
| `tags` | `calendar` (and `white_check_mark` on tests) |
| `click` | the change-details page for that class — from `-public-base` |
| `priority` | default |

The default server is `-ntfy-base` (`https://ntfy.sh`); any topic can override it
with its own `baseUrl` for a self-hosted ntfy. Overrides are stored with trailing
slashes trimmed, so the dashboard cannot list two rows that are the same
subscription.

> **Two matching subscriptions to the same topic produce two notifications.**
> Rows are matched per element and there is no de-duplication pass, so a
> class-wide change that matches both a `CLASS` row and a `STUDENT` row pointing
> at the same server and topic delivers twice, with the same title. Keep one row
> per (element, server, topic) you actually want.

---

## API endpoints

**Status and health** (unauthenticated, for containers and monitoring)

```
GET /status      # cheap liveness probe
GET /healthz     # 200 while every school with pooled classes is being polled, else 503
GET /me          # the signed-in user's effective permission level
```

`/metrics` is deliberately not in that list. The Prometheus endpoint labels every
series with the school name and reports the pool size and poll counters beside
it — not secrets, but a free inventory of the deployment. It used to be a bare
route on this handler, so anyone who could reach a public hostname got all of it
in one unauthenticated GET. It is now off unless you ask for it, and then it
listens somewhere else entirely:

```sh
untis-server -metrics-addr 127.0.0.1:9109 ...   # scrape from the host
docker run -p 127.0.0.1:9109:9109 ...          # inside a container
```

It serves `/metrics` and the detailed `/healthz`, and nothing else.

`/healthz` is on the public handler because a healthcheck needs it, but it
returns only the verdict and the shape — `status`, `uptime_sec`, how many schools
are watched, how many are behind. The per-school detail it used to return
(school name, class count, scan progress, poll counters) was public JSON, and the
reason strings quote class counts, so both moved to the gated listener along with
`/metrics`. `/status` names no school and stays as it is.
```

`/healthz` reports per school the pooled class count, how far the recon scan has
covered through the target horizon, poll counters, and how long ago the last
**successful** class poll was. A poll that never reached the school server does
not count, so a proxy that is up but serving stale timetables is reported as
degraded rather than looking healthy.

**Session and school selection.** The app logs in through
`/WebUntis/jsonrpc_intern.do?school=<name>&m=<method>` and gets a `JSESSIONID`
cookie; all `jsonrpc.do` calls then run on that session. `?school=` picks the
school on the login request.

**Subscriptions and changes**

```
GET  /api/timetable/changes?since=<version>
GET  /api/timetable/stream
GET  /api/webhooks        /api/webhooks/{id}     # self-service
GET  /api/ntfy            /api/ntfy/{id}         # self-service
```

**Homework completion** (session-authenticated; the viewer comes from the session
cookie and never from the request body)

```
GET  /api/homework/flags                  # this student's own flags
POST /api/homework/done  {"homeworkId":123,"done":true}
```

The same flags also arrive enriched on the homework the app already reads: every
`homeWorks[]` entry in a `getHomeWork2017` or `getPeriodData2017` response gains
`done` (bool) and `doneAt` (RFC3339 or null), next to the untouched teacher-owned
`completed`. Untis's `completed` is the teacher's mark and is never written
through the proxy, so a student can answer for themselves without ever overwriting
a teacher. Writes are idempotent, and an enrichment that changes nothing returns
the upstream bytes unchanged.

**Absence notes** (session-authenticated, same contract)

```
GET  /api/absence/notes[?school=<name>]     # this student's own notes
POST /api/absence/notes   {"absenceKey":300001,"note":"bring workbook"}
POST /api/absence/notes   {"absenceKey":300001,"note":""}   # clears the note
```

Every entry in a `getStudentAbsences2017` response gains a private `note` and a
`derived` block (weekday, date, class name, reason text, and the subject of the
lesson the absence displaced where that is unambiguous). Upstream `text` is the
*teacher's* comment and `excuse.text` is upstream's own excuse text — neither is
touched, and the student's note is a separate field.

**Notes are private to the student, by construction.** They are stored per viewer
and are only ever attached to an absence whose own `studentId` matches the
session. `getStudentAbsences2017` is not a class-scoped method, so it is never
replayed as a boosted teacher, and no editor or teacher response has a code path
to a note lookup. An absence with no `studentId`, or one belonging to a different
student, is passed through completely undecorated.

**Calendar**

```
POST /api/calendar/token        # request a calendar subscription token
GET  /api/calendar/{token}.ics  # the iCal feed (Outlook/Google subscribe)
GET  /week/{token}              # mobile week page for the same token (no app needed)
```

**Admin** — `/admin` (UI), `/admin/login`, and the `/admin/*` JSON API
(consumers, perms, pool, tokens, schools, webhooks, ntfy, recon, status).

---

## Calendar subscriptions

`POST /api/calendar/token` requires a logged-in session. Exactly one target:

| Field | Access | Description |
|---|---|---|
| `{"classId": 5000}` | pool members | class timetable |
| `{"personal": true}` | self | your own personal student timetable |
| `{"personId": 5006}` | self (must be your own person id) | student timetable — same as `personal`, explicit id form (used by the dashboard student picker) |
| `{"teacherId": 123}` | recon or boosted | teacher timetable (reconstructed) |
| `{"roomId": 456}` | recon or boosted | room timetable (reconstructed) |
| `{"subjectId": 789}` | recon or boosted | subject timetable (reconstructed) |

Optional: `{"timezone": "Europe/Berlin"}` (default).

It returns a stable URL:

```
https://<host>/api/calendar/7aff8e9dd4707fde8c992c3902be0c96.ics
```

Paste that into **Outlook → Add calendar → From internet** (or iCloud/Google).
`/week/{token}` renders the coming seven days server-side — no JavaScript, no
app, no subscription — resolving names the same way the feed does and marking
`entfällt`, `Vertretung` and `Klausur` when the school server reports them, so a
parent can just bookmark the URL. The token is the credential, exactly like the
`.ics` feed.

**Timing:** the server re-fetches at most `-ttl` (5 min) stale. Calendar
providers re-subscribe on their own schedule (often hours), so expect calendar
edits to appear well after the change-detection stream, which fires within one
`-poll-interval` (~60s).

The dashboard's token tab can mint a student token from its **student search**
picker; `untisctl calendar student` does the same from the CLI.

---

## `untisctl` — operator CLI

A single tool to manage the database (perms, users, pool, tokens, status)
instead of stacking flags. It edits the SQLite DB directly, so it can run against
a live server's data file.

```sh
go build ./cmd/untisctl
./untisctl -db data/untis.db <command>
```

### users — accounts and secrets

```sh
untisctl users list
untisctl users add --user Asmith --secret <base32-key> --method key
untisctl users add --user X --secret <password> --method password
untisctl users admin --user evan        # grant admin (or --off to revoke)
untisctl users remove --user Asmith     # also removes secret, perms, personal tokens
untisctl users check                   # live login test for every stored account
untisctl users check --remove-classless # also delete accounts the API reports without a class
```

Users store either a password (`method=password`) or a **base32 TOTP secret**
(`method=key`, 16 chars, e.g. `<REDACTED-TOTP-SEED>`). The server logs these
accounts into WebUntis on their behalf.

**Paste-the-secret login:** at the `keyLogin`/`getUserData2017` login you may
paste the **base32 shared secret itself** where the 6-digit TOTP goes — the
proxy derives the live code and validates it upstream, then remembers the secret
so the account is replayable afterwards. A rejected/foreign paste never
overwrites a stored key (it is only persisted after the real server accepts the
login).

### perms

See [Permissions](#permissions).

### pool — classes

```sh
untisctl pool list   # pooled classes + their owner account
```

A class is in the pool as soon as any user belongs to it (`class_id` set).

### calendar — calendar subscriptions

```sh
untisctl calendar list                                # all tokens
untisctl calendar create --class 5000                 # class timetable
untisctl calendar create --teacher Müller             # teacher (fuzzy name)
untisctl calendar create --room Aula                  # room (fuzzy name)
untisctl calendar create --subject BIO                # subject (fuzzy name)
untisctl calendar create --personal --user jdoe     # personal timetable
untisctl calendar student --user jdoe               # student timetable (interactive DB user picker)
untisctl calendar revoke <tok>                        # revoke one
untisctl tokens list                                  # alias for calendar list
untisctl tokens revoke <tok>                          # alias for calendar revoke
```

Fuzzy name lookup: exact match → substring match → Levenshtein suggestions.
Numeric IDs pass through directly. Ambiguous matches list all candidates.

### totp — one-time codes / QR for a key account

```sh
untisctl totp Asmith              # current 6-digit code + seconds left
untisctl totp Asmith --scan       # otpauth:// URI + ANSI QR (needs qrencode)
```

Flags are accepted in any position (`totp Asmith --scan`). Reads the account's
base32 secret from the DB and computes the live RFC-6238 code — handy for the app
login when you don't have an authenticator handy.

### status — database stats

```sh
untisctl status
# users, pool size, recon element counts, perms rows, token count
```

### backup — restorable copy of the database

```sh
untisctl backup                       # data/untis.db.untis-backup-<UTC>.db next to the live file
untisctl backup --out /mnt/nas/untis.db
untisctl backup --keep 7              # keep the 7 newest automatic backups
```

The database holds every account secret, calendar token and notification
subscription, and upgrades run schema migrations that cannot be undone, so take
a copy before every deploy. `backup` uses SQLite's `VACUUM INTO`: safe against a
live server, non-blocking for readers, and the result is a single
self-contained file with no `-wal` sidecar. It refuses to overwrite an existing
file, and `--keep` only prunes files carrying the automatic
`.untis-backup-<timestamp>.db` name of that same database.

---

## Provisioning new students

Once per person, provision their WebUntis account into the DB so the proxy can
authenticate as them (becomes a pool owner / can log in):

1. Add the account with `untisctl users add` (key or password).
2. Optionally grant reconstruction perms with `untisctl perms grant ...`.
3. `untisctl users check` to confirm the stored logins actually work.

The seed tool (`cmd/seed`) does the same from a `credentials.txt` INI-style file
and also fills in `person_id` / `class_id` by logging in live.

---

## Internals worth knowing

These are the two things that make the app's editor work, and both are
counter-intuitive enough to be worth writing down.

### The absence/lesson editor needs a teacher identity

The app builds its absence editor (and the per-lesson editing sheet) from
`getPeriodData2017`. Upstream answers that method with the **class roster, the
class register, the absence state and the per-period `can` capabilities only when
the request authenticates as a teacher**. The identical call made with a student
identity comes back with an empty answer:

| field of a `getPeriodData2017` result | as a student | as a teacher |
|---|---|---|
| `referencedStudents` | `0` | class size |
| `studentIds` / `classRoles` | `0` | class size / entries |
| `absences` | `null` | the actual entries |
| `can` | `READ_HOMEWORK`, `READ_PERIODINFO` | includes `READ_STUD_ABSENCE`, `WRITE_STUD_ABSENCE`, `READ/WRITE_CLASSREGEVENT`, … |

So an editor's screen comes up **empty** unless that call is made as a teacher —
no `can` rewrite can fix it, because the data itself is withheld. `classScopedMethods`
in `internal/proxy/proxy.go` lists the class-scoped methods (`getPeriodData2017`
plus the absence/lesson writes) that are therefore **re-authenticated as the
boosted teacher source for users holding `editor`**, on both the
self-authenticating `jsonrpc_intern.do` path and the session `jsonrpc.do` path.

This is deliberately narrow:

- **Personal data stays personal.** Everything else — the user's own timetable,
  their own absences, `getUserData2017` — keeps running on the requester's own
  account. The editor flag widens *editing*, it never turns a personal read into
  somebody else's.
- **Only editors are lifted.** A non-editor's `getPeriodData2017` is forwarded
  with their own account, so nobody reaches class data they were not granted.
- **A write still needs the permission upstream grants it.** Routing through the
  teacher lets the write succeed for someone who legitimately holds `editor`; it
  does not grant a teacher more than the teacher already has.

### Capability rewriting

Upstream derives each period's `can` list from the account the request
authenticated as, and the app hides any UI whose capability is missing. The proxy
rewrites `can` on the way out (`internal/proxy/caps.go`):

- **`READ_LESSONTOPIC` is always added.** `text.lesson` is part of every period we
  already send, but a pooled class is fetched with the class owner's student
  key, whose capabilities are only `READ_HOMEWORK`/`READ_PERIODINFO` — without
  this the app shows teacher, class and room and silently hides the lesson topic.
- **Every non-`READ_` capability is dropped unless the user holds `editor`.** A
  boosted user is forwarded through a *teacher* account, which returns the full
  teacher capability set; without the rewrite the app would offer editing on
  classes the user only has read access to. The server-side write-method gate
  refuses those calls regardless, so this only removes dead buttons — and with
  `editor` they come back unchanged.

In the app, classes are always shown; **teacher/room/subject element types are
hidden unless the user has recon or is boosted** (the masterData `displayAllowed`
flags are set from the user's effective access). For a recon user the flags are
also restricted to the elements the pooled classes actually reference, so the
app's pickers offer only elements that can be reconstructed.

---

## Troubleshooting

**"The app calls nothing for absences"** — check the log before concluding the
app is broken. The absence editor is fed by `getPeriodData2017`, and a screen
that stays empty is usually the student-vs-teacher identity issue described
above, not a missing call. To confirm an editor is actually being boosted, the
log prints `[intern] class-scoped <method>: <user> -> <teacher>`; if you do not
see it, the user does not hold `editor`. See
[`DEPLOYMENT.md`](DEPLOYMENT.md#reading-the-log) for the log format.

**"I can't log into the dashboard"** is nearly always the database path, not the
permission — see [the `-db` note](#configuration) and
[`DEPLOYMENT.md`](DEPLOYMENT.md#the-database-path-trap).

**"My ntfy notification showed up as raw JSON"** — the envelope was posted to
`/<topic>` instead of the server root; see [ntfy push](#ntfy-push).

---

## Docs

Everything is in a `.md` file in this repository. Reference material first —
those describe how the current code behaves and are kept in step with it.

| Doc | Read it for |
|---|---|
| [`DEPLOYMENT.md`](DEPLOYMENT.md) | running it, Docker, the tunnel, `-metrics-addr`, backups, the log, ops troubleshooting |
| [`docs/APP-INTEGRATION.md`](docs/APP-INTEGRATION.md) | the contract for a client that wants live changes |
| [`docs/ADMIN-WEBHOOKS-NTFY.md`](docs/ADMIN-WEBHOOKS-NTFY.md) | full `/admin/*` API, ntfy, multi-school |
| [`docs/GOD-API-PLAN.md`](docs/GOD-API-PLAN.md) | the Basic/Reconstruction/Boosted/Editor permission tiers and planned upstream API coverage |

In progress — agreed in discussion, not yet built:

| Doc | Read it for |
|---|---|
| [`docs/PLANNED-FEATURES.md`](docs/PLANNED-FEATURES.md) | personal homework done, private absence notes, per-student Technik events, and the notification-delivery outbox |

Then the checklists. These are **dated records of test passes, not living
specs** — a `[x]` means it passed on the date it was run against the version named
in its heading, not that it passes today. Re-run them rather than trusting a tick,
and check the version before comparing output.

| Doc | Read it for |
|---|---|
| [`FEATURE-CHECKLIST.md`](FEATURE-CHECKLIST.md) | what is implemented, and when |
| [`MANUAL-QA-CHECKLIST.md`](MANUAL-QA-CHECKLIST.md) | the manual pass, step by step — **the one to follow for a new deploy** |
| [`TESTING-CHECKLIST.md`](TESTING-CHECKLIST.md) | API surface walk-through, coverage checklist |

Two conventions, so a stale number cannot quietly pass as current:

- Anything version-specific is written as **`<published tag>` or `dev`**, never a
  literal tag. `dev` in `/status` means the version was not compiled in; a real tag
  means it was.
- Anything about **which endpoints are public** is asserted from the public
  address. `/status` and `/healthz` are there; `/metrics` and the per-school
  health detail are not, unless `-metrics-addr` is set. See
  [`DEPLOYMENT.md`](DEPLOYMENT.md#health-and-monitoring).
