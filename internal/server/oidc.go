package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC single sign-on (Phase 2 of the Teploy RBAC contract). Dash acts as an
// OpenID Connect relying party: login is delegated to an external identity
// provider (the customer's own Okta/Azure AD/Google/Keycloak — "generic OIDC" —
// or Teploy Platform acting as the IdP for Cloud). The IdP authenticates the
// user; Dash trusts the signed ID token, maps a claim to admin/editor/viewer,
// and mints its normal session cookie. Role is re-read from the token on every
// login, so the IdP stays authoritative — no local role copy to drift.
//
// Local username/password accounts remain available as the break-glass path
// (env bootstrap admin, existing operators) so a down IdP never locks everyone
// out. SSO is enabled only when TEPLOY_DASH_OIDC_ISSUER + _CLIENT_ID are set.

const (
	oidcStateCookie = "teploy_dash_oidc_state"
	oidcFlowTTL     = 10 * time.Minute
)

// oidcAuth holds SSO configuration and the (lazily discovered) provider. The
// provider is built on first use rather than at startup so an IdP that is down
// at boot doesn't permanently disable SSO — the next login retries discovery.
type oidcAuth struct {
	issuer       string
	clientID     string
	clientSecret string
	redirectURL  string // optional; derived per-request from Host when empty
	scopes       []string
	label        string

	usernameClaim string
	roleClaim     string
	groupsClaim   string
	adminGroup    string
	editorGroup   string
	viewerGroup   string
	defaultRole   string

	initMu   sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier

	flowMu sync.Mutex
	flows  map[string]*oidcFlow
}

// oidcFlow is one in-progress login, keyed by the OAuth state parameter and
// bound to the initiating browser by the state cookie. It carries the nonce and
// PKCE verifier that the callback must present, plus the post-login redirect.
type oidcFlow struct {
	nonce    string
	verifier string
	next     string
	exp      time.Time
}

