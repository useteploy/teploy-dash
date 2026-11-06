package server

// observe.go — Programme B items 2-4 (plan section 10): Dash ↔ Observe.
//
// Observe is the one sibling with a deep connect flow; every other product
// gets at most a nav link. The owner pastes a service token minted in
// Observe (Settings → Service Tokens, scopes sites:create + keys:mint at
// minimum) — dash never sees an Observe admin credential, and the token is
// the only secret this file ever holds.
//
// Never guess:
//   - the base URL comes from the owner (prefilled from a CONFIRMED probe
//     when one exists — the doc's dashboard_url, else the address that
//     answered), and every call goes there;
//   - ingest_url comes only from the discovery doc, re-read at every plan;
//     an empty one is an unknown and the telemetry plan refuses to build;
//   - instance_id is confirmed by the owner against the discovery doc and
//     re-checked before every write: a changed id (a restored copy, a
//     different Observe behind the same URL) refuses until reconnect.
//
// The token is a SECRET in the shape backupstorage/notifications taught:
// stored in ~/.teploy/observe.json (0600, atomic write — the existing
// secrets-file pattern for dash's own secrets; the age-encrypted store is
// the deferred P2 item recorded in AUDIT_OPEN with it), never in an API
// response, audit row or log line, and scrubbed from any error text before
// it is stored or answered. The view carries only sha256(token)[:12].
//
// Verification has no "list sites" route to call (the service API offers
// only scoped reads per site), so dash verifies with the read Observe does
// offer — alerts/rules on a random site id that cannot exist. The guard's
// own ordering makes that a validity probe: 401 = refused token, 403/404 =
// the token authenticated (403 also proves the scope check ran). The cost
// is one "denied" audit row on Observe's trail per verification, which is
// Observe's designed answer to token-shaped calls; recorded in AUDIT_OPEN.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/durable"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/operation"
)

const (
	observeRequestTimeout = 10 * time.Second
	observeBodyLimit      = 256 << 10
	// observeTokenPrefix is Observe's service-token grammar: anything else
	// is refused before the first request (fail fast, no probe with junk).
	observeTokenPrefix = "obs_svc_"
)

// ── persisted state ──────────────────────────────────────────────────────────

