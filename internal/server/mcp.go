package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/mcp"
	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/remote"
)

// MCP integration. The endpoint lives at POST /api/mcp with its own bearer
// auth (exempt from the session gate); token management lives at
// /api/mcp-tokens behind the normal session auth.
//
// Sync-safety invariant: every method of mcpBackend either READS through the
// dashboard's existing state paths (fleet collection over server state files,
// monitor store) or MUTATES through the operation service — the identical
// journal, validation, queue, cancellation, and idempotency the UI's buttons
// use (A19). MCP stores nothing about deployments, so a fourth client
// (terminal, UI, webhooks, MCP) joins the same single source of truth.

// mcpBackend adapts Server to the mcp.Backend interface.
type mcpBackend struct{ s *Server }

func jsonText(v interface{}) (string, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// resolveServer requires a REGISTERED server name and returns its connection
// details. Passing an alias where the CLI expects a raw --host (or an unknown
// name) produced wrong-target or confusing failures (A19).
func (b mcpBackend) resolveServer(name string) (remote.ServerConn, error) {
	srv, ok := b.s.lookupServer(name)
	if !ok {
		return remote.ServerConn{}, fmt.Errorf("server not found: %s (see teploy_list_servers)", name)
	}
	return srv, nil
}

// enqueueMutation routes an MCP mutation through the operation service so it
// shares the UI's validation, journal, queue, cancellation, and idempotency
// instead of calling the CLI directly (A19). The verified token rides the
// context (WithToken) and becomes the operation's actor (A27).
func (b mcpBackend) enqueueMutation(ctx context.Context, req operation.Request) (string, error) {
	if b.s.operations == nil {
		return "", fmt.Errorf("operation service unavailable")
	}
	var actor *operation.Actor
	if tok, ok := mcp.TokenFromContext(ctx); ok {
		actor = &operation.Actor{Kind: "mcp", Subject: "mcp-token/" + tok.ID, Label: tok.Name}
	}
	op, _, err := b.s.operations.Enqueue(req, "", actor)
	if err != nil {
		return "", err
	}
	out, err := jsonText(op)
	if err != nil {
		return "", err
	}
	return out, nil
}

func (b mcpBackend) ListApps(ctx context.Context) (string, error) {
	if apps, ok := b.s.fleet.get(); ok {
		return jsonText(apps)
	}
	apps, err := b.s.collectFleetApps(ctx)
	if err != nil {
		return "", err
	}
	b.s.fleet.publish(b.s.fleet.snapshotGeneration(), apps)
	return jsonText(apps)
}

func (b mcpBackend) GetApp(ctx context.Context, server, app string) (string, error) {
	apps, err := b.s.collectFleetApps(ctx)
	if err != nil {
		return "", err
	}
	for _, a := range apps {
		if a.Server == server && a.App == app {
			return jsonText(a)
		}
	}
	return "", fmt.Errorf("app %q not found on server %q", app, server)
}

func (b mcpBackend) AppLogs(ctx context.Context, server, app string, lines int) (string, error) {
	if !cli.IsInstalled() {
		return "", fmt.Errorf("teploy CLI not installed on the dash host")
	}
	srv, err := b.resolveServer(server)
	if err != nil {
		return "", err
	}
	result, err := cli.Logs(srv.Host, srv.User, app, lines)
	if err != nil {
		return "", err
	}
	// F055: Logs runs the plain delegate, where a NON-ZERO EXIT is carried
	// in Result.ExitCode with a nil Go error. Without checking it, an SSH
	// failure or missing app returned its stderr as a SUCCESSFUL log
	// payload — misleading for agents and users alike.
	if err := cli.CheckExit(result); err != nil {
		return "", err
	}
	out := result.Stdout
	if strings.TrimSpace(out) == "" {
		out = result.Stderr
	}
	if strings.TrimSpace(out) == "" {
		out = "(no log output)"
	}
	return out, nil
}

func (b mcpBackend) ListServers(ctx context.Context) (string, error) {
	type serverInfo struct {
		Name string `json:"name"`
		Host string `json:"host"`
	}
	var out []serverInfo
	for _, srv := range b.s.serversBestEffort() {
		out = append(out, serverInfo{Name: srv.Name, Host: srv.Host})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return jsonText(out)
}

func (b mcpBackend) ListMonitors(ctx context.Context) (string, error) {
	if b.s.store == nil {
		return "[]", nil
	}
	monitors, err := b.s.store.ListMonitors()
	if err != nil {
		return "", err
	}
	type monitorInfo struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		Type      string  `json:"type"`
		Enabled   bool    `json:"enabled"`
		Uptime24h float64 `json:"uptime_24h_percent"`
	}
	var out []monitorInfo
	since := time.Now().Add(-24 * time.Hour)
	for _, m := range monitors {
		mi := monitorInfo{ID: m.ID, Name: m.Name, Type: m.Type, Enabled: m.Enabled}
		if stats, err := b.s.store.GetStats(m.ID, since); err == nil && stats != nil {
			mi.Uptime24h = stats.UptimePercent
		}
		out = append(out, mi)
	}
	return jsonText(out)
}

func (b mcpBackend) ListEnvKeys(ctx context.Context, server, app string) (string, error) {
	if !cli.IsInstalled() {
		return "", fmt.Errorf("teploy CLI not installed on the dash host")
	}
	srv, err := b.resolveServer(server)
	if err != nil {
		return "", err
	}
	raw, err := cli.EnvList(srv.Host, srv.User, app)
	if err != nil {
		return "", err
	}
	// Values must never cross the MCP boundary — reduce whatever shape the
	// CLI returned to a sorted list of names.
	keys := envKeysOnly(raw)
	return jsonText(keys)
}

// envKeysOnly extracts variable names from the CLI's env-list JSON, which may
// be an object keyed by name or an array of {key|name: ...} records.
func envKeysOnly(raw interface{}) []string {
	var keys []string
	switch v := raw.(type) {
	case map[string]interface{}:
		for k := range v {
			keys = append(keys, k)
		}
	case []interface{}:
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if k, ok := m["key"].(string); ok && k != "" {
					keys = append(keys, k)
					continue
				}
				if k, ok := m["name"].(string); ok && k != "" {
					keys = append(keys, k)
				}
			}
		}
	}
	sort.Strings(keys)
	if keys == nil {
		keys = []string{}
	}
	return keys
}

