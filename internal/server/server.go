package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/remote"
	"github.com/useteploy/teploy-dash/internal/state"
	"github.com/useteploy/teploy-dash/internal/store"
)

// Config holds server configuration.
type Config struct {
	Host           string
	Port           int
	DeploymentsDir string
	Monitor        *monitor.Runner
	Store          store.Store
	// AuthUser and AuthPass enable HTTP Basic Auth on all routes except
	// /api/health. If both are empty, auth is disabled (dev mode).
	AuthUser string
	AuthPass string
	// Frontend is the embedded SPA filesystem (rooted at the frontend/
	// directory: contains index.html, css/, js/). Required — the binary is
	// not portable without an embedded UI.
	Frontend fs.FS
}

// fleetCache caches aggregated multi-server app state to avoid SSH on every request.
type fleetCache struct {
	mu      sync.RWMutex
	apps    []remote.AppState
	builtAt time.Time
	ttl     time.Duration
}

func (fc *fleetCache) get() ([]remote.AppState, bool) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	if time.Since(fc.builtAt) > fc.ttl {
		return nil, false
	}
	return fc.apps, true
}

func (fc *fleetCache) set(apps []remote.AppState) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.apps = apps
	if apps == nil {
		fc.builtAt = time.Time{} // zero time forces cache miss on next read
	} else {
		fc.builtAt = time.Now()
	}
}

// Server is the teploy-dash HTTP server.
type Server struct {
	mux      *http.ServeMux
	config   Config
	state    *state.Reader
	monitor  *monitor.Runner
	store    store.Store
	fleet    *fleetCache
	frontend fs.FS
}

// New creates a new server.
func New(config Config) *Server {
	s := &Server{
		mux:      http.NewServeMux(),
		config:   config,
		state:    state.NewReader(config.DeploymentsDir),
		monitor:  config.Monitor,
		store:    config.Store,
		fleet:    &fleetCache{ttl: 60 * time.Second},
		frontend: config.Frontend,
	}
	s.routes()
	return s
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(addr string) error {
	handler := http.Handler(s.mux)
	if s.config.AuthUser != "" || s.config.AuthPass != "" {
		handler = basicAuthMiddleware(s.config.AuthUser, s.config.AuthPass, s.mux)
	}
	return http.ListenAndServe(addr, handler)
}

// basicAuthMiddleware protects all routes except /api/health with HTTP Basic
// Auth. Health is exempt so liveness probes work without credentials.
// Uses subtle.ConstantTimeCompare to prevent timing attacks.
func basicAuthMiddleware(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="teploy-dash"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	// Deployment management
	s.mux.HandleFunc("/api/servers", s.handleServers)
	s.mux.HandleFunc("/api/servers/", s.handleServerDetail)
	s.mux.HandleFunc("/api/apps", s.handleApps)
	s.mux.HandleFunc("/api/apps/", s.handleAppAction)
	s.mux.HandleFunc("/api/deploy", s.handleDeploy)
	s.mux.HandleFunc("/api/config/servers", s.handleConfigServers)
	s.mux.HandleFunc("/api/config/servers/", s.handleConfigServerAction)
	s.mux.HandleFunc("/api/notifications", s.handleNotifications)
	s.mux.HandleFunc("/api/registries", s.handleRegistries)
	s.mux.HandleFunc("/api/registries/", s.handleRegistryAction)
	s.mux.HandleFunc("/api/groups", s.handleGroups)
	s.mux.HandleFunc("/api/groups/", s.handleGroupAction)

	// Templates (Umbrel-style app catalog)
	s.mux.HandleFunc("/api/templates", s.handleTemplates)
	s.mux.HandleFunc("/api/templates/install", s.handleTemplateInstall)

	// Uptime monitors
	s.mux.HandleFunc("/api/monitors", s.handleMonitors)
	s.mux.HandleFunc("/api/monitors/", s.handleMonitor)

	// WebSocket log streaming
	s.mux.HandleFunc("/ws/logs/", s.handleLogsWS)

	// System
	s.mux.HandleFunc("/api/cli/status", s.handleCLIStatus)
	s.mux.HandleFunc("/api/health", s.handleHealth)

	// Frontend
	s.mux.HandleFunc("/", s.handleFrontend)
}

// ── Fleet App Listing ────────────────────────────────────────────────────

// handleApps returns all apps across all configured servers.
// Results are cached for 60s to avoid SSH on every page load.
func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	if apps, ok := s.fleet.get(); ok {
		writeData(w, apps)
		return
	}

	apps := s.collectFleetApps(r.Context())
	s.fleet.set(apps)
	writeData(w, apps)
}