type observeConnection struct {
	BaseURL      string   `json:"base_url"`
	InstanceID   string   `json:"instance_id"`
	Product      string   `json:"product"`
	Version      string   `json:"version,omitempty"`
	DashboardURL string   `json:"dashboard_url,omitempty"`
	IngestURL    string   `json:"ingest_url,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// Token is the service token: a SECRET. TokenFP is sha256(token)[:12]
	// hex, the only form views ever see.
	Token         string    `json:"token"`
	TokenFP       string    `json:"token_fp"`
	AllowInternal bool      `json:"allow_internal,omitempty"`
	ScopesSeen    []string  `json:"scopes_seen,omitempty"`
	ConnectedAt   time.Time `json:"connected_at"`
	ConnectedBy   string    `json:"connected_by"`
}

// observeAppTelemetry is one app's opt-in (plan item 4): what dash wired,
// where it points, and what a disable must undo.
type observeAppTelemetry struct {
	Server      string    `json:"server"`
	App         string    `json:"app"`
	ExternalRef string    `json:"external_ref"`
	SiteID      string    `json:"site_id,omitempty"`
	KeyID       string    `json:"key_id,omitempty"`
	KeyLabel    string    `json:"key_label,omitempty"`
	IngestURL   string    `json:"ingest_url"`
	InstanceID  string    `json:"instance_id"`
	EnvKeys     []string  `json:"env_keys"`
	SecretKeys  []string  `json:"secret_keys"`
	AppliedAt   time.Time `json:"applied_at"`
	AppliedBy   string    `json:"applied_by"`
}

type observeRecord struct {
	Connection *observeConnection             `json:"connection,omitempty"`
	Apps       map[string]observeAppTelemetry `json:"apps,omitempty"`
}

type observeState struct {
	mu  sync.Mutex
	rec observeRecord
	// path is empty when the file is disabled (unreadable and could not be
	// set aside); state then lives in memory only.
	path string
}

func observeFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".teploy", "observe.json")
}

func (s *Server) initObserve() {
	o := &s.observe
	if err := o.load(); err != nil {
		// Unreadable connection state must not take the page down: set it
		// aside (kept for inspection) and start empty, like the caches.
		if aside := setAside(observeFilePath(), err); aside == nil {
			o.load()
		} else {
			log.Printf("[observe] %s is unreadable (%v) and could not be set aside: %v", observeFilePath(), err, aside)
			o.path = ""
		}
	}
}

func (o *observeState) load() error {
	o.path = observeFilePath()
	raw, err := os.ReadFile(o.path)
	if errors.Is(err, os.ErrNotExist) {
		o.rec = observeRecord{}
		return nil
	}
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		o.rec = observeRecord{}
		return nil
	}
	var rec observeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return err
	}
	o.rec = rec
	return nil
}

func (o *observeState) saveLocked() error {
	if o.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(o.rec, "", "  ")
	if err != nil {
		return err
	}
	// 0600 + atomic: the file holds a service token; a partial write must
	// never replace a complete one (the notifications.json discipline).
	if err := os.MkdirAll(filepath.Dir(o.path), 0755); err != nil {
		return err
	}
	return durable.Replace(o.path, raw, 0600)
}

// connection returns a copy of the connection state (token included —
// callers inside this file use it to authenticate; views never embed it).
func (o *observeState) connection() (observeConnection, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rec.Connection == nil {
		return observeConnection{}, false
	}
	return *o.rec.Connection, true
}

// scrubSecret removes every occurrence of the secret (and its line-split
// components) from text before it is stored, answered or logged.
func scrubSecret(text, secret string) string {
	if secret == "" {
		return text
	}
	parts := []string{secret}
	parts = append(parts, strings.FieldsFunc(secret, func(r rune) bool { return r == '\n' || r == '\r' })...)
	for _, p := range parts {
		if p != "" {
			text = strings.ReplaceAll(text, p, "[REDACTED]")
		}
	}
	return text
}

func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6])
}

// ── the HTTP client ──────────────────────────────────────────────────────────

func observeHTTPClient(allowInternal bool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:     monitor.PolicyDialContext(allowInternal),
			IdleConnTimeout: 30 * time.Second,
		},
		Timeout: observeRequestTimeout,
	}
}

// observeResponse is one call's outcome: status (0 = transport error), body
// (bounded), and the error already scrubbed of any credential.
type observeResponse struct {
	Status int
	Body   []byte
	Err    error
}

func observeCall(ctx context.Context, conn *observeConnection, method, path string, payload any) observeResponse {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return observeResponse{Err: err}
		}
		body = strings.NewReader(string(raw))
	}
	url := strings.TrimRight(conn.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return observeResponse{Err: fmt.Errorf("building the request: %w", err)}
	}
	req.Header.Set("Authorization", "Bearer "+conn.Token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := observeHTTPClient(conn.AllowInternal)
	resp, err := client.Do(req)
	if err != nil {
		return observeResponse{Err: errors.New(sanitizeDomainError(scrubSecret(err.Error(), conn.Token)))}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, observeBodyLimit))
	if err != nil {
		return observeResponse{Status: resp.StatusCode, Err: errors.New(sanitizeDomainError(scrubSecret(err.Error(), conn.Token)))}
	}
	return observeResponse{Status: resp.StatusCode, Body: raw}
}

// fetchObserveDoc reads the unauthenticated discovery doc from base. The
// token plays no part; the doc is the identity every write is checked
// against.
func fetchObserveDoc(ctx context.Context, baseURL string, allowInternal bool) (*siblingDiscoveryDoc, error) {
	url := strings.TrimRight(baseURL, "/") + teployDiscoveryPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	client := observeHTTPClient(allowInternal)
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New(sanitizeDomainError(err.Error()))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, siblingDocLimit))
	if err != nil {
		return nil, errors.New(sanitizeDomainError(err.Error()))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s (no Teploy discovery doc — is this the dashboard listener, and is discovery on?)", hostOf(baseURL), resp.Status)
	}
	var doc siblingDiscoveryDoc
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Product == "" {
		return nil, errors.New("the address answered, but not with a Teploy discovery doc")
	}
	return &doc, nil
}

func hostOf(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil {
		return u.Host
	}
	return baseURL
}

// validateObserveBaseURL normalizes the owner-pasted base URL. Scheme must
// be http/https and the host non-empty; anything else is refused before a
// single request is made.
func validateObserveBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("base URL must be http(s)://host[:port]")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("base URL must not carry a path")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/"), nil
}

// verifyObserveToken authenticates the token with the harmless read the
// service API offers (see the file comment): alerts/rules for a random site
// id that cannot exist. 401 refuses; 403/404 (or 2xx) prove the token
// authenticated — 403 also proves the scope check ran, so alerts:read is
// only "seen" on 2xx/404.
func verifyObserveToken(ctx context.Context, conn *observeConnection) (scopesSeen []string, err error) {
	var probe [16]byte
	if _, err := rand.Read(probe[:]); err != nil {
		return nil, fmt.Errorf("building the verification probe: %w", err)
	}
	siteID := "probe-" + hex.EncodeToString(probe[:])
	resp := observeCall(ctx, conn, http.MethodGet, "/api/v1/service/sites/"+siteID+"/alerts/rules?limit=1", nil)
	switch {
	case resp.Err != nil:
		return nil, resp.Err
	case resp.Status == http.StatusUnauthorized:
		return nil, errors.New("Observe refused the service token (revoked, or not an obs_svc_ token)")
	case resp.Status == http.StatusForbidden:
		// Valid token, missing alerts:read. Verification passes — the
		// connect flow needs sites:create + keys:mint, checked at use.
		return nil, nil
	case resp.Status == http.StatusNotFound:
		return []string{"alerts:read"}, nil
	case resp.Status == http.StatusTooManyRequests || resp.Status == http.StatusServiceUnavailable:
		return nil, fmt.Errorf("Observe is busy (HTTP %d, Retry-After given) — try again", resp.Status)
	case resp.Status >= 200 && resp.Status < 300:
		return []string{"alerts:read"}, nil
	default:
		return nil, fmt.Errorf("unexpected answer from Observe's service API: HTTP %d", resp.Status)
	}
}

// observeDocStillCurrent re-reads the discovery doc and requires the same
// instance to answer: the fence every write (connect, plan, apply) re-checks
// so credentials never land against an Observe that moved.
func observeDocStillCurrent(ctx context.Context, conn *observeConnection) (*siblingDiscoveryDoc, error) {
	doc, err := fetchObserveDoc(ctx, conn.BaseURL, conn.AllowInternal)
	if err != nil {
		return nil, fmt.Errorf("the discovery doc could not be read at %s: %w", conn.BaseURL, err)
	}
	if doc.InstanceID == "" || doc.InstanceID != conn.InstanceID {
		return nil, fmt.Errorf("the Observe at %s is not the connected instance (doc says %q, connected to %q) — reconnect to the right instance before changing anything", conn.BaseURL, doc.InstanceID, conn.InstanceID)
	}
	if doc.Product != "observe" {
		return nil, fmt.Errorf("the discovery doc at %s says product %q, not observe", conn.BaseURL, doc.Product)
	}
	return doc, nil
}

// ── views ────────────────────────────────────────────────────────────────────

type observeConnectionView struct {
	BaseURL       string    `json:"base_url"`
	InstanceID    string    `json:"instance_id"`
	Product       string    `json:"product"`
	Version       string    `json:"version,omitempty"`
	DashboardURL  string    `json:"dashboard_url,omitempty"`
	IngestURL     string    `json:"ingest_url,omitempty"`
	Capabilities  []string  `json:"capabilities,omitempty"`
	TokenFP       string    `json:"token_fp"`
	AllowInternal bool      `json:"allow_internal,omitempty"`
	ScopesSeen    []string  `json:"scopes_seen,omitempty"`
	ConnectedAt   time.Time `json:"connected_at"`
	ConnectedBy   string    `json:"connected_by"`
}

func (c *observeConnection) view() observeConnectionView {
	caps := c.Capabilities
	if caps == nil {
		caps = []string{}
	}
	scopes := c.ScopesSeen
	if scopes == nil {
		scopes = []string{}
	}
	return observeConnectionView{
		BaseURL: c.BaseURL, InstanceID: c.InstanceID, Product: c.Product, Version: c.Version,
		DashboardURL: c.DashboardURL, IngestURL: c.IngestURL, Capabilities: caps,
		TokenFP: c.TokenFP, AllowInternal: c.AllowInternal, ScopesSeen: scopes,
		ConnectedAt: c.ConnectedAt, ConnectedBy: c.ConnectedBy,
	}
}

// siblingObserveHint is the prefill a CONFIRMED probe offers the connect
// form. Advisory only: the owner edits and confirms.
type siblingObserveHint struct {
	Available  bool   `json:"available"`
	BaseURL    string `json:"base_url,omitempty"`
	Product    string `json:"product,omitempty"`
	Version    string `json:"version,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
	Note       string `json:"note,omitempty"`
}