func (b mcpBackend) Deploy(ctx context.Context, server, app, image, domain string, port int) (string, error) {
	// Route through the operation service exactly like the UI's deploy form:
	// validated target, journaled, queued, cancelable (A19). The returned
	// payload is the queued operation, not a completed deploy.
	return b.enqueueMutation(ctx, operation.Request{
		Kind: operation.KindDeploy, Mode: "ad-hoc",
		Server: server, App: app, Image: image, Domain: domain, Port: port,
	})
}

func (b mcpBackend) Rollback(ctx context.Context, server, app string) (string, error) {
	return b.enqueueMutation(ctx, operation.Request{
		Kind: operation.KindRollback, Server: server, App: app,
	})
}

func (b mcpBackend) AppAction(ctx context.Context, server, app, action string) (string, error) {
	switch action {
	case "stop", "start", "restart":
		// Container lifecycle goes through the operation service — the same
		// path the UI buttons use (A19). The old direct-SSH mutations had no
		// journal, validation, or cancellation.
		return b.enqueueMutation(ctx, operation.Request{
			Kind: operation.KindAppLifecycle, Server: server, App: app, Action: action,
		})
	default:
		// Everything else (lock, unlock, maintenance on/off) delegates to
		// the CLI, exactly like the UI.
		if !cli.IsInstalled() {
			return "", fmt.Errorf("teploy CLI not installed on the dash host")
		}
		if _, err := b.resolveServer(server); err != nil {
			return "", err
		}
		result, err := b.s.cliAppRun(ctx, server, app, strings.Fields(action)...)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(result.Stdout) == "" {
			return fmt.Sprintf("%s: ok", action), nil
		}
		return result.Stdout, nil
	}
}

