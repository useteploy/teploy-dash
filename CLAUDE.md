# teploy-ui

Standalone self-hosted dashboard with uptime monitoring. Reads CLI state files, delegates actions to CLI, monitors endpoints independently.

## Quick Reference

| Item | Details |
|------|---------|
| **Type** | Standalone dashboard + uptime monitoring |
| **Stack** | Go, vanilla HTML/CSS/JS (Alpine.js), Nucleus or JSONL |
| **State** | Reads CLI state files at `/deployments/` |
| **Actions** | Shells out to `teploy` CLI binary |
| **Monitoring** | HTTP/TCP/ping checks with configurable intervals |
| **Storage** | Nucleus (preferred) or JSONL files (fallback) |
| **Port** | 3456 (default) |

## Architecture

```
teploy-ui (single Go binary)
|
+-- Reads CLI state files (/deployments/*/state.json)
|     Shows: apps, versions, deploy history, container status
|
+-- Uptime monitoring (Nucleus or JSONL)
|     HTTP/TCP/ping checks on configurable intervals
|     Status history, incident tracking
|     Alerting (webhook, SMTP)
|
+-- Delegates actions to CLI
|     deploy, rollback, logs, env -> shells out to `teploy` binary
|
+-- Multi-server fleet (optional)
      HTTP API pulls status from other teploy-ui instances
```

## Why No Desync

CLI writes state to `/deployments/{app}/state.json`. UI reads those files. Whether you deploy via CLI, UI button, or webhook — same state files. One source of truth. UI never writes deployment state itself.

## Project Structure

```
teploy-ui/
├── cmd/teploy-ui/main.go       entrypoint, flags, start server
├── internal/
│   ├── server/server.go         HTTP server + API routes
│   ├── state/reader.go          reads CLI state files (read-only)
│   ├── monitor/monitor.go       uptime check runner (HTTP/TCP/ping)
│   ├── store/
│   │   ├── store.go             Store interface
│   │   ├── nucleus.go           Nucleus implementation (pgwire)
│   │   └── file.go              JSONL file fallback
│   ├── alert/alert.go           webhook + SMTP alert dispatcher
│   ├── fleet/aggregator.go      pull status from other instances
│   └── cli/delegate.go          shell out to teploy CLI
├── frontend/                    HTML/CSS/JS (Alpine.js)
├── go.mod
└── CLAUDE.md
```

## Build & Run

```bash
make build                                          # builds ./teploy-ui
./teploy-ui                                         # default: port 3456, reads /deployments/
./teploy-ui --port 8080                             # custom port
./teploy-ui --nucleus-url postgresql://localhost:5432/teploy_ui  # use Nucleus
./teploy-ui --deployments /opt/deployments          # custom state dir
```

## API

| Endpoint | Method | What |
|----------|--------|------|
| `/api/apps` | GET | list deployed apps (from CLI state) |
| `/api/apps/{name}` | GET | get app details |
| `/api/monitors` | GET | list uptime monitors with 24h stats |
| `/api/monitors` | POST | create/update a monitor |
| `/api/monitors/{id}` | GET | monitor details + check history |
| `/api/monitors/{id}` | DELETE | delete a monitor |
| `/api/cli/status` | GET | CLI installed + version |
| `/api/cli/deploy` | POST | trigger deploy via CLI |
| `/api/cli/rollback` | POST | trigger rollback via CLI |
| `/api/health` | GET | health check |

## Key Decisions

| Decision | Choice | Why |
|----------|--------|-----|
| State reading | Read CLI files directly | One source of truth, no desync |
| Actions | Shell out to CLI | CLI is the engine, UI is the dashboard |
| Storage | Nucleus with JSONL fallback | Dogfood Nucleus, graceful degradation |
| Frontend | Vanilla + Alpine.js | No build step, same pattern as CLI embedded UI |
| Multi-server | HTTP pull from other instances | No SSH needed, simple |
