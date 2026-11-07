# Teploy Dash

Self-hosted deployment dashboard for the [Teploy CLI](https://github.com/useteploy/teploy),
plus uptime monitoring — in one Go binary.

The CLI writes deployment state to `/deployments/{app}/state` (a plain
`key=value` text file). Dash
reads those files (read-only) and shells out to `teploy` for actions
(deploy, rollback, env edits, logs). Same source of truth whether you
deploy from the terminal, the UI, or a webhook — no SSH-vs-UI desync.

Optional uptime monitoring runs HTTP / TCP checks and push (heartbeat)
monitors, stores history in Nucleus or a local JSONL file, and turns
failures into debounced incidents delivered to ntfy, Slack-compatible chat,
Discord, email, a signed webhook or Teploy Ship.

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
Sign-in sessions last 30 days from last use (renewed at most once a day) and
are kept in `sessions.json` beside it — stored as SHA-256 hashes of the cookie,
never the cookie itself — so restarts and redeploys don't sign anyone out.

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
   exact stderr (`'domain' is required`). Deploying an app that already
   exists on the server (deployed via the CLI) needs no form at all — its
   page has the action buttons.
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
  `~/.teploy/groups.json`; dash is the only writer — the CLI no longer reads
  or writes it).
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
- **Fleet monitors, automatic.** Every public domain a deployed app serves
  gets an HTTP monitor on `https://<domain>/` within five minutes of Dash
  seeing it, judged up on any answer below 500 (a root that returns 401 is a
  live site; a 5xx or no answer is not). Retired when no app serves the
  domain any more, and only after a sweep that reached every server.
  Placeholder, IP-literal and wildcard domains are skipped, and no monitor is
  added for a host-ingress domain with no sign of a front (see
  [Domain health](#domain-health)).
  Disable one to silence it; deleting it brings it back on the next sweep.
- 24-hour stats (uptime %, total / up / down checks, average response time).
- Storage: Nucleus over pgwire (preferred) or rolling JSONL files
  (fallback). Daily cleanup for the file store keeps 7 days of checks.
- Manual "test now" runs a check immediately without saving.
- **Push / heartbeat monitors.** Type `push` gets a secret URL
  (`/api/push/<token>`, no login, rate-limited), shown on the monitor page
  to users who can edit monitors. A backup job or cron task requests it when
  it finishes (`?status=down&msg=...` reports a failure); the monitor goes
  down when no ping arrives within the interval + grace (default 10% of the
  interval, at least 5 minutes).
- Alerts: see [Alerting](#alerting).

### Domain health

**Domains** has one row per domain, joining four layers and naming the first
that fails in plain words: "no DNS record", "route points at a container that no
longer exists", "certificate failing for 14 days".

| Layer | Source | Cadence |
|---|---|---|
| DNS, route, certificate obtain | `teploy domains check --json`, run per server by the CLI on the dashboard host | 15 minutes |
| Certificate expiry, HTTP | the monitors' own checks | 60 seconds |

Certificate expiry comes from the monitor's TLS handshake, so it covers every
monitored domain, including ones on no Teploy server and tunnelled ones (where it
is the edge's certificate). DNS, route and obtain need teploy v0.1.51 or later on
the dashboard host (capability `domains-check-v1`); an older CLI is a note on the
page, and the monitor layers keep working. Nothing new runs on your servers.

A server whose check failed is named above the table, and its rows read
**unknown** with the age of the last good result. Unknown never opens or closes an
incident. Each row shows how old its evidence is.

Incidents use `source: "domain"`, one per domain and cause, through the same
debounce, holds, silences and retirement as monitors:

| Rule | Opens when | Severity | Debounce |
|---|---|---|---|
| DNS | no record or wrong target, on a route that serves the domain | critical | 2 checks to open, 2 to recover |
| Certificate expiry | under 14 days, or a third of the lifetime if shorter | warning; critical under 3 days, or half the warning point if shorter, which re-alerts | 1 reading |
| Certificate obtain | failing for over 24 hours | warning | 1 reading |

- A Cloudflare answer is a hint, not a failure. A host-ingress or external
  domain that nothing fronts is a label and is not checked.
- While any HTTP monitor for a domain has an open incident, the DNS and obtain
  rules and an already-expired certificate do not open a second one: the monitor's
  alert owns the outage and the page names the cause. If the cause outlives it, the
  domain rule opens then. A certificate that is only expiring still alerts.
- A rule closes on real evidence only. Unknown, or a clock reset by a recreated
  Caddy, closes nothing.
- The first results from a server are adopted: what was already broken arrives as
  one summary, not an alert per domain.
- A rule retires (`incident_recovered`, reason `retired`) when its domain goes
  away, only after a sweep that fully read the servers holding it. A dead server
  elsewhere holds up nothing.

Fleet monitors follow the same evidence: a host-ingress domain gets a monitor when
a route on any server names it, or public DNS answers. Otherwise it is
undetermined: none is added and an existing one stays, because a tunnel or another
box's proxy may be fronting it.

### Backups

**Backups** is one page for the whole fleet's protection, built on the teploy
CLI's fleet engine (v0.1.52 on the dashboard host, capability `backup-fleet-v1`;
an older CLI is a note and the page stays empty — mixed fleets keep working
because the engine runs on each server, not here).

**Protect my data** (teploy v0.1.55, capability `backup-setup-v1`) takes a new user to
verified, encrypted, off-site backups from this page, in six steps. The manual controls
stay under **Advanced**; both drive the same engine.

1. **Check your servers**, before any key is asked for: reachable, teploy version, aws,
   age, python3, free disk. Tools and old server binaries are only changed with consent.
2. **Storage.** Paste an expiring Backblaze key with listBuckets, writeBuckets and
   writeKeys. Dash reads the capabilities and S3 endpoint from it, reuses a bucket or
   creates a private `teploy-backups-<random>`, merges its lifecycle rules, and mints
   one key per server limited to the bucket and `teploy/<server>/`, without deleteFiles.
   Each key must really hide a test object before it reaches a server, or setup stops:
   retention cannot work without that. The pasted key lives only in that request.
3. **Recovery key.** Made in your browser and saved as a file, which you then pick again
   so the browser can check it. Only the public key reaches Dash.
4. **Alerts**, ntfy or email with a test send. Skippable, and it keeps reminding you.
5. **Turn on.** Shows what each server will cover, including databases teploy does not
   manage, which are backed up too unless you untick them, and storage as a multiple of
   one backup. "Back up now" or "Start with the next scheduled run".
6. **Prove recovery.** Pick the key file again: Dash reads the first 128 KiB of a real
   backup from the server, your browser decrypts it, and Dash records the result.

**Protected** means a verified backup on each covered target, a restore test passed
within 21 days for each covered database, and no database left out of a backup. Volume-only
servers need no restore test. If you chose to skip a database, the page says "Protected,
except" and names it. A server that has a target is left alone unless you tick it, because a
push replaces its target and Dash cannot put the old key back. New,
rebuilt and retired servers each have their own short action on the page.
[docs/recovery.md](docs/recovery.md) covers getting data back; it has not been rehearsed
yet.

- **First-run checklist** — storage → alert channel → recovery-key drill → first
  verified backup — shown until complete, then never again.
- **Connect storage once.** Presets for Backblaze B2 (recommended: application
  keys can be limited to a bucket *and name prefix*, so one bucket isolates every
  server) and Cloudflare R2, S3-compatible endpoints behind "advanced". Dash
  probes write/read/delete first, then pushes `server backup-target set` to every
  server with the credentials on stdin, then forgets them: the record keeps the
  bucket name and per-server prefixes only. A B2 key that cannot delete is the
  documented good setup, so long as it can still hide: the probe confirms the test object
  is hidden, and stops if the store refuses (a compromised server cannot destroy history;
  the bucket's lifecycle rules remove hidden versions after 14 days). Rotation re-prompts,
  pushes, verifies each server and tells you when to revoke the old key.
- **Recovery key.** `teploy backup keygen` output shown exactly once (re-auth
  required; the private key is stored nowhere); the public half is pushed to
  every server and every object is age-encrypted to it *and* the server's own
  identity — verify and restore run on the server, the owner key recovers
  anything. Until the **decrypt drill** passes (run `teploy backup fetch <key>
  --identity` locally, paste the JSON it prints), a persistent banner — not an
  alert — says recovery is unproven.
- **One policy** — frequency, retention presets (simple: keep 14 days; standard:
  7 daily, 4 weekly, 12 monthly), volumes with a size cap — written to every
  server through the CLI, which changes only what the flags name. Per-app exclude
  (audited, reason required) and per-app volume schedule overrides are read-
  merge-write through the same surface.
- **The fleet table** — every database and volume: engine, last good backup and
  its age, newest copy size, projected steady-state storage, encryption, restore
  test verdict, and the coverage word (covered / stale / failing / uncovered /
  adopted / excluded). A server that could not be read reads **unknown**
  everywhere, never healthy. Source: `teploy backup status --json` per server,
  read every 30 minutes into `<data>/backups.json`; page loads read the cache.
- **Automatic restore tests** — one dash-owned test per covered database, weekly,
  in a configurable off-hours window (UTC hours, default 02:00–06:00), one per
  server at a time, with a free RAM/disk precheck over SSH before launch. They
  run `accessory verify-backup` (a scratch container on the server; the running
  database is never touched) and their history is kept.
- **Alerts** (`source: "backup"`, one incident per cause): target unreachable,
  missing aws/age tooling, backups failing, stale (no success in twice the
  interval), uncovered database appeared. Missed heartbeats — a server whose
  cron stopped — alert through the push monitor Dash creates per server
  (`source: "heartbeat"`). First complete read of a server adopts what was
  already broken into one summary; recovery needs fresh evidence; a rule whose
  subject leaves a complete read is retired.
- **Retiring hand-written backup jobs** (report-only): a row reads *safe to
  remove* once Dash has watched two consecutive good policy backups arrive AND a
  restore test of the same engine kind passed. The known hand-written job names
  (akiroo-backup, teploy-backup.sh, teploy-wp-dbbackup, omni-backup,
  teploy-nucleus-backup, dash-state-backup) are listed as the owner's checklist,
  shown only when Dash finds signs of such jobs: legacy cron lines, or a policy
  something else manages. Removal itself stays a human action on the server, and
  old dumps stay until they age out. Dash cannot list a server's systemd/cron jobs
  (no CLI read for that exists yet, recorded as a gap).
- **Dash itself** is backed up as the `dash/volumes` entry when it runs from the
  deployment template. Objects encrypted to the owner key and the server open with
  the owner key alone. Dash's volumes hold its SSH key, so a policy with no recovery
  key excludes them on the server proved to host this Dash (`TEPLOY_APP` and the
  `dev.teploy.product=dash` label, through `server siblings`). If that cannot be proved,
  the page warns rather than excluding by name. Paths outside the template's volumes are
  reported as uncovered. See [`docs/recovery.md`](docs/recovery.md).

Every action is audited; connecting storage, the recovery key and every step of
"Protect my data" need `administer.credentials`, the policy and excludes
`execute.mutate`, and nothing here is exposed over MCP.
### Cleanup

**Cleanup** answers "what is left over on each server?" with the CLI's read-only
audit (`teploy server audit --json`, capability `server-audit-v1`, teploy v0.1.53
on the dashboard host): stale and dead routes, state with no containers, versions
beyond the keep policy, reclaimable build cache, dangling volumes, large writable
layers, unmanaged containers, a Caddyfile with no Caddy, a missing cron daemon and
disk pressure — each finding with its evidence, size and severity.

- An audit takes minutes over SSH, so **Run audit** is a durable operation you can
  follow; the page shows the cached report with its age while it runs, and one
  audit per server at a time. A failed run keeps the last report and names the
  failure — it never clears the page.
- Honesty first: a partially-audited server is labelled partially audited (the
  findings its unread checks would have produced are missing, not clean), an
  unreachable one says so, and a server never audited says that too.
- A **Fix** button exists only where the CLI named the command (`remedy.argv`) and
  dash can rebuild it from its own allowlist (`route remove`, `releases prune`,
  `server prune`, `remove`, `redeploy`). Everything else is report-only with its
  evidence — including remedies a newer CLI might name that this dash has not
  learned yet, which show the command to run by hand.
- Destructive fixes (removing orphaned state, pruning versions) ask for the
  finding's app name typed exactly — enforced by the server, not just the dialog.
  Nothing destructive is one click.
- After a fix finishes, the page offers the re-audit that proves the finding
  cleared.
- Findings can be **dismissed** per finding id: hidden from the default view but
  counted and reversible, recorded in the audit trail. A fresh audit that fully
  read the server and no longer reports the finding retires the dismissal; a
  partial or unreachable audit retires nothing (missing is not cleared).

### Siblings (product discovery)

**Siblings** answers "what other Teploy products run on my fleet, and can I trust
it?" with the CLI's label report (`teploy server siblings --json`, capability
`siblings-v1`, teploy v0.1.54) plus dash's own probe: each candidate's published
ports are tried for the product's discovery endpoint (`/.well-known/teploy.json`;
the forgejo family answers `/api/v1/version`), short timeout, read-only.

- Probes run on the monitor network guard: tailnet/LAN addresses are probed only
  after an admin allows internal probing (the same rule as internal monitors —
  an editor cannot flip it), and loopback-only binds read "not probeable from
  dash" instead of pretending.
- Verdicts are honest: **confirmed** (a discovery doc answered, with its product,
  version, instance id and published URLs), **found, not discoverable** (the
  product answered but ships discovery off — Ship does by default), **no
  answer**, **not probeable**, or **blocked (internal)**. A doc with no
  dashboard or ingest URL says so — dash never guesses one.
- Products the CLI does not recognise pass through and render as unrecognised
  labels, so a product newer than the CLI stays visible.
- The **nav switcher** fills only from confirmed docs' own `dashboard_url`
  (explicit `TEPLOY_NAV_*_URL` still wins, the fleet-name fallback stays); a
  confirmed forgejo joins as a plain link. The sweep runs hourly, cached in
  `<data>/siblings.json`; **Rescan now** re-reads on demand.

### Observe connect

Observe is the one sibling with a deep connect flow (the Observe side:
`docs/service-tokens.md`). **Connect Observe** on the Siblings page:

1. Mint a service token in Observe (Settings → Service Tokens; scopes
   `sites:create` and `keys:mint` at minimum). Dash never sees an Observe admin
   login.
2. **Preview the instance**: dash reads the unauthenticated discovery doc so you
   confirm the instance id *before* any token moves. Connect re-reads the doc and
   enforces the confirmed id, then verifies the token with a harmless
   authenticated read.
3. The token is stored in `~/.teploy/observe.json` (0600, atomic — the same
   secrets-file pattern as `notifications.json`); the UI ever shows only a
   fingerprint, and errors are scrubbed of the token before they are stored or
   answered. Disconnect forgets it and tells you to revoke it in Observe.

**Per-app telemetry** is opt-in, one app at a time, and always a plan first: the
plan names the site that will be ensured by `external_ref` (created if absent —
idempotent), the labelled telemetry key that will be minted, every environment
write (`OBSERVE_ENDPOINT`, `OBSERVE_API_KEY` (secret),
`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS` (secret),
`OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`, `OTEL_SERVICE_NAME`,
`OTEL_RESOURCE_ATTRIBUTES=service.version=$TEPLOY_VERSION,…`), and the redeploy
that makes them reach the container. Approving the plan sends its fingerprint;
apply recomputes the plan and refuses if anything moved. Values ride the CLI's
stdin contracts, never argv; the minted key appears in no response, record or
log. **Stop telemetry** removes exactly the recorded keys, redeploys, and says
the site and key live on in Observe until revoked there. Before every write dash
re-checks the discovery doc's instance id — a restored Observe behind the same
URL refuses until you reconnect.

### Group suggestions

The Projects page offers **suggested groups**: apps that share a registrable
domain (`publicsuffix` — the union of an app's domains, so `www.` and subdomains
fold into one), or — when they publish no domain — the first hyphen token of
their name. Singletons never appear; an app already in any group is never
suggested; dash never writes a group itself. Accepting a suggestion uses the
ordinary groups API (create, then server-bound assignment per app) — one
suggestion at a time, your click, your groups.

### Alerting

**Incidents, not flips.** A monitor opens an incident after `fail_after`
consecutive failed checks (default 3; push monitors 1) and recovers after
`recover_after` passing checks (default 2; push 1). The streak is restored
from check history after a restart. On the first start with incident
tracking (no `incidents.json`), outages that already exist are announced as
one summary, not one alert per monitor. Each incident has a stable
`incident_id`, is sent once when it opens and once when it recovers (a
recovery only if the open was sent), and optionally repeats every
`repeat_every`. Deleting or disabling a monitor, or the fleet reconciler
retiring one, closes its incident with reason `retired`. Restore tests open
on a failed run and recover on the next pass. Failed app operations
(deploy, redeploy, rollback, backup, backup restore, maintenance) open a
`source: "operation"` incident per operation kind on an app; the next
success of that kind recovers it, otherwise it closes with reason `retired`
after 24 hours so receivers never keep a stale card.

**Held, not dropped.** While a monitor's app is in maintenance, holds a
deploy lock, or has a queued/running Dash operation (and for 2 minutes
after), opens are held: failures keep counting and, if the monitor is still
failing when the hold ends, the incident opens then. An operation holds at
most 2 minutes + its timeout from its start (deploy 30 min), so a hung
deploy cannot hide an outage; a lock alone at most 32 minutes, maintenance
at most 24 hours. A held monitor that is still failing when its hold ends
opens within a minute. Maintenance
and lock flags come from the fleet cache (refreshed by page loads and the
5-minute fleet sweep). **Silences** (everything, one app, or one monitor;
1 minute to 30 days) hold the same way and close the open incidents they
cover with reason `suppressed_end`. Held alerts are recorded as
`suppressed` in the delivery history.

**Channels** (Settings > Notifications, stored in
`~/.teploy/notifications.json`, mode 0600; secrets are never returned):

| Type | Delivers |
|---|---|
| `ntfy` | POST to `<server>/<topic>` with `Title`, `Priority` (urgent for critical down, high for warning, default for recovered), plain-word `Tags`, `Click` (the public Dash URL), optional bearer token. |
| `slack` | `{"text": ...}` — Slack incoming webhooks; Mattermost, Rocket.Chat and Google Chat accept the same body. |
| `discord` | One embed, held to 2000 characters; 429 `retry_after` honored. |
| `email` | SMTP with required STARTTLS unless plaintext is allowed for a LAN relay. |
| `webhook` | The signed Event v2 JSON body (below). |
| `ship` | The signed Event v2 body to a Teploy Ship intake URL with its own secret. |

Channel and heartbeat URLs go through the same network guard as monitors:
loopback, private, Tailscale (CGNAT) and metadata addresses are refused
unless the channel or receiver allows an internal address. Delivery errors
never show a URL path, secret or token. A pending open whose incident
recovers first is marked `superseded` instead of being sent after the
recovery.

Each channel can be limited to some kinds (opened / reminders /
recovered), has **Send test** (`kind: "test"`), and shows its delivery
health; when a channel dead-letters a delivery and another channel exists,
Dash alerts through the other one. Receivers' `Retry-After` is honored.
When the **first** channel is configured, Dash sends one summary of
everything already down instead of one alert per monitor (machine channels
still receive each incident's open so they can resolve it later). A
pre-v2 file with `webhook_url` / `smtp_*` migrates to channels with ids
`webhook` and `email`; pending deliveries replay unchanged.

**Event v2 (webhook, ship).** Every v1 field keeps its meaning and
`status` is always `down` or `up`:

```json
{"monitor_id":"fleet-8f1c","monitor_name":"shop.example","status":"down","message":"unexpected status 502",
 "occurred_at":"2026-11-03T22:31:00Z","source":"monitor","delivery_id":"9c2e...","version":2,
 "kind":"incident_opened","reason":"","incident_id":"inc_3f9a...","rule_id":"fleet-8f1c","service":"shop",
 "app":"shop","server":"deploy-1","target":"https://shop.example/","severity":"critical",
 "started_at":"2026-11-03T22:28:00Z","duration_s":0,"consecutive_failures":3,
 "url":"https://dash.example.com/monitors/fleet-8f1c"}
```

`kind` is `incident_opened | incident_repeat | incident_recovered | test`;
`reason` on recovery is `""` (healthy), `retired` or `suppressed_end`;
`source` is `monitor | heartbeat | restore-test | operation | audit` (audit
= Dash's own alert-channel health). Headers: `X-Teploy-Timestamp`,
`X-Teploy-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." +
body)>` when a secret is set, `X-Teploy-Delivery: <delivery_id>`,
`X-Teploy-Event-Version: 2`. Dedupe on `X-Teploy-Delivery`, group on
`incident_id` (fallback `monitor_id`).

**Dash heartbeat.** Dash cannot report its own death, so Settings holds
one or more ping URLs that Dash GETs every N minutes (default 5, timeout
10s): healthchecks.io (hosted or self-hosted), an Uptime Kuma push monitor,
Better Stack or Cronitor heartbeats, a Teploy Observe cron check-in, or
another Dash's push monitor. Dash warns when a receiver resolves to its own
host. ntfy is not a receiver (it cannot notice silence).

Set the **public Dash URL** so alerts link back to the monitor or
operation. Every channel, heartbeat and silence change is recorded in the
audit trail (`GET /api/audit`).

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
monitor, only its **name**, current **up/down** state, and **24-hour uptime %**,
plus open monitor incidents and maintenance (app maintenance mode or an
active silence) listed by monitor name with their start / expiry times.
It deliberately never exposes monitor targets/IPs, server names, response
bodies, silence reasons, or any config. Off by default; when off, `/status` and `/api/status`
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
| `TEPLOY_DASH_MAX_LIVE_PER_PRINCIPAL` | `100` | Bound queued/running operations for one authenticated local user, OIDC subject or MCP token across all targets. Excess admission returns 429; idempotent replays still work. Negative disables this bound; anonymous work keeps the global bound. |
| `TEPLOY_DASH_IDEMPOTENCY_WINDOW` | `24h` | How long an admitted `Idempotency-Key` is honored, measured from the operation's admission time. Keys are namespaced per principal: two users (or MCP tokens) using the same key enqueue independent operations; the same principal's replay or conflicting reuse resolves within this window. Past the window the key is reusable for new work, including after a restart. |
| `TEPLOY_DASH_UNSAFE_LEGACY_SECRET_ARGV` | _(off)_ | Explicit opt-in for CLIs without the secret-stdin contract: without it, secret-bearing `env set` / `kv set` / template-variable writes are REFUSED (502 with upgrade guidance) instead of silently putting the value on the process list. Empty values keep working either way. |
| `TEPLOY_NAV_OBSERVE_URL` | _(none)_ | URL of your Teploy Observe dashboard. When set, it appears in the top-left cross-product switcher. |
| `TEPLOY_NAV_SHIP_URL` | _(none)_ | URL of your Teploy Ship dashboard. When set, it appears in the top-left cross-product switcher. |
| `TEPLOY_NAV_CLOUD_URL` | _(none)_ | URL of your Teploy Cloud dashboard (for example `https://cloud.teploy.com`). When set, it appears in the top-left cross-product switcher. |

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
| GET / POST | `/api/notifications` | Alert settings: channel list (secrets as `*_set` flags, credential URLs as hints), public Dash URL, heartbeat receivers + interval with their last ping and self-host warning. POST patches `public_url`, `heartbeat_interval_minutes` and the legacy flat `webhook_*` / `smtp_*` fields (which edit channels `webhook` / `email`). `administer.credentials`. |
| PUT / DELETE | `/api/notifications/channels/{id}` | Create or patch one channel (`type` on create only; absent secret = keep, `""` = clear) / delete. `administer.credentials`. |
| POST | `/api/notifications/channels/{id}/test` | Send a `kind: "test"` event now; answers `{ok, error}`. `administer.credentials`. |
| PUT / DELETE | `/api/notifications/heartbeats/{id}` | Save / remove a self-heartbeat receiver `{name, url}`. `administer.credentials`. |
| POST | `/api/notifications/heartbeats/{id}/test` | Ping the receiver now. `administer.credentials`. |
| GET | `/api/domains` | Domain health: one row per domain (DNS, route, certificate, HTTP, first failing layer, evidence age), per-server check state, CLI gate. `view.metadata`. |
| GET | `/api/backups` | The fleet backups view: cached per-server status (coverage rows, targets, tooling, last run), storage record (never credentials), recovery-key state + drill, the applied policy, dash-owned restore tests with history, migration readiness, projected storage. `view.metadata`. |
| POST | `/api/backups/refresh` | Ask for a new fleet status read now (the page follows the cache). `execute.mutate`. |
| POST | `/api/backups/storage` | Probe write/read/delete, then push `server backup-target set` to every server with the credentials on stdin and forget them. `{provider: b2\|r2\|custom, bucket, region/account_id/endpoint, base_prefix, access_key, secret_key, rotate?}`; a refused delete needs `delete_refusal_ok`, except on B2, where it connects with a warning, as does an accepted delete that leaves the object readable. `administer.credentials`. |
| POST | `/api/backups/setup/preflight` | Check servers: `{servers?, install_tools?, upgrade_binary?}`; the last two name servers whose tools or binary may be changed. `administer.credentials`, as is everything under `/api/backups/setup/`. |
| POST | `/api/backups/setup/storage/check` | Read a Backblaze key: `{key_id, application_key, bucket?}` returns its missing capabilities, expiry and buckets with their warnings. The key is forgotten with the request. |
| POST | `/api/backups/setup/storage/apply` | Set up a bucket and a key per named server: `{password, key_id, application_key, servers, bucket?, accept_warnings?, replace_configured?, rotate?, empty_base?, hide_days?, s3_managed?}`. Rerunning skips servers verified in the operation that pushed their key, and pushes a new key to the rest. Older keys go only after a push that succeeded and verified. |
| POST | `/api/backups/setup/owners` | Clear a rebuilt server's old marker: `{server, fingerprint, force?, password?}`. Forcing needs the password. |
| POST | `/api/backups/setup/leftovers/dismiss` | Dismiss a notice about a key Dash could not delete: `{key_id}`. |
| POST | `/api/backups/setup/recovery` | Record the public key the browser made: `{password, public_key}`. |
| POST | `/api/backups/setup/alerts/skip` | Skip alerts for now; the page keeps reminding. |
| POST | `/api/backups/setup/coverage` | What named servers would cover, with the storage multiplier: `{servers}`. |
| POST | `/api/backups/setup/turn-on` | Write the policy to named servers: `{password, servers, start: now\|next, skip?, interval?, preset?, skip_volumes?, volume_max_gb?}`. Unmanaged databases are adopted unless named in `skip` by server. What is left out keeps its existing value. |
| POST | `/api/backups/setup/run` | Back up now: `{servers}`, through a detached run. |
| POST | `/api/backups/setup/proof/start` | Read the first 128 KiB of the newest encrypted backup, as stored, for the browser to decrypt. |
| POST | `/api/backups/setup/proof/result` | Record the outcome: `{key, passed, detail?}`. |
| POST | `/api/backups/setup/retire` | Retire a server: `{password, key_id, application_key, server, data: keep\|remove, confirm?}`. |
| POST | `/api/backups/recovery-key/generate` | `teploy backup keygen` — the private key is returned exactly once and stored nowhere. Re-auth (account password) required. `administer.credentials`. |
| POST | `/api/backups/recovery-key/confirm` | "I saved it": pushes the public key as `--recipient` to every server. `administer.credentials`. |
| POST | `/api/backups/recovery-key/drill` | Record the decrypt drill: `{output}` is the JSON `teploy backup fetch --identity` printed; checked against the assigned object and its plausible plaintext size. `administer.credentials`. |
| POST | `/api/backups/policy` | The one policy to every server: `{interval, preset: simple\|standard, volumes, volume_max_gb, window_start_hour?, window_end_hour?}`. `execute.mutate`. |
| POST | `/api/backups/exclude` | Read-merge-write one server's exclude list: `{server, name, excluded, reason}` (reason required to exclude; audited). `execute.mutate`. |
| POST | `/api/backups/override` | Per-app volume schedule override or its removal: `{server, app, cron, keep_last, max_age_days, remove?}`. `execute.mutate`. |
| GET | `/api/cleanup` | Fleet cleanup overview: one row per server with its last audit's state (`ok`/`partial`/`unreachable`/`never`/`stale`), age, finding counts by severity, dismissed and remediable counts, and any live audit operation. `view.metadata`. |
| GET | `/api/cleanup/{server}` | One server's cached audit: checks (unknown ones named), disk attribution, findings with evidence, remedy verdict (`available`/`runnable`, the argv shown) and dismissal state. `view.metadata`. |
| POST | `/api/cleanup/{server}/audit` | Queue a durable `server audit` operation (one per server at a time; 409 while one is live). `execute.deploy`. |
| POST | `/api/cleanup/{server}/remedy` | Run a finding's remedy as an operation: `{id, confirm}`; `confirm` must be the finding's app or subject name exactly when the remedy is destructive. The command is rebuilt from dash's allowlist, never passed through from the envelope. `execute.deploy`. |
| POST | `/api/cleanup/{server}/dismiss` `/undismiss` | Hide or restore a finding by id (audited; a full audit that no longer reports the finding retires the dismissal). `execute.mutate`. |
| GET | `/api/siblings` | Sibling discovery: per-server read state and one probe row per (server, product) with its verdict (`confirmed` / `found_not_discoverable` / `unreachable` / `not_probeable` / `blocked_internal`), the discovery doc when confirmed, and the `allow_internal` setting. `view.metadata`. |
| POST | `/api/siblings/scan` | Start a sweep now (409 while one runs). `execute.mutate`. |
| POST | `/api/siblings/settings` | `{allow_internal}` — probe tailnet/LAN addresses. Admin session required to flip (the A13 rule); audited. `execute.mutate`. |
| GET | `/api/observe` | Observe connection state (token as fingerprint only), per-app telemetry opt-ins, and the confirmed-probe prefill hint. `administer.credentials`. |
| GET | `/api/observe/preview?url=&allow_internal=` | Read the discovery doc at an address so the instance id can be confirmed before any token moves. `administer.credentials`. |
| POST | `/api/observe/connect` | `{base_url, token, confirm_instance_id, allow_internal}` — the confirmed id must match the doc; the token is verified with a harmless read; stored 0600 in `~/.teploy/observe.json`. `administer.credentials`. |
| DELETE | `/api/observe/connection` | Forget the connection (the token stays valid in Observe until revoked there). `administer.credentials`. |
| GET | `/api/observe/telemetry/plan?server=&app=` | The exact apply plan (site ensure, key mint, every env/secret write, the redeploy) with a fingerprint. No side effects. `administer.credentials`. |
| POST | `/api/observe/telemetry` | `{server, app, fingerprint}` — apply the approved plan; a moved fingerprint refuses (409). Needs `execute.deploy` too (the redeploy). |
| POST | `/api/observe/telemetry/disable` | `{server, app}` — remove exactly the recorded env/secret keys, queue the redeploy. `administer.credentials` + `execute.deploy`. |
| GET | `/api/group-suggestions` | Grouping diff: apps that share a registrable domain (or name prefix), never grouped apps, singletons dropped. `view.metadata`. |
| GET | `/api/alerts/incidents` | Open incidents and held (suppressed) rules. `view.metadata`. |
| GET | `/api/alerts/deliveries` | Recent alert deliveries (delivered, pending, dead-lettered, suppressed) and per-channel health. `view.metadata`. |
| GET / POST | `/api/alerts/silences` | Active silences / create `{scope: all\|app\|monitor, target, reason, duration_minutes}`. POST is `execute.mutate`. |
| DELETE | `/api/alerts/silences/{id}` | End a silence. `execute.mutate`. |
| GET | `/api/audit` | Audit trail of alerting changes (channels, tests, heartbeats, silences). `administer.credentials`. |
| GET / POST / HEAD | `/api/push/{token}` | Push monitor ping (no session; the token is the credential; 12/min per token, and 30 unknown-token misses/min per address). `?status=down&msg=...` records a failure. |
| GET | `/api/monitors/{id}/push-url` | A push monitor's secret ping path (audited). `execute.mutate`; the monitor list and detail omit `push_token` for readers without it. |
| GET / POST | `/api/sources` | List / register git sources (D04): `{forge, clone_url, default_branch?, credential_ref?, display_name?}` — `forge` is `github`/`forgejo`/`gitea`/`gitlab`/`generic`; the clone URL is canonicalized (credentials stripped, scp/ssh forms folded onto https, `.git` dropped) and identity is forge + URL, so same-named repos on two forges never collide. Create returns the webhook secret exactly once. |
| GET / PATCH / DELETE | `/api/sources/{id}` | Detail (with recent webhook deliveries and their dispositions) / update display name, default branch, credential reference / delete. Identity fields are immutable. |
| POST | `/api/sources/{id}/verify` | Run the credential verifier; a failure marks the source degraded with the exact reason (visible everywhere, deliveries are then recorded-and-refused, never silently dropped). Answers 501 when no verifier is configured. |
| POST | `/api/sources/{id}/rotate-secret` | Replace the webhook secret; returns the new value exactly once. |
| POST | `/hooks/sources/{id}` | Inbound forge webhook (no session — the signature IS the auth: `X-Hub-Signature-256` HMAC-SHA256, or GitLab `X-Gitlab-Token` / `X-Gitlab-Signature` HMAC-SHA256; Gitea/Forgejo native signature headers are accepted too). Authenticated pushes to the watched default branch that carry a pinnable commit are verified against the live branch head and recorded in a durable per-source ledger (dedupe by delivery id). A verified push for a repository an app builds from **wakes that app's server-side admitter** (`teploy trigger poll --app X --host S --now --hint <sha>`; D4): dash re-verifies its repo→app routing hints against the app's config, answers the forge immediately and records the poll result in the ledger — it never decides what deploys. Pushes with no connected app are recorded as ignored with reason "no app is connected to this repository yet". Duplicate deliveries answer `{"status":"duplicate"}`; pings/tags/deletions/unwatched branches and pull request events (previews are not supported yet) are recorded-and-ignored. |
| GET | `/api/apps/{server}/{app}/source` | The app's git source state (`teploy config source show --json`): connected repository, branch, poll, credential fingerprint, watermark. Requires the CLI's `git-source-v1` token; 501 with the upgrade remedy otherwise. |
| GET | `/api/apps/{server}/{app}/source/timer` | Whether the server's git-source poll timer is installed (the doctor's `git-trigger` check, cached ~10 min per server). |
| POST | `/api/apps/{server}/{app}/source/key` | Generate the app's ed25519 deploy key on its server (`teploy config source key --type ssh --json`); the private half never leaves the server. |
| POST | `/api/apps/{server}/{app}/source/host-key` | Pin the forge's SSH host keys (`teploy config source host-key --json`); `{expected_fingerprint}` (SHA256:...) refuses a host presenting anything else. |
| POST | `/api/apps/{server}/{app}/source/register` | `{forge, repository, branch, webhook_base, public_key}` — register the deploy key and push webhook through the forge API when dash holds a credential for that repository (the operator-configured provider whose scope covers it), otherwise create the source and return the webhook URL (+ the once-only secret, to credential-admins) for the operator to paste. Widens the source's branch watch when a second app connects another branch of the same repository. |
| POST | `/api/apps/{server}/{app}/source/reset-watermark` | `teploy trigger reset` — clear a ref's watermark after a force-push. `{ref, confirm}`: the ref must be typed back exactly. |
| POST | `/api/apps/{server}/{app}/source/retry` | `teploy trigger retry` — clear a failed commit's retry budget and poll immediately. |
| POST | `/api/apps/{server}/{app}/source/install-timer` | `teploy trigger install-timer` — install the server's minutely systemd poll timer (host-scoped). |
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
| `execute.mutate` | env/KV writes and dashboard config mutations (monitors, groups, homepage). |
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

### Git source credentials and webhooks

The Sources page registers a forge repository and shows credential verdicts
and webhook deliveries. Source APIs require `administer.credentials`.
`POST /api/sources/{id}/verify` uses a production verifier. An absent or
invalid provider configuration fails closed and records a visible degraded
verdict. It does not return a successful verification without contacting the
repository provider.

Set `TEPLOY_DASH_SOURCE_PROVIDERS` to an operator-owned JSON file with mode 0600.
Each name is an exact forge/repository scope, selected by a source's
`credential_ref`. Choose exactly one credential source: `token_file` (0600),
`token_env` (environment variable name), or a GitHub App `private_key_file`
(0600 RSA PEM) with positive `app_id` and `installation_id`. Credentials never
belong in clone URLs, request bodies, command arguments or audit records.

```json
{
  "team-app": {
    "forge": "github",
    "repository": "https://github.com/team/app",
    "token_env": "TEAM_APP_READ_TOKEN"
  },
  "team-app-installation": {
    "forge": "github",
    "repository": "https://github.com/team/app",
    "app_id": 123,
    "installation_id": 456,
    "private_key_file": "/run/secrets/team-app.pem"
  }
}
```

These identifiers and paths are examples; provision your own installation and
credentials. App access tokens are freshly signed/exchanged for one repository
with `contents:read`. The installation, returned repository scope, permissions
and expiry are checked. Files are read on each use, so key/token rotation
requires no restart. Tokens are not persisted or cached.
GitHub/GitLab/Gitea/Forgejo verification checks repository identity and
content-read access through the provider API; `generic` uses authenticated
Git `ls-remote`. GitLab requires API read and repository read permissions.

`api_url` can explicitly select an enterprise/self-hosted API origin. HTTPS,
exact configured hosts, verified TLS, public IP admission, bounded responses,
15-second verification, cancellation and refusal of redirects are the defaults.
Only an operator configuration with `allow_private:true` permits private network
addresses or plain HTTP for a trusted self-hosted provider. Git also pins its
admitted DNS addresses and disables redirects, system/global Git configuration,
credential helpers and interactive prompts. Authentication uses a temporary
private askpass file. Remote error bodies and transport diagnostics never
become credential verdicts.

**Nothing deploys from a webhook yet.** The former dash-side deploy pipeline
(manifest store, webhook-triggered applies, PR previews) was removed: app
settings live on the app's server (Settings tab / `teploy config`), and
authenticated pushes are recorded in the delivery ledger only, ignored with
reason "no app is connected to this repository yet", until connecting an app
to a repository ships. Push verification still checks the forge's current
branch SHA (GitHub, GitLab, Gitea and Forgejo native branch APIs; generic
HTTPS uses authenticated Git `ls-remote`); commit timestamps are not delivery
freshness clocks. Gitea/Forgejo native event, delivery and hex SHA256-signature
headers are accepted alongside GitHub-compatible headers. GitLab accepts its
token header or `X-Gitlab-Signature` HMAC-SHA256 variant. Pull request
deliveries are recorded-and-ignored (previews are not supported yet).
