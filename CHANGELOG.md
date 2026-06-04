# Changelog

All notable changes to teploy-dash are recorded here.

## [Unreleased]

### Security
- Closed a path-traversal / arbitrary-file-write RCE: a client-controlled monitor ID flowed into `filepath.Join` in the file store, so `POST /api/monitors` with an id like `../../etc/cron.d/x` wrote a file as root. Monitor IDs are now validated (`^[A-Za-z0-9_-]+$`) at the HTTP boundary and in every file-store method.
- Adding/editing a server in the dashboard no longer silently downgrades a non-root fleet server to root — the SSH user/role are forwarded to the CLI and preserved on edit.
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
