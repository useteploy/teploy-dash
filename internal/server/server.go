package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/manifest"
	"github.com/useteploy/teploy-dash/internal/mcp"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/remote"
	"github.com/useteploy/teploy-dash/internal/restoretest"
	"github.com/useteploy/teploy-dash/internal/state"
	"github.com/useteploy/teploy-dash/internal/store"
)

var appNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validAppName(name string) bool {
	return name != "." && name != ".." && appNamePattern.MatchString(name)
}

// Config holds server configuration.
type Config struct {
	Host           string
	Port           int
	DeploymentsDir string
	DataDir        string
	Monitor        *monitor.Runner
	Restore        *restoretest.Runner
	Store          store.Store
	// AuthUser and AuthPass are bootstrap credentials from env vars. If AuthPass
	// is empty and no auth.json exists, the server starts in setup mode.
	// If NoAuth is true, authentication is disabled entirely (dev mode).
	AuthUser string
	AuthPass string
	NoAuth   bool
	// PublicStatus enables the unauthenticated /status page + /api/status.
	// Off by default — it exposes monitor uptime without a login.
	PublicStatus bool
	// Frontend is the embedded SPA filesystem (rooted at the frontend/
	// directory: contains index.html, css/, js/). Required — the binary is
	// not portable without an embedded UI.
	Frontend fs.FS
	// Version is the dash build version (for MCP serverInfo).
	Version string
	// Backend is the active store backend ("nucleus" or "file"), surfaced
	// through /api/health so a silent Nucleus-connect-failure fallback stays
	// visible after the startup log line has scrolled away (DASH-003).
	Backend string
	// Operation hooks are primarily for tests and alternate CLI packaging. The
	// production defaults resolve servers.yml and execute the bundled CLI.
	OperationResolver  operation.Resolver
	OperationExecutor  operation.Executor
	OperationMaxEvents int
	// CLI/read hooks keep machine-contract handling testable without changing
	// production behavior.
	CLIRunner          func(context.Context, ...string) (*cli.Result, error)
	CLIInstalled       func() bool
	RemoteListApps     func(context.Context, remote.ServerConn) ([]remote.AppState, error)
	RemoteServerStatus func(context.Context, remote.ServerConn) (*remote.ServerStatus, error)
}

// fleetCache caches aggregated multi-server app state to avoid SSH on every request.
type fleetCache struct {
	mu      sync.RWMutex
	apps    []remote.AppState
	builtAt time.Time
	ttl     time.Duration
	// lastGood survives both TTL expiry and invalidation. The TTL exists to keep
	// app *status* fresh; consumers that only read stable facts (where a sibling
	// dashboard lives) want the last known answer rather than none.
	lastGood []remote.AppState
	// refreshing is a single-flight latch: a background refresh SSHes every
	// server, so concurrent stale reads must not each start their own sweep.
	refreshing bool
}

// beginRefresh claims the right to run a background refresh. Returns false when
// one is already in flight, so callers simply serve what they have.
func (fc *fleetCache) beginRefresh() bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.refreshing {
		return false
	}
	fc.refreshing = true
	return true
}

func (fc *fleetCache) endRefresh() {
	fc.mu.Lock()
	fc.refreshing = false
	fc.mu.Unlock()
}

// snapshot returns the last successfully collected fleet regardless of age.
func (fc *fleetCache) snapshot() []remote.AppState {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.lastGood
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
		fc.lastGood = apps
	}
}

// Server is the teploy-dash HTTP server.
type Server struct {
	mux                *http.ServeMux
	config             Config
	gate               *authGate
	state              *state.Reader
	monitor            *monitor.Runner
	restore            *restoretest.Runner
	store              store.Store
	fleet              *fleetCache
	frontend           fs.FS
	mcpTokens          *mcp.TokenStore
	operations         *operation.Manager
	operationInitErr   error
	manifests          *manifest.Store
	manifestInitErr    error
	runCLI             cliRunner
	cliInstalled       func() bool
	remoteListApps     func(context.Context, remote.ServerConn) ([]remote.AppState, error)
	remoteServerStatus func(context.Context, remote.ServerConn) (*remote.ServerStatus, error)
	capabilitiesCache  capabilityCache

	httpSrvMu sync.Mutex
	httpSrv   *http.Server
}

// New creates a new server.
func New(config Config) *Server {
	s := &Server{
		mux:      http.NewServeMux(),
		config:   config,
		state:    state.NewReader(config.DeploymentsDir),
		monitor:  config.Monitor,
		restore:  config.Restore,
		store:    config.Store,
		fleet:    &fleetCache{ttl: 60 * time.Second},
		frontend: config.Frontend,
	}
	s.runCLI = config.CLIRunner
	if s.runCLI == nil {
		s.runCLI = cli.RunContext
	}
	s.cliInstalled = config.CLIInstalled
	if s.cliInstalled == nil {
		s.cliInstalled = cli.IsInstalled
	}
	s.remoteListApps = config.RemoteListApps
	if s.remoteListApps == nil {
		s.remoteListApps = remote.ListApps
	}
	s.remoteServerStatus = config.RemoteServerStatus
	if s.remoteServerStatus == nil {
		s.remoteServerStatus = remote.GetServerStatus
	}
	if !config.NoAuth {
		s.gate = newAuthGate(config.AuthUser, config.AuthPass, filepath.Join(config.DataDir, "auth.json"))
		if oa := newOIDCAuth(); oa != nil {
			s.gate.oidc = oa
			// SSO satisfies authentication even with no local users, so don't
			// force local first-run setup — the login page offers the SSO button.
			s.gate.credMu.Lock()
			s.gate.setupRequired = false
			s.gate.credMu.Unlock()
		}
	}
	if s.restore != nil {
		// Restore-test runs go through the CLI delegate with --host <server>;
		// non-root fleets also need --user, resolved the same way cliAppRun does.
		s.restore.SetUserResolver(s.serverUser)
	}
	s.manifests, s.manifestInitErr = manifest.New(config.DataDir)
	if s.manifestInitErr != nil {
		log.Printf("manifests: disabled: %v", s.manifestInitErr)
	}
	resolver := config.OperationResolver
	if resolver == nil {
		resolver = s.resolveOperationServer
	}
	executor := config.OperationExecutor
	if executor == nil {
		executor = func(ctx context.Context, command operation.Command, emit func(operation.Stream, string)) (int, error) {
			defer s.fleet.set(nil)
			return executeOperation(ctx, command, emit)
		}
	}
	s.operations, s.operationInitErr = operation.New(config.DataDir, operation.Options{
		MaxEvents: config.OperationMaxEvents,
		Resolver:  resolver,
		ProjectResolver: func(server, app, revision string) (string, error) {
			if s.manifests == nil {
				return "", fmt.Errorf("manifest service unavailable")
			}
			return s.manifests.ProjectDir(server, app, revision)
		},
		Executor: executor,
	})
	if s.operationInitErr != nil {
		log.Printf("operations: disabled: %v", s.operationInitErr)
	}
	s.routes()
	return s
}