func (b mcpBackend) SetEnv(ctx context.Context, server, app, key, value string) (string, error) {
	if !cli.IsInstalled() {
		return "", fmt.Errorf("teploy CLI not installed on the dash host")
	}
	if !validEnvKey(key) {
		return "", fmt.Errorf("invalid env var name")
	}
	srv, err := b.resolveServer(server)
	if err != nil {
		return "", err
	}
	if _, err := cli.EnvSet(ctx, srv.Host, srv.User, app, key, value); err != nil {
		return "", err
	}
	return fmt.Sprintf("set %s (applies on next deploy/restart)", key), nil
}

func (b mcpBackend) UnsetEnv(ctx context.Context, server, app, key string) (string, error) {
	if !cli.IsInstalled() {
		return "", fmt.Errorf("teploy CLI not installed on the dash host")
	}
	if !validEnvKey(key) {
		return "", fmt.Errorf("invalid env var name")
	}
	srv, err := b.resolveServer(server)
	if err != nil {
		return "", err
	}
	if _, err := cli.EnvUnset(srv.Host, srv.User, app, key); err != nil {
		return "", err
	}
	return fmt.Sprintf("unset %s", key), nil
}

// initMCP wires the token store, MCP handler, and token-management routes.
// Called from route registration; a token-store failure disables MCP but
// never blocks the dashboard.
func (s *Server) initMCP(version string) {
	tokens, err := mcp.NewTokenStore(s.config.DataDir)
	if err != nil {
		log.Printf("[mcp] disabled — token store: %v", err)
		return
	}
	s.mcpTokens = tokens
	handler := mcp.NewHandler(tokens, mcp.Tools(mcpBackend{s: s}), version)

	// The MCP endpoint authenticates itself (bearer) — registered on the
	// mux like everything else, but exempted from the session gate in wrap.
	s.mux.Handle("/api/mcp", handler)
	s.mux.HandleFunc("/api/mcp-tokens", s.handleMCPTokens)
	s.mux.HandleFunc("/api/mcp-tokens/", s.handleMCPTokenDelete)
}

// handleMCPTokens lists (GET) or creates (POST) MCP tokens. Session-authed.
func (s *Server) handleMCPTokens(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if s.mcpTokens == nil {
		writeError(w, "MCP is disabled")
		return
	}
	switch r.Method {
	case "GET":
		type tokenView struct {
			ID       string    `json:"id"`
			Name     string    `json:"name"`
			ReadOnly bool      `json:"read_only"`
			Created  time.Time `json:"created_at"`
			LastUsed time.Time `json:"last_used,omitempty"`
		}
		var out []tokenView
		for _, t := range s.mcpTokens.List() {
			out = append(out, tokenView{ID: t.ID, Name: t.Name, ReadOnly: t.ReadOnly, Created: t.CreatedAt, LastUsed: t.LastUsed})
		}
		if out == nil {
			out = []tokenView{}
		}
		writeData(w, out)
	case "POST":
		var body struct {
			Name     string `json:"name"`
			ReadOnly bool   `json:"read_only"`
		}
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		plaintext, t, err := s.mcpTokens.Create(strings.TrimSpace(body.Name), body.ReadOnly)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		// The plaintext appears exactly once, in this response.
		writeData(w, map[string]interface{}{
			"id": t.ID, "name": t.Name, "read_only": t.ReadOnly, "token": plaintext,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleMCPTokenDelete revokes a token: DELETE /api/mcp-tokens/{id}
func (s *Server) handleMCPTokenDelete(w http.ResponseWriter, r *http.Request) {
	if s.mcpTokens == nil {
		writeError(w, "MCP is disabled")
		return
	}
	if r.Method != "DELETE" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/mcp-tokens/")
	if id == "" {
		writeError(w, "token id required")
		return
	}
	if err := s.mcpTokens.Delete(id); err != nil {
		writeError(w, err.Error())
		return
	}
	writeData(w, map[string]bool{"ok": true})
}
