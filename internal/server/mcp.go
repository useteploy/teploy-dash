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
	"github.com/useteploy/teploy-dash/internal/remote"
)

// MCP integration. The endpoint lives at POST /api/mcp with its own bearer
// auth (exempt from the session gate); token management lives at
// /api/mcp-tokens behind the normal session auth.
//
// Sync-safety invariant: every method of mcpBackend either READS through the
// dashboard's existing state paths (fleet collection over server state files,
// monitor store) or MUTATES through the existing teploy-CLI delegate / SSH
// ops — the identical calls the UI buttons make. MCP stores nothing about
// deployments, so a fourth client (terminal, UI, webhooks, MCP) joins the
// same single source of truth instead of adding a second one.

// mcpBackend adapts Server to the mcp.Backend interface.
type mcpBackend struct{ s *Server }

func jsonText(v interface{}) (string, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (b mcpBackend) ListApps(ctx context.Context) (string, error) {
	if apps, ok := b.s.fleet.get(); ok {
		return jsonText(apps)
	}
	apps, err := b.s.collectFleetApps(ctx)
	if err != nil {
		return "", err
	}
	b.s.fleet.set(apps)
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
	result, err := cli.Logs(server, b.s.serverUser(server), app, lines)
	if err != nil {
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
	for _, srv := range b.s.resolveServers() {
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
	raw, err := cli.EnvList(server, b.s.serverUser(server), app)
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
	if !cli.IsInstalled() {
		return "", fmt.Errorf("teploy CLI not installed on the dash host")
	}
	result, err := cli.Deploy(server, b.s.serverUser(server), app, image, domain, port)
	if err != nil {
		return "", err
	}
	b.s.fleet.set(nil)
	return result.Stdout, nil
}

func (b mcpBackend) Rollback(ctx context.Context, server, app string) (string, error) {
	if !cli.IsInstalled() {
		return "", fmt.Errorf("teploy CLI not installed on the dash host")
	}
	result, err := cli.Rollback(server, b.s.serverUser(server), app)
	if err != nil {
		return "", err
	}
	b.s.fleet.set(nil)
	return result.Stdout, nil
}

func (b mcpBackend) AppAction(ctx context.Context, server, app, action string) (string, error) {
	switch action {
	// Container lifecycle goes over direct SSH — the same path the UI
	// buttons use for these three.
	case "stop", "start", "restart":
		srv, ok := b.s.lookupServer(server)
		if !ok {
			return "", fmt.Errorf("server not found: %s", server)
		}
		var err error
		switch action {
		case "stop":
			err = remote.StopApp(ctx, srv, app)
		case "start":
			err = remote.StartApp(ctx, srv, app)
		case "restart":
			err = remote.RestartApp(ctx, srv, app)
		}
		if err != nil {
			return "", err
		}
		b.s.fleet.set(nil)
		return fmt.Sprintf("%s: ok", action), nil
	default:
		// Everything else (lock, unlock, maintenance on/off) delegates to
		// the CLI, exactly like the UI.
		if !cli.IsInstalled() {
			return "", fmt.Errorf("teploy CLI not installed on the dash host")
		}
		result, err := b.s.cliAppRun(server, app, strings.Fields(action)...)
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
	if _, err := cli.EnvSet(server, b.s.serverUser(server), app, key, value); err != nil {
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
	if _, err := cli.EnvUnset(server, b.s.serverUser(server), app, key); err != nil {
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
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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
