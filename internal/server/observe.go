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
