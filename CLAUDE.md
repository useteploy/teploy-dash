# teploy-dash

Self-hosted dashboard for the Teploy CLI plus uptime monitoring. One static Go binary, embedded SPA. See README.md for user-facing docs.

## Open work is tracked outside this repo

Maintainer note: everything currently open across the Teploy products lives in
the umbrella queue at `../_internal/NEXT_SESSION.md` (private, not part of this
repo). Read it before planning work here. The two planning docs in
`_internal/` are a current audit (`PARITY_AUDIT_2026-07-24.md`) and a
partly-superseded plan (`PARITY-PLAN.md`) — verify either against source.

## Quick Reference

| Item | Details |
|------|---------|
| **Type** | Standalone dashboard + uptime monitoring |
| **Stack** | Go, vanilla HTML/CSS/JS (Alpine.js), Nucleus or JSONL |
| **State** | Reads CLI state files from `/deployments/{app}/state` |
| **Actions** | Shells out to `teploy` CLI binary (delegate model) |
| **Multi-server** | SSH-polls each server in CLI's servers.yml (60s fleet cache); NOT a peer-pull from other teploy-dash instances |
| **Monitoring** | HTTP / TCP / ping checks with configurable intervals |
| **Storage** | Nucleus (pgwire, preferred) or JSONL files (fallback) |
| **Auth** | Session-cookie auth (24h TTL, bcrypt on-disk, setup mode on first run); `TEPLOY_DASH_PASSWORD` env var optional; `--no-auth` for dev |
| **Port** | 3456 (default) |

## Architecture

```
teploy-dash (single Go binary)
|
+-- Reads CLI state files (/deployments/{app}/state) over SSH per server
|     Shows: apps, versions, deploy history, container status
|     60s fleet cache so the page doesn't SSH on every refresh
|
+-- Uptime monitoring (Nucleus or JSONL)
|     HTTP / TCP / ping checks on configurable intervals
|     Status history (no incident tracking)
|     Webhook + SMTP alerts on state transitions
|
+-- Delegates actions to CLI (deploy, rollback, env, etc.)
|     shells out to `teploy` binary; the CLI stays the source of truth
|
+-- Multi-server fleet view
      SSH-polls every server in CLI's servers.yml
      (NOT peer-pull between teploy-dash instances)
```

## Why No Desync

CLI writes state to `/deployments/{app}/state`. Dash reads those files (read-only) and shells out to the CLI for every action. Whether you deploy from terminal, UI button, or CI webhook — same state files. UI never writes deployment state itself.

## Project Structure

```
teploy-dash/
├── cmd/teploy-dash/
│   ├── main.go                  entrypoint, flags, embed FS, start server
│   └── frontend/                HTML/CSS/JS (Alpine.js) — embedded via //go:embed
├── internal/
│   ├── server/
│   │   ├── server.go            HTTP server, auth middleware, all API routes,
│   │   │                        fleet cache (multi-server aggregator lives here,
│   │   │                        not in a separate fleet/ package)
│   │   └── ws.go                WebSocket log streamer
│   ├── state/reader.go          parses CLI state files
│   ├── monitor/monitor.go       HTTP / TCP / ping check runner
│   ├── store/
│   │   ├── store.go             Store interface
│   │   ├── nucleus.go           Nucleus implementation (pgwire)
│   │   └── file.go              JSONL file fallback
│   ├── alert/alert.go           webhook + SMTP alert dispatcher
│   ├── remote/remote.go         SSH-based fleet querying (ListApps per server)
│   ├── ssh/client.go            SSH client wrapper used by remote/
│   └── cli/delegate.go          shells out to `teploy` CLI
├── scripts/install.sh           POSIX installer with optional systemd unit
├── Dockerfile.goreleaser        runtime image used by goreleaser dockers block
├── .goreleaser.yaml             linux+darwin binaries + GHCR multi-arch
├── go.mod
└── CLAUDE.md
```

## Build & Run

```bash
make build                                     # builds ./teploy-dash
./teploy-dash                                  # default: port 3456, reads /deployments/
./teploy-dash --port 8080                      # custom port
./teploy-dash --nucleus-url postgresql://localhost:5432/teploy_dash  # use Nucleus
./teploy-dash --deployments /opt/deployments   # custom state dir
./teploy-dash --no-auth                        # local dev only
```

`TEPLOY_DASH_PASSWORD` is optional; if absent and no `auth.json` exists, setup mode runs on first visit.

## API

Full route list at README.md "API" section. Routes are registered in `internal/server/server.go` via `mux.HandleFunc`. All non-`/api/health` routes go through `authGate.wrap()` when auth is configured.

## Key Decisions

| Decision | Choice | Why |
|----------|--------|-----|
| State reading | Read CLI files directly via SSH | One source of truth, no desync |
| Actions | Shell out to CLI | CLI is the engine, dash is the dashboard |
| Storage | Nucleus with JSONL fallback | Dogfood Nucleus; graceful degradation if Nucleus down |
| Frontend | Vanilla + Alpine.js | No build step, same pattern as CLI's embedded UI |
| Multi-server | SSH to each server per fleet refresh | No agent on remote needed; cache for 60s |
| Auth | Session cookies + bcrypt on-disk (`auth.json`) | Setup mode on first run; change-password in Settings; env-var bootstrap for Docker |

## Where to find more

- **User-facing docs:** README.md (install, run, features, API, flags, env vars).
- **Cross-session context for Claude:** memory at `~/.claude/projects/.../memory/`. Start with `deployment_system_overview.md` for the broader Teploy ecosystem.
- **Private notes (gitignored):** `_internal/` at repo root (per [[internal_notes_convention]]).
