# Teploy Dash

Self-hosted deployment dashboard for the [Teploy CLI](https://github.com/useteploy/teploy),
plus uptime monitoring — in one Go binary.

The CLI writes deployment state to `/deployments/{app}/state` (a plain
`key=value` text file). Dash
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

Anonymous image volumes are NOT reused when the container is replaced, so
dashboard data (users, monitors, MCP tokens) and the CLI's `~/.teploy`
configuration need named volumes to survive upgrades. The generated password
below is printed to the container log once — copy it out, or use first-run
setup (`/setup` with the bootstrap token from the log) instead:

```bash
docker run -d -p 127.0.0.1:3456:3456 \
  -e TEPLOY_DASH_PASSWORD=$(openssl rand -base64 24) \
  -v deployments:/deployments \
  -v dash-data:/var/teploy-dash \
  -v teploy-config:/root/.teploy \
  ghcr.io/useteploy/teploy-dash:latest
```

For remote (non-localhost) access, put TLS in front (e.g. Caddy) rather than
exposing the container directly; a dedicated SSH identity plus a pre-provisioned
known_hosts (`-v ./known_hosts:/root/.ssh/known_hosts:ro`) is preferable to
mounting a personal SSH directory.

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

## First success

Terminal to first deploy action through the dashboard. Every step below
was executed against a scratch SSH+Docker host; each names the surface it
drives.

1. **Have the `teploy` CLI on the same machine** — Dash delegates every
   action to it and reads the state it writes. `GET /api/cli/status`
   answers whether it is on `$PATH` (the dashboard also tells you in
   Settings). Nothing else works until this does.
2. **Connect a server.** Servers come from the CLI's
   `~/.teploy/servers.yml` — add one with `teploy server add <name>
   <host> --user <user>` (or Dash's onboarding flow, which shells to the
   same command). Dash then SSH-polls each server; the fleet view shows
   every configured server on every response, with freshness
   (`fresh`/`stale`/`unknown`) and last-known apps even for unreachable
   ones.
3. **Check readiness.** The onboarding preflight
   (`GET /api/onboarding/preflight?server=<name>`, surfaced in the UI)
   checks the CLI, machine interface, SSH, host read, Docker, disk
   headroom, and Caddy — each with `pass`/`fail`/`unknown`, a severity,
   and a remediation hint. Fix blocking checks before deploying; this is
   the "why is my server not deployable" answer, not a generic error.
4. **First deploy through the UI.** The Deploy form submits
   `POST /api/deploy {server, app, image, domain?, port?}` — an ad-hoc
   image deploy **requires a domain** (Caddy routing); omitting it is
   refused before any effect and the operation journal shows the CLI's
   exact stderr (`'domain' is required`). Projects can also deploy from
   registered git sources (webhook-driven) or dash-managed manifests.
   Deploying an app that already exists on the server (deployed via the
   CLI) needs no form at all — its page has the action buttons.
5. **Watch the operation.** Every action is an operation:
   `queued → running → succeeded | failed | canceled | ...` (the full
   state list is below). The Activity view and the operation's event
   journal (SSE at `/api/operations/{id}/events`) stream the CLI's live
   stdout/stderr. A failed operation shows the CLI's real error text —
   that text, plus `teploy doctor` on the target, is the diagnosis path.

Operation states you can see: `queued`, `running`, `cancel_requested`,
`stopping`, and terminal `succeeded`, `failed`, `canceled`,
`already_committed` (a cancellation whose effect landed anyway — not
retryable, no rollback was performed), `interrupted` (Dash restarted
mid-flight; reconciled against the target's receipts before retry is
allowed). Retry is refused while an interrupted operation is still
reconciling.

## Operational limits

- **One Dash process**, one deployment per data dir; instances do not
  peer or share state. Multiple dashes pointing at the same
  `~/.teploy/servers.yml` each poll independently.
- **Fleet view is a 60-second cache**; freshness turns `stale` past 2
  minutes. Actions are never served from the cache — they always run the
  CLI live.
- **Actions are bounded**: 8 concurrent CLI subprocesses across all
  targets (`TEPLOY_DASH_MAX_CONCURRENT_OPERATIONS`), 50 queued
  operations per server+app and 500 manager-wide before HTTP 429, a 24h
  idempotency window per principal, and retention defaults of 30 days /
  5000 finished operations (env-tunable — see the table above).
- **Monitor history without Nucleus** falls back to rolling JSONL files
  with a 7-day check retention (`--nucleus-url` uses Nucleus instead).
- **Public status page is off by default** and, when enabled, exposes
  only monitor name, up/down state, and 24h uptime — never targets,
  server names, or response bodies.

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
- Live log tailing per app over Server-Sent Events (`/api/logs/`).
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
| `TEPLOY_DASH_OPERATION_JOURNAL_BYTES` | `4194304` | Per-operation event-journal cap in bytes. When crossed, the journal is compacted to the retained tail (oldest events dropped, marked as a gap on replay). |
| `TEPLOY_DASH_OPERATION_HISTORY_DAYS` | `30` | Retention age for finished operations: records and event journals older than this are deleted at startup. `0` keeps the default; set a huge value to effectively disable. |
| `TEPLOY_DASH_MAX_OPERATIONS` | `5000` | Maximum retained operation records (oldest finished operations are removed first, live ones are never removed). |
| `TEPLOY_DASH_MAX_QUEUED_PER_TARGET` | `50` | Per-target admission budget: how many non-finished operations may be queued for one server+app before further enqueues are rejected with HTTP 429 (never silently dropped). Idempotent replays of already-queued work still pass. |
| `TEPLOY_DASH_MAX_LIVE_OPERATIONS` | `500` | Manager-wide budget: total non-finished operations across ALL targets before further enqueues are rejected with HTTP 429. |
| `TEPLOY_DASH_MAX_CONCURRENT_OPERATIONS` | `8` | How many operations may run their CLI subprocess at the same time, across every target. Queued work waits for a slot. |
| `TEPLOY_DASH_IDEMPOTENCY_WINDOW` | `24h` | How long an admitted `Idempotency-Key` is honored, measured from the operation's admission time. Keys are namespaced per principal: two users (or MCP tokens) using the same key enqueue independent operations; the same principal's replay or conflicting reuse resolves within this window. Past the window the key is reusable for new work, including after a restart. |
| `TEPLOY_DASH_UNSAFE_LEGACY_SECRET_ARGV` | _(off)_ | Explicit opt-in for CLIs without the secret-stdin contract: without it, secret-bearing `env set` / `kv set` / template-variable writes are REFUSED (502 with upgrade guidance) instead of silently putting the value on the process list. Empty values keep working either way. |
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
are matched (admin > editor > viewer); otherwise the default role. Every SSO
sign-in is recorded in `users.json` as a principal keyed by
`oidc:<issuer-hash>:<sub>` — the issuer-namespaced subject, not the display
name. Roles stay IdP-authoritative (refreshed on every sign-in, read live by
active sessions), and an administrator can list (`GET /api/sso`) or revoke
(`POST /api/sso/revoke`) a principal's sessions without touching the IdP;
revocation forces a fresh sign-in. Both surfaces are in the UI:
Settings > SSO lists the identities (with last sign-in) and
Settings > Users has a per-account Revoke sessions button.

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
| GET | `/healthz` | Liveness probe, dependency-free (auth-exempt). Restarting cannot help a broken dependency, so this stays green through store outages. |
| GET | `/readyz` | Readiness probe (auth-exempt): `ready` (200) / `degraded` (200 — serving with impaired persistence or operation service) / `unavailable` (503 — store unreachable or auth-store outage). Use for load-balancer health checks. |
| GET | `/status`, `/api/status` | Public status page + JSON (auth-exempt; 404 unless `--public-status`). Exposes only name/up-down/24h-uptime. |
| GET | `/api/cli/status` | Whether the `teploy` CLI is on `$PATH` and its version. |
| GET | `/api/apps` | Fleet app list across all configured servers. |
| GET | `/api/fleet` | Per-server observation envelopes: every configured server on every response, each with a stable ID, freshness (`fresh`/`stale`/`unknown`, threshold 2m), collection + last-success timestamps, partial error, and last-known apps. Unreachable servers stay visible. |
| GET | `/api/apps/{server}/{app}/status` | Single app status. |
| POST | `/api/apps/{server}/{app}/{action}` | `stop`, `start`, `restart`, `rollback`, `lock`, `unlock`, `maintenance/on`, `maintenance/off`. |
| GET / POST | `/api/apps/{server}/{app}/env` | List (with values — requires the `reveal.secrets` capability) / set env vars. |
| GET | `/api/apps/{server}/{app}/env/keys` | Env var NAMES only (metadata; viewer-visible). |
| DELETE | `/api/apps/{server}/{app}/env/{key}` | Unset env var. |
| GET | `/api/apps/{server}/{app}/log` | Recent CLI deploy log. |
| GET | `/api/apps/{server}/{app}/drift` | Live containers vs the deployed version. |
| GET | `/api/apps/{server}/{app}/stats` | Per-container CPU / memory / IO. |
| GET | `/api/apps/{server}/{app}/health` | On-demand health probe against the running app. |
| GET / POST / DELETE | `/api/apps/{server}/{app}/kv` | List keys (`?pattern=`) / set (`{key,value,ttl}`) / delete (`?key=`) in the shared Nucleus KV store. `?accessory=` defaults to `nucleus`. |
| GET | `/api/apps/{server}/{app}/kv/value` | Read one value (`?key=`; requires `reveal.secrets`). Returns `exists:false` for an unset key. |
| GET | `/api/apps/{server}/{app}/accessories` | List accessories (DBs, queues, etc). |
| GET | `/api/apps/{server}/{app}/db-actions` | D05 database-action inventory: restart / version-upgrade / credential-rotation / data-restore / destructive-removal, each with `supported` (from dash's server-state mode), blast radius, and — for the unsupported classes — the exact remedy. The dashboard does not wire unsupported actions to closest-match commands. |
| GET | `/api/logs/{server}/{app}` | Live log stream (SSE; `?process=`, `?lines=`). Same-origin only. |
| GET / POST / DELETE | `/api/config/servers` `/api/config/servers/{name}` | Manage servers via CLI. |
| GET / POST | `/api/registries` | List / login to image registries. |
| DELETE | `/api/registries/{server}` | Logout. |
| GET | `/api/templates` | App catalog, validated and enriched per the D05 reviewed-package shape: entries carry `version_state` (`versioned`/`unversioned` — today's catalog is unversioned and says so), and when dash has recorded an install, `installed` (`server`, `version`, `operation_id`) plus `upgrade` (`from`, `to`, `notes`, `backup_scope`) when the catalog version advanced past it. A catalog that fails validation is a 502, not a shorter list. |
| POST | `/api/templates/install` | Install a template app. Optional `template_version` pins the install to the selected catalog version; dash re-verifies the pin against the catalog at submit and answers 409 (with the current version and upgrade pointer) if it moved. |
| GET / POST | `/api/groups` | List / create groups. |
| Various | `/api/groups/{name}/...` | Assign apps and projects, rename, delete. |
| GET / POST | `/api/onboarding/preflight` | Per-server onboarding readiness envelope (`?server=<name|candidate-host>`, POST body `{"server": ...}`): checks for teploy CLI presence + version + machine interface, SSH reachability, host read, Docker, disk headroom, and Caddy state — each with `result` (`pass`/`fail`/`unknown`), `severity` (`blocking`/`warning`), detail, and an actionable remediation hint. Unknown server names and failed discovery answer a visible error envelope, never a dropped body. |
| GET | `/api/onboarding/entry` | Create-entry gate state (`?server=<name>`): `{gated, reason, preflight}` — `gated` when no server is selected or any blocking check fails. Additive: `/api/deploy` keeps its direct contract. |
| GET / POST | `/api/monitors` | List with 24h stats / create. |
| GET / DELETE | `/api/monitors/{id}` | Detail + history / delete. |
| POST | `/api/monitors/{id}/test` | Run a check immediately. |
| GET / POST | `/api/restore-tests` | List / create scheduled backup verifications. |
| GET / DELETE | `/api/restore-tests/{id}` | Detail / delete. |
| POST | `/api/restore-tests/{id}/run` | Verify the latest backup now (restores into a scratch container via `teploy accessory verify-backup`). Returns HTTP 409 when a run for this test is already in flight — retry when it completes. |
| GET / POST | `/api/notifications` | Read / write alert config. |
| GET / POST | `/api/sources` | List / register git sources (D04): `{forge, clone_url, default_branch?, credential_ref?, display_name?}` — `forge` is `github`/`forgejo`/`gitea`/`gitlab`/`generic`; the clone URL is canonicalized (credentials stripped, scp/ssh forms folded onto https, `.git` dropped) and identity is forge + URL, so same-named repos on two forges never collide. Create returns the webhook secret exactly once. |
| GET / PATCH / DELETE | `/api/sources/{id}` | Detail (with recent webhook deliveries and their dispositions) / update display name, default branch, credential reference / delete. Identity fields are immutable. |
| POST | `/api/sources/{id}/verify` | Run the credential verifier; a failure marks the source degraded with the exact reason (visible everywhere, deliveries are then recorded-and-refused, never silently dropped). Answers 501 when no verifier is configured. |
| POST | `/api/sources/{id}/rotate-secret` | Replace the webhook secret; returns the new value exactly once. |
| POST | `/hooks/sources/{id}` | Inbound forge webhook (no session — the signature IS the auth: `X-Hub-Signature-256` HMAC-SHA256, or `X-Gitlab-Token`). Authenticated pushes to the watched default branch that carry a pinnable commit are recorded in a durable per-source ledger (dedupe by delivery id) and admitted onto the operation queue as `git-managed` manifest applies carrying the source id and the authenticated commit; a newer delivery supersedes still-queued work from the same source. Duplicate deliveries answer `{"status":"duplicate"}`; pings/tags/deletions/unwatched branches are recorded-and-ignored. |
| GET | `/api/sso` | List SSO principals (admin). |
| POST | `/api/sso/revoke` | Revoke all sessions of one SSO principal `{subject}` (admin). |
| POST | `/api/users/{username}/revoke-sessions` | Revoke all sessions of one local account (admin). |
| POST | `/api/users/{username}/narrow-to-preset` | Replace a legacy-profile account's pre-matrix permissions with its role's preset (admin). Takes effect on live sessions immediately. |
| PUT | `/api/users/{username}` | Change role `{"role"}` or set an explicit capability list `{"capabilities": [...]}` (admin; one per request). |
| GET / PUT | `/api/homepage` | Service links (Home grid + pinned header icons). PUT requires an `If-Match` header carrying the `ETag` the GET returned; a stale token answers 412 and a missing one 428. |

All non-health routes require a valid session cookie. Sessions are issued by
`POST /api/login` (24-hour TTL). Failed login attempts are rate-limited
per source IP.

### Access control: roles and capabilities

Authorization is a capability matrix. Routes require capabilities; roles are
presets over them. A request missing its capability gets `403` with the
capability named (`{"error":"forbidden: this action requires the
reveal.secrets capability"}`).

| Capability | Grants |
|------------|--------|
| `view.metadata` | Non-secret reads: fleet/status/drift/stats/health, env and KV key listings, operations, monitors. |
| `reveal.secrets` | Reading env and KV **values**. The one deliberately-not-viewer read. |
| `execute.deploy` | Deploy, rollback, container lifecycle, maintenance, remove, lock/unlock, template install, operation cancel/retry. |
| `execute.mutate` | env/KV writes and dashboard config mutations (monitors, groups, homepage, manifests). |
| `restore.data` | Restore-test create/edit/delete/run (destructive against backup data). |
| `administer.credentials` | MCP tokens, server config, image registries, notification channels, git sources. |
| `administer.users` | Accounts and SSO principals. |
| `view.logs` | Container/service logs and replay (deliberate access per the audit trail policy). |

Role presets: **viewer** = `view.metadata`; **editor** (operator) = viewer +
`execute.deploy` + `execute.mutate` + `view.logs`; **admin** = everything.
Note new viewers do NOT receive env/KV secret contents — the UI shows names
only; values need `reveal.secrets`.

**Legacy accounts.** Accounts created before the capability matrix keep their
exact previous permissions under a `legacy` profile (viewers keep value
reads, editors keep restore runs) — never silently narrowed, never silently
widened. Settings → Users lists every account with its profile and effective
capabilities; **Narrow to preset** is the explicit act that moves an account
onto its role's preset (effective on live sessions immediately). New accounts
start on presets. `PUT /api/users/{u}` with an explicit `capabilities` list
stores a custom set (empty = locked account).

**MCP tokens.** Tokens minted after the matrix carry an explicit capability
set: the default is the operator preset minus secrets (`view.metadata`,
`execute.deploy`, `execute.mutate`, `view.logs` — no MCP tool returns secret
values); read-only tokens get `view.metadata`. `POST /api/mcp-tokens`
accepts an explicit `capabilities` array. A tool is listed and callable only
when the token holds its capability; refusals name it. Tokens minted before
the matrix keep their read-only-derived behavior exactly.

## Architecture

```
Browser
   |
   v
teploy-dash (Go, ~17MB)  --- session-cookie auth middleware
   |                         embedded SPA (Alpine.js)
   |                         60s fleet cache                         SSE log streamer
   |
   +--> reads CLI state files at /deployments/{app}/state (key=value)
   +--> shells out to `teploy` for actions (deploy, rollback, env, ...)
   +--> SSH to fleet servers for stop / start / restart
   +--> uptime checks --(pgwire)--> Nucleus
                       \-(disk)----> JSONL files
```

## License

FSL-1.1-MIT (Functional Source License) — any use is permitted except
offering a competing product, and each version automatically becomes MIT two
years after its release. See `LICENSE`. The embedded `frontend/js/alpine.js`
is Alpine.js, MIT-licensed and used unmodified.