func (s *Server) observeHint() siblingObserveHint {
	sb := &s.siblings
	sb.mu.Lock()
	defer sb.mu.Unlock()
	var best *siblingProbe
	for _, rec := range sb.snap.Servers {
		for _, p := range rec.Probes {
			if p.State != probeConfirmed || p.Doc == nil || p.Doc.Product != "observe" {
				continue
			}
			if best == nil || p.ProbedAt.After(best.ProbedAt) {
				best = p
			}
		}
	}
	if best == nil {
		return siblingObserveHint{Available: false, Note: "no confirmed Observe discovery on the fleet — enter the address by hand"}
	}
	// Prefer the doc's own dashboard URL; fall back to the address that
	// answered, which is evidence, not a guess.
	base := best.Doc.DashboardURL
	if base == "" {
		base = best.BaseURL
	}
	return siblingObserveHint{Available: true, BaseURL: base, Product: best.Doc.Product, Version: best.Doc.Version, InstanceID: best.Doc.InstanceID}
}

func (s *Server) observeView(ctx context.Context) map[string]any {
	view := map[string]any{"hint": s.observeHint()}
	o := &s.observe
	o.mu.Lock()
	conn := o.rec.Connection
	var apps []observeAppTelemetry
	if len(o.rec.Apps) > 0 {
		apps = make([]observeAppTelemetry, 0, len(o.rec.Apps))
		for _, a := range o.rec.Apps {
			apps = append(apps, a)
		}
	}
	o.mu.Unlock()
	if apps != nil {
		sort.Slice(apps, func(i, j int) bool {
			if apps[i].Server != apps[j].Server {
				return apps[i].Server < apps[j].Server
			}
			return apps[i].App < apps[j].App
		})
		view["apps"] = apps
	} else {
		view["apps"] = []observeAppTelemetry{}
	}
	if conn != nil {
		view["connected"] = true
		view["connection"] = conn.view()
		return view
	}
	view["connected"] = false
	return view
}

// ── the API ──────────────────────────────────────────────────────────────────

