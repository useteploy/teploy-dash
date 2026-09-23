package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"golang.org/x/crypto/bcrypt"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/durable"
	"github.com/useteploy/teploy-dash/internal/manifest"
	"github.com/useteploy/teploy-dash/internal/mcp"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/outbox"
	"github.com/useteploy/teploy-dash/internal/remote"
	"github.com/useteploy/teploy-dash/internal/restoretest"
	sshclient "github.com/useteploy/teploy-dash/internal/ssh"
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
	// Outbox is the durable alert delivery queue (D08). Optional: when nil,
	// monitor responses carry no delivery status and the monitor runner
	// should have been wired to a direct dispatcher instead.
	Outbox *outbox.Outbox
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
	// Operation retention knobs (useteploy__teploy-dash-04). Zero values take
	// the operation package defaults; negative values disable a bound.
	OperationMaxJournalBytes int64
	OperationMaxHistoryAge   time.Duration
	OperationMaxOperations   int
	// OperationMaxQueued bounds non-terminal operations per target before
	// enqueues are rejected (A12/A27 remainder; 0 = package default,
	// negative disables).
	OperationMaxQueued int
	// OperationMaxLive bounds TOTAL non-terminal operations across targets,
	// and OperationMaxConcurrent bounds simultaneous CLI executions (R17;
	// 0 = package default, negative disables).
	OperationMaxLive       int
	OperationMaxConcurrent int
	// OperationIdempotencyWindow bounds how long an admitted idempotency key
	// is honored, measured from the admitted operation's creation (D02
	// namespacing; 0 = package default 24h, negative disables expiry).
	OperationIdempotencyWindow time.Duration
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
	// observations is the per-server envelope sweep (D01): one envelope per
	// configured server, present whether the probe succeeded or not.
	// obsBuiltAt tracks when the envelope sweep ran; the apps caches track
	// when a SUCCESSFUL app list was gathered — a fully-degraded sweep
	// refreshes the envelopes but must not clobber the last-known app list.
	observations []ServerObservation
	obsBuiltAt   time.Time
	// generation is the invalidation token (R50): a refresh captures it
	// before collecting and publishes only if it still matches — a sweep
	// that started BEFORE a mutation (which invalidates) must not re-publish
	// its stale snapshot as freshly built afterwards.
	generation uint64
	// lastGood survives both TTL expiry and invalidation. The TTL exists to keep
	// app *status* fresh; consumers that only read stable facts (where a sibling
	// dashboard lives) want the last known answer rather than none.
	lastGood    []remote.AppState
	lastGoodObs []ServerObservation
	// refreshing is a single-flight latch: a background refresh SSHes every
	// server, so concurrent stale reads must not each start their own sweep.
	refreshing bool
}

// beginRefresh claims the right to run a background refresh. Returns false when
// one is already in flight, so callers simply serve what they have. R50: it
// also returns the CURRENT generation, which the refresh must present to
// publish — a stale generation is dropped.
func (fc *fleetCache) beginRefresh() (uint64, bool) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.refreshing {
		return 0, false
	}
	fc.refreshing = true
	return fc.generation, true
}

func (fc *fleetCache) endRefresh() {
	fc.mu.Lock()
	fc.refreshing = false
	fc.mu.Unlock()
}

// snapshotGeneration reads the current generation WITHOUT claiming a refresh
// (for synchronous cold reads).
func (fc *fleetCache) snapshotGeneration() uint64 {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.generation
}

// publish stores a completed sweep only when its captured generation is
// still current (R50). The envelope cache always refreshes (a degraded sweep
// is the current truth about the fleet); the app caches refresh only when the
// sweep produced a successful app list — a zero-success sweep must leave the
// last-known apps serving, exactly as a failed refresh did before envelopes
// existed. Reports whether the snapshot was published.
func (fc *fleetCache) publish(generation uint64, envelopes []ServerObservation) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if generation != fc.generation {
		return false
	}
	if envelopes == nil {
		fc.apps = nil
		fc.builtAt = time.Time{} // zero time forces cache miss on next read
		fc.observations = nil
		fc.obsBuiltAt = time.Time{}
		return true
	}
	now := time.Now()
	fc.observations = envelopes
	fc.obsBuiltAt = now
	fc.lastGoodObs = envelopes
	apps := flattenFleetApps(envelopes)
	if len(apps) > 0 || !fleetAllFailed(envelopes) {
		fc.apps = apps
		fc.builtAt = now
		fc.lastGood = apps
	}
	return true
}

// invalidate drops the cached snapshot and advances the generation so any
// in-flight refresh cannot republish its pre-mutation view (R50).
func (fc *fleetCache) invalidate() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.generation++
	fc.apps = nil
	fc.builtAt = time.Time{}
	fc.observations = nil
	fc.obsBuiltAt = time.Time{}
}

// snapshot returns the last successfully collected fleet regardless of age.
func (fc *fleetCache) snapshot() []remote.AppState {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.lastGood
}

// snapshotObservations returns the last collected envelope sweep regardless
// of age — the per-server last-known state that keeps unreachable hosts
// visible in /api/fleet responses.
func (fc *fleetCache) snapshotObservations() []ServerObservation {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.lastGoodObs
}

func (fc *fleetCache) get() ([]remote.AppState, bool) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	if time.Since(fc.builtAt) > fc.ttl {
		return nil, false
	}
	return fc.apps, true
}

