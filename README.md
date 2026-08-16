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

Downloads the installer from the latest release (not the mutable `main`
branch) and verifies its SHA-256 against the release's `checksums.txt`
before executing it:

```bash
(
  set -e
  curl -fsSLO https://github.com/useteploy/teploy-dash/releases/latest/download/install.sh
  curl -fsSLO https://github.com/useteploy/teploy-dash/releases/latest/download/checksums.txt
  grep " install.sh\$" checksums.txt > checksum.txt
  if command -v sha256sum >/dev/null 2>&1; then sha256sum -c checksum.txt || exit 1; else shasum -a 256 -c checksum.txt || exit 1; fi
  sh install.sh
)
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
to create your username and password. Credentials are stored as bcrypt hashes
in `/var/teploy-dash/users.json` (a legacy single-user `auth.json` from older
versions is migrated into it automatically on first load).

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
- **KV** tab: browse, read, set and delete keys in an app's shared Nucleus
  KV store, via `teploy kv`. Values are fetched one at a time when you ask
  for them, never prefetched or cached — each read is a live CLI call.
  Reading needs `viewer`, writing needs `editor`. The store is one global
  keyspace with no server-enforced namespaces: key prefixes are convention,
  so anything sharing that accessory sees the same keys.

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

### Service links
- **Settings → Links** holds shortcuts to whatever else you run — Forgejo,
  Proxmox, TrueNAS, a NAS UI, anything with a URL.
- **Home** and **Header** are independent per link: a card on Home, an icon in
  the top-right of every page, or both. Up to eight header icons are shown.
- Icons use the site's favicon by default. A link can instead carry SVG path
  data (24×24 viewBox) in its **Icon** field, drawn in the current text colour
  so it is white on the dark theme and black on the light one — GitHub and X
  have that built in. Failing both, coloured initials.
- Stored in `homepage.json` in the data dir; editing requires the `editor`
  role.

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
| `--public-status` | `false` | Serve an unauthenticated public status page at `/status`. Off by default. |

## MCP (AI clients)

Dash ships an [MCP](https://modelcontextprotocol.io) server at `POST /api/mcp`,
so Claude Code, Cursor, or any MCP client can inspect your fleet and run
deploy actions. Every action goes through the same teploy CLI delegation the
dashboard buttons use, and every read comes from the server state files the
CLI writes — MCP adds a fourth client to the single source of truth, not a
second source of truth. There is nothing new to drift.

Create a token under **Settings → MCP** (read-only tokens see only read
tools), then:

```bash
claude mcp add teploy --transport http \
  --header "Authorization: Bearer <token>" \
  https://dash.example.com/api/mcp
```

Tools: `teploy_list_apps`, `teploy_get_app`, `teploy_app_logs`,
`teploy_list_servers`, `teploy_list_monitors`, `teploy_list_env_keys`
(names only — values never cross the MCP boundary), plus actions
`teploy_deploy`, `teploy_rollback`, `teploy_restart`, `teploy_stop`,
`teploy_start`, `teploy_lock`/`unlock`, `teploy_maintenance_on`/`off`,
`teploy_set_env`, `teploy_unset_env`. Tokens are 256-bit secrets stored
hashed in the dash data dir; revocation is immediate.

## Public status page

`--public-status` (or `TEPLOY_DASH_PUBLIC_STATUS=1`) serves a customer-facing
status page at `/status` — no login required. It shows, for each **enabled**
monitor, only its **name**, current **up/down** state, and **24-hour uptime %**.
It deliberately never exposes monitor targets/IPs, server names, response
bodies, or any config. Off by default; when off, `/status` and `/api/status`
return 404.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TEPLOY_DASH_USER` | `admin` | Username used when `TEPLOY_DASH_PASSWORD` is set (env-var bootstrap mode). |
| `TEPLOY_DASH_PASSWORD` | _(optional)_ | Bootstrap password. If set, credentials are taken from this env var. If absent and no `users.json` exists, the first run shows the setup page to create an account. |
| `TEPLOY_DASH_PUBLIC_STATUS` | _(off)_ | Set to `1`/`true` to enable the public `/status` page (same as `--public-status`). |
| `TEPLOY_DASH_TRUSTED_PROXY` | _(none)_ | Comma-separated proxy IPs/CIDRs. When set, the real client IP is read from `X-Forwarded-For` (for rate-limiting) and `X-Forwarded-Proto` is trusted for the secure-cookie flag. Set this when running behind Caddy/nginx. |
| `TEPLOY_NAV_OBSERVE_URL` | _(none)_ | URL of your Teploy Observe dashboard. When set, it appears in the top-left cross-product switcher. |
| `TEPLOY_NAV_SHIP_URL` | _(none)_ | URL of your Teploy Ship dashboard. When set, it appears in the top-left cross-product switcher. |