// ListenAndServe starts the HTTP server, storing the *http.Server so Shutdown
// can later stop it accepting new connections and drain in-flight requests.
// Returns nil on a normal Shutdown-triggered close (http.ErrServerClosed),
// matching the http.Server convention that callers shouldn't treat that as a
// real error.
func (s *Server) ListenAndServe(addr string) error {
	s.warmFleet()
	srv := s.httpServer(addr)
	s.httpSrvMu.Lock()
	s.httpSrv = srv
	s.httpSrvMu.Unlock()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown stops the HTTP server from accepting new connections and waits
// (bounded by ctx) for in-flight requests to finish. Safe to call before
// ListenAndServe has run (e.g. in tests) — it's then a no-op.
func (s *Server) Shutdown(ctx context.Context) error {
	s.httpSrvMu.Lock()
	srv := s.httpSrv
	s.httpSrvMu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// refreshFleetAsync refreshes the fleet behind a request. Single-flighted, so a
// burst of stale reads causes one sweep, not one per request. Uses a background
// context: the refresh must outlive the request that triggered it, or a client
// navigating away would cancel it and the cache would never re-warm.
func (s *Server) refreshFleetAsync() {
	if !cli.IsInstalled() || !s.fleet.beginRefresh() {
		return
	}
	go func() {
		defer s.fleet.endRefresh()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		apps, err := s.collectFleetApps(ctx)
		if err != nil {
			log.Printf("[fleet] background refresh failed (serving last known state): %v", err)
			return
		}
		s.fleet.set(apps)
	}()
}

// warmFleet populates the fleet cache once in the background at startup.
// Without it the cache only fills when someone opens the deployments page, so
// immediately after a restart the first fleet view pays the full SSH sweep and
// the product switcher — which infers siblings from fleet state — is missing
// entries until then. Runs detached so a slow or unreachable fleet never delays
// the listener.
func (s *Server) warmFleet() {
	if !cli.IsInstalled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		apps, err := s.collectFleetApps(ctx)
		if err != nil {
			log.Printf("[fleet] startup warm failed (will fill on first request): %v", err)
			return
		}
		s.fleet.set(apps)
	}()
}

func (s *Server) httpServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
}

func (s *Server) handler() http.Handler {
	handler := http.Handler(s.mux)
	handler = limitMutationBodies(handler)
	if s.gate != nil {
		handler = s.gate.wrap(handler)
	}
	return handler
}

func limitMutationBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMutating(r.Method) {
			if r.ContentLength > maxRequestBodySize {
				jsonError(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		}
		next.ServeHTTP(w, r)
	})
}

// authGate protects all routes except /api/health, /login, /api/login, and
// /api/logout with session-cookie auth, plus per-source-IP failed-attempt
// backoff (brute-force resistance) and a same-origin requirement on
// state-changing requests (CSRF defense).
type authGate struct {
	// Bootstrap credentials from env vars (plaintext, fallback when no users
	// file). The env-var user is always treated as an admin.
	user, pass string
	// On-disk users (bcrypt hashes + role). Protected by credMu.
	usersFile     string // users.json — canonical multi-user store
	legacyFile    string // auth.json — single-user file migrated on first load
	credMu        sync.RWMutex
	users         map[string]*dashUser
	setupRequired bool
	// Optional OIDC single sign-on. nil when not configured.
	oidc *oidcAuth
	// Rate limiting
	trustedProxies []*net.IPNet
	mu             sync.Mutex
	fails          map[string]*failInfo
	// Sessions carry the authenticated user's identity + role so the gate can
	// enforce RBAC and handlers can attribute actions.
	sessMu   sync.Mutex
	sessions map[string]*sessionInfo
	// bootstrapToken gates account creation while setupRequired is true — a
	// fresh, remotely-reachable instance would otherwise let ANY visitor claim
	// the first (admin) account. Generated once in newAuthGate, printed to the
	// log (never returned in an HTTP response), single-use (setupRequired
	// flipping false on success makes it moot), and time-limited so an
	// abandoned setup doesn't stay claimable indefinitely.
	bootstrapToken       string
	bootstrapTokenExpiry time.Time
}

// bootstrapTokenTTL bounds how long a printed setup token remains valid.
// Long enough for an operator to copy it from the log and finish setup in one
// sitting; short enough that a token from a log an operator forgot about
// isn't a standing credential.
const bootstrapTokenTTL = 30 * time.Minute

// sessionInfo is one live session: which user, what role, and when it expires.
type sessionInfo struct {
	user string
	role string
	exp  time.Time
}

type failInfo struct {
	count int
	until time.Time
}

const (
	authMaxFails       = 5
	authLockWindow     = time.Minute
	sessionTTL         = 24 * time.Hour
	sessionCookie      = "teploy_dash_session"
	maxRequestBodySize = 1 << 20
)

func newAuthGate(user, pass, credFile string) *authGate {
	g := &authGate{
		user:           user,
		pass:           pass,
		usersFile:      filepath.Join(filepath.Dir(credFile), "users.json"),
		legacyFile:     credFile,
		users:          make(map[string]*dashUser),
		trustedProxies: parseTrustedProxies(os.Getenv("TEPLOY_DASH_TRUSTED_PROXY")),
		fails:          make(map[string]*failInfo),
		sessions:       make(map[string]*sessionInfo),
	}
	// Load stored users (migrating a legacy single-user auth.json if present).
	// If none exist and no env-var password is set, enter setup mode so the
	// operator can create the first account.
	if err := g.loadUsers(); err != nil && pass == "" {
		g.setupRequired = true
		g.bootstrapToken = generateBootstrapToken()
		g.bootstrapTokenExpiry = time.Now().Add(bootstrapTokenTTL)
		log.Printf("=====================================================================")
		log.Printf("First-run setup required. Bootstrap token (valid %s): %s", bootstrapTokenTTL, g.bootstrapToken)
		log.Printf("Enter this token on the /setup page to create the initial admin account.")
		log.Printf("=====================================================================")
	}
	return g
}

func generateBootstrapToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// checkBootstrapToken reports whether the supplied token is the live,
// unexpired bootstrap token. Constant-time compare against a real token; a
// timing difference on a missing/expired token doesn't disclose anything
// because there is no valid token to find in that state.
func (g *authGate) checkBootstrapToken(supplied string) bool {
	g.credMu.RLock()
	token, expiry := g.bootstrapToken, g.bootstrapTokenExpiry
	g.credMu.RUnlock()
	if token == "" || supplied == "" || time.Now().After(expiry) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) == 1
}

func (g *authGate) newSession(user, role string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	token := hex.EncodeToString(b)
	g.sessMu.Lock()
	defer g.sessMu.Unlock()
	now := time.Now()
	for k, si := range g.sessions {
		if now.After(si.exp) {
			delete(g.sessions, k)
		}
	}
	g.sessions[token] = &sessionInfo{user: user, role: normalizeRole(role), exp: now.Add(sessionTTL)}
	return token
}

// lookupSession returns the live session for a token, or false if absent/expired.
func (g *authGate) lookupSession(token string) (*sessionInfo, bool) {
	if token == "" {
		return nil, false
	}
	g.sessMu.Lock()
	defer g.sessMu.Unlock()
	si, ok := g.sessions[token]
	if !ok {
		return nil, false
	}
	if time.Now().After(si.exp) {
		delete(g.sessions, token)
		return nil, false
	}
	return si, true
}

func (g *authGate) deleteSession(token string) {
	g.sessMu.Lock()
	defer g.sessMu.Unlock()
	delete(g.sessions, token)
}

// deleteUserSessions invalidates every live session belonging to one user —
// used when their password or role changes, or the account is removed.
func (g *authGate) deleteUserSessions(user string) {
	g.sessMu.Lock()
	defer g.sessMu.Unlock()
	for k, si := range g.sessions {
		if si.user == user {
			delete(g.sessions, k)
		}
	}
}

