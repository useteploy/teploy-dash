# Teploy Dash

Self-hosted deployment dashboard for the [Teploy CLI](https://github.com/useteploy/teploy),
plus uptime monitoring — in one Go binary.

The CLI writes deployment state to `/deployments/{app}/state.json`. Dash
reads those files (read-only) and shells out to `teploy` for actions
(deploy, rollback, env edits, logs). Same source of truth whether you
deploy from the terminal, the UI, or a webhook — no SSH-vs-UI desync.

Optional uptime monitoring runs HTTP / TCP checks on configurable
intervals, stores history in Nucleus or a local JSONL file, and fires
webhook / SMTP alerts on state transitions.

One binary. ~17MB. Default port 3456.

## Install

### Homebrew (macOS, Linux)

```bash
brew install useteploy/tap/teploy-dash
```

### Docker (GHCR, multi-arch)

```bash
docker run -d -p 3456:3456 \
  -e TEPLOY_DASH_PASSWORD=$(openssl rand -base64 24) \
  -v /deployments:/deployments \
  ghcr.io/useteploy/teploy-dash:latest
```

### Install script

```bash
curl -sL https://raw.githubusercontent.com/useteploy/teploy-dash/main/scripts/install.sh | sh
```

On Linux the script also installs a `teploy-dash.service` systemd unit and
generates a random admin password into `/etc/teploy-dash/teploy-dash.env`
(printed on completion). Skip with `TEPLOY_DASH_NO_SERVICE=1`.

### Build from source

```bash
git clone https://github.com/useteploy/teploy-dash.git
cd teploy-dash
go build ./cmd/teploy-dash
```

The frontend lives at `cmd/teploy-dash/frontend/` and is embedded into
the binary at build time via `//go:embed`. The compiled binary is fully
portable — copy it anywhere and run it.

## Run

```bash
teploy-dash
```

Open `http://localhost:3456`. On first launch you'll be taken to a setup page
to create your username and password. Credentials are stored as a bcrypt hash
in `/var/teploy-dash/auth.json`.

You can also pre-set a password via environment variable (useful for Docker or
automated deploys — the setup page is skipped when this is present):

```bash
TEPLOY_DASH_PASSWORD=yourpassword teploy-dash
```

You can change your password any time from **Settings → Account** inside the UI.

```bash
teploy-dash --port 8080                                 # custom port
teploy-dash --deployments /opt/deployments              # custom CLI state dir
teploy-dash --nucleus-url postgres://localhost:5432/teploy_dash   # use Nucleus
teploy-dash --no-auth                                   # local dev only
```

## Features

### Deployment dashboard
- Live list of apps across every server in `servers.yml` (CLI config).
- Per-app status, current vs previous image hash, domain, container state.
- Stop / start / restart over SSH; deploy, rollback, lock, maintenance,
  registry login, env get/set/unset all delegated to the CLI.
- Multi-server fleet view with 60-second cache so the page doesn't SSH
  on every refresh.
- Persistent groups + projects (organisation overlay stored in
  `~/.teploy/groups.json`, format-compatible with the CLI's embedded UI).
- Umbrel-style template catalog: install pre-defined apps with one click.
- WebSocket log tailing per app (with SSE fallback for clients that
  don't speak WS).

### Uptime monitoring
- HTTP, TCP, and ping checks with per-monitor interval and timeout.
  HTTP checks honor an optional exact expected status code (when unset,
  any 2xx/3xx is healthy). Ping is a TCP-connect probe (the target needs
  a `host:port`); raw ICMP is not used.
- 24-hour stats (uptime %, total / up / down checks, average response time).
- Storage: Nucleus over pgwire (preferred) or rolling JSONL files
  (fallback). Daily cleanup for the file store keeps 7 days of checks.
- Manual "test now" runs a check immediately without saving.
- Webhook + SMTP alerts on state transitions (up → down, recovered).
- Notification config persisted to `~/.teploy/notifications.json`
  and reloadable from the UI without restart.

### Why no desync
The CLI is the source of truth. Dash never writes deployment state — it
reads the same JSON files the CLI writes, and shells out to the CLI for
every action. Whether the deploy came from the terminal, this UI, or a
CI webhook, everything reconciles to the same files.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `3456` | HTTP listen port. |
| `--host` | `0.0.0.0` | HTTP listen host. |
| `--data` | `/var/teploy-dash` | Data dir for monitor history (file-store mode). |
| `--deployments` | `/deployments` | Where the CLI writes per-app state files. |
| `--nucleus-url` | _(empty)_ | Optional Nucleus / Postgres URL for monitor storage. Falls back to JSONL on connect failure. |
| `--no-auth` | `false` | Disable authentication entirely. **Local dev only.** |

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TEPLOY_DASH_USER` | `admin` | Username used when `TEPLOY_DASH_PASSWORD` is set (env-var bootstrap mode). |
| `TEPLOY_DASH_PASSWORD` | _(optional)_ | Bootstrap password. If set, credentials are taken from this env var. If absent and no `auth.json` exists, the first run shows the setup page to create an account. |

## API

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/health` | Liveness probe (auth-exempt). |
| GET | `/api/cli/status` | Whether the `teploy` CLI is on `$PATH` and its version. |
| GET | `/api/apps` | Fleet app list across all configured servers. |
| GET | `/api/apps/{server}/{app}/status` | Single app status. |
| POST | `/api/apps/{server}/{app}/{action}` | `stop`, `start`, `restart`, `rollback`, `lock`, `unlock`, `maintenance/on`, `maintenance/off`. |
| GET / POST | `/api/apps/{server}/{app}/env` | List / set env vars. |
| DELETE | `/api/apps/{server}/{app}/env/{key}` | Unset env var. |
| GET | `/api/apps/{server}/{app}/log` | Recent CLI deploy log. |
| GET | `/api/apps/{server}/{app}/accessories` | List accessories (DBs, queues, etc). |
| GET | `/ws/logs/{server}/{app}` | WebSocket log stream (SSE fallback). |
| GET / POST / DELETE | `/api/config/servers` `/api/config/servers/{name}` | Manage servers via CLI. |
| GET / POST | `/api/registries` | List / login to image registries. |
| DELETE | `/api/registries/{server}` | Logout. |
| GET | `/api/templates` | App catalog. |
| POST | `/api/templates/install` | Install a template app. |
| GET / POST | `/api/groups` | List / create groups. |
| Various | `/api/groups/{name}/...` | Assign apps and projects, rename, delete. |
| GET / POST | `/api/monitors` | List with 24h stats / create. |
| GET / DELETE | `/api/monitors/{id}` | Detail + history / delete. |
| POST | `/api/monitors/{id}/test` | Run a check immediately. |
| GET / POST | `/api/notifications` | Read / write alert config. |

All non-health routes require a valid session cookie. Sessions are issued by
`POST /api/login` (24-hour TTL). Failed login attempts are rate-limited
per source IP.

## Architecture

```
Browser
   |
   v
teploy-dash (Go, ~17MB)  --- session-cookie auth middleware
   |                         embedded SPA (Alpine.js)
   |                         60s fleet cache
   |                         WebSocket log streamer
   |
   +--> reads CLI state files at /deployments/{app}/state.json
   +--> shells out to `teploy` for actions (deploy, rollback, env, ...)
   +--> SSH to fleet servers for stop / start / restart
   +--> uptime checks --(pgwire)--> Nucleus
                       \-(disk)----> JSONL files
```

## License

AGPL-3.0-or-later. See `LICENSE`. The embedded `frontend/js/alpine.js`
is Alpine.js, MIT-licensed and used unmodified.