### Single sign-on (OIDC)

Optional. When `TEPLOY_DASH_OIDC_ISSUER` and `TEPLOY_DASH_OIDC_CLIENT_ID` are set,
the login page offers an SSO button and Dash acts as an OpenID Connect relying
party (authorization-code flow with PKCE). Password login stays available as the
break-glass path. Register `https://<your-dash-host>/oidc/callback` as the
redirect URI with your provider.

| Variable | Default | Description |
|----------|---------|-------------|
| `TEPLOY_DASH_OIDC_ISSUER` | _(none)_ | IdP issuer URL (discovery base, e.g. `https://your-org.okta.com`). Required to enable SSO. |
| `TEPLOY_DASH_OIDC_CLIENT_ID` | _(none)_ | OAuth client ID. Required to enable SSO. |
| `TEPLOY_DASH_OIDC_CLIENT_SECRET` | _(none)_ | OAuth client secret. Omit for a public (PKCE-only) client. |
| `TEPLOY_DASH_OIDC_REDIRECT_URL` | _(derived)_ | Callback URL. Derived from the request Host when unset; set it explicitly behind a proxy that rewrites Host. Must be `.../oidc/callback`. |
| `TEPLOY_DASH_OIDC_SCOPES` | `openid profile email` | Space/comma-separated scopes (`openid` is always included). Add `groups` if you use group-based role mapping. |
| `TEPLOY_DASH_OIDC_LABEL` | `Single sign-on` | Text on the SSO button. |
| `TEPLOY_DASH_OIDC_USERNAME_CLAIM` | `preferred_username` | Token claim used as the Dash username (falls back to `email`, then `sub`). |
| `TEPLOY_DASH_OIDC_ROLE_CLAIM` | `teploy_role` | Token claim carrying the role directly (`admin`/`editor`/`viewer`). Checked first. |
| `TEPLOY_DASH_OIDC_GROUPS_CLAIM` | `groups` | Token claim listing the user's groups, used when no direct role claim matches. |
| `TEPLOY_DASH_OIDC_ADMIN_GROUP` | _(none)_ | Group whose members become `admin`. |
| `TEPLOY_DASH_OIDC_EDITOR_GROUP` | _(none)_ | Group whose members become `editor`. |
| `TEPLOY_DASH_OIDC_VIEWER_GROUP` | _(none)_ | Group whose members become `viewer`. |
| `TEPLOY_DASH_OIDC_DEFAULT_ROLE` | `viewer` | Role for an authenticated user matching no role claim or group (least privilege). |

Role resolution order: a recognized `teploy_role` claim wins; otherwise groups
are matched (admin > editor > viewer); otherwise the default role. SSO users are
not stored in `users.json` — their role comes fresh from the IdP on every login,
so manage them in your IdP, not in Settings → Users.

#### Self-hosted identity providers

Any OIDC provider works. Two are worth calling out because if you already run
Teploy you probably already run one of them, so SSO costs you no new software.

**Forgejo** (or Gitea) is a full OIDC provider. Its discovery document
advertises `openid profile email groups` and a `groups` claim.