func (s *Server) handleObserve(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/observe"), "/")
	path = strings.Trim(path, "/")
	noStore(w)
	switch {
	case path == "" && r.Method == http.MethodGet:
		writeData(w, s.observeView(r.Context()))
	case path == "preview" && r.Method == http.MethodGet:
		s.handleObservePreview(w, r)
	case path == "connect" && r.Method == http.MethodPost:
		s.handleObserveConnect(w, r)
	case path == "connection" && r.Method == http.MethodDelete:
		s.handleObserveDisconnect(w, r)
	case path == "telemetry/plan" && r.Method == http.MethodGet:
		s.handleObserveTelemetryPlan(w, r)
	case path == "telemetry" && r.Method == http.MethodPost:
		s.handleObserveTelemetryApply(w, r)
	case path == "telemetry/disable" && r.Method == http.MethodPost:
		s.handleObserveTelemetryDisable(w, r)
	default:
		writeErrorStatus(w, "observe route not found", http.StatusNotFound)
	}
}

// handleObservePreview reads the discovery doc for a pasted address so the
// owner can confirm the instance identity BEFORE any token moves. Read-only,
// unauthenticated on Observe's side.
func (s *Server) handleObservePreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Siblings }, "Observe connect", siblingsMinCLI) {
		return
	}
	base, err := validateObserveBaseURL(r.URL.Query().Get("url"))
	if err != nil {
		writeError(w, err.Error())
		return
	}
	doc, err := fetchObserveDoc(r.Context(), base, r.URL.Query().Get("allow_internal") == "true")
	if err != nil {
		writeError(w, "the discovery doc did not answer: "+err.Error())
		return
	}
	if doc.Product != "observe" {
		writeError(w, fmt.Sprintf("the discovery doc at %s says product %q, not observe", base, doc.Product))
		return
	}
	if doc.InstanceID == "" {
		writeError(w, "the discovery doc carries no instance_id — dash will not connect to an instance it cannot identify")
		return
	}
	writeData(w, doc)
}