// collectFleetApps gathers app state from all servers in parallel.
func (s *Server) collectFleetApps(ctx context.Context) []remote.AppState {
	servers := s.resolveServers()

	if len(servers) == 0 {
		// Fall back to local state files when no servers configured.
		localApps, _ := s.state.ListApps()
		var apps []remote.AppState
		for _, a := range localApps {
			apps = append(apps, remote.AppState{
				App:          a.App,
				Server:       "local",
				Domain:       a.Domain,
				CurrentHash:  a.CurrentHash,
				PreviousHash: a.PreviousHash,
				Status:       a.Status,
			})
		}
		return apps
	}

	type result struct {
		apps []remote.AppState
		err  error
	}

	ch := make(chan result, len(servers))
	for _, srv := range servers {
		srv := srv
		go func() {
			apps, err := remote.ListApps(ctx, srv)
			ch <- result{apps, err}
		}()
	}

	var all []remote.AppState
	for range servers {
		r := <-ch
		if r.err == nil {
			all = append(all, r.apps...)
		}
	}
	return all
}

// resolveServers returns server connections from the CLI's servers.yml via the CLI delegate.
func (s *Server) resolveServers() []remote.ServerConn {
	if !cli.IsInstalled() {
		return nil
	}
	result, err := cli.ServerList()
	if err != nil {
		return nil
	}

	var raw map[string]struct {
		Host string `json:"host"`
		User string `json:"user"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &raw); err != nil {
		return nil
	}

	var servers []remote.ServerConn
	for name, s := range raw {
		user := s.User
		if user == "" {
			user = "root"
		}
		servers = append(servers, remote.ServerConn{
			Name: name,
			Host: s.Host,
			User: user,
		})
	}
	return servers
}

// lookupServer finds a server connection by name.
func (s *Server) lookupServer(name string) (remote.ServerConn, bool) {
	for _, srv := range s.resolveServers() {
		if srv.Name == name {
			return srv, true
		}
	}
	return remote.ServerConn{}, false
}

// serverUser returns the configured SSH user for a server, or "" if unknown
// (the CLI then defaults to root). Threading this into delegate calls lets
// dash drive non-root fleets, not just root servers.
func (s *Server) serverUser(name string) string {
	if srv, ok := s.lookupServer(name); ok {
		return srv.User
	}
	return ""
}

// cliAppRun runs an app-scoped teploy subcommand, appending --host/--app and
// --user (when the server has a non-root user). `parts` is the subcommand plus
// any leading flags/positionals; flag order doesn't matter to cobra so trailing
// flags like --json can be passed in parts.
func (s *Server) cliAppRun(serverName, appName string, parts ...string) (*cli.Result, error) {
	args := append([]string{}, parts...)
	args = append(args, "--host", serverName, "--app", appName)
	if u := s.serverUser(serverName); u != "" {
		args = append(args, "--user", u)
	}
	return cli.Run(args...)
}

// ── App Actions ──────────────────────────────────────────────────────────

// handleAppAction handles /api/apps/{server}/{app}/{action}
// stop, start, restart use SSH direct; rollback and env use CLI delegate.
func (s *Server) handleAppAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/apps/")
	parts := strings.SplitN(path, "/", 3)

	if len(parts) < 2 {
		writeError(w, "invalid path — expected /api/apps/{server}/{app}/{action}")
		return
	}

	serverName := parts[0]
	appName := parts[1]
	action := ""
	if len(parts) >= 3 {
		action = parts[2]
	}

	switch {
	case action == "status" && r.Method == "GET":
		// Return from fleet cache if available, else fetch directly.
		if apps, ok := s.fleet.get(); ok {
			for _, a := range apps {
				if a.App == appName && a.Server == serverName {
					writeData(w, a)
					return
				}
			}
		}
		srv, ok := s.lookupServer(serverName)
		if !ok {
			writeError(w, "server not found: "+serverName)
			return
		}
		apps, err := remote.ListApps(r.Context(), srv)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		for _, a := range apps {
			if a.App == appName {
				writeData(w, a)
				return
			}
		}
		writeError(w, "app not found")

	case action == "env" && r.Method == "GET":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := cli.EnvList(serverName, s.serverUser(serverName), appName)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)

	case action == "env" && r.Method == "POST":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		var body struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		result, err := cli.EnvSet(serverName, s.serverUser(serverName), appName, body.Key, body.Value)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)

	case strings.HasPrefix(action, "env/") && r.Method == "DELETE":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		key := strings.TrimPrefix(action, "env/")
		result, err := cli.EnvUnset(serverName, s.serverUser(serverName), appName, key)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)

	case action == "log" && r.Method == "GET":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(serverName, appName, "log", "--json")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeRawJSON(w, result.Stdout)

	case action == "accessories" && r.Method == "GET":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(serverName, appName, "accessory", "list", "--json")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeRawJSON(w, result.Stdout)

	case r.Method == "POST":
		s.handleAppPost(w, r, serverName, appName, action)

	default:
		writeError(w, "not found")
	}
}

func (s *Server) handleAppPost(w http.ResponseWriter, r *http.Request, serverName, appName, action string) {
	srv, hasSrv := s.lookupServer(serverName)

	switch action {
	case "stop":
		if !hasSrv {
			writeError(w, "server not found: "+serverName)
			return
		}
		if err := remote.StopApp(r.Context(), srv, appName); err != nil {
			writeError(w, err.Error())
			return
		}
		s.fleet.set(nil) // invalidate cache
		writeData(w, map[string]bool{"ok": true})

	case "start":
		if !hasSrv {
			writeError(w, "server not found: "+serverName)
			return
		}
		if err := remote.StartApp(r.Context(), srv, appName); err != nil {
			writeError(w, err.Error())
			return
		}
		s.fleet.set(nil)
		writeData(w, map[string]bool{"ok": true})

	case "restart":
		if !hasSrv {
			writeError(w, "server not found: "+serverName)
			return
		}
		if err := remote.RestartApp(r.Context(), srv, appName); err != nil {
			writeError(w, err.Error())
			return
		}
		s.fleet.set(nil)
		writeData(w, map[string]bool{"ok": true})

	case "rollback":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(serverName, appName, "rollback")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		s.fleet.set(nil)
		writeData(w, result)

	case "lock":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(serverName, appName, "lock")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)

	case "unlock":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(serverName, appName, "unlock")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)

	case "maintenance/on":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(serverName, appName, "maintenance", "on")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)

	case "maintenance/off":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(serverName, appName, "maintenance", "off")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)

	default:
		if strings.HasPrefix(action, "accessories/") {
			if !cli.IsInstalled() {
				writeError(w, "teploy CLI not installed")
				return
			}
			accParts := strings.Split(strings.TrimPrefix(action, "accessories/"), "/")
			if len(accParts) == 2 {
				// accParts = [name, subcommand] e.g. ["postgres", "stop"].
				accName, sub := accParts[0], accParts[1]
				switch sub {
				case "stop", "start", "logs":
					// Address {app}-{accessory} containers by name — resolvable
					// from server state via --app/--user.
					result, err := s.cliAppRun(serverName, appName, "accessory", sub, accName)
					if err != nil {
						writeError(w, err.Error())
						return
					}
					writeData(w, result)
					return
				default:
					// upgrade/backup/restore need accessory image config from
					// teploy.yml, which dash doesn't have. Reject clearly.
					writeError(w, "accessory "+sub+" must be run from the app directory (needs teploy.yml); only stop/start/logs are available from the dashboard")
					return
				}
			}
		}
		writeError(w, "unknown action: "+action)
	}
}

// ── WebSocket Log Streaming ──────────────────────────────────────────────

func (s *Server) handleLogsWS(w http.ResponseWriter, r *http.Request) {
	// Path: /ws/logs/{server}/{app}
	path := strings.TrimPrefix(r.URL.Path, "/ws/logs/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid path — expected /ws/logs/{server}/{app}", 400)
		return
	}
	serverName, appName := parts[0], parts[1]

	srv, ok := s.lookupServer(serverName)
	if !ok {
		http.Error(w, "server not found: "+serverName, 404)
		return
	}

	// Upgrade to WebSocket manually (no external dep — use chunked streaming).
	// Check for WebSocket upgrade; fall back to SSE if not a WS request.
	upgradeHeader := r.Header.Get("Upgrade")
	if strings.ToLower(upgradeHeader) != "websocket" {
		// SSE fallback for clients that don't support WebSocket.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", 500)
			return
		}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		pw := &sseWriter{w: w, flusher: flusher}
		remote.StreamLogs(ctx, srv, appName, pw) //nolint:errcheck
		return
	}

	// Proper WebSocket upgrade using net/http hijack.
	wsHandler(w, r, func(ctx context.Context, send func(string)) {
		pw := &wsLineWriter{send: send}
		remote.StreamLogs(ctx, srv, appName, pw) //nolint:errcheck
	})
}

// sseWriter wraps http.ResponseWriter as an io.Writer that formats SSE events.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s *sseWriter) Write(p []byte) (int, error) {
	lines := strings.Split(strings.TrimRight(string(p), "\n"), "\n")
	for _, line := range lines {
		_, err := s.w.Write([]byte("data: " + line + "\n\n"))
		if err != nil {
			return 0, err
		}
	}
	s.flusher.Flush()
	return len(p), nil
}

// wsLineWriter sends each line as a WebSocket message.
type wsLineWriter struct {
	send func(string)
}

func (w *wsLineWriter) Write(p []byte) (int, error) {
	lines := strings.Split(strings.TrimRight(string(p), "\n"), "\n")
	for _, line := range lines {
		if line != "" {
			w.send(line)
		}
	}
	return len(p), nil
}

// ── Templates ────────────────────────────────────────────────────────────

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !cli.IsInstalled() {
		writeData(w, []interface{}{})
		return
	}
	result, err := cli.Run("template", "list", "--json")
	if err != nil {
		writeData(w, []interface{}{})
		return
	}
	writeRawJSON(w, result.Stdout)
}

func (s *Server) handleTemplateInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !cli.IsInstalled() {
		writeError(w, "teploy CLI not installed")
		return
	}

	var body struct {
		Template string            `json:"template"`
		Domain   string            `json:"domain"`
		Server   string            `json:"server"`
		Vars     map[string]string `json:"vars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if body.Template == "" || body.Domain == "" || body.Server == "" {
		writeError(w, "template, domain, and server are required")
		return
	}

	args := []string{"template", "install", body.Template,
		"--domain", body.Domain,
		"--server", body.Server,
	}
	for k, v := range body.Vars {
		args = append(args, "--var", k+"="+v)
	}

	result, err := cli.Run(args...)
	if err != nil {
		writeError(w, err.Error())
		return
	}

	s.fleet.set(nil) // invalidate cache after install
	writeData(w, result)
}