1. Register an OAuth2 application — Site Administration → Applications for an
   org-wide one, or user Settings → Applications for a personal one. Set the
   redirect URI to `https://<your-dash-host>/oidc/callback`.
2. Point Dash at it:

```bash
TEPLOY_DASH_OIDC_ISSUER=https://forgejo.example.com
TEPLOY_DASH_OIDC_CLIENT_ID=<client id>
TEPLOY_DASH_OIDC_CLIENT_SECRET=<client secret>
TEPLOY_DASH_OIDC_SCOPES="openid profile email groups"
TEPLOY_DASH_OIDC_ADMIN_GROUP=platform:owners
TEPLOY_DASH_OIDC_EDITOR_GROUP=platform:deployers
```

- Request `groups` explicitly. It is not in the default scopes, and without it
  no group matches, so every user lands on `TEPLOY_DASH_OIDC_DEFAULT_ROLE`.
- Forgejo emits one entry per org (`platform`) and one per team
  (`platform:deployers`). Group comparison is exact and case-sensitive, so copy
  the names as Forgejo spells them.
- Forgejo cannot mint a custom claim, so leave `ROLE_CLAIM` at its default and
  map roles by group.
- Each dashboard needs its own OAuth2 application because the redirect URIs
  differ, but all three can map against the same orgs and teams.

**OpenBao** also serves OIDC (`identity/oidc/provider`), which is convenient if
you already run it for `teploy secret --provider openbao`. Create a provider,
an assignment, and a client, then use the provider's discovery URL as the
issuer:

```bash
TEPLOY_DASH_OIDC_ISSUER=https://openbao.example.com/v1/identity/oidc/provider/teploy
```

Map roles with a scope template that emits a `groups` array (matched as above),
or one that emits a `teploy_role` string — OpenBao can produce a custom claim,
so the direct role claim is available here and takes precedence over groups.

## API

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/health` | Liveness probe (auth-exempt). |
| GET | `/status`, `/api/status` | Public status page + JSON (auth-exempt; 404 unless `--public-status`). Exposes only name/up-down/24h-uptime. |
| GET | `/api/cli/status` | Whether the `teploy` CLI is on `$PATH` and its version. |
| GET | `/api/apps` | Fleet app list across all configured servers. |
| GET | `/api/apps/{server}/{app}/status` | Single app status. |
| POST | `/api/apps/{server}/{app}/{action}` | `stop`, `start`, `restart`, `rollback`, `lock`, `unlock`, `maintenance/on`, `maintenance/off`. |
| GET / POST | `/api/apps/{server}/{app}/env` | List / set env vars. |
| DELETE | `/api/apps/{server}/{app}/env/{key}` | Unset env var. |
| GET | `/api/apps/{server}/{app}/log` | Recent CLI deploy log. |
| GET | `/api/apps/{server}/{app}/drift` | Live containers vs the deployed version. |
| GET | `/api/apps/{server}/{app}/stats` | Per-container CPU / memory / IO. |
| GET | `/api/apps/{server}/{app}/health` | On-demand health probe against the running app. |
| GET / POST / DELETE | `/api/apps/{server}/{app}/kv` | List keys (`?pattern=`) / set (`{key,value,ttl}`) / delete (`?key=`) in the shared Nucleus KV store. `?accessory=` defaults to `nucleus`. |
| GET | `/api/apps/{server}/{app}/kv/value` | Read one value (`?key=`). Returns `exists:false` for an unset key. |
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
| GET / POST | `/api/restore-tests` | List / create scheduled backup verifications. |
| GET / DELETE | `/api/restore-tests/{id}` | Detail / delete. |
| POST | `/api/restore-tests/{id}/run` | Verify the latest backup now (restores into a scratch container via `teploy accessory verify-backup`). |
| GET / POST | `/api/notifications` | Read / write alert config. |
| GET / PUT | `/api/homepage` | Service links (Home grid + pinned header icons). |

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
