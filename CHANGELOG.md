# Changelog

All notable changes to teploy-dash are recorded here.

## [Unreleased]

### Fixed
- App actions (lock, unlock, maintenance on/off, status, deploy log, accessory list/stop/start/logs) and the env editor now pass `--app <name>` and `--user` to the `teploy` CLI. Previously the delegate wrappers and the direct `cli.Run` calls in the server passed only `--host`, so the CLI fell back to loading a `teploy.yml` from its working directory — which dash doesn't have — and the action either failed or targeted the wrong app; and without `--user`, actions only worked on root-SSH servers. Now the server's configured SSH user is threaded through and the target app is resolved from server state. Requires teploy-cli ≥ the build that added `--app` to these commands. (The `env` wrappers also gained `--app`; the `logs` wrapper now uses the correct `--tail` flag instead of the nonexistent `--lines`.) Accessory `upgrade/backup/restore` are rejected with a clear message from the dashboard — they need `teploy.yml` on the CLI host.
- `internal/state` was reading `state.json` with JSON parsing, but the teploy CLI writes `state` (no extension) in key=value format. The mismatch silently returned an empty app list from the local-state fallback path in `server.collectFleetApps` (taken when no servers are configured). Rewritten to read the actual filename + format. Includes table-test coverage for the parser + ListApps directory walk.

### Docs
- README: removed stale Scoop install section (Windows + scoops were dropped from goreleaser in v0.1.1 — the install command was broken). Replaced with Docker / GHCR multi-arch instructions for the image we DO publish.
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
