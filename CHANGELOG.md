# Changelog

All notable changes to teploy-dash are recorded here.

## [Unreleased]

## [0.1.14] - 2026-07-28

### Changed
- The header leads with the Teploy wordmark and lets the product switcher
  carry the product name, replacing the combined "TEPLOY DASH" that showed the
  brand twice. The same shape now applies across Dash, Observe and Ship.
- The switcher always names the current product. It previously hid entirely
  when no sibling was configured, so a single-product install had nothing
  saying which dashboard it was; it becomes a dropdown only when a sibling is
  reachable.

### Fixed
- The fleet cache is warmed once at startup. Sibling discovery reads that
  cache, so after a restart the switcher was missing entries until someone
  opened the deployments page — and that first fleet view paid a full SSH
  sweep.

## [0.1.13] - 2026-07-27

### Added
- The cross-product switcher configures itself. Dash already SSH-polls the
  whole fleet, so it now infers a sibling's URL from deploy state instead of
  requiring `TEPLOY_NAV_*` for products it can already see: a running app named
  `observe`/`ship` resolves to its domain, or to the server address and
  published port when it has none (`ingress: host`). Reserved documentation
  domains (RFC 2606, e.g. the sample `observe.example.com`) are rejected rather
  than linked. `TEPLOY_NAV_*` still wins when set — needed for a product behind
  a tunnel, or on a server this dashboard does not manage — and discovery reads
  only the warm fleet cache, so `/api/nav` never triggers an SSH sweep.

## [0.1.12] - 2026-07-27

### Fixed
- The Docker image could not build. An unused indirect dependency had drifted
  up to a version requiring Go 1.25 while the builder image, CI, and `go.mod`
  all target 1.23/1.24, so `go mod download` — which resolves the whole module
  graph, not just what is imported — failed before compilation started. The
  release binaries were unaffected (they only build what is imported), which
  is why this surfaced at deploy time rather than in CI. Pinned back to a
  version the rest of the graph agrees with; every module now builds on 1.24.

## [0.1.11] - 2026-07-27

### Added
- Teams and roles. The dashboard is now multi-user with three roles matching
  teploy-observe: **admin** (manage users, servers, settings, secrets),
  **editor** (deploy, rollback, restart, env — the operator), and **viewer**
  (read-only). Accounts live in `users.json`; a pre-existing single-user
  `auth.json` migrates automatically to the first admin on upgrade, so no
  operator is locked out. Roles are enforced at the auth gate: reads need
  viewer, mutations need editor, and account/credential/config routes need
  admin — failing closed so an unclassified mutating route requires editor,
  never viewer. Login now takes a username; manage accounts in Settings →
  Users (admin only). Changing a password or role signs out only that user's
  sessions, and the last admin can't be demoted or removed. RBAC governs the
  dashboard surface — it does not replace server SSH controls.
- Single sign-on (OIDC). Dash can act as an OpenID Connect relying party:
  delegate login to your own identity provider (Okta, Azure AD/Entra, Google
  Workspace, Keycloak, Authentik — "generic OIDC") or to Teploy Platform acting
  as the IdP for Cloud. The IdP authenticates the user; Dash verifies the signed
  ID token (authorization-code flow with PKCE, state, and nonce) and maps a
  claim to the same admin/editor/viewer roles — a `teploy_role` claim wins,
  otherwise a group claim is matched to configured admin/editor/viewer groups,
  otherwise a configurable default (viewer). The role is re-read from the token
  on every login, so the IdP stays authoritative. Local username/password login
  remains as the break-glass path so a down IdP never locks everyone out. Enable
  by setting `TEPLOY_DASH_OIDC_ISSUER` and `TEPLOY_DASH_OIDC_CLIENT_ID` (see the
  README for the full variable list); when unset, SSO is simply absent.
- Cross-product dashboard switcher. A top-left dropdown lets you jump between the
  deployed Teploy dashboards — Dash, Observe, and Ship. Configure the sibling
  URLs with `TEPLOY_NAV_OBSERVE_URL` and `TEPLOY_NAV_SHIP_URL` (the same env
  convention is used by all three products); the switcher only appears once at
  least one sibling URL is set. Served from `/api/nav`.
- Resources panel on the app detail page: CPU, memory, network and block I/O
  per container, from the CLI's `stats`. Stopped containers are filtered out —
  Docker reports them as all-zero rather than omitting them, and showing those
  rows reads as "idle" when the app is actually down.

## [0.1.10] - 2026-07-26