// newOIDCAuth reads SSO configuration from the environment. It returns nil (SSO
// disabled) unless at least the issuer and client ID are set — everything else
// has a safe default.
func newOIDCAuth() *oidcAuth {
	issuer := strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_ISSUER"))
	clientID := strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_CLIENT_ID"))
	if issuer == "" || clientID == "" {
		return nil
	}
	scopes := parseOIDCScopes(os.Getenv("TEPLOY_DASH_OIDC_SCOPES"))
	defaultRole := normalizeRole(strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_DEFAULT_ROLE")))
	o := &oidcAuth{
		issuer:        issuer,
		clientID:      clientID,
		clientSecret:  strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_CLIENT_SECRET")),
		redirectURL:   strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_REDIRECT_URL")),
		scopes:        scopes,
		label:         orDefault(strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_LABEL")), "Single sign-on"),
		usernameClaim: orDefault(strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_USERNAME_CLAIM")), "preferred_username"),
		roleClaim:     orDefault(strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_ROLE_CLAIM")), "teploy_role"),
		groupsClaim:   orDefault(strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_GROUPS_CLAIM")), "groups"),
		adminGroup:    strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_ADMIN_GROUP")),
		editorGroup:   strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_EDITOR_GROUP")),
		viewerGroup:   strings.TrimSpace(os.Getenv("TEPLOY_DASH_OIDC_VIEWER_GROUP")),
		defaultRole:   defaultRole,
		flows:         make(map[string]*oidcFlow),
	}
	log.Printf("auth: OIDC SSO enabled (issuer %s)", issuer)
	return o
}

func parseOIDCScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{oidc.ScopeOpenID, "profile", "email"}
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if !seen[oidc.ScopeOpenID] {
		out = append([]string{oidc.ScopeOpenID}, out...)
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ensure lazily discovers the provider and builds the ID-token verifier. Only a
// successful result is cached; a failure is returned to this caller and retried
// on the next login.
func (o *oidcAuth) ensure(ctx context.Context) error {
	o.initMu.Lock()
	defer o.initMu.Unlock()
	if o.provider != nil {
		return nil
	}
	p, err := oidc.NewProvider(ctx, o.issuer)
	if err != nil {
		return err
	}
	o.provider = p
	o.verifier = p.Verifier(&oidc.Config{ClientID: o.clientID})
	return nil
}

// oauthConfig builds the OAuth2 config for a given redirect URL. ensure must
// have succeeded first (o.provider set).
func (o *oidcAuth) oauthConfig(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     o.clientID,
		ClientSecret: o.clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     o.provider.Endpoint(),
		Scopes:       o.scopes,
	}
}

// storeFlow records an in-progress login and prunes expired ones.
func (o *oidcAuth) storeFlow(state string, f *oidcFlow) {
	o.flowMu.Lock()
	defer o.flowMu.Unlock()
	now := time.Now()
	for k, v := range o.flows {
		if now.After(v.exp) {
			delete(o.flows, k)
		}
	}
	o.flows[state] = f
}

// takeFlow returns and removes the flow for a state (one-time use).
func (o *oidcAuth) takeFlow(state string) (*oidcFlow, bool) {
	o.flowMu.Lock()
	defer o.flowMu.Unlock()
	f, ok := o.flows[state]
	if !ok {
		return nil, false
	}
	delete(o.flows, state)
	if time.Now().After(f.exp) {
		return nil, false
	}
	return f, true
}

// resolveUsername picks a stable identity from the token claims. It prefers the
// configured username claim, then preferred_username, email, and sub.
func (o *oidcAuth) resolveUsername(claims map[string]any) string {
	for _, c := range []string{o.usernameClaim, "preferred_username", "email", "sub"} {
		if c == "" {
			continue
		}
		if s := claimString(claims[c]); s != "" {
			return s
		}
	}
	return ""
}

// resolveRole maps the token claims to a Teploy role. A direct role claim (e.g.
// teploy_role, as Platform emits) wins; failing that, a group claim is matched
// against the configured admin/editor/viewer groups (admin > editor > viewer);
// failing that, the configured default role (viewer unless overridden).
func (o *oidcAuth) resolveRole(claims map[string]any) string {
	if o.roleClaim != "" {
		if r, ok := knownRole(claimString(claims[o.roleClaim])); ok {
			return r
		}
	}
	if o.groupsClaim != "" {
		groups := claimStrings(claims[o.groupsClaim])
		for _, g := range groups {
			if o.adminGroup != "" && g == o.adminGroup {
				return RoleAdmin
			}
		}
		for _, g := range groups {
			if o.editorGroup != "" && g == o.editorGroup {
				return RoleEditor
			}
		}
		for _, g := range groups {
			if o.viewerGroup != "" && g == o.viewerGroup {
				return RoleViewer
			}
		}
	}
	return o.defaultRole
}

// knownRole reports whether s is exactly one of the canonical roles (so an
// unrecognized claim value falls through to group/default resolution rather
// than being silently coerced to viewer).
func knownRole(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case RoleAdmin:
		return RoleAdmin, true
	case RoleEditor:
		return RoleEditor, true
	case RoleViewer:
		return RoleViewer, true
	}
	return "", false
}

func claimString(v any) string {
	s, _ := v.(string)
	return s
}

func claimStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// effectiveRedirect returns the OAuth redirect URL, preferring the explicitly
// configured value (required when Dash runs behind a proxy that rewrites Host)
// and otherwise deriving it from the request.
func (g *authGate) effectiveRedirect(r *http.Request, o *oidcAuth) string {
	if o.redirectURL != "" {
		return o.redirectURL
	}
	scheme := "http"
	if g.secureCookie(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/oidc/callback"
}

// handleOIDCLogin starts the authorization-code flow: it mints state, nonce and
// a PKCE verifier, stashes them server-side, sets the state cookie, and
// redirects the browser to the IdP.
func (g *authGate) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	o := g.oidc
	if o == nil {
		http.Error(w, "SSO is not configured", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	if err := o.ensure(ctx); err != nil {
		log.Printf("auth: OIDC discovery failed: %v", err)
		g.oidcFail(w, r, "SSO provider is unavailable — try again shortly")
		return
	}
	state := oidcRandToken()
	nonce := oidcRandToken()
	verifier := oauth2.GenerateVerifier()
	o.storeFlow(state, &oidcFlow{
		nonce:    nonce,
		verifier: verifier,
		next:     sanitizeNext(r.URL.Query().Get("next")),
		exp:      time.Now().Add(oidcFlowTTL),
	})
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: state, Path: "/", MaxAge: int(oidcFlowTTL.Seconds()),
		HttpOnly: true, Secure: g.secureCookie(r), SameSite: http.SameSiteLaxMode,
	})
	cfg := o.oauthConfig(g.effectiveRedirect(r, o))
	authURL := cfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback completes the flow: verify state, exchange the code,
// verify the ID token (signature, audience, expiry, nonce), map the claims to a
// username and role, and issue the session cookie.
func (g *authGate) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	o := g.oidc
	if o == nil {
		http.Error(w, "SSO is not configured", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		g.oidcFail(w, r, "SSO error: "+strings.TrimSpace(e+" "+q.Get("error_description")))
		return
	}

	state := q.Get("state")
	cookie, err := r.Cookie(oidcStateCookie)
	// Clear the state cookie regardless of outcome — it's single-use.
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: g.secureCookie(r), SameSite: http.SameSiteLaxMode,
	})
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		g.oidcFail(w, r, "SSO state mismatch — please sign in again")
		return
	}
	flow, ok := o.takeFlow(state)
	if !ok {
		g.oidcFail(w, r, "SSO session expired — please sign in again")
		return
	}

	if err := o.ensure(ctx); err != nil {
		g.oidcFail(w, r, "SSO provider is unavailable — try again shortly")
		return
	}
	cfg := o.oauthConfig(g.effectiveRedirect(r, o))
	tok, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(flow.verifier))
	if err != nil {
		log.Printf("auth: OIDC token exchange failed: %v", err)
		g.oidcFail(w, r, "SSO sign-in failed — please try again")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		g.oidcFail(w, r, "SSO response was missing an ID token")
		return
	}
	idToken, err := o.verifier.Verify(ctx, rawID)
	if err != nil {
		log.Printf("auth: OIDC ID-token verification failed: %v", err)
		g.oidcFail(w, r, "SSO sign-in failed — please try again")
		return
	}
	if idToken.Nonce != flow.nonce {
		g.oidcFail(w, r, "SSO sign-in failed — please try again")
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		g.oidcFail(w, r, "SSO sign-in failed — please try again")
		return
	}
	username := o.resolveUsername(claims)
	if username == "" {
		g.oidcFail(w, r, "SSO identity has no usable username claim")
		return
	}
	// "token" is reserved for the bearer/bootstrap identity in sibling products;
	// keep it out of the session namespace here too.
	if strings.EqualFold(username, "token") {
		g.oidcFail(w, r, "SSO username is reserved")
		return
	}
	role := o.resolveRole(claims)
	g.recordSuccess(g.clientIP(r))
	g.issueSessionCookie(w, r, username, role)
	http.Redirect(w, r, flow.next, http.StatusFound)
}

// oidcFail logs nothing sensitive and bounces the user back to the login page
// with a short human-readable message.
func (g *authGate) oidcFail(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/login?error="+url.QueryEscape(msg), http.StatusFound)
}

// sanitizeNext keeps post-login redirects on this origin (no open redirect).
func sanitizeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		return next
	}
	return "/"
}

func oidcRandToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}