// parseTrustedProxies parses a comma-separated list of proxy IPs/CIDRs. When the
// dashboard runs behind a reverse proxy (e.g. Caddy), set TEPLOY_DASH_TRUSTED_PROXY
// to the proxy's address so per-IP rate-limiting keys on the real client (from
// X-Forwarded-For) instead of collapsing every client onto the proxy's IP.
func parseTrustedProxies(s string) []*net.IPNet {
	var nets []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			if strings.Contains(part, ":") {
				part += "/128"
			} else {
				part += "/32"
			}
		}
		if _, n, err := net.ParseCIDR(part); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}

func (g *authGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Setup mode: no credentials configured yet. Only let through the
		// setup page and its API endpoint.
		g.credMu.RLock()
		inSetup := g.setupRequired
		g.credMu.RUnlock()
		if inSetup {
			// When SSO is configured, setup mode is never entered (New clears it),
			// so this branch only runs for the local-account first-run flow.
			switch r.URL.Path {
			case "/api/health", "/setup", "/api/setup":
				next.ServeHTTP(w, r)
			default:
				if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
					jsonError(w, "setup required", http.StatusServiceUnavailable)
				} else {
					http.Redirect(w, r, "/setup", http.StatusFound)
				}
			}
			return
		}

		// Always allow: health, login page, login/logout API, the public
		// status page (its handlers 404 when the feature is disabled), and
		// the MCP endpoint — it enforces its own bearer-token auth and is
		// used by non-browser clients that have no session cookie.
		switch r.URL.Path {
		case "/api/health", "/login", "/api/login", "/api/logout", "/api/login/methods",
			"/status", "/api/status", "/api/mcp",
			// The browser requests the tab icon before anyone has signed in; it
			// is a static brand asset and discloses nothing.
			"/favicon.svg", "/favicon.ico":
			next.ServeHTTP(w, r)
			return
		case "/oidc/login", "/oidc/callback":
			// Pre-auth SSO endpoints — no session required yet, but still
			// subject to the same per-IP lockout as password login so they
			// can't be used to brute-force sign-in or spam the in-flight
			// OIDC flow map unthrottled.
			if g.lockedOut(g.clientIP(r)) {
				http.Error(w, "too many failed attempts — try again shortly", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		ip := g.clientIP(r)
		if g.lockedOut(ip) {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
				jsonError(w, "too many failed attempts — try again shortly", http.StatusTooManyRequests)
			} else {
				http.Error(w, "too many failed attempts — try again shortly", http.StatusTooManyRequests)
			}
			return
		}

		cookie, cookieErr := r.Cookie(sessionCookie)
		var session *sessionInfo
		if cookieErr == nil {
			session, _ = g.lookupSession(cookie.Value)
		}
		if session == nil {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
				jsonError(w, "unauthorized", http.StatusUnauthorized)
			} else {
				nextPath := r.URL.Path
				if r.URL.RawQuery != "" {
					nextPath += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, "/login?next="+url.QueryEscape(nextPath), http.StatusFound)
			}
			return
		}

		// CSRF: reject cross-origin state-changing requests. SameSite=Lax on
		// the cookie already blocks most CSRF; this is a belt-and-suspenders
		// check for browsers or proxies that don't enforce SameSite.
		if isMutating(r.Method) && !strings.HasPrefix(r.URL.Path, "/ws/") && !sameOrigin(r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}

		// RBAC: enforce the minimum role for this route. Fail closed — a
		// mutating route with no explicit classification requires editor, never
		// viewer, so a new endpoint can't silently be viewer-writable.
		if need := requiredRole(r.Method, r.URL.Path); !roleAllows(session.role, need) {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
				jsonError(w, "forbidden: this action requires the "+need+" role", http.StatusForbidden)
			} else {
				http.Error(w, "forbidden", http.StatusForbidden)
			}
			return
		}
		next.ServeHTTP(w, withUser(r, session))
	})
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleLogin validates the submitted password and issues a session cookie.
func (g *authGate) handleLogin(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := g.clientIP(r)
	if g.lockedOut(ip) {
		jsonError(w, "too many failed attempts — try again shortly", http.StatusTooManyRequests)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := strictDecode(r, &body); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	user, ok := g.authenticate(body.Username, body.Password)
	if !ok {
		g.recordFail(ip)
		jsonError(w, "incorrect username or password", http.StatusUnauthorized)
		return
	}
	g.recordSuccess(ip)
	g.issueSessionCookie(w, r, user.Username, user.Role)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleSetup creates the initial account. Only works in setup mode.
func (g *authGate) handleSetup(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	g.credMu.RLock()
	inSetup := g.setupRequired
	g.credMu.RUnlock()
	if !inSetup {
		jsonError(w, "account already configured", http.StatusConflict)
		return
	}
	var body struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		ConfirmPassword string `json:"confirm_password"`
		BootstrapToken  string `json:"bootstrap_token"`
	}
	if err := strictDecode(r, &body); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !g.checkBootstrapToken(body.BootstrapToken) {
		jsonError(w, "missing or invalid bootstrap token — check the server log", http.StatusUnauthorized)
		return
	}
	if body.Username == "" {
		body.Username = "admin"
	}
	if body.Password != body.ConfirmPassword {
		jsonError(w, "passwords do not match", http.StatusBadRequest)
		return
	}
	// The first account is always an admin.
	if err := g.createUser(body.Username, body.Password, RoleAdmin); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.issueSessionCookie(w, r, body.Username, RoleAdmin)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleChangePassword changes the password. Requires an authenticated session.
func (g *authGate) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	if err := strictDecode(r, &body); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	session, ok := currentUser(r)
	if !ok {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, ok := g.authenticate(session.user, body.CurrentPassword); !ok {
		jsonError(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}
	if body.NewPassword != body.ConfirmPassword {
		jsonError(w, "passwords do not match", http.StatusBadRequest)
		return
	}
	if err := g.setPassword(session.user, body.NewPassword); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Invalidate only this user's sessions — other users stay signed in.
	g.deleteUserSessions(session.user)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: g.secureCookie(r), SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (g *authGate) issueSessionCookie(w http.ResponseWriter, r *http.Request, user, role string) {
	token := g.newSession(user, role)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   g.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (g *authGate) secureCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !g.isTrustedProxy(host) {
		return false
	}
	proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(proto, "https")
}

// handleLogout clears the session cookie and invalidates the session.
func (g *authGate) handleLogout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		g.deleteSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   g.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (g *authGate) lockedOut(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	fi := g.fails[ip]
	return fi != nil && fi.count >= authMaxFails && time.Now().Before(fi.until)
}

func (g *authGate) recordFail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	// Drop expired entries so the map can't grow unbounded under IP-rotating
	// brute force (entries from IPs that never succeed were never pruned).
	for k, v := range g.fails {
		if k != ip && now.After(v.until) {
			delete(g.fails, k)
		}
	}
	fi := g.fails[ip]
	if fi == nil {
		fi = &failInfo{}
		g.fails[ip] = fi
	}
	fi.count++
	fi.until = now.Add(authLockWindow)
}

func (g *authGate) recordSuccess(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, ip)
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// sameOrigin reports whether a state-changing request is same-origin (or from a
// non-browser client that can't be a CSRF vector). Prefers the Fetch-Metadata
// header, falls back to comparing the Origin host to the request host.
func sameOrigin(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin" || sfs == "none"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		return err == nil && u.Host == r.Host
	}
	return true // no Origin / Fetch-Metadata → not a browser CSRF request
}

// clientIP returns the address used for per-IP rate-limiting. By default it's
// the direct peer (RemoteAddr) — X-Forwarded-For is NOT trusted, since a client
// could spoof it to evade the backoff. Only when the direct peer is a configured
// trusted proxy (TEPLOY_DASH_TRUSTED_PROXY) is the forwarded client IP used, so
// running behind Caddy doesn't collapse every client onto the proxy's IP.
//
// The chain is walked from the RIGHT, skipping addresses that are themselves
// trusted proxies. Forward proxies append the address they received the request
// from, so the rightmost non-trusted entry is the one the proxy actually
// observed; anything to its left was supplied by the client and can be a
// forgery. Reading the FIRST entry instead would let a brute-forcer prepend a
// fresh IP to every request (and pin a lockout onto a victim's IP).
func (g *authGate) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !g.isTrustedProxy(host) {
		return host
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		if candidate == "" {
			continue
		}
		if g.isTrustedProxy(candidate) {
			continue // another trusted hop, not the client
		}
		return candidate
	}
	// Every entry was a trusted proxy (or the header was empty): the peer
	// itself is the closest thing to a client address we can vouch for.
	return host
}

func (g *authGate) isTrustedProxy(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range g.trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) routes() {
	// Auth (only when auth is enabled)
	if s.gate != nil {
		s.mux.HandleFunc("/login", s.handleLoginPage)
		s.mux.HandleFunc("/setup", s.handleSetupPage)
		s.mux.HandleFunc("/api/login", s.gate.handleLogin)
		s.mux.HandleFunc("/api/logout", s.gate.handleLogout)
		s.mux.HandleFunc("/api/setup", s.gate.handleSetup)
		s.mux.HandleFunc("/api/auth/password", s.gate.handleChangePassword)
		s.mux.HandleFunc("/api/auth/me", s.handleWhoami)
		s.mux.HandleFunc("/api/login/methods", s.handleLoginMethods)
		s.mux.HandleFunc("/api/users", s.handleUsers)
		s.mux.HandleFunc("/api/users/", s.handleUserAction)
		if s.gate.oidc != nil {
			s.mux.HandleFunc("/oidc/login", s.gate.handleOIDCLogin)
			s.mux.HandleFunc("/oidc/callback", s.gate.handleOIDCCallback)
		}
	}

	// Homepage
	s.mux.HandleFunc("/api/homepage", s.handleHomepage)

	// Deployment management
	s.mux.HandleFunc("/api/servers", s.handleServers)
	s.mux.HandleFunc("/api/servers/", s.handleServerDetail)
	s.mux.HandleFunc("/api/apps", s.handleApps)
	s.mux.HandleFunc("/api/apps/", s.handleAppAction)
	s.mux.HandleFunc("/api/deploy", s.handleDeploy)
	s.mux.HandleFunc("/api/operations", s.handleOperations)
	s.mux.HandleFunc("/api/operations/", s.handleOperation)
	s.mux.HandleFunc("/api/manifests", s.handleManifests)
	s.mux.HandleFunc("/api/manifests/", s.handleManifest)
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

	// Restore tests (scheduled backup verification)
	s.mux.HandleFunc("/api/restore-tests", s.handleRestoreTests)
	s.mux.HandleFunc("/api/restore-tests/", s.handleRestoreTest)

	// WebSocket log streaming
	s.mux.HandleFunc("/ws/logs/", s.handleLogsWS)

	// System
	s.mux.HandleFunc("/api/cli/status", s.handleCLIStatus)
	s.mux.HandleFunc("/api/capabilities", s.handleCapabilities)
	s.mux.HandleFunc("/api/nav", s.handleNav)
	s.mux.HandleFunc("/api/health", s.handleHealth)

	// MCP: bearer-authed AI-client endpoint + session-authed token management.
	s.initMCP(s.config.Version)

	// Public status page (opt-in; handlers 404 when disabled). Bypasses auth
	// via the gate allowlist below.
	s.mux.HandleFunc("/status", s.handleStatusPage)
	s.mux.HandleFunc("/api/status", s.handleStatusAPI)

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

	// Stale-while-revalidate: a full sweep SSHes every server and can take tens
	// of seconds, which is the whole delay when opening the deployments page.
	// Serve the last known fleet at once and refresh behind the request, so the
	// page paints immediately and is current a moment later. Deliberately not a
	// background ticker — that would SSH the fleet forever even when nobody is
	// looking; this only refreshes in response to real use.
	if stale := s.fleet.snapshot(); len(stale) > 0 {
		s.refreshFleetAsync()
		w.Header().Set("X-Fleet-Cache", "stale")
		writeData(w, stale)
		return
	}

	apps, err := s.collectFleetApps(r.Context())
	if err != nil {
		writeErrorStatus(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.fleet.set(apps)
	writeData(w, apps)
}

// collectFleetApps gathers app state from all servers in parallel.
func (s *Server) collectFleetApps(ctx context.Context) ([]remote.AppState, error) {
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
		return apps, nil
	}

	// Bound the whole fleet refresh so a single slow/unreachable server can't
	// stall the page (per-server SSH is also bounded in internal/ssh).
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	type result struct {
		apps []remote.AppState
		err  error
	}

	ch := make(chan result, len(servers))
	for _, srv := range servers {
		srv := srv
		go func() {
			apps, err := s.readMachineApps(ctx, srv)
			ch <- result{apps, err}
		}()
	}

	var all []remote.AppState
	errCount := 0
	for range servers {
		r := <-ch
		if r.err != nil {
			// Previously every per-server error was silently dropped, so a
			// fully-unreachable fleet rendered as an empty success with no clue
			// why. Log each failure so the operator can see it.
			errCount++
			log.Printf("[fleet] server query failed: %v", r.err)
			continue
		}
		all = append(all, r.apps...)
	}
	if errCount == len(servers) && len(all) == 0 {
		return nil, fmt.Errorf("fleet app collection failed for all %d server(s)", len(servers))
	}
	return all, nil
}

// resolveServers returns server connections from the CLI's servers.yml via the CLI delegate.
func (s *Server) resolveServers() []remote.ServerConn {
	if !s.cliInstalled() {
		return nil
	}
	result, err := s.runCLI(context.Background(), "server", "list", "--json")
	if err != nil {
		// Don't silently treat a CLI failure as "no servers" (which would fall
		// through to the empty local-state path) — log it so the cause is visible.
		log.Printf("[fleet] could not list servers from the teploy CLI: %v", err)
		return nil
	}
	if result.ExitCode != 0 {
		log.Printf("[fleet] could not list servers from the teploy CLI: %v", commandFailure([]string{"server", "list", "--json"}, result))
		return nil
	}

	var raw map[string]struct {
		Host string `json:"host"`
		User string `json:"user"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &raw); err != nil {
		log.Printf("[fleet] could not parse server list: %v", err)
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

// serverHost resolves a server name to its configured host/IP, falling back to
// the name itself when unknown. The bundled CLI's --host, in app-scoped mode
// (--app), is treated as a raw host and is NOT resolved against servers.yml, so
// passing the alias fails with "no such host". Resolve it here.
func (s *Server) serverHost(name string) string {
	if srv, ok := s.lookupServer(name); ok && srv.Host != "" {
		return srv.Host
	}
	return name
}

// cliAppRun runs an app-scoped teploy subcommand, appending --host/--app and
// --user (when the server has a non-root user). `parts` is the subcommand plus
// any leading flags/positionals; flag order doesn't matter to cobra so trailing
// flags like --json can be passed in parts.
func (s *Server) cliAppRun(serverName, appName string, parts ...string) (*cli.Result, error) {
	args := append([]string{}, parts...)
	args = append(args, "--host", s.serverHost(serverName), "--app", appName)
	if u := s.serverUser(serverName); u != "" {
		args = append(args, "--user", u)
	}
	// Route through the injected runner (defaults to the real CLI) rather than
	// calling cli.RunChecked directly, so every app-scoped endpoint is
	// testable — which is what Config.CLIRunner exists for. cli.CheckExit
	// keeps RunChecked's rule that a non-zero exit is an error, so behavior is
	// unchanged.
	result, err := s.runCLI(context.Background(), args...)
	if err != nil {
		return result, err
	}
	return result, cli.CheckExit(result, args)
}

// ── App Actions ──────────────────────────────────────────────────────────

// handleAppAction handles /api/apps/{server}/{app}/{action}. Mutations are
// delegated to the CLI, with long-running actions tracked as operations.
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
	// env values and kv values are both secret-shaped payloads; keep them out
	// of any intermediary cache.
	if action == "env" || strings.HasPrefix(action, "env/") ||
		action == "kv" || strings.HasPrefix(action, "kv/") {
		noStore(w)
	}

	// Reject anything that isn't a plain identifier BEFORE it reaches an SSH
	// shell command or a CLI delegate. server/app names are interpolated into
	// remote `docker` invocations; without this a name like `x'; rm -rf / #`
	// would be remote code execution as the SSH user (root) on the fleet.
	if !store.ValidID(serverName) || !validAppName(appName) {
		writeError(w, "invalid server or app name")
		return
	}

	// Up-front existence check for every action so an unconfigured server name
	// returns a clear "server not found" instead of an opaque SSH/CLI error.
	if _, ok := s.lookupServer(serverName); !ok {
		writeError(w, "server not found: "+serverName)
		return
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
		apps, err := s.readMachineApps(r.Context(), srv)
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
		result, err := cli.EnvList(s.serverHost(serverName), s.serverUser(serverName), appName)
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
		strictDecode(r, &body)
		if !validEnvKey(body.Key) {
			writeError(w, "invalid env var name")
			return
		}
		result, err := cli.EnvSet(s.serverHost(serverName), s.serverUser(serverName), appName, body.Key, body.Value)
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
		if !validEnvKey(key) {
			writeError(w, "invalid env var name")
			return
		}
		result, err := cli.EnvUnset(s.serverHost(serverName), s.serverUser(serverName), appName, key)
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

	// Drift is read-only and quick, so it answers inline rather than becoming
	// an operation (those are for mutations). --exit-code is deliberately not
	// passed: detected drift is a successful answer, not a command failure.
	case action == "drift" && r.Method == "GET":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(serverName, appName, "drift", "--json")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeRawJSON(w, result.Stdout)

	// Per-container CPU/memory/IO. Read-only and quick, so it answers inline
	// like drift rather than becoming an operation.
	case action == "stats" && r.Method == "GET":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(serverName, appName, "stats", "--json")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeRawJSON(w, result.Stdout)

	// On-demand health probe against the running app. Read-only and quick, so
	// it answers inline like drift and stats. Note this actively probes the
	// app (an HTTP request per attempt, up to the CLI's health timeout) rather
	// than reading recorded state, so it is deliberately NOT part of the app
	// detail page's initial load — it runs when asked for.
	case action == "health" && r.Method == "GET":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(serverName, appName, "health", "--json")
		if err != nil {
			// A failing health check exits non-zero AND prints its JSON verdict.
			// That is an answer, not a transport failure — surface the verdict
			// rather than an error banner, or an unhealthy app looks like a
			// broken dashboard.
			if result != nil && strings.TrimSpace(result.Stdout) != "" {
				writeRawJSON(w, result.Stdout)
				return
			}
			writeError(w, err.Error())
			return
		}
		writeRawJSON(w, result.Stdout)

	// The shared Nucleus KV store. Like drift and stats these answer inline —
	// one SSH round trip each against an already-running accessory container,
	// not a change to deploy state. Handlers live in kv.go; see the file
	// header for why nothing here is cached.
	case action == "kv" && r.Method == "GET":
		s.handleKVList(w, r, serverName, appName)

	case action == "kv/value" && r.Method == "GET":
		s.handleKVGet(w, r, serverName, appName)

	case action == "kv" && r.Method == "POST":
		s.handleKVSet(w, r, serverName, appName)

	case action == "kv" && r.Method == "DELETE":
		s.handleKVDelete(w, r, serverName, appName)

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
	switch action {
	case "stop", "start", "restart":
		if !s.cliInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		s.enqueueOperation(w, r, operation.Request{
			Kind: operation.KindAppLifecycle, Server: serverName, App: appName, Action: action,
		})

	case "rollback":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		s.enqueueOperation(w, r, operation.Request{Kind: operation.KindRollback, Server: serverName, App: appName})

	case "remove":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		var body struct {
			Purge    bool   `json:"purge"`
			Redirect string `json:"redirect"`
		}
		strictDecode(r, &body)
		s.enqueueOperation(w, r, operation.Request{
			Kind: operation.KindRemove, Server: serverName, App: appName,
			Purge: body.Purge, Redirect: body.Redirect,
		})

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
		s.fleet.set(nil)
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
		s.fleet.set(nil)
		writeData(w, result)

	case "maintenance/on", "maintenance/off":
		if !s.cliInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		s.enqueueOperation(w, r, operation.Request{
			Kind: operation.KindMaintenance, Server: serverName, App: appName,
			Action: strings.TrimPrefix(action, "maintenance/"),
		})

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
	// Reject cross-origin WS/SSE connections so a malicious page the operator
	// visits can't open the log stream using cached Basic-Auth creds. A
	// non-browser client (no Origin) is allowed.
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
		return
	}

	// Path: /ws/logs/{server}/{app}
	path := strings.TrimPrefix(r.URL.Path, "/ws/logs/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid path — expected /ws/logs/{server}/{app}", 400)
		return
	}
	serverName, appName := parts[0], parts[1]
	// Same injection guard as handleAppAction — appName reaches a remote shell.
	if !store.ValidID(serverName) || !validAppName(appName) {
		http.Error(w, "invalid server or app name", 400)
		return
	}

	srv, ok := s.lookupServer(serverName)
	if !ok {
		http.Error(w, "server not found: "+serverName, 404)
		return
	}
	process := r.URL.Query().Get("process")
	if process == "" {
		process = "web"
	}
	if !store.ValidID(process) {
		http.Error(w, "invalid process name", http.StatusBadRequest)
		return
	}
	lines, err := strconv.Atoi(r.URL.Query().Get("lines"))
	if err != nil || lines < 1 {
		lines = 200
	}
	if lines > 1000 {
		lines = 1000
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
		remote.StreamLogs(ctx, srv, appName, process, lines, pw) //nolint:errcheck
		return
	}

	// Proper WebSocket upgrade using net/http hijack.
	wsHandler(w, r, func(ctx context.Context, send func(string)) {
		pw := &wsLineWriter{send: send}
		remote.StreamLogs(ctx, srv, appName, process, lines, pw) //nolint:errcheck
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
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if body.Template == "" || body.Domain == "" || body.Server == "" {
		writeError(w, "template, domain, and server are required")
		return
	}

	s.enqueueOperation(w, r, operation.Request{
		Kind: operation.KindTemplateInstall, Server: body.Server,
		Template: body.Template, Domain: body.Domain, Vars: body.Vars,
	})
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
	// The CLI returns a { name: {host, user} } map. Enrich each entry with an
	// "online" flag from a short TCP dial to the SSH port, so the Servers page
	// shows real reachability instead of defaulting every server to offline.
	var servers map[string]map[string]interface{}
	if err := json.Unmarshal([]byte(result.Stdout), &servers); err != nil {
		writeRawJSON(w, result.Stdout) // unknown shape — pass through unchanged
		return
	}
	var wg sync.WaitGroup
	for _, cfg := range servers {
		host, _ := cfg["host"].(string)
		if host == "" {
			continue
		}
		wg.Add(1)
		go func(cfg map[string]interface{}, host string) {
			defer wg.Done()
			cfg["online"] = tcpReachable(host, 2*time.Second)
		}(cfg, host)
	}
	wg.Wait()
	writeData(w, servers)
}

// tcpReachable reports whether host's SSH port accepts a TCP connection within
// timeout. Used as a lightweight liveness probe for the Servers page.
func tcpReachable(host string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (s *Server) handleServerDetail(w http.ResponseWriter, r *http.Request) {
	if !s.cliInstalled() {
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
	case "status":
		srv, ok := s.lookupServer(serverName)
		if !ok {
			writeError(w, "unknown server: "+serverName)
			return
		}
		machineStatus, unsupported, err := s.readMachineServer(r.Context(), serverName)
		if err != nil {
			writeErrorStatus(w, err.Error(), http.StatusBadGateway)
			return
		}
		if unsupported {
			st, err := s.remoteServerStatus(r.Context(), srv)
			if err != nil {
				writeErrorStatus(w, err.Error(), http.StatusBadGateway)
				return
			}
			st.ObservedAt = time.Now().UTC()
			st.Errors = []remote.ObservationError{}
			st.Source = "ssh_fallback"
			writeData(w, st)
			return
		}
		st := mapMachineServer(machineStatus, serverName, time.Now())
		writeData(w, st)
	case "proxy":
		srv, ok := s.lookupServer(serverName)
		if !ok {
			writeError(w, "unknown server: "+serverName)
			return
		}
		machineStatus, unsupported, err := s.readMachineServer(r.Context(), serverName)
		if err != nil {
			writeErrorStatus(w, err.Error(), http.StatusBadGateway)
			return
		}
		if unsupported {
			st, err := s.remoteServerStatus(r.Context(), srv)
			if err != nil {
				writeErrorStatus(w, err.Error(), http.StatusBadGateway)
				return
			}
			writeData(w, fallbackProxy(st, time.Now()))
			return
		}
		writeData(w, mapMachineProxy(machineStatus, time.Now()))
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
	strictDecode(r, &body)

	s.enqueueOperation(w, r, operation.Request{
		Kind: operation.KindDeploy, Server: body.Server, App: body.App,
		Mode: "ad-hoc", Image: body.Image, Domain: body.Domain, Port: body.Port,
	})
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
		if err := strictDecode(r, &body); err != nil || body.Name == "" {
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
			if err := strictDecode(r, &body); err != nil || body.Name == "" {
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
		if err := strictDecode(r, &body); err != nil || body.App == "" {
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
		if err := strictDecode(r, &body); err != nil || body.Name == "" {
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
		if err := strictDecode(r, &body); err != nil || body.Name == "" {
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
			if err := strictDecode(r, &body); err != nil || body.App == "" {
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
			User string `json:"user"`
			Role string `json:"role"`
		}
		strictDecode(r, &body)
		result, err := cli.ServerAdd(body.Name, body.Host, body.User, body.Role)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, result)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// lookupServerUserRole reads a server's configured SSH user + role from the
// CLI's servers.yml (the source of truth, not the 60s fleet cache). Used to
// preserve those fields on an edit that doesn't re-specify them.
func (s *Server) lookupServerUserRole(name string) (user, role string) {
	result, err := cli.ServerList()
	if err != nil {
		return "", ""
	}
	var raw map[string]struct {
		User string `json:"user"`
		Role string `json:"role"`
	}
	if json.Unmarshal([]byte(result.Stdout), &raw) != nil {
		return "", ""
	}
	if srv, ok := raw[name]; ok {
		return srv.User, srv.Role
	}
	return "", ""
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
		if err := strictDecode(r, &body); err != nil || body.Host == "" {
			writeError(w, "host is required")
			return
		}
		newName := body.Name
		if newName == "" {
			newName = name
		}
		// Edit is remove+add, which would drop the SSH user/role if the form
		// didn't resend them — silently downgrading a non-root server back to
		// root. Preserve the existing values (read from servers.yml, not the
		// cache) when the form leaves a field blank; a form value wins.
		exUser, exRole := s.lookupServerUserRole(name)
		user := body.User
		if user == "" {
			user = exUser
		}
		role := body.Role
		if role == "" {
			role = exRole
		}
		// Only remove when renaming. ServerAdd is an upsert, so a same-name edit
		// updates in place and keeps the server's tags/vpn_ip. Doing remove+add
		// as two processes for a same-name edit would drop tags/vpn_ip, because
		// the re-add reads servers.yml after the remove already deleted them.
		if newName != name {
			// Capture the original host before removing so we can restore the
			// server if the re-add under the new name fails (otherwise a failed
			// rename silently loses the server config entirely).
			origHost := body.Host
			if orig, ok := s.lookupServer(name); ok && orig.Host != "" {
				origHost = orig.Host
			}
			if _, err := cli.ServerRemove(name); err != nil {
				writeError(w, err.Error())
				return
			}
			if _, err := cli.ServerAdd(newName, body.Host, user, role); err != nil {
				// Restore the original entry rather than leave the server lost.
				_, _ = cli.ServerAdd(name, origHost, user, role)
				writeError(w, "rename failed (original server restored): "+err.Error())
				return
			}
			writeData(w, map[string]string{"status": "updated"})
			return
		}
		if _, err := cli.ServerAdd(newName, body.Host, user, role); err != nil {
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
	// 0600: the file holds the SMTP password.
	return os.WriteFile(path, raw, 0600)
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	switch r.Method {
	case "GET":
		cfg := loadNotificationsConfig()
		// Never return the SMTP password to the client; expose only whether one
		// is configured.
		writeData(w, map[string]any{
			"webhook_url":   cfg.WebhookURL,
			"smtp_host":     cfg.SMTPHost,
			"smtp_port":     cfg.SMTPPort,
			"smtp_user":     cfg.SMTPUser,
			"smtp_pass_set": cfg.SMTPPass != "",
			"email_to":      cfg.EmailTo,
			"email_from":    cfg.EmailFrom,
		})
	case "POST":
		var cfg alert.Config
		if err := strictDecode(r, &cfg); err != nil {
			writeError(w, "invalid request body")
			return
		}
		// Preserve the stored password when the client submits an empty value
		// (the GET never reveals it, so the form can't round-trip it).
		if cfg.SMTPPass == "" {
			cfg.SMTPPass = loadNotificationsConfig().SMTPPass
		}
		if err := saveNotificationsConfig(cfg); err != nil {
			writeError(w, err.Error())
			return
		}
		// Update the runners' alert dispatchers with the new config.
		if s.monitor != nil {
			s.monitor.SetAlerter(alert.New(cfg))
		}
		if s.restore != nil {
			s.restore.SetAlerter(alert.New(cfg))
		}
		writeData(w, map[string]bool{"saved": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleRegistries(w http.ResponseWriter, r *http.Request) {
	noStore(w)
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
		strictDecode(r, &body)
		// Pass the password over stdin (--token reads it there) instead of on
		// the argv, where it would be visible in the host's process list.
		result, err := cli.RunWithStdin(body.Password, "registry", "login", body.Server, "--username", body.Username, "--token")
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
		result, err := cli.RunChecked("registry", "remove", server)
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
		if err := strictDecode(r, &m); err != nil {
			http.Error(w, "invalid request body", 400)
			return
		}
		// Validate at the boundary. The ID becomes a filename in the file
		// store, so a bad ID is a path-traversal / arbitrary-write vector —
		// reject anything outside [A-Za-z0-9_-]. POST is also the edit path, so
		// the ID must always be present. (Also validates type/target.)
		if !store.ValidID(m.ID) {
			http.Error(w, "invalid monitor id (use letters, digits, '_' or '-')", 400)
			return
		}
		if strings.TrimSpace(m.Target) == "" {
			http.Error(w, "monitor target is required", 400)
			return
		}
		switch m.Type {
		case "http", "tcp", "ping":
		default:
			http.Error(w, "monitor type must be http, tcp, or ping", 400)
			return
		}
		// A monitor should only ever issue a safe HTTP method — never a
		// destructive verb against the monitored endpoint.
		switch strings.ToUpper(m.Method) {
		case "", "GET", "HEAD", "POST":
		default:
			http.Error(w, "monitor method must be GET, HEAD, or POST", 400)
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
		if !store.ValidID(id) {
			http.Error(w, "invalid monitor id", 400)
			return
		}
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
	if !store.ValidID(id) {
		http.Error(w, "invalid monitor id", 400)
		return
	}

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
		// Stop the checker first so it can't record a check mid-delete, then
		// remove the persisted config. Without this the ticker + goroutine
		// leaked and kept monitoring a deleted monitor.
		if s.monitor != nil {
			s.monitor.Remove(id)
		}
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
	capabilities := s.capabilities(r.Context())
	version := capabilities.CLI.Version
	if version != "" {
		version = "teploy " + version
	}
	writeJSON(w, map[string]interface{}{
		"installed": capabilities.CLI.Installed,
		"version":   version,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	backend := s.config.Backend
	if backend == "" {
		backend = "file"
	}
	writeJSON(w, map[string]string{"status": "ok", "backend": backend})
}

// teployNav returns the cross-product dashboard switcher entries: the current
// app (marked, no link) plus any sibling Teploy dashboards whose URL is
// configured via TEPLOY_NAV_{DASH,OBSERVE,SHIP}_URL. Same env convention across
// Dash, Observe, and Ship, so one set of vars drives the switcher everywhere.
func (s *Server) teployNav(current string) map[string]interface{} {
	products := []struct{ key, label, env string }{
		{"dash", "Dash", "TEPLOY_NAV_DASH_URL"},
		{"observe", "Observe", "TEPLOY_NAV_OBSERVE_URL"},
		{"ship", "Ship", "TEPLOY_NAV_SHIP_URL"},
	}
	apps := make([]map[string]string, 0, len(products))
	for _, p := range products {
		if p.key == current {
			apps = append(apps, map[string]string{"key": p.key, "label": p.label, "url": ""})
			continue
		}
		// An explicit URL always wins: the operator may front a product with a
		// domain, a tunnel, or a port this dashboard can't infer.
		url := strings.TrimSpace(os.Getenv(p.env))
		if url == "" {
			url = s.discoverSibling(p.key)
		}
		if url != "" {
			apps = append(apps, map[string]string{"key": p.key, "label": p.label, "url": url})
		}
	}
	return map[string]interface{}{"current": current, "apps": apps}
}

// discoverSibling finds a sibling product already deployed on this fleet, so the
// switcher configures itself for the common case where all three were deployed
// with teploy. Reads only the warm fleet cache — never triggers an SSH sweep, so
// nav stays cheap; a cold cache simply means no inferred URL until the next
// fleet refresh.
func (s *Server) discoverSibling(product string) string {
	fleet := s.fleet.snapshot()
	if len(fleet) == 0 {
		return ""
	}
	return discoverSiblingURL(product, fleet, func(server string) string {
		if srv, found := s.lookupServer(server); found {
			return srv.Host
		}
		return ""
	})
}

// discoverSiblingURL is the pure core of sibling discovery: it derives a URL
// from fleet state alone, so it is directly testable without SSH or the CLI.
func discoverSiblingURL(product string, fleet []remote.AppState, hostOf func(server string) string) string {
	for _, app := range fleet {
		if app.Status != "running" || !isProductApp(app.App, product) {
			continue
		}
		// A real domain is the best URL: it survives the app moving servers.
		for _, d := range strings.Split(app.Domain, ",") {
			if d = strings.TrimSpace(d); d != "" && !isPlaceholderDomain(d) {
				return "https://" + d
			}
		}
		// No usable domain (ingress: host, or a docs-placeholder domain) — fall
		// back to the server's own address and published port.
		if host := hostOf(app.Server); host != "" && app.CurrentPort > 0 {
			return fmt.Sprintf("http://%s:%d", host, app.CurrentPort)
		}
	}
	return ""
}

// isProductApp reports whether a deployed app name denotes the given Teploy
// product. Deliberately narrow — an app merely containing "ship" (say,
// "shipping-api") is not Teploy Ship.
func isProductApp(appName, product string) bool {
	switch strings.ToLower(strings.TrimSpace(appName)) {
	case product, "teploy-" + product:
		return true
	}
	return false
}

// isPlaceholderDomain reports whether a domain is one of the reserved
// documentation names (RFC 2606) rather than a real host. Teploy's own sample
// configs ship `observe.example.com`, and linking the switcher at that would
// send the operator nowhere.
func isPlaceholderDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	for _, suffix := range []string{".example.com", ".example.net", ".example.org", ".example", ".invalid", ".test", ".localhost", ".local"} {
		if strings.HasSuffix(d, suffix) {
			return true
		}
	}
	return d == "localhost"
}

// handleNav serves the cross-product dashboard switcher config.
func (s *Server) handleNav(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	writeJSON(w, s.teployNav("dash"))
}

// ── Frontend ──────────────────────────────────────────────────────────────

// handleLoginPage serves the standalone login.html page. An already-signed-in
// visitor is sent to the dashboard instead: /login is auth-exempt so the page
// would otherwise render a sign-in form to someone whose session works fine,
// which reads as being signed out.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.gate != nil {
		if cookie, err := r.Cookie(sessionCookie); err == nil {
			if _, ok := s.gate.lookupSession(cookie.Value); ok {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
		}
	}
	s.serveStandalonePage(w, "login.html")
}

// handleSetupPage serves the standalone setup.html page.
func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	// If already configured, redirect to login.
	if s.gate != nil {
		s.gate.credMu.RLock()
		inSetup := s.gate.setupRequired
		s.gate.credMu.RUnlock()
		if !inSetup {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
	}
	s.serveStandalonePage(w, "setup.html")
}

func (s *Server) serveStandalonePage(w http.ResponseWriter, name string) {
	if s.frontend == nil {
		http.Error(w, "frontend not embedded", http.StatusInternalServerError)
		return
	}
	data, err := fs.ReadFile(s.frontend, name)
	if err != nil {
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

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

// ── Homepage ──────────────────────────────────────────────────────────────

type HomepageItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
	// Pinned surfaces the shortcut as an icon in the header, right side. One
	// list backs both the Home grid and the header so a service (Forgejo,
	// Proxmox, TrueNAS) is only ever entered once.
	Pinned bool `json:"pinned,omitempty"`
	// Hidden keeps the shortcut off the Home grid. With Pinned, that makes a
	// header-only link; the two surfaces are independent.
	Hidden bool `json:"hidden,omitempty"`
	// DarkIcon marks a favicon that is dark on transparent (GitHub's mark, for
	// one) so the UI can brighten it on dark backgrounds. Not detectable in the
	// browser: favicons are cross-origin, which taints the canvas.
	DarkIcon bool `json:"dark_icon,omitempty"`
	// Icon is SVG path data on a 24x24 viewBox, drawn in the current text
	// colour instead of the site's favicon — so the mark tracks the theme
	// (white on dark, black on light) rather than carrying its own background.
	// Some hosts have a built-in glyph and need nothing here.
	Icon string `json:"icon,omitempty"`
}

type homepageData struct {
	Items []HomepageItem `json:"items"`
}

func (s *Server) homepageFilePath() string {
	return filepath.Join(s.config.DataDir, "homepage.json")
}

func (s *Server) loadHomepage() (homepageData, error) {
	raw, err := os.ReadFile(s.homepageFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return homepageData{Items: []HomepageItem{}}, nil
		}
		return homepageData{}, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return homepageData{Items: []HomepageItem{}}, nil
	}
	var data homepageData
	if err := json.Unmarshal(raw, &data); err != nil {
		return homepageData{}, err
	}
	if data.Items == nil {
		data.Items = []HomepageItem{}
	}
	return data, nil
}

func (s *Server) saveHomepage(data homepageData) error {
	path := s.homepageFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0644)
}

func (s *Server) handleHomepage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		data, err := s.loadHomepage()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, data.Items)
	case "PUT":
		var items []HomepageItem
		if err := strictDecode(r, &items); err != nil {
			writeError(w, "invalid JSON: "+err.Error())
			return
		}
		if items == nil {
			items = []HomepageItem{}
		}
		if err := s.saveHomepage(homepageData{Items: items}); err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, items)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────

// strictDecode decodes exactly one JSON object from r.Body into dst,
// rejecting unknown fields and a second concatenated JSON value. The
// per-request body-size cap is already applied globally by
// limitMutationBodies (see handler()), so this only tightens what shape is
// accepted, not how much is read. DASH-008: request handlers previously used
// a bare json.NewDecoder(...).Decode(), which silently accepted mistyped
// client fields and multiple concatenated JSON values.
func strictDecode(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func writeData(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
}

func writeError(w http.ResponseWriter, msg string) {
	writeErrorStatus(w, msg, http.StatusBadRequest)
}

func writeErrorStatus(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

// validEnvKey enforces POSIX env var naming (^[A-Za-z_][A-Za-z0-9_]*$). This
// refuses invalid names outright and, as a side effect, prevents a leading-dash
// name from being mis-parsed as a flag when passed to the teploy CLI.
func validEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false // can't start with a digit
			}
		default:
			return false
		}
	}
	return true
}

// writeRawJSON wraps a delegated CLI command's stdout (expected to be JSON,
// since the caller passed --json) as {"data": ...}. Unparseable output means
// the CLI produced something other than the JSON its own flag promised — a
// version mismatch, a stray warning on stdout, or corrupted output — so it is
// reported as a typed 502 rather than concatenated raw into the response
// body, which could itself produce invalid JSON (or, if the CLI output were
// ever attacker-influenced, a response-shape injection).
func writeRawJSON(w http.ResponseWriter, raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		writeData(w, nil)
		return
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		writeErrorStatus(w, "delegated command returned non-JSON output", http.StatusBadGateway)
		return
	}
	writeData(w, parsed)
}

// ── Restore Tests ─────────────────────────────────────────────────────────

// bucketPattern matches safe S3 bucket/region values for CLI argv use —
// mirrors the CLI's own backup.ValidateBucket charset.
var bucketPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func (s *Server) handleRestoreTests(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, []interface{}{})
		return
	}

	switch r.Method {
	case "GET":
		tests, err := s.store.ListRestoreTests()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if tests == nil {
			tests = []store.RestoreTest{}
		}
		writeJSON(w, tests)

	case "POST":
		var t store.RestoreTest
		if err := strictDecode(r, &t); err != nil {
			http.Error(w, "invalid request body", 400)
			return
		}
		// Every field below reaches the teploy CLI's argv (and from there a
		// remote shell), so validate all of them at the boundary — same rule
		// as monitors/app actions.
		if !store.ValidID(t.ID) {
			http.Error(w, "invalid restore test id (use letters, digits, '_' or '-')", 400)
			return
		}
		if !store.ValidID(t.Server) || !store.ValidID(t.App) || !store.ValidID(t.Accessory) {
			http.Error(w, "server, app, and accessory are required (letters, digits, '_' or '-')", 400)
			return
		}
		if !bucketPattern.MatchString(t.Bucket) || len(t.Bucket) > 63 {
			http.Error(w, "invalid bucket name", 400)
			return
		}
		if t.Region == "" {
			t.Region = "us-east-1"
		}
		if !bucketPattern.MatchString(t.Region) || len(t.Region) > 25 {
			http.Error(w, "invalid region", 400)
			return
		}
		if t.IntervalHours < 1 {
			t.IntervalHours = 24
		}
		// Preserve the last result across config edits: the client only
		// round-trips config fields, and an upsert that zeroed the result
		// columns would show "never run" after every edit.
		if prev, err := s.store.GetRestoreTest(t.ID); err == nil && prev != nil {
			t.LastRunAt = prev.LastRunAt
			t.LastOK = prev.LastOK
			t.LastDetail = prev.LastDetail
			t.LastMetric = prev.LastMetric
			t.LastDate = prev.LastDate
			t.LastDurationMs = prev.LastDurationMs
		}
		if err := s.store.SaveRestoreTest(t); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if s.restore != nil {
			s.restore.Reload(t)
		}
		writeJSON(w, t)

	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleRestoreTest(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "restore tests not configured (no store)", 404)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/restore-tests/")

	// POST /api/restore-tests/{id}/run — run one verification now,
	// synchronously, persisting the result. Runs download a backup and boot a
	// scratch container, so this can take minutes; the CLI delegate's own
	// timeout backstops a hang.
	if strings.HasSuffix(path, "/run") && r.Method == "POST" {
		id := strings.TrimSuffix(path, "/run")
		if !store.ValidID(id) {
			http.Error(w, "invalid restore test id", 400)
			return
		}
		t, err := s.store.GetRestoreTest(id)
		if err != nil {
			http.Error(w, "restore test not found", 404)
			return
		}
		if s.restore == nil {
			http.Error(w, "restore-test runner not available", 500)
			return
		}
		updated := s.restore.RunNow(*t)
		writeJSON(w, updated)
		return
	}

	id := path
	if !store.ValidID(id) {
		http.Error(w, "invalid restore test id", 400)
		return
	}

	switch r.Method {
	case "GET":
		t, err := s.store.GetRestoreTest(id)
		if err != nil {
			http.Error(w, "restore test not found", 404)
			return
		}
		writeJSON(w, t)

	case "DELETE":
		// Stop the schedule first so a tick can't re-persist a deleted test.
		if s.restore != nil {
			s.restore.Remove(id)
		}
		if err := s.store.DeleteRestoreTest(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]bool{"deleted": true})

	default:
		http.Error(w, "method not allowed", 405)
	}
}