func (fc *fleetCache) getObservations() ([]ServerObservation, bool) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	if time.Since(fc.obsBuiltAt) > fc.ttl {
		return nil, false
	}
	return fc.observations, true
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
	outbox             *outbox.Outbox
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

	// homepageMu serializes the homepage load-compare-save sequence (R13).
	homepageMu sync.Mutex
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
		outbox:   config.Outbox,
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
		// R01: restore runs resolve ONE registered target through a
		// fail-closed lookup — an unregistered alias or broken discovery
		// fails the run instead of silently falling back to the alias as a
		// hostname or the CLI's root user.
		s.restore.SetTargetResolver(func(name string) (restoretest.Target, error) {
			srv, err := s.lookupServerStrict(context.Background(), name)
			if err != nil {
				return restoretest.Target{}, err
			}
			return restoretest.Target{Host: srv.Host, User: srv.User}, nil
		})
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
			defer s.fleet.invalidate()
			return executeOperation(ctx, command, emit)
		}
	}
	// A11/UPSTREAM-1: template variables ride the CLI's --var-stdin contract
	// when the installed CLI has it. F015: the probe reports unverified
	// states as errors, and Build fails closed for secret-bearing installs
	// rather than caching a transient failure as "unsupported" (argv
	// fallback only for a VERIFIED unsupported CLI). R03: a verified
	// unsupported CLI refuses secret values outright unless the operator
	// opted into the legacy argv transport.
	operation.SetVarStdinSupport(cli.VarStdinSupport)
	operation.SetLegacySecretArgVAllowed(cli.LegacySecretArgVAllowed)
	s.operations, s.operationInitErr = operation.New(config.DataDir, operation.Options{
		MaxEvents:               config.OperationMaxEvents,
		MaxJournalBytes:         config.OperationMaxJournalBytes,
		MaxHistoryAge:           config.OperationMaxHistoryAge,
		MaxOperations:           config.OperationMaxOperations,
		MaxQueuedPerTarget:      config.OperationMaxQueued,
		MaxLiveOperations:       config.OperationMaxLive,
		MaxConcurrentExecutions: config.OperationMaxConcurrent,
		IdempotencyWindow:       config.OperationIdempotencyWindow,
		Resolver:                resolver,
		ProjectResolver: func(server, app, revision string) (string, error) {
			if s.manifests == nil {
				return "", fmt.Errorf("manifest service unavailable")
			}
			return s.manifests.ProjectDir(server, app, revision)
		},
		Executor: executor,
		// D02: reconciliation and honest cancellation outcomes read the
		// target's receipts through the same machine interface the fleet
		// uses. The method value resolves s.runCLI at call time.
		ReceiptReader: s.operationReceipt,
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

// Serve runs the HTTP server on an ALREADY-BOUND listener (R39): main binds
// before starting background services, so a failed bind cannot leave
// monitors, restore schedules, and cleanup goroutines racing an early exit.
func (s *Server) Serve(ln net.Listener) error {
	s.warmFleet()
	srv := s.httpServer(ln.Addr().String())
	s.httpSrvMu.Lock()
	s.httpSrv = srv
	s.httpSrvMu.Unlock()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
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

// CloseHTTP force-closes all remaining HTTP connections (F022). Called when
// the bounded drain timed out: continuing with live stragglers lets a
// request keep mutating (or admitting operations) while the rest of the
// shutdown sequence closes stores underneath it.
func (s *Server) CloseHTTP() {
	s.httpSrvMu.Lock()
	srv := s.httpSrv
	s.httpSrvMu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// DrainOperations joins in-flight operation work, bounded by ctx (A39/A47):
// first a graceful drain until ctx expires, then a force-cancel plus a fixed
// grace so terminal states persist. Call after Shutdown so no new operations
// are admitted while draining, and before closing the store.
func (s *Server) DrainOperations(ctx context.Context) {
	if s.operations == nil {
		return
	}
	s.operations.Shutdown(ctx)
}

// refreshFleetAsync refreshes the fleet behind a request. Single-flighted, so a
// burst of stale reads causes one sweep, not one per request. Uses a background
// context: the refresh must outlive the request that triggered it, or a client
// navigating away would cancel it and the cache would never re-warm.
func (s *Server) refreshFleetAsync() {
	if !cli.IsInstalled() {
		return
	}
	generation, claimed := s.fleet.beginRefresh()
	if !claimed {
		return
	}
	go func() {
		defer s.fleet.endRefresh()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		envelopes, err := s.collectFleetObservations(ctx, s.fleet.snapshotObservations())
		if err != nil {
			log.Printf("[fleet] background refresh failed (serving last known state): %v", err)
			return
		}
		if fleetAllFailed(envelopes) {
			log.Printf("[fleet] background refresh observed errors on all %d server(s) (serving last known state)", len(envelopes))
		}
		if !s.fleet.publish(generation, envelopes) {
			log.Printf("[fleet] background refresh discarded: the fleet changed while it was running")
		}
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
	// R50: warmFleet shares the single-flight latch with the stale-refresh
	// path instead of bypassing it — a cold request burst at startup
	// previously launched one full sweep per request.
	generation, claimed := s.fleet.beginRefresh()
	if !claimed {
		return
	}
	go func() {
		defer s.fleet.endRefresh()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		envelopes, err := s.collectFleetObservations(ctx, nil)
		if err != nil {
			log.Printf("[fleet] startup warm failed (will fill on first request): %v", err)
			return
		}
		if fleetAllFailed(envelopes) {
			log.Printf("[fleet] startup warm observed errors on all %d server(s)", len(envelopes))
		}
		if !s.fleet.publish(generation, envelopes) {
			log.Printf("[fleet] startup warm discarded: the fleet changed while it was running")
		}
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
	if s.gate != nil {
		handler = s.gate.wrap(handler)
	}
	// R04: browser-origin protection on state-changing requests lives
	// OUTSIDE the optional auth gate. With --no-auth the gate is absent and
	// mutations previously had no origin check at all — any page that could
	// reach the loopback listener could drive state changes. (Authenticated
	// modes are checked twice — here and in the gate — which is harmless.)
	handler = guardBrowserMutations(handler)
	// Body limits apply to every mode before any decoding, including the
	// gate's own login/setup handlers.
	handler = limitMutationBodies(handler)
	// R10: the hardening headers are the OUTERMOST middleware so they also
	// cover responses generated by the auth gate itself (401/403/503) —
	// they previously ran inside the gate and were skipped exactly on the
	// login/authorization error paths.
	return baselineHeaders(handler)
}

// guardBrowserMutations rejects cross-origin state-changing requests
// independently of authentication (R04).
func guardBrowserMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMutating(r.Method) && !sameOrigin(r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// baselineHeaders sets the conservative response hardening baseline: nosniff,
// no framing, no referrer leakage, and a CSP that blocks framing/object/base
// abuse without constraining the bundled Alpine/inline-script frontend (a
// stricter script-src needs an Alpine configuration change first — A10).
func baselineHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'; object-src 'none'; base-uri 'self'")
		next.ServeHTTP(w, r)
	})
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
	// On-disk users (bcrypt hashes + role) and external (SSO) principals.
	// Both protected by credMu; both share the store-wide epoch counter.
	usersFile      string // users.json — canonical multi-user + principal store
	legacyFile     string // auth.json — single-user file migrated on first load
	credMu         sync.RWMutex
	users          map[string]*dashUser
	oidcPrincipals map[string]*dashPrincipal
	epochCounter   uint64 // monotonic AuthEpoch source; guarded by credMu
	setupRequired  bool
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
	// setupMu serializes first-run setup: the setup-required check and the
	// account creation must be one critical section, or two holders of the
	// bootstrap token could race past the check and each create a different
	// initial admin (A03).
	setupMu sync.Mutex
	// initErr is set when the credential store exists but cannot be loaded
	// (unreadable/corrupt users.json, corrupt legacy auth.json). The gate then
	// refuses every request except /api/health — an authentication outage is
	// not setup mode and must not fall back to older credentials (A04).
	initErr error
}

// bootstrapTokenTTL bounds how long a printed setup token remains valid.
// Long enough for an operator to copy it from the log and finish setup in one
// sitting; short enough that a token from a log an operator forgot about
// isn't a standing credential.
const bootstrapTokenTTL = 30 * time.Minute

// sessionInfo is one live session: which principal (sub), what display name,
// what role, and when it expires. epoch is the principal's AuthEpoch at
// issuance; every request revalidates it against the live principal row —
// local account or OIDC identity alike — so a session issued against revoked
// state stops working on its next use (A02/A03). For local sessions sub is
// the username; for SSO sessions it is the issuer-namespaced principal id.
type sessionInfo struct {
	sub   string
	user  string
	role  string
	exp   time.Time
	epoch uint64
	local bool
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
		oidcPrincipals: make(map[string]*dashPrincipal),
		trustedProxies: parseTrustedProxies(os.Getenv("TEPLOY_DASH_TRUSTED_PROXY")),
		fails:          make(map[string]*failInfo),
		sessions:       make(map[string]*sessionInfo),
	}
	// Load stored users. Only a genuinely fresh install (no users.json and no
	// legacy auth.json anywhere) with no env-var password enters setup mode.
	// A load failure on an EXISTING store is an authentication outage: the
	// gate keeps serving 503s (see wrap) rather than silently falling back to
	// legacy credentials or an open setup flow (A04).
	if err := g.loadUsers(); err != nil {
		if errors.Is(err, errNoUsers) && pass == "" {
			g.setupRequired = true
			g.bootstrapToken = generateBootstrapToken()
			g.bootstrapTokenExpiry = time.Now().Add(bootstrapTokenTTL)
			log.Printf("=====================================================================")
			log.Printf("First-run setup required. Bootstrap token (valid %s): %s", bootstrapTokenTTL, g.bootstrapToken)
			log.Printf("Enter this token on the /setup page to create the initial admin account.")
			log.Printf("=====================================================================")
		} else if errors.Is(err, errNoUsers) {
			// errNoUsers with an env password set is the env-bootstrap mode:
			// the env credential becomes the FIRST STORED ACCOUNT right now
			// (A02). Materializing it means there is no implicit fallback
			// authentication path left to re-enable if the account is later
			// deleted — deleting it removes the identity entirely. The
			// env-var password is intentionally not held to the 8-character
			// policy: it is the operator's pre-issued credential and the old
			// implicit path accepted it at any length.
			name := strings.TrimSpace(user)
			if name == "" {
				name = "admin"
			}
			if err := g.materializeEnvBootstrap(name, pass); err != nil {
				g.initErr = fmt.Errorf("materialize env bootstrap account: %w", err)
				log.Printf("auth: %v — refusing logins", g.initErr)
			} else {
				log.Printf("auth: environment bootstrap account %q created (later TEPLOY_DASH_PASSWORD changes no longer reset it; change the password from Settings)", name)
			}
		} else {
			g.initErr = err
			log.Printf("auth: credential store unavailable — refusing logins: %v", err)
		}
	}
	return g
}

// materializeEnvBootstrap writes the env credential's account directly (no
// minimum-length check — see newAuthGate). Caller has confirmed no users
// store exists at all.
func (g *authGate) materializeEnvBootstrap(name, pass string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcryptCost)
	if err != nil {
		return err
	}
	g.credMu.Lock()
	defer g.credMu.Unlock()
	candidate := cloneUsersLocked(g.users)
	candidate[name] = &dashUser{Username: name, PasswordHash: string(hash), Role: RoleAdmin, AuthEpoch: 1}
	if err := saveUsersFile(g.usersFile, candidate, g.oidcPrincipals, 1); err != nil {
		return err
	}
	g.epochCounter = 1
	g.users = candidate
	g.setupRequired = false
	return nil
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
	// Snapshot the account's current epoch so the session can be revalidated
	// against later credential/role changes (A03). Absent accounts (tests,
	// external principals) get epoch 0 and are treated as non-local below
	// when no matching account exists at validation time.
	g.credMu.RLock()
	epoch := uint64(0)
	local := false
	if u, ok := g.users[user]; ok {
		epoch = u.AuthEpoch
		local = true
	}
	g.credMu.RUnlock()
	return g.newSessionFor(user, user, role, epoch, local)
}

// newSessionFor issues a session for one principal (sub) with an explicit
// epoch/local pair. Login uses the epoch captured by authenticate (the same
// read that verified the hash), and the OIDC callback uses the epoch captured
// by upsertOIDCPrincipal's locked persistence — both close the issuance race
// where a revocation lands between verification and session creation (A02).
func (g *authGate) newSessionFor(sub, user, role string, epoch uint64, local bool) string {
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
	g.sessions[token] = &sessionInfo{sub: sub, user: user, role: normalizeRole(role), exp: now.Add(sessionTTL), epoch: epoch, local: local}
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

// deleteUserSessions invalidates every live session belonging to one
// principal (local account or SSO identity) — used when their password or
// role changes, the account is removed, or an admin revokes sessions. The
// durable revocation is the epoch bump; this in-memory wipe is cleanup.
func (g *authGate) deleteUserSessions(sub string) {
	g.sessMu.Lock()
	defer g.sessMu.Unlock()
	for k, si := range g.sessions {
		if si.sub == sub {
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
		// Credential-store outage: refuse everything except the liveness
		// probe. Serving logins against a stale/legacy fallback or opening
		// setup mode would be worse than a clear 503.
		if g.initErr != nil && r.URL.Path != "/api/health" && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			jsonError(w, "authentication store unavailable — check server logs", http.StatusServiceUnavailable)
			return
		}

		// Setup mode: no credentials configured yet. Only let through the
		// setup page and its API endpoint.
		g.credMu.RLock()
		inSetup := g.setupRequired
		g.credMu.RUnlock()
		if inSetup {
			// When SSO is configured, setup mode is never entered (New clears it),
			// so this branch only runs for the local-account first-run flow.
			switch r.URL.Path {
			case "/api/health", "/healthz", "/readyz", "/setup", "/api/setup":
				// A06: setup is a state-changing route holding the bootstrap
				// token — it gets the same same-origin requirement as every
				// other mutation instead of bypassing the check below.
				if isMutating(r.Method) && !sameOrigin(r) {
					http.Error(w, "cross-origin request blocked", http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
			default:
				if strings.HasPrefix(r.URL.Path, "/api/") {
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
		//
		// Login, logout, and setup are state-changing too (login-CSRF could
		// sign the victim into an attacker-known account), so they get the
		// same same-origin check as authenticated mutations (A05).
		switch r.URL.Path {
		case "/api/health", "/healthz", "/readyz", "/login", "/api/login", "/api/logout", "/api/login/methods",
			"/api/setup",
			"/status", "/api/status", "/api/mcp",
			// The browser requests the tab icon before anyone has signed in; it
			// is a static brand asset and discloses nothing.
			"/favicon.svg", "/favicon.ico":
			if isMutating(r.Method) && !sameOrigin(r) {
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
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
		cookie, cookieErr := r.Cookie(sessionCookie)
		var session *sessionInfo
		if cookieErr == nil {
			session, _ = g.lookupSession(cookie.Value)
		}
		// A live session is validated against the CURRENT principal state
		// before it authorizes anything — unconditionally, for local and
		// SSO sessions alike (A02): the session's epoch must match the
		// principal row's, and its ROLE is read live from the row rather
		// than trusted from issuance time. A missing row (deleted account,
		// revoked principal, or a session from a pre-principal install) is
		// a dead session. This is the security boundary;
		// deleteUserSessions remains only as memory cleanup (A03).
		if session != nil {
			g.credMu.RLock()
			var live *sessionInfo
			if session.local {
				if u := g.users[session.sub]; u != nil && u.AuthEpoch == session.epoch {
					live = &sessionInfo{sub: session.sub, user: session.user, role: normalizeRole(u.Role), exp: session.exp, epoch: session.epoch, local: true}
				}
			} else if p := g.oidcPrincipals[session.sub]; p != nil && p.AuthEpoch == session.epoch {
				live = &sessionInfo{sub: session.sub, user: session.user, role: normalizeRole(p.Role), exp: session.exp, epoch: session.epoch, local: false}
			}
			g.credMu.RUnlock()
			if live == nil {
				g.deleteSession(cookie.Value)
				session = nil
			} else {
				session = live
			}
		}
		// Login throttling keys on the client IP, so applying it before the
		// session check punished already-authenticated users sharing a NAT
		// with someone hammering the login form. Lockout now applies only to
		// unauthenticated traffic (A06).
		if session == nil && g.lockedOut(ip) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				jsonError(w, "too many failed attempts — try again shortly", http.StatusTooManyRequests)
			} else {
				http.Error(w, "too many failed attempts — try again shortly", http.StatusTooManyRequests)
			}
			return
		}
		if session == nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
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
		if isMutating(r.Method) && !sameOrigin(r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}

		// RBAC: enforce the minimum role for this route. Fail closed — a
		// mutating route with no explicit classification requires editor, never
		// viewer, so a new endpoint can't silently be viewer-writable.
		if need := requiredRole(r.Method, r.URL.Path); !roleAllows(session.role, need) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
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
	// Embed the epoch captured by authenticate's single locked read; a reset
	// landing after this point leaves the new session immediately invalid.
	g.issueSessionCookie(w, r, user.Username, user.Username, user.Role, user.AuthEpoch, true)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleSetup creates the initial account. Only works in setup mode. The
// setup-required check and the account creation run under setupMu as one
// critical section so two concurrent bootstrap-token holders cannot both pass
// the precondition and create different initial admins (A03). The request
// body is decoded BEFORE the lock is taken — setup is rare and
// contention-free, but a slow/stalled body must not hold the only writer.
func (g *authGate) handleSetup(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
	g.setupMu.Lock()
	defer g.setupMu.Unlock()
	g.credMu.RLock()
	inSetup := g.setupRequired
	g.credMu.RUnlock()
	if !inSetup {
		jsonError(w, "account already configured", http.StatusConflict)
		return
	}
	if !g.checkBootstrapToken(body.BootstrapToken) {
		jsonError(w, "missing or invalid bootstrap token — check the server log", http.StatusUnauthorized)
		return
	}
	// Canonicalize the username once and use that value for BOTH storage and
	// the session, so a whitespace-padded name can't create two identities.
	body.Username = strings.TrimSpace(body.Username)
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
	g.credMu.RLock()
	epoch := g.users[body.Username].AuthEpoch
	g.credMu.RUnlock()
	g.issueSessionCookie(w, r, body.Username, body.Username, RoleAdmin, epoch, true)
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
	// R08: password self-service operates on LOCAL accounts only. An SSO
	// session's display name can equal a local username even though the
	// principals are deliberately distinct — letting the SSO session reach
	// the local password change conflated two identities (knowledge of the
	// local password was still required, so this was namespace confusion,
	// not a passwordless takeover).
	if !session.local {
		jsonError(w, "change your SSO password at your identity provider", http.StatusForbidden)
		return
	}
	// authenticate returns the account's AuthEpoch alongside the verdict; the
	// change below compares against it, so an administrative reset landing
	// between verification and commit fails this request instead of
	// overwriting the newer credential (A03).
	user, ok := g.authenticate(session.user, body.CurrentPassword)
	if !ok {
		jsonError(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}
	if body.NewPassword != body.ConfirmPassword {
		jsonError(w, "passwords do not match", http.StatusBadRequest)
		return
	}
	// A session identity with no stored account is the (pre-materialization)
	// env-bootstrap admin; their first password change persists the account
	// (admin role). Since A02 the env credential is materialized at startup,
	// so this branch is a compatibility fallback — everyone else is a stored
	// user and gets the compare-and-swap reset (A01/A03).
	var err error
	if user.AuthEpoch != 0 || g.hasStoredUser(session.user) {
		err = g.setPasswordCAS(session.user, user.AuthEpoch, body.NewPassword, true)
	} else {
		err = g.setPasswordMigratingEnv(session.user, body.NewPassword)
	}
	if err != nil {
		if errors.Is(err, ErrStaleEpoch) {
			jsonError(w, err.Error(), http.StatusConflict)
		} else {
			jsonError(w, err.Error(), http.StatusBadRequest)
		}
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

func (g *authGate) issueSessionCookie(w http.ResponseWriter, r *http.Request, sub, user, role string, epoch uint64, local bool) {
	token := g.newSessionFor(sub, user, role, epoch, local)
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

// hasStoredUser reports whether a local account exists under the name.
func (g *authGate) hasStoredUser(name string) bool {
	g.credMu.RLock()
	defer g.credMu.RUnlock()
	_, ok := g.users[name]
	return ok
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
	if fi == nil || now.After(fi.until) {
		// A brand-new window — or one whose lockout already fully expired —
		// starts clean, so a stale count can't re-lock an IP after a single
		// later failure (A07).
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
// header, falls back to comparing the FULL origin (scheme + host + port,
// normalized) against the request's own scheme and host — R05: the previous
// host-only comparison accepted http:// against https:// and any origin that
// merely shared a host string.
func sameOrigin(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin" || sfs == "none"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		got, err := originKey(u)
		if err != nil {
			return false
		}
		expected, err := originKey(&url.URL{Scheme: requestScheme(r), Host: r.Host})
		if err != nil {
			return false
		}
		return got == expected
	}
	return true // no Origin / Fetch-Metadata → not a browser CSRF request
}

// originKey normalizes an origin to a comparable scheme://host:port string
// (R05): HTTP(S) only, no credentials/opaque paths/queries/fragments, host
// lowercased, default ports made explicit.
func originKey(u *url.URL) (string, error) {
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return "", errors.New("invalid origin")
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("invalid origin port")
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(n)), nil
}

// trustedProxyNetsOnce parses TEPLOY_DASH_TRUSTED_PROXY once for scheme
// inference (the authGate keeps its own copy for client-IP extraction; both
// read the same environment).
var (
	trustedProxyNetsOnce sync.Once
	trustedProxyNets     []*net.IPNet
)

func trustedProxyContains(ip net.IP) bool {
	trustedProxyNetsOnce.Do(func() {
		trustedProxyNets = parseTrustedProxies(os.Getenv("TEPLOY_DASH_TRUSTED_PROXY"))
	})
	for _, n := range trustedProxyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// requestScheme infers the connection's scheme for origin comparison: TLS
// directly, or https behind a configured trusted proxy's X-Forwarded-Proto.
// Untrusted forwarded headers are ignored (R05: the scheme is part of the
// origin, so a spoofable scheme would collapse the boundary again).
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); strings.EqualFold(proto, "https") {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			if ip := net.ParseIP(host); ip != nil && trustedProxyContains(ip) {
				return "https"
			}
		}
	}
	return "http"
}

// clientIP returns the address used for per-IP rate-limiting. By default it's
// the direct peer (RemoteAddr) — X-Forwarded-For is NOT trusted, since a client
// could spoof it to evade the backoff. Only when the chain resolves through
// configured trusted proxies (TEPLOY_DASH_TRUSTED_PROXY) is the forwarded
// client IP used. The chain is walked RIGHT to LEFT: each hop is only followed
// while the hop before it is itself a trusted proxy, so a client-supplied
// spoofed entry appended by an honest proxy cannot become the identity (a
// leftmost read would take the attacker's value, A07).
func (g *authGate) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	current := net.ParseIP(host)
	if current == nil || !g.isTrustedProxyIP(current) {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if strings.TrimSpace(xff) == "" {
		return host
	}
	hops := strings.Split(xff, ",")
	if len(hops) > 32 {
		return host
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		next := net.ParseIP(hop)
		if next == nil {
			// Malformed entry: stop at the last valid address rather than
			// trusting anything further left.
			return current.String()
		}
		if !g.isTrustedProxyIP(next) {
			return next.String()
		}
		current = next
	}
	return current.String()
}

func (g *authGate) isTrustedProxy(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return g.isTrustedProxyIP(ip)
}

func (g *authGate) isTrustedProxyIP(ip net.IP) bool {
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
		s.mux.HandleFunc("/api/login/methods", s.handleLoginMethods)
		s.mux.HandleFunc("/api/users", s.handleUsers)
		s.mux.HandleFunc("/api/users/", s.handleUserAction)
		s.mux.HandleFunc("/api/sso", s.handleSSOPrincipals)
		s.mux.HandleFunc("/api/sso/revoke", s.handleSSORevoke)
		if s.gate.oidc != nil {
			s.mux.HandleFunc("/oidc/login", s.gate.handleOIDCLogin)
			s.mux.HandleFunc("/oidc/callback", s.gate.handleOIDCCallback)
		}
	}

	// A05: the identity endpoint exists in EVERY mode. With auth disabled it
	// answers explicitly (mode "disabled", full capabilities) instead of
	// falling through to the SPA, which the settings page read as "viewer"
	// and used to hide administration controls that work fine in no-auth.
	s.mux.HandleFunc("/api/auth/me", s.handleWhoami)

	// Homepage
	s.mux.HandleFunc("/api/homepage", s.handleHomepage)

	// Deployment management
	s.mux.HandleFunc("/api/servers", s.handleServers)
	s.mux.HandleFunc("/api/servers/", s.handleServerDetail)
	s.mux.HandleFunc("/api/apps", s.handleApps)
	s.mux.HandleFunc("/api/apps/", s.handleAppAction)
	// D01: per-server observation envelopes (all servers, every response).
	s.mux.HandleFunc("/api/fleet", s.handleFleet)
	s.mux.HandleFunc("/api/deploy", s.handleDeploy)
	// D03: onboarding preflight — host connection/capability readiness
	// BEFORE app details; the create-entry surfaces the gate verdict.
	s.mux.HandleFunc("/api/onboarding/preflight", s.handleOnboardingPreflight)
	s.mux.HandleFunc("/api/onboarding/entry", s.handleOnboardingEntry)
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

	// Log streaming: SSE-only under /api/ (A24/A32/A33 — the hand-written
	// WebSocket transport and its /ws/ prefix are gone).
	s.mux.HandleFunc("/api/logs/", s.handleLogs)

	// System
	s.mux.HandleFunc("/api/cli/status", s.handleCLIStatus)
	s.mux.HandleFunc("/api/capabilities", s.handleCapabilities)
	s.mux.HandleFunc("/api/nav", s.handleNav)
	s.mux.HandleFunc("/api/health", s.handleHealth)
	// Liveness/readiness probes (A39/A47): unauthenticated by design, like
	// /api/health, so orchestrators can route without a session.
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/readyz", s.handleReadyz)

	// MCP: bearer-authed AI-client endpoint + session-authed token management.
	s.initMCP(s.config.Version)

	// Public status page (opt-in; handlers 404 when disabled). Bypasses auth
	// via the gate allowlist below.
	s.mux.HandleFunc("/status", s.handleStatusPage)
	s.mux.HandleFunc("/api/status", s.handleStatusAPI)

	// A36: /api/ is reserved for JSON APIs. Unmatched API paths must return a
	// real 404 envelope, not the SPA's index.html with HTTP 200 (more
	// specific registrations above still win in ServeMux).
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, "API route not found", http.StatusNotFound)
	})

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

	envelopes, err := s.collectFleetObservations(r.Context(), nil)
	if err != nil {
		writeErrorStatus(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.fleet.publish(s.fleet.snapshotGeneration(), envelopes)
	apps, err := fleetAppsOrError(envelopes)
	if err != nil {
		writeErrorStatus(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeData(w, apps)
}

// handleFleet is the D01 truth-telling endpoint: one observation envelope per
// configured server, EVERY server every time. An unreachable or degraded
// server is present with its partial error, its freshness, and its last-known
// apps — it never vanishes from a successful partial response. /api/apps
// keeps the historical flat contract (current sweep's successful apps only);
// this endpoint carries the per-server honesty the fleet view needs.
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	if envelopes, ok := s.fleet.getObservations(); ok {
		writeData(w, buildFleetResponse(envelopes, time.Now().UTC()))
		return
	}

	// Same stale-while-revalidate contract as /api/apps: serve the last known
	// envelopes at once, refresh behind the request.
	if stale := s.fleet.snapshotObservations(); len(stale) > 0 {
		s.refreshFleetAsync()
		w.Header().Set("X-Fleet-Cache", "stale")
		writeData(w, buildFleetResponse(stale, time.Now().UTC()))
		return
	}

	// Cold path: no fleet endpoint exists for a fleet whose server set
	// cannot even be discovered — the same 502 /api/apps answers. A dead
	// fleet (discovery OK, every probe failing) still gets envelopes.
	envelopes, err := s.collectFleetObservations(r.Context(), nil)
	if err != nil {
		writeErrorStatus(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.fleet.publish(s.fleet.snapshotGeneration(), envelopes)
	writeData(w, buildFleetResponse(envelopes, time.Now().UTC()))
}

// resolveServers returns server connections from the CLI's servers.yml via the CLI delegate.
// A failure is an ERROR, not an empty list: callers must not mistake a broken
// discovery (missing CLI, non-zero exit, malformed JSON) for an unconfigured
// installation and fall back to local-state mode (A34). R47: the caller's
// context bounds the discovery subprocess too, so a canceled request stops
// the lookup instead of running to the delegate ceiling.
func (s *Server) resolveServers(ctx context.Context) ([]remote.ServerConn, error) {
	if !s.cliInstalled() {
		return nil, nil
	}
	result, err := s.runCLI(ctx, "server", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("listing servers from the teploy CLI: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, commandFailure([]string{"server", "list", "--json"}, result)
	}

	var raw map[string]struct {
		Host string `json:"host"`
		User string `json:"user"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &raw); err != nil {
		return nil, fmt.Errorf("parsing the server list: %w", err)
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
	return servers, nil
}

// serversBestEffort resolves servers for lookups (name resolution, nav
// inference) where a discovery failure degrades to "unknown server" rather
// than an error; the enqueue path re-resolves through the operation
// resolver, which fails visibly.
func (s *Server) serversBestEffort() []remote.ServerConn {
	servers, err := s.resolveServers(context.Background())
	if err != nil {
		log.Printf("[fleet] server discovery failed: %v", err)
		return nil
	}
	return servers
}

// lookupServerStrict resolves one registered server or fails. R01: restore
// verification must never aim at an unregistered name; unlike
// lookupServer/serversBestEffort this does not degrade to "unknown".
func (s *Server) lookupServerStrict(ctx context.Context, name string) (remote.ServerConn, error) {
	servers, err := s.resolveServers(ctx)
	if err != nil {
		return remote.ServerConn{}, fmt.Errorf("server discovery failed: %w", err)
	}
	for _, srv := range servers {
		if srv.Name == name {
			if srv.Host == "" {
				return remote.ServerConn{}, fmt.Errorf("server %q has no configured host", name)
			}
			return srv, nil
		}
	}
	return remote.ServerConn{}, fmt.Errorf("server not found: %s", name)
}

// lookupServer finds a server connection by name.
func (s *Server) lookupServer(name string) (remote.ServerConn, bool) {
	for _, srv := range s.serversBestEffort() {
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
// flags like --json can be passed in parts. R47: the caller's context bounds
// both the server lookup and the subprocess, so a canceled HTTP request stops
// the work instead of holding it to the delegate's ceiling.
func (s *Server) cliAppRun(ctx context.Context, serverName, appName string, parts ...string) (*cli.Result, error) {
	servers, err := s.resolveServers(ctx)
	if err != nil {
		return nil, fmt.Errorf("server discovery failed: %w", err)
	}
	var target *remote.ServerConn
	for i := range servers {
		if servers[i].Name == serverName {
			target = &servers[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("server not found: %s", serverName)
	}
	args := append([]string{}, parts...)
	args = append(args, "--host", target.Host, "--app", appName)
	if target.User != "" {
		args = append(args, "--user", target.User)
	}
	// Route through the injected runner (defaults to the real CLI) rather than
	// calling cli.RunChecked directly, so every app-scoped endpoint is
	// testable — which is what Config.CLIRunner exists for. cli.CheckExit
	// keeps RunChecked's rule that a non-zero exit is an error, so behavior is
	// unchanged.
	result, err := s.runCLI(ctx, args...)
	if err != nil {
		return result, err
	}
	return result, cli.CheckExit(result)
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
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		if !validEnvKey(body.Key) {
			writeError(w, "invalid env var name")
			return
		}
		result, err := cli.EnvSet(r.Context(), s.serverHost(serverName), s.serverUser(serverName), appName, body.Key, body.Value)
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
		result, err := s.cliAppRun(r.Context(), serverName, appName, "log", "--json")
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
		result, err := s.cliAppRun(r.Context(), serverName, appName, "drift", "--json")
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
		result, err := s.cliAppRun(r.Context(), serverName, appName, "stats", "--json")
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
		if !s.cliInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(r.Context(), serverName, appName, "health", "--json")
		if err != nil {
			// R51: only a COMPLETED non-zero exit may be read as a verdict.
			// The CLI prints its JSON health verdict and exits non-zero for
			// an unhealthy app — that is an answer. A transport failure
			// (timeout, cancellation, capture overflow) with partial stdout
			// is NOT: the old branch accepted any nonempty stdout alongside
			// any error, turning a timed-out command into a "healthy" 200.
			var exited *cli.ExitStatusError
			if !errors.As(err, &exited) {
				writeErrorStatus(w, "health command did not complete: "+err.Error(), http.StatusBadGateway)
				return
			}
			if result == nil || strings.TrimSpace(result.Stdout) == "" {
				writeErrorStatus(w, "health command returned no verdict", http.StatusBadGateway)
				return
			}
			writeRawJSON(w, result.Stdout)
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
		result, err := s.cliAppRun(r.Context(), serverName, appName, "accessory", "list", "--json")
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
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		s.enqueueOperation(w, r, operation.Request{
			Kind: operation.KindRemove, Server: serverName, App: appName,
			Purge: body.Purge, Redirect: body.Redirect,
		})

	case "lock":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(r.Context(), serverName, appName, "lock")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		s.fleet.invalidate()
		writeData(w, result)

	case "unlock":
		if !cli.IsInstalled() {
			writeError(w, "teploy CLI not installed")
			return
		}
		result, err := s.cliAppRun(r.Context(), serverName, appName, "unlock")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		s.fleet.invalidate()
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
					result, err := s.cliAppRun(r.Context(), serverName, appName, "accessory", sub, accName)
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

// ── Log Streaming (SSE) ──────────────────────────────────────────────────

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	// Reject cross-origin stream connections so a malicious page the
	// operator visits can't open the log stream using cached Basic-Auth
	// creds. A non-browser client (no Origin) is allowed.
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
		return
	}

	// Path: /api/logs/{server}/{app}
	path := strings.TrimPrefix(r.URL.Path, "/api/logs/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid path — expected /api/logs/{server}/{app}", 400)
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

	// SSE-only log streaming (A24/A32/A33: the hand-written WebSocket
	// transport is deleted; the browser's EventSource — with its built-in
	// Last-Event-ID reconnect semantics — is the single log path).
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// R57: arbitrary writer chunks are reassembled into logical lines and
	// emitted as JSON payloads (a chunk split mid-line, or a carriage
	// return inside one, used to become multiple fake log entries). R60:
	// every frame rides a per-write deadline and the stream starts with an
	// explicit comment frame; stream failures surface as a typed
	// stream-error event instead of a silent close.
	stream := newSSELogStream(w, flusher)
	if err := stream.writeFrame([]byte(": connected\n\n")); err != nil {
		return
	}
	if err := remote.StreamLogs(ctx, srv, appName, process, lines, stream); err != nil && ctx.Err() == nil {
		payload, _ := json.Marshal(map[string]string{"error": "log stream failed: " + err.Error()})
		_ = stream.writeFrame(append(append([]byte("event: stream-error\ndata: "), payload...), '\n', '\n'))
	}
}

// sseLogStream frames SSH output as SSE. It reassembles writer chunks into
// complete logical lines (R57) and bounds every write with a deadline so a
// non-reading client cannot pin the handler (R60).
type sseLogStream struct {
	w          http.ResponseWriter
	flusher    http.Flusher
	controller *http.ResponseController
	pending    []byte
}

func newSSELogStream(w http.ResponseWriter, flusher http.Flusher) *sseLogStream {
	return &sseLogStream{w: w, flusher: flusher, controller: http.NewResponseController(w)}
}

const (
	sseWriteDeadline = 10 * time.Second
	sseMaxLogLine    = 64 << 10
)

func (s *sseLogStream) writeFrame(frame []byte) error {
	// Best-effort deadline: unsupported instrumentation (test recorders)
	// must not kill the stream — only a WRITE failure aborts.
	_ = s.controller.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
	if _, err := s.w.Write(frame); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *sseLogStream) emitLine(line string) error {
	payload, err := json.Marshal(map[string]string{"line": line})
	if err != nil {
		return err
	}
	frame := append(append([]byte("event: log\ndata: "), payload...), '\n', '\n')
	return s.writeFrame(frame)
}

// Write implements io.Writer for the SSH producer, reassembling arbitrary
// chunks into logical lines (R57): a line split across writes stays one
// event; CRLF and lone CR are treated as line breaks, never interpolated
// into the SSE framing.
func (s *sseLogStream) Write(p []byte) (int, error) {
	consumed := 0
	for len(p) > 0 {
		nl := bytes.IndexByte(p, '\n')
		cr := bytes.IndexByte(p, '\r')
		take, sepLen := len(p), 0
		if nl >= 0 && (cr < 0 || nl < cr) {
			take, sepLen = nl, 1
		} else if cr >= 0 {
			take = cr
			sepLen = 1
			if nl == cr+1 {
				sepLen = 2 // CRLF is one break
			}
		}
		if len(s.pending)+take > sseMaxLogLine {
			return consumed, errors.New("log line exceeds the streaming budget")
		}
		s.pending = append(s.pending, p[:take]...)
		consumed += take
		p = p[take:]
		if sepLen == 0 {
			break // incomplete tail; wait for the next chunk
		}
		line := string(s.pending)
		s.pending = s.pending[:0]
		p = p[sepLen:]
		consumed += sepLen
		if err := s.emitLine(line); err != nil {
			return consumed, err
		}
	}
	return consumed, nil
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
		// A CLI failure is a dependency failure, not an empty catalog (A30).
		writeErrorStatus(w, "template list failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if result.ExitCode != 0 {
		writeErrorStatus(w, "template list failed: "+strings.TrimSpace(result.Stderr), http.StatusBadGateway)
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
		writeErrorStatus(w, "server list failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if result.ExitCode != 0 {
		writeErrorStatus(w, "server list failed: "+strings.TrimSpace(result.Stderr), http.StatusBadGateway)
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
// F012: the endpoint is normalized exactly like the SSH dialer — an explicit
// host:port used to be wrapped in ANOTHER port 22 ([example.test:2222]:22),
// probing (and reporting offline) a target the real SSH code never uses.
func tcpReachable(host string, timeout time.Duration) bool {
	addr, err := sshclient.NormalizeAddress(host)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
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
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}

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
	return atomicFileWrite(path, raw, 0644)
}

// groupsMu serializes whole read-modify-write transactions on groups.json
// (R11). Atomic replacement prevents torn FILES, not lost UPDATES: two
// concurrent handlers previously both read the same version and the later
// writer erased the first writer's change. The CLI can also write this file
// (a shared contract) — cross-process locking remains deferred with the
// A35 store/schema work; within the dashboard the transaction is now atomic.
var groupsMu sync.Mutex

// updateGroups runs one read-modify-write transaction against groups.json.
// Request bodies must be decoded BEFORE calling; change mutates the loaded
// document and saveErrors abort with the previous document intact.
func updateGroups(change func(*groupData) error) (groupData, error) {
	groupsMu.Lock()
	defer groupsMu.Unlock()
	data, err := loadGroupsFile()
	if err != nil {
		return groupData{}, err
	}
	if err := change(&data); err != nil {
		return data, err
	}
	if err := saveGroupsFile(data); err != nil {
		return data, err
	}
	return data, nil
}

// errGroupExists reports a create/rename colliding with an existing name.
var errGroupExists = errors.New("a group by that name already exists")

// groupNameRE is the creation/renaming grammar for groups and projects
// (R12): route-safe (no slash, no leading dot/dot-segment), bounded. Slash
// in a name made the entry unaddressable through the /api/groups/{name}
// route, which splits the decoded path.
var groupNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,63}$`)

// checkedGroupName validates and canonicalizes a NEW group/project name.
// Existing stores with out-of-grammar names stay readable; only create and
// rename enforce the grammar.
func checkedGroupName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if !groupNameRE.MatchString(name) || name == "." || name == ".." ||
		strings.HasPrefix(name, ".") {
		return "", errors.New("name must be 1-64 route-safe characters (letters, digits, space, '_', '.', '-'; no leading dot)")
	}
	return name, nil
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
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "name is required")
			return
		}
		name, err := checkedGroupName(body.Name)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		// R11: create runs as one transaction — the existence check and the
		// save see the same document under the groups lock.
		if _, err := updateGroups(func(data *groupData) error {
			for _, g := range data.Groups {
				if g.Name == name {
					return errGroupExists
				}
			}
			data.Groups = append(data.Groups, groupEntry{Name: name, Apps: []string{}})
			return nil
		}); err != nil {
			if errors.Is(err, errGroupExists) {
				writeErrorStatus(w, err.Error(), http.StatusConflict)
			} else {
				writeError(w, err.Error())
			}
			return
		}
		writeData(w, map[string]string{"status": "created"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// errGroupNotFound is the transaction sentinel for "the named group does
// not exist" (R11) — it carries the handler's verdict out of the locked
// transaction.
var errGroupNotFound = errors.New("group not found")

func (s *Server) handleGroupAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.Split(path, "/")
	groupName := parts[0]

	if len(parts) == 1 {
		switch r.Method {
		case "DELETE":
			// DELETE /api/groups/{name} — R11: the whole filter+save runs as
			// one transaction.
			if _, err := updateGroups(func(data *groupData) error {
				filtered := make([]groupEntry, 0, len(data.Groups))
				for _, g := range data.Groups {
					if g.Name != groupName {
						filtered = append(filtered, g)
					}
				}
				data.Groups = filtered
				return nil
			}); err != nil {
				writeError(w, err.Error())
				return
			}
			writeData(w, map[string]string{"status": "deleted"})
		case "PUT":
			// PUT /api/groups/{name} — rename. R12: the destination collision
			// check the project rename already had, applied inside the R11
			// transaction — renaming A onto existing B used to create
			// duplicate names, and deleting B then filtered out BOTH.
			var body struct {
				Name string `json:"name"`
			}
			if err := strictDecode(r, &body); err != nil {
				writeError(w, "name is required")
				return
			}
			newName, err := checkedGroupName(body.Name)
			if err != nil {
				writeError(w, err.Error())
				return
			}
			_, terr := updateGroups(func(data *groupData) error {
				for i, g := range data.Groups {
					if g.Name == groupName {
						if newName != groupName {
							for _, other := range data.Groups {
								if other.Name == newName {
									return errGroupExists
								}
							}
						}
						data.Groups[i].Name = newName
						return nil
					}
				}
				return errGroupNotFound
			})
			switch {
			case errors.Is(terr, errGroupExists):
				writeErrorStatus(w, terr.Error(), http.StatusConflict)
			case errors.Is(terr, errGroupNotFound):
				writeError(w, "group not found")
			case terr != nil:
				writeError(w, terr.Error())
			default:
				writeData(w, map[string]string{"status": "renamed"})
			}
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
		_, terr := updateGroups(func(data *groupData) error {
			for i, g := range data.Groups {
				if g.Name == groupName {
					filtered := make([]string, 0, len(g.Apps))
					for _, a := range g.Apps {
						if a != appName {
							filtered = append(filtered, a)
						}
					}
					data.Groups[i].Apps = filtered
					return nil
				}
			}
			return errGroupNotFound
		})
		if terr != nil {
			writeError(w, terr.Error())
			return
		}
		writeData(w, map[string]string{"status": "unassigned"})
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
		already := false
		_, terr := updateGroups(func(data *groupData) error {
			for i, g := range data.Groups {
				if g.Name == groupName {
					for _, a := range g.Apps {
						if a == body.App {
							already = true
							return nil
						}
					}
					data.Groups[i].Apps = append(data.Groups[i].Apps, body.App)
					return nil
				}
			}
			return errGroupNotFound
		})
		if terr != nil {
			writeError(w, terr.Error())
			return
		}
		if already {
			writeData(w, map[string]string{"status": "already assigned"})
			return
		}
		writeData(w, map[string]string{"status": "assigned"})

	case resource == "projects" && r.Method == "POST" && len(parts) == 2:
		// POST /api/groups/{name}/projects — create project
		var body struct {
			Name string `json:"name"`
		}
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "name is required")
			return
		}
		name, err := checkedGroupName(body.Name)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		_, terr := updateGroups(func(data *groupData) error {
			for i, g := range data.Groups {
				if g.Name == groupName {
					for _, p := range g.Projects {
						if p.Name == name {
							return errGroupExists
						}
					}
					data.Groups[i].Projects = append(data.Groups[i].Projects, projectEntry{Name: name, Apps: []string{}})
					return nil
				}
			}
			return errGroupNotFound
		})
		if terr != nil {
			if errors.Is(terr, errGroupExists) {
				writeErrorStatus(w, "a project named "+name+" already exists in this group", http.StatusConflict)
			} else {
				writeError(w, terr.Error())
			}
			return
		}
		writeData(w, map[string]string{"status": "created"})

	case resource == "projects" && len(parts) == 3 && r.Method == "PUT":
		// PUT /api/groups/{name}/projects/{project} — rename project
		projectName := parts[2]
		var body struct {
			Name string `json:"name"`
		}
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "name is required")
			return
		}
		newName, err := checkedGroupName(body.Name)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		_, terr := updateGroups(func(data *groupData) error {
			for i, g := range data.Groups {
				if g.Name != groupName {
					continue
				}
				for j, p := range g.Projects {
					if p.Name != projectName {
						continue
					}
					if newName != projectName {
						for _, other := range g.Projects {
							if other.Name == newName {
								return errGroupExists
							}
						}
					}
					data.Groups[i].Projects[j].Name = newName
					return nil
				}
			}
			return errGroupNotFound
		})
		if terr != nil {
			if errors.Is(terr, errGroupExists) {
				writeErrorStatus(w, "a project named "+newName+" already exists in this group", http.StatusConflict)
			} else {
				writeError(w, terr.Error())
			}
			return
		}
		writeData(w, map[string]string{"status": "renamed"})
		return

	case resource == "projects" && len(parts) == 3 && r.Method == "DELETE":
		// DELETE /api/groups/{name}/projects/{project} — delete project
		projectName := parts[2]
		_, terr := updateGroups(func(data *groupData) error {
			for i, g := range data.Groups {
				if g.Name == groupName {
					filtered := make([]projectEntry, 0, len(g.Projects))
					for _, p := range g.Projects {
						if p.Name != projectName {
							filtered = append(filtered, p)
						}
					}
					data.Groups[i].Projects = filtered
					return nil
				}
			}
			return errGroupNotFound
		})
		if terr != nil {
			writeError(w, terr.Error())
			return
		}
		writeData(w, map[string]string{"status": "deleted"})
		return

	case resource == "projects" && len(parts) == 5 && parts[3] == "apps" && r.Method == "DELETE":
		// DELETE /api/groups/{name}/projects/{project}/apps/{app} — unassign from project
		projectName := parts[2]
		appName := parts[4]
		_, terr := updateGroups(func(data *groupData) error {
			for i, g := range data.Groups {
				if g.Name != groupName {
					continue
				}
				for j, p := range g.Projects {
					if p.Name != projectName {
						continue
					}
					filtered := make([]string, 0, len(p.Apps))
					for _, a := range p.Apps {
						if a != appName {
							filtered = append(filtered, a)
						}
					}
					data.Groups[i].Projects[j].Apps = filtered
					return nil
				}
			}
			return errGroupNotFound
		})
		if terr != nil {
			writeError(w, terr.Error())
			return
		}
		writeData(w, map[string]string{"status": "unassigned"})
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
			already := false
			_, terr := updateGroups(func(data *groupData) error {
				for i, g := range data.Groups {
					if g.Name != groupName {
						continue
					}
					for j, p := range g.Projects {
						if p.Name != projectName {
							continue
						}
						for _, a := range p.Apps {
							if a == body.App {
								already = true
								return nil
							}
						}
						data.Groups[i].Projects[j].Apps = append(data.Groups[i].Projects[j].Apps, body.App)
						return nil
					}
				}
				return errGroupNotFound
			})
			if terr != nil {
				writeError(w, terr.Error())
				return
			}
			if already {
				writeData(w, map[string]string{"status": "already assigned"})
				return
			}
			writeData(w, map[string]string{"status": "assigned"})
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
			writeErrorStatus(w, "server list failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if result.ExitCode != 0 {
			writeErrorStatus(w, "server list failed: "+strings.TrimSpace(result.Stderr), http.StatusBadGateway)
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
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "invalid request body")
			return
		}
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

// writeServerError maps the CLI's typed server-registry errors to honest
// HTTP statuses (UPSTREAM-2): an existing destination is a conflict, a
// missing source is not found. Anything else stays a 400.
func writeServerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, cli.ErrServerExists):
		writeErrorStatus(w, "a server by that name already exists", http.StatusConflict)
	case errors.Is(err, cli.ErrServerNotFound):
		writeErrorStatus(w, "server not found", http.StatusNotFound)
	default:
		writeError(w, err.Error())
	}
}

// lookupServerRecord reads a server's configured host/SSH user/role from the
// CLI's servers.yml (the source of truth, not the 60s fleet cache). R52: a
// failed read is an ERROR — the caller must refuse the edit rather than
// silently fall back to empty values (which fed root/default metadata into
// the update path).
func (s *Server) lookupServerRecord(name string) (host, user, role string, err error) {
	result, err := cli.ServerList()
	if err != nil {
		return "", "", "", fmt.Errorf("server list failed: %w", err)
	}
	var raw map[string]struct {
		Host string `json:"host"`
		User string `json:"user"`
		Role string `json:"role"`
	}
	if json.Unmarshal([]byte(result.Stdout), &raw) != nil {
		return "", "", "", errors.New("server list returned unreadable output")
	}
	srv, ok := raw[name]
	if !ok {
		return "", "", "", fmt.Errorf("server not found: %s", name)
	}
	return srv.Host, srv.User, srv.Role, nil
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
		// Edit through the CLI's atomic `server rename` / `server update`
		// (UPSTREAM-2). The old remove+add emulation dropped tags/vpn_ip on
		// every edit, was non-atomic across two processes, and its rollback
		// could itself fail silently (A38).
		//
		// R52: rename and field updates must arrive as SEPARATE requests.
		// PUT ran rename-then-update unconditionally: a failed update left
		// the rename committed while the endpoint answered a generic error,
		// and the client could not tell which identity survived. A mixed
		// request is a 409; the UI sequences the two steps itself.
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
		// The authoritative record must be READABLE before anything is
		// changed (R52): a failed registry read used to degrade to empty
		// user/role, silently queuing a root-default update.
		exHost, exUser, exRole, err := s.lookupServerRecord(name)
		if err != nil {
			writeErrorStatus(w, err.Error(), http.StatusBadGateway)
			return
		}
		// Preserve the configured SSH user/role when the form leaves a field
		// blank — a silent downgrade back to root was the old failure mode.
		user := body.User
		if user == "" {
			user = exUser
		}
		role := body.Role
		if role == "" {
			role = exRole
		}
		renaming := newName != name
		changingFields := body.Host != exHost || user != exUser || role != exRole
		switch {
		case renaming && changingFields:
			writeErrorStatus(w, "rename and field updates must be separate requests (rename first, then edit the fields)", http.StatusConflict)
			return
		case renaming:
			if _, err := cli.ServerRename(name, newName); err != nil {
				writeServerError(w, err)
				return
			}
		default:
			if _, err := cli.ServerUpdate(name, body.Host, user, role); err != nil {
				writeServerError(w, err)
				return
			}
		}
		s.fleet.invalidate()
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
// R14: an unreadable or corrupt file is an ERROR, not "notifications not
// configured" — the caller decides how to degrade; a silent empty config
// let a later partial overwrite destroy stored secrets.
func LoadNotificationsConfig() (alert.Config, error) {
	return loadNotificationsConfig()
}

func loadNotificationsConfig() (alert.Config, error) {
	raw, err := os.ReadFile(notificationsFilePath())
	if errors.Is(err, os.ErrNotExist) {
		return alert.Config{}, nil
	}
	if err != nil {
		return alert.Config{}, fmt.Errorf("read notifications config: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return alert.Config{}, nil
	}
	var cfg alert.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return alert.Config{}, fmt.Errorf("decode notifications config: %w", err)
	}
	return cfg, nil
}

func saveNotificationsConfig(cfg alert.Config) error {
	path := notificationsFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// 0600 + atomic: the file holds the SMTP password, and a partial write
	// must never replace a complete config (A32).
	return atomicFileWrite(path, raw, 0600)
}

// notificationsMu serializes the notification config's read-merge-validate-
// save-publication sequence (R14): concurrent patches previously overwrote
// one another because each read the file outside any lock.
var notificationsMu sync.Mutex

// atomicFileWrite writes data to path via a same-directory temp file + rename
// so a failed or partial write cannot replace the previous complete file.
// R15: the replacement shares the audited durable semantics (unique temp,
// file sync, rename, PARENT-DIRECTORY sync after the rename) used by the
// credential and token stores — a rename without the dir sync is not
// crash-durable on filesystems that need explicit directory synchronization.
func atomicFileWrite(path string, data []byte, mode os.FileMode) error {
	return durable.Replace(path, data, mode)
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	switch r.Method {
	case "GET":
		cfg, err := loadNotificationsConfig()
		if err != nil {
			// R14: corrupt/unreadable state is a configuration error, not a
			// silently empty one.
			writeErrorStatus(w, "notification configuration unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		// Never return secrets to the client; expose only whether one is
		// configured.
		writeData(w, map[string]any{
			"webhook_url":         cfg.WebhookURL,
			"webhook_secret_set":  cfg.WebhookSecret != "",
			"smtp_host":           cfg.SMTPHost,
			"smtp_port":           cfg.SMTPPort,
			"smtp_user":           cfg.SMTPUser,
			"smtp_pass_set":       cfg.SMTPPass != "",
			"email_to":            cfg.EmailTo,
			"email_from":          cfg.EmailFrom,
			"smtp_allow_insecure": cfg.SMTPAllowInsecure,
		})
	case "POST":
		// Patch DTO (A34): the GET response contains read-only view flags
		// (smtp_pass_set, webhook_secret_set) that the old POST body could not
		// round-trip — strict decoding rejected the UI's own save. Secret
		// fields are pointers: absent = preserve the stored value; present =
		// replace (empty string clears it deliberately).
		var patch struct {
			WebhookURL        *string `json:"webhook_url"`
			WebhookSecret     *string `json:"webhook_secret"`
			SMTPHost          *string `json:"smtp_host"`
			SMTPPort          *int    `json:"smtp_port"`
			SMTPUser          *string `json:"smtp_user"`
			SMTPPass          *string `json:"smtp_pass"`
			EmailTo           *string `json:"email_to"`
			EmailFrom         *string `json:"email_from"`
			SMTPAllowInsecure *bool   `json:"smtp_allow_insecure"`
		}
		if err := strictDecode(r, &patch); err != nil {
			writeError(w, "invalid request body")
			return
		}
		notificationsMu.Lock()
		defer notificationsMu.Unlock()
		// R14: refuse patches against unreadable/corrupt state — merging
		// into a fabricated empty config destroyed the secrets the request
		// deliberately omitted. The whole read-merge-save sequence runs
		// under one lock so concurrent patches cannot overwrite one another.
		cfg, err := loadNotificationsConfig()
		if err != nil {
			writeErrorStatus(w, "notification configuration unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if patch.WebhookURL != nil {
			cfg.WebhookURL = strings.TrimSpace(*patch.WebhookURL)
		}
		if patch.WebhookSecret != nil {
			cfg.WebhookSecret = strings.TrimSpace(*patch.WebhookSecret)
		}
		if patch.SMTPHost != nil {
			cfg.SMTPHost = strings.TrimSpace(*patch.SMTPHost)
		}
		if patch.SMTPPort != nil {
			cfg.SMTPPort = *patch.SMTPPort
		}
		if patch.SMTPUser != nil {
			cfg.SMTPUser = *patch.SMTPUser
		}
		if patch.SMTPPass != nil {
			cfg.SMTPPass = *patch.SMTPPass
		}
		if patch.EmailTo != nil {
			cfg.EmailTo = *patch.EmailTo
		}
		if patch.EmailFrom != nil {
			cfg.EmailFrom = *patch.EmailFrom
		}
		if patch.SMTPAllowInsecure != nil {
			cfg.SMTPAllowInsecure = *patch.SMTPAllowInsecure
		}
		if err := saveNotificationsConfig(cfg); err != nil {
			writeError(w, err.Error())
			return
		}
		// Update the restore runner's dispatcher with the new config. The
		// MONITOR path needs no rewiring under D08: it goes through the
		// alert outbox, which reads the live config at every attempt — a
		// patch here fixes pending retries on their next attempt.
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
			writeErrorStatus(w, "registry list failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if result.ExitCode != 0 {
			writeErrorStatus(w, "registry list failed: "+strings.TrimSpace(result.Stderr), http.StatusBadGateway)
			return
		}
		writeRawJSON(w, result.Stdout)
	case "POST":
		var body struct {
			Server   string `json:"server"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		// Pass the password over stdin (--token reads it there) instead of on
		// the argv, where it would be visible in the host's process list.
		result, err := cli.RunWithStdin(r.Context(), body.Password, "registry", "login", body.Server, "--username", body.Username, "--token")
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

// Monitor input bounds (A14). Intervals live between one fast poll and one
// slow daily sweep; timeouts must fit inside the interval and one dial. The
// minimum matches the runner's own floor (startMonitor clamps to 10s), so
// the API never accepts an interval the scheduler would silently change
// (A20).
const (
	monitorMinInterval = 10 * time.Second
	monitorMaxInterval = 24 * time.Hour
	monitorMaxTimeout  = time.Minute
)

// validateMonitor normalizes and bounds-checks a submitted monitor so bad
// input fails at the API boundary instead of stretching the scheduler or the
// dialer (A14). Zero values receive the documented defaults rather than an
// error, which keeps hand-written configs and older stored monitors loadable.
func validateMonitor(m *store.Monitor) error {
	m.Name = strings.TrimSpace(m.Name)
	m.Target = strings.TrimSpace(m.Target)
	if m.Target == "" {
		return fmt.Errorf("monitor target is required")
	}
	if len(m.Target) > 2048 {
		return fmt.Errorf("monitor target is too long")
	}
	switch m.Type {
	case "http", "tcp", "ping":
	default:
		return fmt.Errorf("monitor type must be http, tcp, or ping")
	}
	// A monitor should only ever issue a safe HTTP method — never a
	// destructive verb against the monitored endpoint. Normalize to upper so
	// the stored form matches what the check runner compares.
	m.Method = strings.ToUpper(strings.TrimSpace(m.Method))
	switch m.Method {
	case "":
		m.Method = ""
	case "GET", "HEAD", "POST":
	default:
		return fmt.Errorf("monitor method must be GET, HEAD, or POST")
	}
	if m.Interval <= 0 {
		m.Interval = 60 * time.Second
	}
	if m.Interval < monitorMinInterval || m.Interval > monitorMaxInterval {
		return fmt.Errorf("interval must be between %s and %s", monitorMinInterval, monitorMaxInterval)
	}
	if m.Timeout <= 0 {
		m.Timeout = 10 * time.Second
	}
	if m.Timeout < time.Second || m.Timeout > monitorMaxTimeout {
		return fmt.Errorf("timeout must be between 1s and %s", monitorMaxTimeout)
	}
	if m.Timeout > m.Interval {
		return fmt.Errorf("timeout must not exceed the interval")
	}
	if m.ExpectedStatus != 0 && (m.ExpectedStatus < 100 || m.ExpectedStatus > 599) {
		return fmt.Errorf("expected status must be 0 (any 2xx/3xx) or 100-599")
	}
	// "ping" is a TCP-connect probe in this codebase, not ICMP — a bare
	// hostname would dial port 0 and always fail, so require host:port up
	// front for both TCP-like types. R46: SplitHostPort alone accepts an
	// empty host and service-name/out-of-range ports, deferring the breakage
	// to dial time; validate both explicitly.
	if m.Type == "tcp" || m.Type == "ping" {
		host, port, err := net.SplitHostPort(m.Target)
		if err != nil || strings.TrimSpace(host) == "" {
			return fmt.Errorf("%s monitor target must be host:port (TCP reachability probe, not ICMP)", m.Type)
		}
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%s monitor target port must be numeric and between 1 and 65535", m.Type)
		}
	}
	if m.Type == "http" {
		// A20: reject malformed HTTP targets at the boundary instead of
		// accepting a config that fails on every check.
		u, err := url.Parse(m.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return fmt.Errorf("http monitor target must be an absolute HTTP(S) URL without credentials")
		}
		if port := u.Port(); port != "" {
			if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
				return fmt.Errorf("http monitor target has an invalid port")
			}
		}
	}
	return nil
}

// isEffectiveAdmin reports whether the caller holds admin capability in the
// CURRENT mode (R45): an admin session when auth is enabled, or the operator
// themselves in --no-auth mode (where there is no session to check but the
// instance is deliberately single-user). An auth-store outage never reaches
// this — the gate refuses the request first.
func (s *Server) isEffectiveAdmin(r *http.Request) bool {
	if session, ok := currentUser(r); ok {
		return session.role == RoleAdmin
	}
	return s.gate == nil && s.config.NoAuth
}

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
			// Delivery exposes the monitor's last alert-delivery state (D08):
			// pending/delivered/dead-lettered with the last failure.
			Delivery *outbox.DeliveryStatus `json:"delivery,omitempty"`
		}

		var result []monitorWithStats
		for _, m := range monitors {
			mws := monitorWithStats{Monitor: m}
			stats, _ := s.store.GetStats(m.ID, time.Now().Add(-24*time.Hour))
			mws.Stats = stats
			if s.outbox != nil {
				mws.Delivery = s.outbox.LatestForMonitor(m.ID)
			}
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
		// the ID must always be present. (Also validates type/target/bounds.)
		if !store.ValidID(m.ID) || len(m.ID) > 64 {
			http.Error(w, "invalid monitor id (use letters, digits, '_' or '-')", 400)
			return
		}
		if err := validateMonitor(&m); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// A13: allowing a monitor to reach loopback/private/metadata networks
		// is an explicit admin choice (the operator grants the dash host's
		// network position to a probe). An editor may edit other fields of an
		// internal monitor but cannot change the flag in either direction —
		// enabling it requires admin, and this route is how both the create
		// and edit paths arrive. R45: no-auth mode treats the operator as
		// the admin it deliberately is (an auth-store failure is NOT no-auth
		// — the gate refuses requests before this point).
		if !s.isEffectiveAdmin(r) {
			if m.AllowInternal {
				http.Error(w, "internal-network monitoring requires an admin", http.StatusForbidden)
				return
			}
			if prev, err := s.store.GetMonitor(m.ID); err == nil && prev != nil && prev.AllowInternal {
				http.Error(w, "disabling internal-network monitoring requires an admin", http.StatusForbidden)
				return
			}
		}
		if err := s.store.SaveMonitor(&m); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if s.monitor != nil {
			// m carries the store-assigned incarnation, so the new scheduler
			// stamps its checks with it (D08 CAS).
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
		resp := map[string]interface{}{
			"monitor": m,
			"checks":  checks,
			"stats":   stats,
		}
		// D08: the monitor's last alert-delivery state (retry/backoff/
		// dead-letter with the exact last failure) is part of the monitor's
		// truth, not a separate screen to remember.
		if s.outbox != nil {
			if d := s.outbox.LatestForMonitor(id); d != nil {
				resp["delivery"] = d
			}
		}
		writeJSON(w, resp)

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

// handleHealthz is the LIVENESS probe (A39/A47): the process is up and the
// HTTP loop answers. Deliberately dependency-free — a broken store must not
// get the pod restarted when it could serve partial traffic instead.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	writeJSON(w, map[string]string{"status": "ok"})
}

// publicCheck is the unauthenticated /readyz projection of one subsystem
// (R44): a coarse, stable state + machine code. Internal error strings
// (filesystem paths, hostnames, database detail) go to the log, not the
// public response.
type publicCheck struct {
	State string `json:"state"`
	Code  string `json:"code,omitempty"`
}

// handleReadyz is the READINESS probe (A39/A47): dependencies are reachable
// and persistence is not known-degraded. Unlike liveness, failing readiness
// takes the instance out of rotation.
//
//	status "ready"      200 — store reachable, operation persistence healthy
//	status "degraded"   200 — serving, but a subsystem is impaired (operation
//	                         persistence failing, or the operation service
//	                         failed to init); stays in rotation on purpose
//	status "unavailable" 503 — the store is unreachable, or the auth store is
//	                         in its fail-closed outage
//
// R44: the public body carries coarse codes only; detail is logged.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	status := "ready"
	code := http.StatusOK
	checks := map[string]interface{}{}
	backend := s.config.Backend
	if backend == "" {
		backend = "file"
	}
	checks["backend"] = backend
	if s.gate != nil && s.gate.initErr != nil {
		status = "unavailable"
		code = http.StatusServiceUnavailable
		checks["auth"] = publicCheck{State: "unavailable", Code: "AUTH_STORE_UNAVAILABLE"}
		log.Printf("[readyz] auth store unavailable: %v", s.gate.initErr)
	}
	if pinger, ok := s.store.(interface{ Ping() error }); ok {
		if err := pinger.Ping(); err != nil {
			status = "unavailable"
			code = http.StatusServiceUnavailable
			checks["store"] = publicCheck{State: "unavailable", Code: "STORE_UNAVAILABLE"}
			log.Printf("[readyz] store unreachable: %v", err)
		} else {
			checks["store"] = publicCheck{State: "ok"}
		}
	} else if s.store == nil {
		checks["store"] = publicCheck{State: "not_configured"}
	} else {
		checks["store"] = publicCheck{State: "unknown"}
	}
	if s.operations != nil {
		h := s.operations.Health()
		if h.PersistDegraded || h.JournalError != "" {
			if status == "ready" {
				status = "degraded"
			}
			// Coarse codes; the specific errors stay in the log.
			log.Printf("[readyz] operation persistence degraded: record_error=%q event_error=%q journal_error=%q", h.RecordError, h.EventError, h.JournalError)
			checks["operations"] = map[string]interface{}{
				"persist_degraded": h.PersistDegraded,
				"degraded_codes":   degradedCodes(h),
				"live_operations":  h.LiveOperations,
			}
		} else {
			checks["operations"] = map[string]interface{}{
				"persist_degraded": false,
				"live_operations":  h.LiveOperations,
			}
		}
	} else {
		if status == "ready" {
			status = "degraded"
		}
		checks["operations"] = publicCheck{State: "unavailable", Code: "OPERATION_SERVICE_UNAVAILABLE"}
		if s.operationInitErr != nil {
			log.Printf("[readyz] operation service init failed: %v", s.operationInitErr)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "checks": checks})
}

// degradedCodes turns the operation Health projection into stable public
// codes (R44) without the raw error text.
func degradedCodes(h operation.Health) []string {
	var codes []string
	if h.RecordError != "" {
		codes = append(codes, "RECORD_PERSIST_DEGRADED")
	}
	if h.EventError != "" {
		codes = append(codes, "EVENT_JOURNAL_DEGRADED")
	}
	if h.JournalError != "" {
		codes = append(codes, "JOURNAL_STORAGE_DEGRADED")
	}
	return codes
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
// index.html so client-side routing works. The fallback serves page routes
// only — a POST to an unknown path is a 405, not an HTML body (A36).
func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	if s.frontend == nil {
		http.Error(w, "frontend not embedded", http.StatusInternalServerError)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

// shortcutURLRE-bound validation pieces (A40). Shared links render in href,
// window.open, and style bindings for every user, so the whole list is
// validated before it replaces the shared state: bounded count, unique
// route-safe IDs, absolute HTTP(S) URLs without credentials or fragments,
// and a narrow color grammar instead of arbitrary CSS fragments.
var (
	shortcutColorRE = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	shortcutIDRE    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

const maxHomepageItems = 200

func validateShortcutURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return fmt.Errorf("shortcut URL %q must be an absolute HTTP(S) URL without credentials or fragment", raw)
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("shortcut URL %q has an invalid port", raw)
		}
	}
	return nil
}

func validateHomepage(items []HomepageItem) error {
	if len(items) > maxHomepageItems {
		return fmt.Errorf("too many shortcuts (max %d)", maxHomepageItems)
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		item.Name = strings.TrimSpace(item.Name)
		item.URL = strings.TrimSpace(item.URL)
		if item.ID == "" || !shortcutIDRE.MatchString(item.ID) {
			return fmt.Errorf("shortcut id %q must be 1-64 letters, digits, '_' or '-'", item.ID)
		}
		if seen[item.ID] {
			return fmt.Errorf("duplicate shortcut id %q", item.ID)
		}
		seen[item.ID] = true
		if item.Name == "" || len(item.Name) > 100 {
			return fmt.Errorf("shortcut %q must have a name (max 100 characters)", item.ID)
		}
		if len(item.URL) > 2048 {
			return fmt.Errorf("shortcut %q URL is too long", item.ID)
		}
		if err := validateShortcutURL(item.URL); err != nil {
			return err
		}
		if item.Color != "" && !shortcutColorRE.MatchString(item.Color) {
			return fmt.Errorf("shortcut %q color must be #RRGGBB", item.ID)
		}
		if len(item.Description) > 500 || len(item.Icon) > 8192 || strings.ContainsAny(item.Icon, "<>") {
			return fmt.Errorf("shortcut %q description or icon is out of bounds", item.ID)
		}
	}
	return nil
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
	return atomicFileWrite(path, raw, 0644)
}

func (s *Server) handleHomepage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		s.homepageMu.Lock()
		data, err := s.loadHomepage()
		s.homepageMu.Unlock()
		if err != nil {
			writeError(w, err.Error())
			return
		}
		etag := homepageETag(data)
		w.Header().Set("ETag", etag)
		writeData(w, data.Items)
	case "PUT":
		// R13: whole-document replacement requires the client's copy to be
		// current. Two editors previously loaded the same list, each changed
		// a different shortcut, and the second save silently erased the
		// first — a mutex around save alone could not detect it because each
		// request already contained a stale complete list.
		var items []HomepageItem
		if err := strictDecode(r, &items); err != nil {
			writeError(w, "invalid JSON: "+err.Error())
			return
		}
		if items == nil {
			items = []HomepageItem{}
		}
		if err := validateHomepage(items); err != nil {
			writeError(w, err.Error())
			return
		}
		ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
		if ifMatch == "" {
			writeErrorStatus(w, "If-Match is required (reload the shortcuts and retry)", http.StatusPreconditionRequired)
			return
		}
		s.homepageMu.Lock()
		defer s.homepageMu.Unlock()
		current, err := s.loadHomepage()
		if err != nil {
			writeErrorStatus(w, "cannot load shortcuts: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if ifMatch != homepageETag(current) {
			writeErrorStatus(w, "shortcuts changed since you loaded them; reload before saving", http.StatusPreconditionFailed)
			return
		}
		if err := s.saveHomepage(homepageData{Items: items}); err != nil {
			writeError(w, err.Error())
			return
		}
		w.Header().Set("ETag", homepageETag(homepageData{Items: items}))
		writeData(w, items)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// homepageETag derives the document's concurrency token from its canonical
// encoding (R13).
func homepageETag(data homepageData) string {
	raw, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return `"` + hex.EncodeToString(sum[:]) + `"`
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
// ever attacker-influenced, a response-shape injection). R56: EMPTY output is
// equally a 502 — a --json command that printed nothing did not answer, and
// a data:null success hid the dependency failure.
func writeRawJSON(w http.ResponseWriter, raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		writeErrorStatus(w, "delegated command returned no JSON result", http.StatusBadGateway)
		return
	}
	// Forward the exact bytes: decoding into interface{} turns every number
	// into a float64, and re-encoding silently corrupts integers above 2^53
	// (A35). json.RawMessage marshals verbatim; validity is still enforced.
	payload := json.RawMessage(raw)
	if !json.Valid(payload) {
		writeErrorStatus(w, "delegated command returned non-JSON output", http.StatusBadGateway)
		return
	}
	writeData(w, payload)
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
		// A24: configuration input is a CONFIG-ONLY DTO. Result fields
		// (last_ok, last_run_at, ...) are rejected outright — the old
		// full-shape upsert let a caller forge verification results onto a
		// freshly created test.
		var body struct {
			ID            string `json:"id"`
			Server        string `json:"server"`
			App           string `json:"app"`
			Accessory     string `json:"accessory"`
			Bucket        string `json:"bucket"`
			Region        string `json:"region"`
			IntervalHours int    `json:"interval_hours"`
			Enabled       bool   `json:"enabled"`
		}
		if err := strictDecode(r, &body); err != nil {
			http.Error(w, "invalid request body (configuration fields only)", 400)
			return
		}
		// Every field below reaches the teploy CLI's argv (and from there a
		// remote shell), so validate all of them at the boundary — same rule
		// as monitors/app actions.
		if !store.ValidID(body.ID) {
			http.Error(w, "invalid restore test id (use letters, digits, '_' or '-')", 400)
			return
		}
		// R46: app names follow the DEPLOYMENT grammar (dots allowed — the
		// CLI creates dotted app names), not the opaque store-ID grammar
		// that previously rejected them; the server alias and accessory
		// keep the store-ID grammar they always had.
		if !store.ValidID(body.Server) || !validAppName(body.App) || !store.ValidID(body.Accessory) {
			http.Error(w, "server and accessory are required (letters, digits, '_' or '-'); app must be a valid deployment name", 400)
			return
		}
		if !bucketPattern.MatchString(body.Bucket) || len(body.Bucket) > 63 {
			http.Error(w, "invalid bucket name", 400)
			return
		}
		if body.Region == "" {
			body.Region = "us-east-1"
		}
		if !bucketPattern.MatchString(body.Region) || len(body.Region) > 25 {
			http.Error(w, "invalid region", 400)
			return
		}
		if body.IntervalHours < 1 {
			body.IntervalHours = 24
		}
		if body.IntervalHours > 24*365 {
			http.Error(w, "interval_hours must be between 1 and 8760", 400)
			return
		}
		// R01: a restore test may only target a REGISTERED server — reject
		// at creation instead of failing (or worse, silently retargeting)
		// at run time when the resolver fails closed.
		if s.cliInstalled() {
			if _, err := s.lookupServerStrict(r.Context(), body.Server); err != nil {
				writeErrorStatus(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		// Configuration-only DTO (A24): the client never round-trips result
		// fields. R36: Last* preservation happens INSIDE the store's
		// save transaction, so a verification that completes while this
		// edit is in flight can no longer be overwritten by a stale copy.
		t := store.RestoreTest{
			ID: body.ID, Server: body.Server, App: body.App, Accessory: body.Accessory,
			Bucket: body.Bucket, Region: body.Region, IntervalHours: body.IntervalHours, Enabled: body.Enabled,
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
		updated, runErr := s.restore.RunNow(*t)
		if runErr != nil {
			// F036: a run already in flight is a CONFLICT, and the record's
			// previous verdict must not be presented as this request's
			// outcome.
			if errors.Is(runErr, restoretest.ErrAlreadyRunning) {
				http.Error(w, "a verification run for this test is already in progress; retry when it completes", http.StatusConflict)
				return
			}
			http.Error(w, runErr.Error(), 500)
			return
		}
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