### Added
- MCP server at `POST /api/mcp` — Claude Code, Cursor, or any MCP client can
  inspect the fleet and run deploy actions. 17 curated tools; reads come from
  the CLI's server state files, actions delegate to the CLI binary exactly
  like the dashboard buttons, so MCP joins the existing single source of
  truth (no second state store, nothing to desync). Bearer-token auth with
  per-token read-only mode; tokens managed in Settings → MCP (hashed at rest,
  immediate revocation). Env values never cross the MCP boundary — the env
  tool returns variable names only.
- Operation center. Every long-running action — deploy, rollback, remove,
  start/stop/restart, maintenance, template install — is now a recorded
  operation with live streaming logs (SSE, resumable via `Last-Event-ID`),
  cancel, retry, and an idempotency key, instead of a request that blocks
  until the CLI exits. `/operations` lists them; `/operations/{id}` follows
  one live.
- URL routing. Every view has a real path (`/deployments`, `/servers/{name}`,
  `/operations/{id}`), so views are linkable and reloadable and browser
  back/forward work.
- Drift panel on the app detail page: whether an app's live containers still
  match what was deployed — a container stopped outside teploy, or an old
  version still running.
- `Remove` action to retire an app: stops and removes its containers, removes
  its proxy route, and deletes its deploy state. Volumes and accessory data
  are always preserved from the dashboard.
- Signed monitor webhook deliveries.
- `main.version` ldflags variables now exist, so goreleaser's long-standing
  `-X main.version=...` flags actually take effect (previously a silent
  no-op).

### Fixed
- The Deployments view rendered empty for every app: the fleet API serialized
  each app as `app` while the frontend keyed on `name`, so the list crashed on
  undefined keys and showed nothing.
- The Servers page rendered blank, and server detail was a black page — the
  server list is a map, not an array, and the detail endpoint delegated to a
  CLI verb that reports app state rather than host metrics.
- Static-site deploys (no container by design) were reported as stopped.
- CLI-delegated actions (logs, deploys, env, rollback) failed from the
  container: the server alias was passed where a raw host was expected, and
  accepted host keys were never recorded for the bundled CLI to reuse.
- Replaced inline spinners with a top-loading bar in the header.

## [0.1.9] - 2026-07-15

### Added
- Public, unauthenticated status page (opt-in via `--public-status` / `TEPLOY_DASH_PUBLIC_STATUS`; off by default, `/status` and `/api/status` 404 when disabled). Deliberately leaks only monitor name, up/down, and 24h uptime % — no target/IP, server name, response body, or config. `/api/status` is JSON; `/status` is a self-contained, theme-aware HTML page (inline CSS/JS, no external deps) that polls it every 30s and shows an overall operational/degraded/down banner.
- Restore Tests: scheduled proof that backups actually restore. A test picks a server/app/accessory plus an S3 bucket and an interval; the runner shells out to the CLI's `verify-backup` verb (server-state driven, no `teploy.yml` needed) on that schedule and persists only the last structured result. Runs hourly/daily, not on the 10s poll cadence — a test that already ran doesn't re-run on a dash restart. Fail/recovery alerts ride the existing webhook/SMTP dispatcher. New API (`/api/restore-tests` list/upsert, `/{id}` get/delete, `/{id}/run`) and a Restore Tests UI page matching the monitors pattern.

### Fixed
- Bundled `teploy` CLI in the Docker image bumped to v0.1.20 (from v0.1.17, itself a fix for a build that wasn't statically linked and couldn't run in the Alpine-based image at all). Brings every delegate-to-CLI action several releases of fixes forward, plus the new CLI features (OpenBao secrets, plan/drift/heal, kv, staged rollout, and more) within reach of the dashboard's delegate model.

## [0.1.8] - 2026-06-08

### Security
- Closed a path-traversal / arbitrary-file-write RCE: a client-controlled monitor ID flowed into `filepath.Join` in the file store, so `POST /api/monitors` with an id like `../../etc/cron.d/x` wrote a file as root. Monitor IDs are now validated (`^[A-Za-z0-9_-]+$`) at the HTTP boundary and in every file-store method.
- Adding/editing a server in the dashboard no longer silently downgrades a non-root fleet server to root — the SSH user/role are forwarded to the CLI and preserved on edit. A same-name edit upserts in place (no remove+add) so the server's `tags`/`vpn_ip` survive. (Requires teploy-cli with lowercase JSON keys on `server list --json` so the frontend reads the real user — shipped alongside.)
- The per-IP auth backoff honors `X-Forwarded-For` only from a configured `TEPLOY_DASH_TRUSTED_PROXY`, so running behind Caddy keys the lockout on the real client instead of self-DoSing every client onto the proxy IP; the failed-attempt map also prunes expired entries.
- Hardened the auth layer: per-source-IP failed-attempt backoff (brute-force resistance), a same-origin requirement on state-changing requests (CSRF defense — browsers auto-send Basic-Auth creds cross-origin), an Origin check on the WS/SSE log stream, and the registry password is now passed to the CLI over stdin instead of on the argv.
- Monitor HTTP method is constrained to GET/HEAD/POST so a monitor can't be configured to issue a destructive verb. (A private-range SSRF block was deliberately not added — monitoring internal/Tailscale fleet hosts is the primary use case and monitors are admin-only, so a default block would break legitimate monitoring.)