// handleObserveConnect connects dash to one Observe instance: the owner
// pastes a service token and CONFIRMS the instance id from the previewed
// discovery doc; dash re-reads the doc, enforces the same id, and verifies
// the token with a harmless read before anything is stored.
func (s *Server) handleObserveConnect(w http.ResponseWriter, r *http.Request) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Siblings }, "Observe connect", siblingsMinCLI) {
		return
	}
	var body struct {
		BaseURL           string `json:"base_url"`
		Token             string `json:"token"`
		ConfirmInstanceID string `json:"confirm_instance_id"`
		AllowInternal     bool   `json:"allow_internal"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body: "+err.Error())
		return
	}
	base, err := validateObserveBaseURL(body.BaseURL)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	token := strings.TrimSpace(body.Token)
	if !strings.HasPrefix(token, observeTokenPrefix) {
		writeError(w, "the token must be an Observe service token (minted in Observe Settings → Service Tokens; it starts obs_svc_)")
		return
	}
	doc, err := fetchObserveDoc(r.Context(), base, body.AllowInternal)
	if err != nil {
		writeError(w, "the discovery doc did not answer at "+base+": "+err.Error())
		return
	}
	if doc.Product != "observe" {
		writeError(w, fmt.Sprintf("the discovery doc at %s says product %q, not observe", base, doc.Product))
		return
	}
	if doc.InstanceID == "" {
		writeError(w, "the discovery doc carries no instance_id — dash will not connect to an instance it cannot identify")
		return
	}
	if confirm := strings.TrimSpace(body.ConfirmInstanceID); confirm == "" {
		writeError(w, "confirm the instance id from the discovery doc (preview it first)")
		return
	} else if confirm != doc.InstanceID {
		writeError(w, fmt.Sprintf("the confirmed instance id %q does not match the discovery doc (%q) — preview the address and confirm what it says", confirm, doc.InstanceID))
		return
	}

	conn := &observeConnection{
		BaseURL: base, InstanceID: doc.InstanceID, Product: doc.Product, Version: doc.Version,
		DashboardURL: doc.DashboardURL, IngestURL: doc.IngestURL, Capabilities: doc.Capabilities,
		Token: token, TokenFP: tokenFingerprint(token), AllowInternal: body.AllowInternal,
		ConnectedAt: time.Now().UTC(), ConnectedBy: requestActor(r),
	}
	scopes, err := verifyObserveToken(r.Context(), conn)
	if err != nil {
		writeError(w, "the token did not verify: "+scrubSecret(err.Error(), token))
		return
	}
	conn.ScopesSeen = scopes

	o := &s.observe
	o.mu.Lock()
	o.rec.Connection = conn
	if err := o.saveLocked(); err != nil {
		o.rec.Connection = nil
		o.mu.Unlock()
		writeError(w, "the connection could not be stored: "+scrubSecret(err.Error(), token))
		return
	}
	view := conn.view()
	o.mu.Unlock()

	idShown := conn.InstanceID
	if len(idShown) > 8 {
		idShown = idShown[:8]
	}
	s.auditf(r, "observe.connect", "observe/"+doc.InstanceID, "connected to %s (instance %s…, %s)", base, idShown, doc.Version)
	writeData(w, view)
}

// handleObserveDisconnect forgets dash's connection. The token stays valid
// in Observe until revoked there — the response says so.
func (s *Server) handleObserveDisconnect(w http.ResponseWriter, r *http.Request) {
	o := &s.observe
	o.mu.Lock()
	conn := o.rec.Connection
	o.rec.Connection = nil
	err := o.saveLocked()
	o.mu.Unlock()
	if err != nil {
		// Put it back: a failed save must not look like a disconnect.
		o.mu.Lock()
		o.rec.Connection = conn
		o.mu.Unlock()
		writeError(w, "the disconnect could not be stored: "+err.Error())
		return
	}
	if conn != nil {
		s.auditf(r, "observe.disconnect", "observe/"+conn.InstanceID, "disconnected from %s — revoke the token in Observe Settings", conn.BaseURL)
	}
	writeData(w, map[string]any{"ok": true, "note": "Revoke the service token in Observe (Settings → Service Tokens) if you want it dead; dash no longer holds it."})
}

// ── per-app telemetry opt-in (plan item 4) ───────────────────────────────────
//
// Opt-in is plan-first: the plan endpoint computes EXACTLY what apply will
// do (site ensure by external_ref, one labelled telemetry key, the env and
// secret writes with their values or [secret] markers, the redeploy
// operation that makes them reach the container) and returns a fingerprint;
// apply recomputes the plan and refuses a fingerprint that moved, so what
// the owner approved is what runs.

const (
	observeExternalRefPrefix = "dash/"
	observeKeyLabel          = "dash-telemetry"
	// observeResourceAttrs is written literally as the programme contract
	// specifies it: the $TEPLOY_VERSION reference resolves inside the app's
	// container, where the CLI injects TEPLOY_VERSION at deploy time.
	observeResourceAttrs = "service.version=$TEPLOY_VERSION,deployment.environment=production"
)

type observeEnvWrite struct {
	Key    string `json:"key"`
	Value  string `json:"value"` // [secret] marker in plans when the value is a secret
	Secret bool   `json:"secret,omitempty"`
}

type observeTelemetryPlan struct {
	Server      string `json:"server"`
	App         string `json:"app"`
	ExternalRef string `json:"external_ref"`
	KeyLabel    string `json:"key_label"`
	IngestURL   string `json:"ingest_url"`
	InstanceID  string `json:"instance_id"`
	// SiteAction says whether the site already exists under this ref (a
	// local re-apply) or will be created — Observe answers that at apply
	// time (create-by-ref is idempotent), so the plan words it honestly.
	SiteAction string            `json:"site_action"`
	Env        []observeEnvWrite `json:"env"`
	Redeploy   string            `json:"redeploy"`
	// Rotation names the previous opt-in when this apply replaces one.
	Rotation    *observeAppTelemetry `json:"rotation,omitempty"`
	Fingerprint string               `json:"fingerprint"`
}

// observeTelemetryTarget validates a (server, app) pair the caller already
// decoded from its request.
func (s *Server) observeTelemetryTarget(w http.ResponseWriter, serverName, appName string) (string, string, bool) {
	serverName, appName = strings.TrimSpace(serverName), strings.TrimSpace(appName)
	if serverName == "" || appName == "" {
		writeError(w, "server and app are required")
		return "", "", false
	}
	if _, found := s.lookupServer(serverName); !found {
		writeErrorStatus(w, "server not found: "+serverName, http.StatusNotFound)
		return "", "", false
	}
	return serverName, appName, true
}

// buildObserveTelemetryPlan computes the plan and its fingerprint. Pure: no
// Observe call, no CLI call — the owner approves words, not side effects.
func buildObserveTelemetryPlan(serverName, appName, ingestURL, instanceID string, rotation *observeAppTelemetry) observeTelemetryPlan {
	ref := observeExternalRefPrefix + serverName + "/" + appName
	plan := observeTelemetryPlan{
		Server: serverName, App: appName,
		ExternalRef: ref, KeyLabel: observeKeyLabel,
		IngestURL: ingestURL, InstanceID: instanceID,
		SiteAction: "ensure the site for " + ref + " (created on Observe if absent — idempotent)",
		Env: []observeEnvWrite{
			{Key: "OBSERVE_ENDPOINT", Value: ingestURL},
			{Key: "OBSERVE_API_KEY", Value: "[secret: the telemetry key]", Secret: true},
			{Key: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: ingestURL},
			{Key: "OTEL_EXPORTER_OTLP_HEADERS", Value: "[secret: x-api-key=<the telemetry key>]", Secret: true},
			{Key: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "http/protobuf"},
			{Key: "OTEL_SERVICE_NAME", Value: appName},
			{Key: "OTEL_RESOURCE_ATTRIBUTES", Value: observeResourceAttrs},
		},
		Redeploy: "queue a redeploy (durable operation) so the new environment reaches the app's containers",
		Rotation: rotation,
	}
	raw := plan.Server + "\x00" + plan.App + "\x00" + plan.ExternalRef + "\x00" + plan.KeyLabel + "\x00" +
		plan.IngestURL + "\x00" + plan.InstanceID + "\x00" + fmt.Sprint(rotation != nil)
	for _, e := range plan.Env {
		raw += "\x00" + e.Key + "=" + e.Value
	}
	sum := sha256.Sum256([]byte(raw))
	plan.Fingerprint = hex.EncodeToString(sum[:16])
	return plan
}

// handleObserveTelemetryPlan serves GET /api/observe/telemetry/plan: what
// apply would do, with nothing done.
func (s *Server) handleObserveTelemetryPlan(w http.ResponseWriter, r *http.Request) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Siblings && f.Secrets && f.Redeploy }, "Per-app telemetry", siblingsMinCLI) {
		return
	}
	serverName, appName, ok := s.observeTelemetryTarget(w, r.URL.Query().Get("server"), r.URL.Query().Get("app"))
	if !ok {
		return
	}
	conn, connected := s.observe.connection()
	if !connected {
		writeErrorStatus(w, "connect Observe first (no connection is stored)", http.StatusConflict)
		return
	}
	doc, err := observeDocStillCurrent(r.Context(), &conn)
	if err != nil {
		writeErrorStatus(w, err.Error(), http.StatusConflict)
		return
	}
	if doc.IngestURL == "" {
		// Never guess: a separate ingest listener without OBSERVE_INGEST_URL
		// publishes no ingest address; the owner sets it in Observe.
		writeErrorStatus(w, "the discovery doc publishes no ingest URL (Observe has a separate ingest listener and OBSERVE_INGEST_URL is unset) — set it in Observe and plan again", http.StatusConflict)
		return
	}
	o := &s.observe
	o.mu.Lock()
	rotation, rotating := o.rec.Apps[serverName+"/"+appName]
	o.mu.Unlock()
	var rotPtr *observeAppTelemetry
	if rotating {
		rotPtr = &rotation
	}
	writeData(w, buildObserveTelemetryPlan(serverName, appName, doc.IngestURL, conn.InstanceID, rotPtr))
}

// observeEnsureSite creates-or-gets the site by external_ref (Observe's
// idempotent create; a replay returns the same site).
func observeEnsureSite(ctx context.Context, conn *observeConnection, externalRef, name string) (siteID string, created bool, err error) {
	resp := observeCall(ctx, conn, http.MethodPost, "/api/v1/service/sites", map[string]string{"external_ref": externalRef, "name": name})
	if resp.Err != nil {
		return "", false, resp.Err
	}
	var out struct {
		Created bool `json:"created"`
		Site    struct {
			SiteID      string `json:"site_id"`
			ExternalRef string `json:"external_ref"`
			ManagedBy   string `json:"managed_by"`
		} `json:"site"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return "", false, fmt.Errorf("Observe's site answer could not be read (HTTP %d)", resp.Status)
	}
	switch resp.Status {
	case http.StatusCreated, http.StatusOK:
		if out.Site.SiteID == "" {
			return "", false, fmt.Errorf("Observe's site answer carried no site id")
		}
		return out.Site.SiteID, out.Created, nil
	case http.StatusForbidden:
		return "", false, fmt.Errorf("the service token lacks sites:create — re-mint it in Observe with the scope")
	case http.StatusServiceUnavailable, http.StatusTooManyRequests:
		return "", false, fmt.Errorf("Observe is busy (HTTP %d) — try again", resp.Status)
	default:
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.Status)
		}
		return "", false, fmt.Errorf("creating the site failed: %s", msg)
	}
}