// ── Servers ───────────────────────────────────────────────────────────────

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	if !cli.IsInstalled() {
		writeError(w, "teploy CLI not installed on this host — install from https://teploy.dev")
		return
	}
	result, err := cli.Run("server", "list", "--json")
	if err != nil {
		writeData(w, []interface{}{})
		return
	}
	writeRawJSON(w, result.Stdout)
}

func (s *Server) handleServerDetail(w http.ResponseWriter, r *http.Request) {
	if !cli.IsInstalled() {
		writeData(w, map[string]interface{}{"error": "CLI not installed"})
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/servers/"), "/")
	if len(parts) < 2 {
		writeError(w, "invalid path")
		return
	}
	serverName := parts[0]
	action := parts[1]

	switch action {
	case "status", "proxy":
		// Server-level status (no specific app in scope here).
		result, err := cli.Run("status", "--host", serverName, "--json")
		if err != nil {
			writeData(w, nil)
			return
		}
		writeRawJSON(w, result.Stdout)
	default:
		writeError(w, "unknown action: "+action)
	}
}

// ── Deploy ────────────────────────────────────────────────────────────────

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !cli.IsInstalled() {
		writeError(w, "teploy CLI not installed")
		return
	}
	var body struct {
		Server string `json:"server"`
		App    string `json:"app"`
		Image  string `json:"image"`
		Domain string `json:"domain"`
		Port   int    `json:"port"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	result, err := cli.Deploy(body.Server, s.serverUser(body.Server), body.App, body.Image, body.Domain, body.Port)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	s.fleet.set(nil)
	writeData(w, result)
}

// ── Groups ────────────────────────────────────────────────────────────────
// Persistent group/project organization stored in ~/.teploy/groups.json.
// Same file format as the CLI's embedded UI for interoperability.

type groupData struct {
	Groups []groupEntry `json:"groups"`
}

type groupEntry struct {
	Name     string         `json:"name"`
	Apps     []string       `json:"apps"`
	Projects []projectEntry `json:"projects,omitempty"`
}

type projectEntry struct {
	Name string   `json:"name"`
	Apps []string `json:"apps"`
}

func groupsFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".teploy", "groups.json")
}

func loadGroupsFile() (groupData, error) {
	raw, err := os.ReadFile(groupsFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return groupData{Groups: []groupEntry{}}, nil
		}
		return groupData{}, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return groupData{Groups: []groupEntry{}}, nil
	}
	if raw[0] == '[' {
		var groups []groupEntry
		if err := json.Unmarshal(raw, &groups); err != nil {
			return groupData{}, err
		}
		return groupData{Groups: groups}, nil
	}
	var data groupData
	if err := json.Unmarshal(raw, &data); err != nil {
		return groupData{}, err
	}
	if data.Groups == nil {
		data.Groups = []groupEntry{}
	}
	return data, nil
}

func saveGroupsFile(data groupData) error {
	path := groupsFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0644)
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		data, err := loadGroupsFile()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, data.Groups)
	case "POST":
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeError(w, "name is required")
			return
		}
		data, err := loadGroupsFile()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		for _, g := range data.Groups {
			if g.Name == body.Name {
				writeError(w, "group already exists")
				return
			}
		}
		data.Groups = append(data.Groups, groupEntry{Name: body.Name, Apps: []string{}})
		if err := saveGroupsFile(data); err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, map[string]string{"status": "created"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleGroupAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.Split(path, "/")
	groupName := parts[0]

	if len(parts) == 1 {
		data, err := loadGroupsFile()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		switch r.Method {
		case "DELETE":
			// DELETE /api/groups/{name}
			filtered := make([]groupEntry, 0, len(data.Groups))
			for _, g := range data.Groups {
				if g.Name != groupName {
					filtered = append(filtered, g)
				}
			}
			data.Groups = filtered
			if err := saveGroupsFile(data); err != nil {
				writeError(w, err.Error())
				return
			}
			writeData(w, map[string]string{"status": "deleted"})
		case "PUT":
			// PUT /api/groups/{name} — rename
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
				writeError(w, "name is required")
				return
			}
			for i, g := range data.Groups {
				if g.Name == groupName {
					data.Groups[i].Name = body.Name
					saveGroupsFile(data)
					writeData(w, map[string]string{"status": "renamed"})
					return
				}
			}
			writeError(w, "group not found")
		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}

	resource := parts[1]

	switch {
	case resource == "apps" && len(parts) == 3 && r.Method == "DELETE":
		// DELETE /api/groups/{name}/apps/{app} — unassign app from group
		appName := parts[2]
		data, err := loadGroupsFile()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		for i, g := range data.Groups {
			if g.Name == groupName {
				filtered := make([]string, 0, len(g.Apps))
				for _, a := range g.Apps {
					if a != appName {
						filtered = append(filtered, a)
					}
				}
				data.Groups[i].Apps = filtered
				saveGroupsFile(data)
				writeData(w, map[string]string{"status": "unassigned"})
				return
			}
		}
		writeError(w, "group not found")
		return

	case resource == "apps" && r.Method == "POST":
		// POST /api/groups/{name}/apps — assign app to group
		var body struct {
			App string `json:"app"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.App == "" {
			writeError(w, "app is required")
			return
		}
		data, err := loadGroupsFile()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		for i, g := range data.Groups {
			if g.Name == groupName {
				for _, a := range g.Apps {
					if a == body.App {
						writeData(w, map[string]string{"status": "already assigned"})
						return
					}
				}
				data.Groups[i].Apps = append(data.Groups[i].Apps, body.App)
				saveGroupsFile(data)
				writeData(w, map[string]string{"status": "assigned"})
				return
			}
		}
		writeError(w, "group not found")

	case resource == "projects" && r.Method == "POST" && len(parts) == 2:
		// POST /api/groups/{name}/projects — create project
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeError(w, "name is required")
			return
		}
		data, err := loadGroupsFile()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		for i, g := range data.Groups {
			if g.Name == groupName {
				for _, p := range g.Projects {
					if p.Name == body.Name {
						writeError(w, "project already exists")
						return
					}
				}
				data.Groups[i].Projects = append(data.Groups[i].Projects, projectEntry{Name: body.Name, Apps: []string{}})
				saveGroupsFile(data)
				writeData(w, map[string]string{"status": "created"})
				return
			}
		}
		writeError(w, "group not found")

	case resource == "projects" && len(parts) == 3 && r.Method == "PUT":
		// PUT /api/groups/{name}/projects/{project} — rename project
		projectName := parts[2]
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeError(w, "name is required")
			return
		}
		data, err := loadGroupsFile()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		for i, g := range data.Groups {
			if g.Name == groupName {
				for j, p := range g.Projects {
					if p.Name == projectName {
						data.Groups[i].Projects[j].Name = body.Name
						saveGroupsFile(data)
						writeData(w, map[string]string{"status": "renamed"})
						return
					}
				}
			}
		}
		writeError(w, "group or project not found")
		return

	case resource == "projects" && len(parts) == 3 && r.Method == "DELETE":
		// DELETE /api/groups/{name}/projects/{project} — delete project
		projectName := parts[2]
		data, err := loadGroupsFile()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		for i, g := range data.Groups {
			if g.Name == groupName {
				filtered := make([]projectEntry, 0, len(g.Projects))
				for _, p := range g.Projects {
					if p.Name != projectName {
						filtered = append(filtered, p)
					}
				}
				data.Groups[i].Projects = filtered
				saveGroupsFile(data)
				writeData(w, map[string]string{"status": "deleted"})
				return
			}
		}
		writeError(w, "group not found")
		return

	case resource == "projects" && len(parts) == 5 && parts[3] == "apps" && r.Method == "DELETE":
		// DELETE /api/groups/{name}/projects/{project}/apps/{app} — unassign from project
		projectName := parts[2]
		appName := parts[4]
		data, err := loadGroupsFile()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		for i, g := range data.Groups {
			if g.Name == groupName {
				for j, p := range g.Projects {
					if p.Name == projectName {
						filtered := make([]string, 0, len(p.Apps))
						for _, a := range p.Apps {
							if a != appName {
								filtered = append(filtered, a)
							}
						}
						data.Groups[i].Projects[j].Apps = filtered
						saveGroupsFile(data)
						writeData(w, map[string]string{"status": "unassigned"})
						return
					}
				}
			}
		}
		writeError(w, "group or project not found")
		return

	case resource == "projects" && len(parts) >= 3:
		projectName := parts[2]
		if len(parts) == 4 && parts[3] == "apps" && r.Method == "POST" {
			// POST /api/groups/{name}/projects/{project}/apps — assign app to project
			var body struct {
				App string `json:"app"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.App == "" {
				writeError(w, "app is required")
				return
			}
			data, err := loadGroupsFile()
			if err != nil {
				writeError(w, err.Error())
				return
			}
			for i, g := range data.Groups {
				if g.Name == groupName {
					for j, p := range g.Projects {
						if p.Name == projectName {
							for _, a := range p.Apps {
								if a == body.App {
									writeData(w, map[string]string{"status": "already assigned"})
									return
								}
							}
							data.Groups[i].Projects[j].Apps = append(data.Groups[i].Projects[j].Apps, body.App)
							saveGroupsFile(data)
							writeData(w, map[string]string{"status": "assigned"})
							return
						}
					}
				}
			}
			writeError(w, "group or project not found")
		} else {
			writeError(w, "not found")
		}

	default:
		writeError(w, "not found")
	}
}

// ── Config ────────────────────────────────────────────────────────────────

func (s *Server) handleConfigServers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		if !cli.IsInstalled() {
			writeData(w, []interface{}{})
			return
		}
		result, err := cli.ServerList()
		if err != nil {
			writeData(w, []interface{}{})
			return
		}
		writeRawJSON(w, result.Stdout)
	case "POST":
		var body struct {
			Name string `json:"name"`
			Host string `json:"host"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		result, err := cli.ServerAdd(body.Name, body.Host)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleConfigServerAction(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/config/servers/")
	switch r.Method {
	case "DELETE":
		result, err := cli.ServerRemove(name)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)
	case "PUT":
		// Edit by remove + re-add. CLI doesn't have a dedicated edit; this
		// is the same approach the embedded UI takes via config.AddServer.
		var body struct {
			Name string `json:"name"`
			Host string `json:"host"`
			User string `json:"user"`
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Host == "" {
			writeError(w, "host is required")
			return
		}
		newName := body.Name
		if newName == "" {
			newName = name
		}
		if _, err := cli.ServerRemove(name); err != nil {
			writeError(w, err.Error())
			return
		}
		if _, err := cli.ServerAdd(newName, body.Host); err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, map[string]string{"status": "updated"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func notificationsFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".teploy", "notifications.json")
}

// LoadNotificationsConfig reads alert configuration from ~/.teploy/notifications.json.
func LoadNotificationsConfig() alert.Config {
	return loadNotificationsConfig()
}

func loadNotificationsConfig() alert.Config {
	raw, err := os.ReadFile(notificationsFilePath())
	if err != nil {
		return alert.Config{}
	}
	var cfg alert.Config
	json.Unmarshal(raw, &cfg)
	return cfg
}

func saveNotificationsConfig(cfg alert.Config) error {
	path := notificationsFilePath()
	os.MkdirAll(filepath.Dir(path), 0755)
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, raw, 0644)
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfg := loadNotificationsConfig()
		writeData(w, cfg)
	case "POST":
		var cfg alert.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeError(w, "invalid request body")
			return
		}
		if err := saveNotificationsConfig(cfg); err != nil {
			writeError(w, err.Error())
			return
		}
		// Update the monitor runner's alert dispatcher with the new config.
		if s.monitor != nil {
			s.monitor.SetAlerter(alert.New(cfg))
		}
		writeData(w, map[string]bool{"saved": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleRegistries(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		result, err := cli.Run("registry", "list", "--json")
		if err != nil {
			writeData(w, []interface{}{})
			return
		}
		writeRawJSON(w, result.Stdout)
	case "POST":
		var body struct {
			Server   string `json:"server"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		result, err := cli.Run("registry", "login", body.Server, "--username", body.Username, "--password", body.Password)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleRegistryAction(w http.ResponseWriter, r *http.Request) {
	server := strings.TrimPrefix(r.URL.Path, "/api/registries/")
	if r.Method == "DELETE" {
		result, err := cli.Run("registry", "remove", server)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)
	} else {
		http.Error(w, "method not allowed", 405)
	}
}

// ── Monitor Routes ────────────────────────────────────────────────────────

func (s *Server) handleMonitors(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, []interface{}{})
		return
	}

	switch r.Method {
	case "GET":
		monitors, err := s.store.ListMonitors()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		type monitorWithStats struct {
			store.Monitor
			Stats *store.UptimeStats `json:"stats,omitempty"`
		}

		var result []monitorWithStats
		for _, m := range monitors {
			mws := monitorWithStats{Monitor: m}
			stats, _ := s.store.GetStats(m.ID, time.Now().Add(-24*time.Hour))
			mws.Stats = stats
			result = append(result, mws)
		}
		writeJSON(w, result)

	case "POST":
		var m store.Monitor
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, "invalid request body", 400)
			return
		}
		if err := s.store.SaveMonitor(m); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if s.monitor != nil {
			s.monitor.Reload(m)
		}
		writeJSON(w, m)

	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "monitoring not configured (no store)", 404)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/monitors/")

	// POST /api/monitors/{id}/test — run one check immediately, don't save.
	if strings.HasSuffix(path, "/test") && r.Method == "POST" {
		id := strings.TrimSuffix(path, "/test")
		m, err := s.store.GetMonitor(id)
		if err != nil {
			http.Error(w, "monitor not found", 404)
			return
		}
		if s.monitor == nil {
			http.Error(w, "monitor runner not available", 500)
			return
		}
		result := s.monitor.CheckNow(*m)
		writeJSON(w, result)
		return
	}

	id := path

	switch r.Method {
	case "GET":
		m, err := s.store.GetMonitor(id)
		if err != nil {
			http.Error(w, "monitor not found", 404)
			return
		}
		checks, _ := s.store.GetChecks(id, time.Now().Add(-24*time.Hour), 100)
		stats, _ := s.store.GetStats(id, time.Now().Add(-24*time.Hour))
		writeJSON(w, map[string]interface{}{
			"monitor": m,
			"checks":  checks,
			"stats":   stats,
		})

	case "DELETE":
		if err := s.store.DeleteMonitor(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]bool{"deleted": true})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// ── System ────────────────────────────────────────────────────────────────

func (s *Server) handleCLIStatus(w http.ResponseWriter, r *http.Request) {
	installed := cli.IsInstalled()
	var version string
	if installed {
		v, _ := cli.Version()
		version = v
	}
	writeJSON(w, map[string]interface{}{
		"installed": installed,
		"version":   version,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

// ── Frontend ──────────────────────────────────────────────────────────────

// handleFrontend serves the embedded SPA. Unknown paths fall back to
// index.html so client-side routing works.
func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	if s.frontend == nil {
		http.Error(w, "frontend not embedded", http.StatusInternalServerError)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}

	data, err := fs.ReadFile(s.frontend, path)
	if err != nil {
		// SPA fallback: serve index.html for unknown routes.
		data, err = fs.ReadFile(s.frontend, "index.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		path = "index.html"
	}

	switch {
	case strings.HasSuffix(path, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(path, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	case strings.HasSuffix(path, ".png"):
		w.Header().Set("Content-Type", "image/png")
	case strings.HasSuffix(path, ".ico"):
		w.Header().Set("Content-Type", "image/x-icon")
	}

	w.Write(data)
}

// ── Helpers ───────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func writeData(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
}

func writeError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeRawJSON(w http.ResponseWriter, raw string) {
	w.Header().Set("Content-Type", "application/json")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		w.Write([]byte(`{"data":null}`))
		return
	}
	var parsed interface{}
	if json.Unmarshal([]byte(raw), &parsed) == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"data": parsed})
	} else {
		w.Write([]byte(`{"data":` + raw + `}`))
	}
}