### Fixed
- Nucleus check inserts no longer collide: the primary key was `time.Now().UnixNano()`, which dropped a check (PK violation) when two landed in the same nanosecond. Now a random int64 (no schema migration).
- Monitor lifecycle no longer leaks: deleting a monitor now stops its checker (ticker + goroutine were leaking and kept recording checks for a deleted monitor); `startMonitor` is idempotent (tears down any existing checker first); and the last-known status is seeded from the most recent persisted check so the first check after a restart can fire a transition alert.
- CLI delegate and Nucleus calls are now time-bounded (a 20m ceiling on the CLI subprocess, 10s per Nucleus query) so a hung SSH session or wedged DB can't block a request forever.
- Fleet failures are surfaced in the logs instead of being silently swallowed — a server-list failure or a fully-unreachable fleet previously rendered as an empty success with no clue why.
- File-store check history is returned newest-first, matching the Nucleus store's ordering, so the UI shows a consistent order regardless of backend.
- Response times are rendered in milliseconds — they were shown as raw `time.Duration` nanoseconds (~1e6× too large) in the monitor list, detail, history, and test-now views.
- Monitor `POST` now validates id / type / target (subsumed by the ID validation above).

### Docs
- CLAUDE.md: dropped the "incident tracking" overclaim (not implemented); main.go cleanup comment references `store.RetentionDays` rather than a stale "30 days".