// observeMintKey mints one labelled telemetry key for the site.
func observeMintKey(ctx context.Context, conn *observeConnection, siteID, label string) (key, keyID string, err error) {
	resp := observeCall(ctx, conn, http.MethodPost, "/api/v1/service/sites/"+siteID+"/keys", map[string]string{"label": label})
	if resp.Err != nil {
		return "", "", resp.Err
	}
	var out struct {
		Key  string `json:"key"`
		Info struct {
			KeyID string `json:"key_id"`
		} `json:"info"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return "", "", fmt.Errorf("Observe's key answer could not be read (HTTP %d)", resp.Status)
	}
	switch resp.Status {
	case http.StatusCreated:
		if out.Key == "" {
			return "", "", fmt.Errorf("Observe's key answer carried no key")
		}
		return out.Key, out.Info.KeyID, nil
	case http.StatusForbidden:
		return "", "", fmt.Errorf("the service token lacks keys:mint — re-mint it in Observe with the scope")
	case http.StatusServiceUnavailable, http.StatusTooManyRequests:
		return "", "", fmt.Errorf("Observe is busy (HTTP %d) — try again", resp.Status)
	default:
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.Status)
		}
		return "", "", fmt.Errorf("minting the telemetry key failed: %s", msg)
	}
}

// writeObserveSecret writes one secret via `secret set KEY --stdin` (value on
// the CLI's stdin, never argv).
func (s *Server) writeObserveSecret(r *http.Request, serverName, appName, key, value string) error {
	if !s.cliInstalled() {
		return errors.New("teploy CLI not installed")
	}
	target, err := s.appTargetArgs(r.Context(), serverName, appName)
	if err != nil {
		return err
	}
	args := append([]string{"secret", "set", key, "--stdin"}, target...)
	args = append(args, s.requestActorArgs(r)...)
	if _, err := s.runCLIStdin(r.Context(), value, args...); err != nil {
		return errors.New(scrubSecret(err.Error(), value))
	}
	return nil
}

// handleObserveTelemetryApply serves POST /api/observe/telemetry: the
// owner-approved plan, executed in order — instance fence, site ensure, key
// mint, env/secret writes, then the redeploy operation.
func (s *Server) handleObserveTelemetryApply(w http.ResponseWriter, r *http.Request) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Siblings && f.Secrets && f.Redeploy }, "Per-app telemetry", siblingsMinCLI) {
		return
	}
	var body struct {
		Server      string `json:"server"`
		App         string `json:"app"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body: "+err.Error())
		return
	}
	serverName, appName, ok := s.observeTelemetryTarget(w, body.Server, body.App)
	if !ok {
		return
	}
	conn, connected := s.observe.connection()
	if !connected {
		writeErrorStatus(w, "connect Observe first (no connection is stored)", http.StatusConflict)
		return
	}
	doc, err := observeDocStillCurrent(r.Context(), &conn)
	if err != nil {
		writeErrorStatus(w, err.Error(), http.StatusConflict)
		return
	}
	if doc.IngestURL == "" {
		writeErrorStatus(w, "the discovery doc publishes no ingest URL — set OBSERVE_INGEST_URL in Observe and plan again", http.StatusConflict)
		return
	}
	o := &s.observe
	o.mu.Lock()
	prev, rotating := o.rec.Apps[serverName+"/"+appName]
	o.mu.Unlock()
	var rotPtr *observeAppTelemetry
	if rotating {
		rotPtr = &prev
	}
	plan := buildObserveTelemetryPlan(serverName, appName, doc.IngestURL, conn.InstanceID, rotPtr)
	if strings.TrimSpace(body.Fingerprint) == "" {
		writeError(w, "the plan's fingerprint is required — load the plan, approve it, then apply")
		return
	}
	if body.Fingerprint != plan.Fingerprint {
		writeErrorStatus(w, "the plan changed since it was approved (Observe moved, or the app changed) — review the plan and approve again", http.StatusConflict)
		return
	}
	// The redeploy is a deploy-flow action: the credential capability that
	// gates this route is not enough to run it.
	if !s.callerHolds(r, "execute.deploy") {
		writeErrorStatus(w, "missing capability: execute.deploy", http.StatusForbidden)
		return
	}

	siteID, _, err := observeEnsureSite(r.Context(), &conn, plan.ExternalRef, appName)
	if err != nil {
		writeError(w, scrubSecret(err.Error(), conn.Token))
		return
	}
	key, keyID, err := observeMintKey(r.Context(), &conn, siteID, plan.KeyLabel)
	if err != nil {
		writeError(w, scrubSecret(err.Error(), conn.Token))
		return
	}

	headers := "x-api-key=" + key
	plain := map[string]string{
		"OBSERVE_ENDPOINT":            plan.IngestURL,
		"OTEL_EXPORTER_OTLP_ENDPOINT": plan.IngestURL,
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		"OTEL_SERVICE_NAME":           appName,
		"OTEL_RESOURCE_ATTRIBUTES":    observeResourceAttrs,
	}
	written := []string{}
	for _, k := range sortedKeys(plain) {
		if _, err := s.envSet(r, serverName, appName, k, plain[k]); err != nil {
			s.observePartialFailure(w, r, serverName, appName, written, err, key)
			return
		}
		written = append(written, k)
	}
	if err := s.writeObserveSecret(r, serverName, appName, "OBSERVE_API_KEY", key); err != nil {
		s.observePartialFailure(w, r, serverName, appName, written, err, key)
		return
	}
	written = append(written, "OBSERVE_API_KEY")
	if err := s.writeObserveSecret(r, serverName, appName, "OTEL_EXPORTER_OTLP_HEADERS", headers); err != nil {
		s.observePartialFailure(w, r, serverName, appName, written, err, key)
		return
	}
	written = append(written, "OTEL_EXPORTER_OTLP_HEADERS")

	record := observeAppTelemetry{
		Server: serverName, App: appName, ExternalRef: plan.ExternalRef,
		SiteID: siteID, KeyID: keyID, KeyLabel: plan.KeyLabel,
		IngestURL: plan.IngestURL, InstanceID: conn.InstanceID,
		EnvKeys:    []string{"OBSERVE_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES"},
		SecretKeys: []string{"OBSERVE_API_KEY", "OTEL_EXPORTER_OTLP_HEADERS"},
		AppliedAt:  time.Now().UTC(), AppliedBy: requestActor(r),
	}

	op, _, err := s.operations.Enqueue(operation.Request{Kind: operation.KindRedeploy, Server: serverName, App: appName}, r.Header.Get("Idempotency-Key"), actorFromRequest(r))
	if err != nil {
		// The env writes stand; the redeploy is the only missing piece.
		o.mu.Lock()
		o.rec.Apps[serverName+"/"+appName] = record
		saveErr := o.saveLocked()
		o.mu.Unlock()
		s.auditf(r, "observe.telemetry", "server/"+serverName+" app/"+appName, "telemetry wired (site %s, key %s) but the redeploy could not be queued: %v — run it from the app page", siteID, keyID, err)
		writeErrorStatus(w, "Telemetry is wired but the redeploy could not be queued: "+err.Error()+". Run the redeploy from the app's page.", http.StatusServiceUnavailable)
		if saveErr != nil {
			log.Printf("[observe] persisting the opt-in failed after a partial apply: %v", saveErr)
		}
		return
	}

	o.mu.Lock()
	if o.rec.Apps == nil {
		o.rec.Apps = map[string]observeAppTelemetry{}
	}
	o.rec.Apps[serverName+"/"+appName] = record
	if err := o.saveLocked(); err != nil {
		o.mu.Unlock()
		s.auditf(r, "observe.telemetry", "server/"+serverName+" app/"+appName, "telemetry wired (site %s, key %s, redeploy %s) but the record could not be stored", siteID, keyID, op.ID)
		writeError(w, "Telemetry is wired and the redeploy is queued, but the opt-in record could not be stored: "+err.Error())
		return
	}
	o.mu.Unlock()

	rotation := ""
	if plan.Rotation != nil {
		rotation = " (rotated from key " + plan.Rotation.KeyID + " — revoke it in Observe if it is no longer used)"
	}
	s.auditf(r, "observe.telemetry", "server/"+serverName+" app/"+appName, "telemetry wired: site %s, key %s%s, 7 env/secret writes, redeploy %s", siteID, keyID, rotation, op.ID)
	writeData(w, map[string]any{
		"site_id": siteID, "key_id": keyID, "operation": op,
		"env_keys": record.EnvKeys, "secret_keys": record.SecretKeys,
		"note": "Redeploy queued — follow it in Activity. The telemetry key was shown to nobody; it lives in the app's secrets.",
	})
}

