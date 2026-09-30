# Hosting and building untis-proxy

Everything about **running**, **deploying** and **building** the proxy. For what
the proxy does and how to use it, see [`README.md`](README.md).

---

## Table of contents

- [Running it locally](#running-it-locally)
- [The database path trap](#the-database-path-trap)
- [Docker Compose on a NAS](#docker-compose-on-a-nas)
- [Exposing it publicly (Cloudflare Tunnel)](#exposing-it-publicly-cloudflare-tunnel)
- [Building and pushing the image](#building-and-pushing-the-image)
- [The release gate](#the-release-gate)
- [Backups](#backups)
- [Reading the log](#reading-the-log)
- [Health and monitoring](#health-and-monitoring)
- [Ops troubleshooting](#ops-troubleshooting)

---

## Running it locally

Copy the template and set your school first — it is the one value with no
default, because every row in the database is keyed by it:

```sh
cp .env.example .env
$EDITOR .env          # set UNTIS_SCHOOL
```

Then:

```sh
./scripts/run.sh          # build, stop any old process, start detached, wait for health
curl http://127.0.0.1:8509/status
```

`run.sh` sources `.env`, so the values you set there apply to the local run the
same way the compose files apply them to a container. It starts the server
**detached** (`setsid`, so it survives the terminal), appends to
`bin/untis-server.log`, and passes `-db data/untis.db` explicitly. It prints the
pid, the database it used and the health code.

To run it in the foreground instead (Ctrl-C stops it):

```sh
set -a; . ./.env; set +a
go build -o bin/untis-server ./cmd/server
./bin/untis-server -db data/untis.db
```

Any `UNTIS_*` variable in `.env` is picked up by both paths — the server binary
reads them as flag defaults, so `-public-base`, `-ntfy-base`, `-env` and the
rest do not need to be repeated as flags. An explicit flag still wins over the
environment.

Override the database path with `UNTIS_DB=/some/other.db ./scripts/run.sh`.

The server needs a populated database (users + secrets) — see
[Provisioning new students](README.md#provisioning-new-students).

---

## The database path trap

The server's own `-db` default is `untis.db` **relative to the working
directory**. Start it from the repo root without `-db` and it silently creates
and uses a brand-new empty database.

Nothing then looks like a permission problem — the admin flag, the boost flags
and the editor flag are all simply absent, and the dashboard reports "not an
admin" for everyone. Pass `-db`, or use `run.sh`.

Check what the process actually has open:

```sh
ls -l /proc/$(pgrep -x untis-server)/fd | grep '\.db'
```

If that shows a bare `untis.db` in the repo root instead of
`data/untis.db`, you are on the wrong database.

---

## Docker Compose on a NAS

Linux host with Docker Compose. Use `compose.nas.yaml` (the image is prebuilt on
Docker Hub, no build on the NAS):

```yaml
services:
  untis-proxy:
    image: <your-namespace>/untisplus-proxy:latest
    container_name: untis-proxy
    restart: unless-stopped
    ports:
      - "8509:8509"
    environment:
      UNTIS_ADDR: ":8509"
      UNTIS_ENV: "prod"
      UNTIS_POLL_INTERVAL: "60s"
      UNTIS_NTFY_BASE: "https://ntfy.sh"
      UNTIS_PUBLIC_BASE: ""
    volumes:
      - ./data:/data
```

That is the shape of it; see `compose.nas.yaml` for the real file, which takes
`UNTIS_SCHOOL` and `UNTIS_IMAGE_REPO` from `.env` and refuses to start without
them. There is no version setting: the image knows its own version, and
`GET /status` reports it.

`compose.yaml` is the local-build variant (builds from the Dockerfile and joins
the `cloudflare-net` / `npm_network` external networks).

### Steps

1. **Create the folder** and drop in `compose.nas.yaml`:
   ```sh
   mkdir -p ~/untis-proxy/data && cd ~/untis-proxy
   ```

2. **Migrate the existing database** (carries over pool, perms, calendar tokens,
   recon snapshot) from the old host:
   ```sh
   scp data/untis.db your-nas:~/untis-proxy/data/untis.db
   ```
   Skip this to start empty (the pool re-scans on boot, but perms/tokens are
   lost).

3. **Start** — it pulls <your-namespace>/untisplus-proxy:latest automatically:
   ```sh
   docker compose -f compose.nas.yaml up -d
   ```

4. **Verify on the LAN**:
   ```sh
   curl http://<NAS-IP>:8509/status
   ```

5. **Manage it**:
   ```sh
   docker exec untis-proxy untisctl -db /data/untis.db status
   docker exec untis-proxy untisctl -db /data/untis.db perms list
   ```

> `untisctl` is baked into the image as `/usr/local/bin/untisctl`. Always pass
> `-db` — inside the container the database is `/data/untis.db`, and the default
> `untis.db` relative to the working directory is a different (empty) file.

---

## Exposing it publicly (Cloudflare Tunnel)

The public URL runs through a **cloudflared (Cloudflare Tunnel)** container in
`host` network mode — a dashboard-managed, token-based tunnel, not the compose
labels themselves.

1. In Cloudflare **Zero Trust → Networks → Tunnels**, create a tunnel and add a
   public hostname (e.g. `untis-proxy.example.com`) → `http://localhost:8509`.
   Then set `UNTIS_PUBLIC_BASE` to that same `https://` URL in `.env`, so the
   click-through link on notifications is absolute rather than a relative path.
2. Run cloudflared on the NAS with that tunnel's token:
   ```sh
   docker run -d --name untis-tunnel --network host --restart unless-stopped \
     cloudflare/cloudflared:latest tunnel --no-autoupdate run --token <TOKEN>
   ```
3. Stop the old `untis-tunnel` on the previous host so two connectors do not
   fight over the hostname.

---

## Building and pushing the image

```sh
docker build -t <your-namespace>/untisplus-proxy:latest -t <your-namespace>/untisplus-proxy:v1.4.1 .
docker push <your-namespace>/untisplus-proxy:latest
docker push <your-namespace>/untisplus-proxy:v1.4.1
```

Usually done for you by [the release gate](#the-release-gate).

---

## The release gate

`scripts/release.sh` is the single path that ships: `go vet`, `go build`, the
full test run, an optional backup, then the Docker Hub push and the git push.

```sh
./scripts/release.sh                                          # gate, then push
VERSION=v1.4.1 ./scripts/release.sh                           # explicit tag
PUSH=0 ./scripts/release.sh                                   # local gate only
BACKUP_DB=data/untis.db ./scripts/release.sh                  # back up first
PUSH=0 BACKUP_DB=data/untis.db ./scripts/release.sh           # local gate + backup
```

| Env | Default | Meaning |
|---|---|---|
| `IMAGE` | *required* | Docker Hub repository, without a tag, e.g. `<your-namespace>/untisplus-proxy` — no default, so a release cannot land in whichever account is logged in 
| `VERSION` | exact git tag, else `dev` | tag pushed to Docker Hub |
| `PUSH` | `1` | set to `0` to skip docker push / git push |
| `BACKUP_DB` | unset | live database to back up before shipping |
| `BACKUP_KEEP` | `7` | automatic backups to keep |

The script is additive on purpose: do not pre-patch the tag or README inside it,
so it fails loudly and pinpoints exactly which step is broken. The format check
is intentionally **not** a `gofmt` gate — some Go toolchain builds rewrite `''`
in comments to typographic quotes, which would produce spurious diffs;
vet/build/test are the real gates.

---

## Backups

The database holds every account secret, calendar token and notification
subscription, and upgrades run schema migrations that cannot be undone. Take a
copy before every deploy:

```sh
untisctl backup                       # data/untis.db.untis-backup-<UTC>.db
untisctl backup --out /mnt/nas/untis.db
untisctl backup --keep 7              # prune to the 7 newest automatic backups
```

`backup` uses SQLite's `VACUUM INTO`: safe to run against a live server, does
not block readers, and produces a single self-contained file with no `-wal`
sidecar. It refuses to overwrite an existing file, and `--keep` only prunes
files carrying the automatic `.untis-backup-<timestamp>.db` name of that same
database.

---

## Reading the log

`bin/untis-server.log` by default, appended across restarts:

| prefix | what it shows |
|---|---|
| `[intern] /WebUntis/jsonrpc_intern.do?…&m=<method> body=…` | every self-authenticating call the app made, with its full body |
| `[public] /WebUntis/jsonrpc.do?… method=<method> user=<name>` | `jsonrpc.do` calls: method and account only, the body stays out of the log (it can carry messages) |
| `[intern] class-scoped <method>: <user> -> <teacher>` | an editor's class-scoped call that was re-authenticated as the teacher source |
| `[notify] …` | change fan-out; a non-2xx receiver status is logged here |
| `[multi-school] auto-registered school "…"` | a school seen for the first time |

The `class-scoped` line is the quickest way to confirm an editor is actually
being boosted: if you do not see it, the user does not hold `editor` (check
`untisctl perms list`).

---

## Health and monitoring

```
GET /status     # cheap liveness probe
GET /healthz    # 200 while every school with pooled classes is being polled, else 503
GET /metrics    # Prometheus text format
```

`/healthz` reports per school the pooled class count, how far the recon scan has
covered through the target horizon, poll counters, and how long ago the last
**successful** class poll was. A poll that never reached the school server does
not count, so a proxy that is up but serving stale timetables is reported as
degraded instead of looking healthy — point your container healthcheck at
`/healthz`, and use `/status` for liveness.

`run.sh` polls `/healthz` and prints the code it got.

---

## Ops troubleshooting

**"I can't log into the dashboard"** — nearly always the database path, not the
permission: `isAdmin` reads the `users.admin` column of whatever `-db` points at.
See [the database path trap](#the-database-path-trap).

**"Everyone reports as not an admin"** — same cause: the running process is on
a fresh empty database where no row has `admin` set. Confirm with
`ls -l /proc/$(pgrep -x untis-server)/fd | grep '\.db'`.

**"Nothing gets delivered to my webhook/ntfy"** — check the `[notify]` lines.
A non-2xx receiver status is logged there. Remember ntfy JSON must be posted to
the server **root**, not `/<topic>` — see
[ntfy push](README.md#ntfy-push).

**"The absence editor is empty in the app"** — this is an identity problem, not
a deployment one. See
[the editor section](README.md#the-absencelesson-editor-needs-a-teacher-identity).