### Fixed (earlier this session)
- Failed delegated CLI actions are now reported as failures in the dashboard. `internal/cli.Run` returns a nil Go error when the `teploy` binary runs but exits non-zero, and the frontend only treats a top-level `error` field as a failure — so a failed deploy, rollback, env set/unset, lock/unlock, maintenance toggle, template install, accessory action, or server/registry add/remove was being shown to the user as success. Mutating commands now go through `cli.RunChecked`, which turns a non-zero exit (with the CLI's stderr) into a Go error so failures flow through the handlers' normal error path.
- Webhook alert delivery now uses a 10s HTTP client timeout. Each alert is sent in its own goroutine via the default (timeout-less) client, so a hanging webhook endpoint leaked a goroutine on every monitor state transition.
- Email alerts strip CR/LF from the monitor name and status before placing them in the `Subject` header, preventing SMTP header injection from a crafted monitor name.
- Uptime monitor HTTP checks now honor the configured `expected_status`. Previously the value was read and stored but never compared — the check always judged up/down purely on the 2xx/3xx range, so a monitor set to expect e.g. `401` or `201` was scored incorrectly. When `expected_status` is unset, any 2xx/3xx is still treated as up. The create-monitor form's Expected Status field is now genuinely optional (blank = lenient 2xx/3xx); it previously defaulted to `200` and always sent it, which would have made the new exact-match logic flag healthy `301`/`302`/`204` responses as down.
- Uptime monitor HTTP checks now apply the monitor's own `timeout` (via a per-request context). Previously HTTP used a hardcoded 10s client timeout and the configured per-monitor timeout was silently ignored (only TCP/ping checks respected it).
- SSH fleet connections now bound the SSH handshake with a deadline (TCP dial was already bounded). `ssh.NewClientConn` honors neither the context nor any built-in timeout, so a host that accepted TCP but stalled the handshake could hang a fleet refresh indefinitely; the multi-server refresh also now has an overall 30s cap.
- App actions (lock, unlock, maintenance on/off, status, deploy log, accessory list/stop/start/logs) and the env editor now pass `--app <name>` and `--user` to the `teploy` CLI. Previously the delegate wrappers and the direct `cli.Run` calls in the server passed only `--host`, so the CLI fell back to loading a `teploy.yml` from its working directory — which dash doesn't have — and the action either failed or targeted the wrong app; and without `--user`, actions only worked on root-SSH servers. Now the server's configured SSH user is threaded through and the target app is resolved from server state. Requires teploy-cli ≥ the build that added `--app` to these commands. (The `env` wrappers also gained `--app`; the `logs` wrapper now uses the correct `--tail` flag instead of the nonexistent `--lines`.) Accessory `upgrade/backup/restore` are rejected with a clear message from the dashboard — they need `teploy.yml` on the CLI host.
- `internal/state` was reading `state.json` with JSON parsing, but the teploy CLI writes `state` (no extension) in key=value format. The mismatch silently returned an empty app list from the local-state fallback path in `server.collectFleetApps` (taken when no servers are configured). Rewritten to read the actual filename + format. Includes table-test coverage for the parser + ListApps directory walk.

### Changed
- Consolidated the CLI state-file parser. The key=value `state` format is now parsed in one place (`internal/state.Parse`); the SSH fleet path (`internal/remote`) feeds the file bytes through it instead of re-implementing the parse, so the two paths can't drift on key names.

### Docs
- README: removed stale Scoop install section (Windows + scoops were dropped from goreleaser in v0.1.1 — the install command was broken). Replaced with Docker / GHCR multi-arch instructions for the image we DO publish.
- README: corrected the uptime monitoring section to match the code — dropped "expected body substring" (never implemented), "p50 / p95 latency" and "last incident" (stats compute uptime %, check counts, and average response time only), and fixed the file-store retention from "30 days" to the actual 7 days. Clarified that ping is a TCP-connect probe (no raw ICMP) and how expected-status matching works.
- README: monitor section now mentions ping (in addition to HTTP / TCP) — matches what `internal/monitor/monitor.go` actually supports.
- CLAUDE.md: corrected project structure (fleet aggregator lives in `internal/server/server.go`, not in a separate `internal/fleet/aggregator.go` that doesn't exist); corrected multi-server description (dash SSH-polls each server in CLI's servers.yml, NOT peer-pull between teploy-dash instances).
- `.goreleaser.yaml`: annotated the goreleaser 2.x deprecations we can't cleanly migrate yet (`brews:`, `dockers:` / `docker_manifests:` → `dockers_v2:`).

### Chore
- `go mod tidy` added missing `golang.org/x/sys v0.28.0 // indirect`.
- New: `/_internal/` and `/INTERNAL.md` gitignored at repo root for private notes (per the cross-repo `internal_notes_convention`).

## v0.1.1 — 2026-05-26

### Added
- Multi-arch Docker images on GHCR: `ghcr.io/useteploy/teploy-dash:{version,latest}` (linux/amd64 + linux/arm64), built via `Dockerfile.goreleaser` + `docker_manifests`. Binary embeds the SPA, so the runtime image is minimal.

### Removed
- Windows builds + Scoop bucket entry. teploy-dash is server-shaped tooling; Windows users were a small slice and the Scoop manifest was a maintenance tax. Linux + macOS only going forward.

## v0.1.0 — 2026-05-10

Initial public release. Ships as a single static Go binary with the
Alpine.js SPA embedded via `//go:embed`.

### Added
- Multi-server fleet view: app list, per-app status, image hashes,
  domain, container state, with a 60-second cache so the page doesn't
  SSH on every refresh.
- App actions: stop / start / restart over SSH; deploy, rollback, lock,
  unlock, maintenance on/off, registry login, env get / set / unset,
  template install — all delegated to the `teploy` CLI so the CLI stays
  the source of truth.
- WebSocket log tailing per app, with SSE fallback for clients that
  don't speak WS.
- Persistent groups + projects (`~/.teploy/groups.json`,
  format-compatible with the CLI's embedded UI).
- Umbrel-style template catalog (`/api/templates`,
  `/api/templates/install`).
- Uptime monitoring: HTTP and TCP checks with per-monitor interval,
  timeout, expected status, expected body substring; 24-hour stats
  (uptime %, p50 / p95 latency); manual "test now"; daily cleanup of
  the file-store keeps 30 days of checks.
- Storage: Nucleus over pgwire (preferred) or rolling JSONL files
  (fallback). Falls back automatically on Nucleus connect failure.
- Webhook + SMTP alerts on monitor state transitions, persisted to
  `~/.teploy/notifications.json` and reloadable from the UI without
  restart.
- HTTP Basic Auth middleware on every route except `/api/health`,
  using `subtle.ConstantTimeCompare` to prevent timing attacks.
  Refuses to start without `TEPLOY_DASH_PASSWORD` unless `--no-auth`
  is set.
- Release infra: `goreleaser` for cross-compiled tarballs (linux,
  darwin, windows × amd64, arm64), Homebrew tap, Scoop bucket, POSIX
  install script with optional systemd unit.