// observePartialFailure answers a failed apply honestly: what is already
// written stands, nothing is recorded, and the owner is told a spare key may
// exist in Observe until revoked there.
func (s *Server) observePartialFailure(w http.ResponseWriter, r *http.Request, serverName, appName string, written []string, err error, key string) {
	msg := scrubSecret(err.Error(), key)
	s.auditf(r, "observe.telemetry", "server/"+serverName+" app/"+appName, "telemetry apply failed after %d write(s): %v", len(written), msg)
	writeErrorStatus(w, fmt.Sprintf("the apply failed part-way (%s). Written so far: %s. Nothing was recorded — re-running the plan is safe (the site is idempotent); a re-run mints a fresh key, so revoke the spare in Observe Settings.", msg, strings.Join(written, ", ")), http.StatusBadGateway)
}

// handleObserveTelemetryDisable undoes exactly what the opt-in recorded:
// the env keys and secrets dash wrote are removed, a redeploy is queued,
// and the owner is told the site and key live on in Observe until revoked
// there. Works without a live connection — it undoes dash's own writes.
func (s *Server) handleObserveTelemetryDisable(w http.ResponseWriter, r *http.Request) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Secrets && f.Redeploy }, "Per-app telemetry", "v0.1.45") {
		return
	}
	var body struct {
		Server string `json:"server"`
		App    string `json:"app"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body: "+err.Error())
		return
	}
	serverName, appName, ok := s.observeTelemetryTarget(w, body.Server, body.App)
	if !ok {
		return
	}
	o := &s.observe
	o.mu.Lock()
	record, exists := o.rec.Apps[serverName+"/"+appName]
	if exists {
		delete(o.rec.Apps, serverName+"/"+appName)
	}
	o.mu.Unlock()
	if !exists {
		writeErrorStatus(w, appName+" on "+serverName+" is not opted in", http.StatusNotFound)
		return
	}

	var removed, failed []string
	for _, k := range record.EnvKeys {
		if _, err := s.envUnset(r, serverName, appName, k); err != nil {
			failed = append(failed, k)
			continue
		}
		removed = append(removed, k)
	}
	for _, k := range record.SecretKeys {
		if _, err := s.cliAppRun(r.Context(), serverName, appName, append([]string{"secret", "rm", k}, s.requestActorArgs(r)...)...); err != nil {
			failed = append(failed, k)
			continue
		}
		removed = append(removed, k)
	}
	if len(failed) > 0 {
		// Put the record back with what is still set: a failed disable must
		// not leave the page believing telemetry is gone.
		remaining := observeAppTelemetry(record)
		remaining.EnvKeys, remaining.SecretKeys = nil, nil
		for _, k := range failed {
			if k == "OBSERVE_API_KEY" || k == "OTEL_EXPORTER_OTLP_HEADERS" {
				remaining.SecretKeys = append(remaining.SecretKeys, strings.SplitN(k, " (", 2)[0])
			} else {
				remaining.EnvKeys = append(remaining.EnvKeys, strings.SplitN(k, " (", 2)[0])
			}
		}
		o.mu.Lock()
		o.rec.Apps[serverName+"/"+appName] = remaining
		o.saveLocked()
		o.mu.Unlock()
		writeError(w, "the disable removed "+strings.Join(removed, ", ")+" but failed on "+strings.Join(failed, ", ")+" — the opt-in stays recorded for what is still set; retry after fixing the failure")
		return
	}

	op, _, err := s.operations.Enqueue(operation.Request{Kind: operation.KindRedeploy, Server: serverName, App: appName}, r.Header.Get("Idempotency-Key"), actorFromRequest(r))
	if err != nil {
		o.mu.Lock()
		o.rec.Apps[serverName+"/"+appName] = record
		o.saveLocked()
		o.mu.Unlock()
		writeErrorStatus(w, "the env writes are gone but the redeploy could not be queued: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	o.mu.Lock()
	o.saveLocked()
	o.mu.Unlock()
	s.auditf(r, "observe.telemetry.disable", "server/"+serverName+" app/"+appName, "telemetry unwired (site %s and key %s stay in Observe until revoked there), redeploy %s", record.SiteID, record.KeyID, op.ID)
	writeData(w, map[string]any{"operation": op, "removed": removed,
		"note": "Redeploy queued so the app restarts without telemetry. The site and key remain in Observe — revoke the key there if you want it dead."})
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
