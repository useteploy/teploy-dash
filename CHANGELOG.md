# Changelog

All notable changes to teploy-dash are recorded here.

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
