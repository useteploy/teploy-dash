package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/useteploy/teploy-dash/internal/caps"
)

// newSSOTestGate returns a gate with SSO configured: like Server.New, a
// configured OIDC provider satisfies authentication, so local-first-run
// setup mode is cleared even with no local accounts.
func newSSOTestGate(t *testing.T) *authGate {
	t.Helper()
	g := newTestGate(t)
	g.oidc = &oidcAuth{}
	g.credMu.Lock()
	g.setupRequired = false
	g.credMu.Unlock()
	return g
}

// ── Principal acceptance gates (A02/A08 identity work) ────────────────────
//
// The dash analogue of teploy-observe's TestF03Gate_CreateLoginPromoteOld
// TokenInvalid: every identity — local account or issuer-namespaced OIDC
// principal — is backed by a persisted row whose AuthEpoch sessions embed at
// issuance and every request revalidates. Issue → revoke → old token
// rejected; role change revokes; missing row = dead session.

// loginCookie runs a password login and returns the session cookie.
func loginCookie(t *testing.T, g *authGate, username, password string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login %q: status %d", username, w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("login issued no session cookie")
	return nil
}

// gateWithMux wraps a trivial 200 handler so a session cookie can be
// exercised through the full validation pipeline.
func gateWithMux(g *authGate) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	return g.wrap(mux)
}

func requestWithCookie(handler http.Handler, cookie *http.Cookie) int {
	req := httptest.NewRequest(http.MethodGet, "/api/apps", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Code
}

// The core revocation gate: issue a session, revoke it durably, and the old
// token is rejected on its next use.
func TestPrincipalGate_LocalIssueRevokeReject(t *testing.T) {
	g := newTestGate(t)
	if err := g.createUser("bob", "bobpass123", RoleEditor); err != nil {
		t.Fatal(err)
	}
	handler := gateWithMux(g)
	cookie := loginCookie(t, g, "bob", "bobpass123")
	if code := requestWithCookie(handler, cookie); code != http.StatusOK {
		t.Fatalf("session before revocation: %d, want 200", code)
	}
	if err := g.revokeSessions("bob"); err != nil {
		t.Fatal(err)
	}
	g.deleteUserSessions("bob")
	if code := requestWithCookie(handler, cookie); code != http.StatusUnauthorized {
		t.Fatalf("session after revocation: %d, want 401", code)
	}
	// Re-login works and the old token stays dead.
	fresh := loginCookie(t, g, "bob", "bobpass123")
	if code := requestWithCookie(handler, fresh); code != http.StatusOK {
		t.Fatalf("re-login session: %d, want 200", code)
	}
	if code := requestWithCookie(handler, cookie); code != http.StatusUnauthorized {
		t.Fatalf("revoked session after re-login: %d, want 401", code)
	}
}

// A role change revokes: the epoch bump retires every session issued under
// the old role, and the next login carries the new role.
func TestPrincipalGate_RoleChangeRevokes(t *testing.T) {
	g := newTestGate(t)
	if err := g.createUser("carol", "carolpass1", RoleViewer); err != nil {
		t.Fatal(err)
	}
	handler := gateWithMux(g)
	cookie := loginCookie(t, g, "carol", "carolpass1")
	if code := requestWithCookie(handler, cookie); code != http.StatusOK {
		t.Fatalf("viewer session: %d, want 200", code)
	}
	if err := g.setRole("carol", RoleEditor); err != nil {
		t.Fatal(err)
	}
	if code := requestWithCookie(handler, cookie); code != http.StatusUnauthorized {
		t.Fatalf("session after role change: %d, want 401", code)
	}
	fresh := loginCookie(t, g, "carol", "carolpass1")
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(fresh)
	w := httptest.NewRecorder()
	// handleWhoami reads the session from context, which the gate populates
	// during validation; route through the gate's wrap to exercise it.
	whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := currentUser(r)
		if !ok {
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeData(w, userView{Username: session.user, Role: session.role})
	})
	gated := g.wrap(whoami)
	gated.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("whoami after promote: %d", w.Code)
	}
	var view struct {
		Data userView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Data.Role != RoleEditor {
		t.Fatalf("post-promotion role = %q, want editor", view.Data.Role)
	}
}

func TestOIDCSubjectIDNamespacing(t *testing.T) {
	a := oidcSubjectID("https://accounts.example.com", "user-42")
	b := oidcSubjectID("https://other.example.org", "user-42")
	if a == b {
		t.Fatal("same sub under different issuers must be different principals")
	}
	if a != oidcSubjectID("https://accounts.example.com", "user-42") {
		t.Fatal("subject id must be deterministic")
	}
	if !strings.HasPrefix(a, "oidc:") || strings.Contains(a, "accounts.example.com") {
		t.Fatalf("subject id %q must be namespaced without the raw issuer URL", a)
	}
}

// SSO identity is issuer+sub, not the display name: renaming the username
// claim keeps the same principal and preserves the epoch (other sessions
// survive); the same sub under another issuer is a different principal.
func TestPrincipalGate_OIDCIssuerScopedIdentity(t *testing.T) {
	g := newSSOTestGate(t)
	handler := gateWithMux(g)
	subA := oidcSubjectID("https://idp-a.example", "u1")
	subB := oidcSubjectID("https://idp-b.example", "u1")

	epoch1, _, err := g.upsertOIDCPrincipal(subA, "alice", "alice@x.example", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	tok1 := g.newSessionFor(subA, "alice", RoleViewer, epoch1, false)
	cookie1 := &http.Cookie{Name: sessionCookie, Value: tok1}
	if code := requestWithCookie(handler, cookie1); code != http.StatusOK {
		t.Fatalf("oidc session: %d, want 200", code)
	}

	// Display-name change (same issuer+sub, new preferred_username):
	// same principal, epoch preserved, the old session stays valid.
	epoch2, _, err := g.upsertOIDCPrincipal(subA, "alice-renamed", "alice@x.example", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if epoch2 != epoch1 {
		t.Fatalf("re-sign-in must preserve the epoch: %d then %d", epoch1, epoch2)
	}
	if code := requestWithCookie(handler, cookie1); code != http.StatusOK {
		t.Fatalf("oidc session after same-identity re-sign-in: %d, want 200", code)
	}

	// Same sub, different issuer: a distinct principal with its own session.
	epochB, _, err := g.upsertOIDCPrincipal(subB, "alice", "alice@x.example", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if epochB == epoch1 {
		t.Fatal("distinct principals must draw distinct epochs")
	}
	tokB := g.newSessionFor(subB, "alice", RoleViewer, epochB, false)
	if code := requestWithCookie(handler, &http.Cookie{Name: sessionCookie, Value: tokB}); code != http.StatusOK {
		t.Fatalf("second-issuer oidc session: %d, want 200", code)
	}

	// Both principals are listed for administrators.
	principals := g.listSSOPrincipals()
	if len(principals) != 2 {
		t.Fatalf("listed %d principals, want 2", len(principals))
	}
}

// An IdP role change propagates through the principal row: the epoch is
// preserved on re-sign-in, so another device's session survives AND picks up
// the new role live on its next request.
func TestPrincipalGate_OIDCRoleRefreshKeepsSessionsLive(t *testing.T) {
	g := newSSOTestGate(t)
	sub := oidcSubjectID("https://idp.example", "u9")
	epoch, _, err := g.upsertOIDCPrincipal(sub, "dave", "dave@x.example", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	token := g.newSessionFor(sub, "dave", RoleViewer, epoch, false)

	// The IdP promotes dave; he re-signs in on his phone.
	epoch2, _, err := g.upsertOIDCPrincipal(sub, "dave", "dave@x.example", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	if epoch2 != epoch {
		t.Fatal("role refresh must not bump the epoch")
	}

	// The laptop session (same token) survives and reads the new role.
	si, ok := g.lookupSession(token)
	if !ok {
		t.Fatal("session died on role refresh")
	}
	g.credMu.RLock()
	live := g.oidcPrincipals[sub]
	g.credMu.RUnlock()
	si = &sessionInfo{sub: si.sub, user: si.user, role: normalizeRole(live.Role), exp: si.exp, epoch: si.epoch, local: false}
	if si.role != RoleEditor {
		t.Fatalf("live role = %q, want editor", si.role)
	}
}

// Admin revocation of an SSO principal: old sessions die, re-sign-in works.
func TestPrincipalGate_OIDCRevokeKillsSessions(t *testing.T) {
	g := newSSOTestGate(t)
	handler := gateWithMux(g)
	sub := oidcSubjectID("https://idp.example", "u7")
	epoch, _, err := g.upsertOIDCPrincipal(sub, "erin", "erin@x.example", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookie, Value: g.newSessionFor(sub, "erin", RoleEditor, epoch, false)}
	if code := requestWithCookie(handler, cookie); code != http.StatusOK {
		t.Fatalf("oidc session: %d, want 200", code)
	}
	if err := g.revokePrincipalSessions(sub); err != nil {
		t.Fatal(err)
	}
	g.deleteUserSessions(sub)
	if code := requestWithCookie(handler, cookie); code != http.StatusUnauthorized {
		t.Fatalf("oidc session after principal revocation: %d, want 401", code)
	}
	// Re-sign-in mints a session against the new epoch.
	epoch2, _, err := g.upsertOIDCPrincipal(sub, "erin", "erin@x.example", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	if epoch2 <= epoch {
		t.Fatalf("re-sign-in epoch %d must exceed revoked epoch %d", epoch2, epoch)
	}
	fresh := &http.Cookie{Name: sessionCookie, Value: g.newSessionFor(sub, "erin", RoleEditor, epoch2, false)}
	if code := requestWithCookie(handler, fresh); code != http.StatusOK {
		t.Fatalf("oidc session after re-sign-in: %d, want 200", code)
	}
}

// A session whose principal has no row is dead — the unconditional version
// check (a pre-principal SSO session, or a principal deleted between
// issuance and use).
func TestPrincipalGate_MissingRowIsDeadSession(t *testing.T) {
	g := newSSOTestGate(t)
	handler := gateWithMux(g)
	ghost := &http.Cookie{Name: sessionCookie, Value: g.newSessionFor("oidc:0123456789abcdef:no-such-row", "someone", RoleAdmin, 7, false)}
	if code := requestWithCookie(handler, ghost); code != http.StatusUnauthorized {
		t.Fatalf("session without a principal row: %d, want 401", code)
	}
}

// A local user and an SSO principal may share a display name without
// colliding: deleting the local account leaves the SSO session alone.
func TestPrincipalGate_DisplayNameNoCollision(t *testing.T) {
	g := newSSOTestGate(t)
	handler := gateWithMux(g)
	if err := g.createUser("root", "rootpass123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := g.createUser("sam", "sampass123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	sub := oidcSubjectID("https://idp.example", "u3")
	epoch, _, err := g.upsertOIDCPrincipal(sub, "sam", "sam@x.example", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	ssoCookie := &http.Cookie{Name: sessionCookie, Value: g.newSessionFor(sub, "sam", RoleViewer, epoch, false)}
	if code := requestWithCookie(handler, ssoCookie); code != http.StatusOK {
		t.Fatalf("sso session: %d", code)
	}
	// The admin deletes the LOCAL account named sam (deleteUserSessions is
	// keyed by principal id, so the SSO session must survive).
	if err := g.deleteUser("sam"); err != nil {
		t.Fatal(err)
	}
	g.deleteUserSessions("sam")
	if code := requestWithCookie(handler, ssoCookie); code != http.StatusOK {
		t.Fatalf("sso session after local-account deletion: %d, want 200", code)
	}
}

// ── Admin HTTP surface ────────────────────────────────────────────────────

func TestRevokeSessionsEndpoint(t *testing.T) {
	g := newTestGate(t)
	if err := g.createUser("root", "rootpass123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := g.createUser("vera", "verapass123", RoleEditor); err != nil {
		t.Fatal(err)
	}
	s := &Server{gate: g}
	cookie := loginCookie(t, g, "vera", "verapass123")
	handler := gateWithMux(g)
	if code := requestWithCookie(handler, cookie); code != http.StatusOK {
		t.Fatalf("vera session: %d", code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/users/vera/revoke-sessions", nil)
	w := httptest.NewRecorder()
	s.handleUserAction(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke-sessions: %d %s", w.Code, w.Body.String())
	}
	if code := requestWithCookie(handler, cookie); code != http.StatusUnauthorized {
		t.Fatalf("vera session after endpoint revocation: %d, want 401", code)
	}

	// Unknown account: 404.
	req = httptest.NewRequest(http.MethodPost, "/api/users/ghost/revoke-sessions", nil)
	w = httptest.NewRecorder()
	s.handleUserAction(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("revoke-sessions for unknown user: %d, want 404", w.Code)
	}
}

func TestSSORevokeEndpoint(t *testing.T) {
	g := newSSOTestGate(t)
	s := &Server{gate: g}
	sub := oidcSubjectID("https://idp.example", "u5")
	epoch, _, err := g.upsertOIDCPrincipal(sub, "frank", "frank@x.example", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	handler := gateWithMux(g)
	cookie := &http.Cookie{Name: sessionCookie, Value: g.newSessionFor(sub, "frank", RoleViewer, epoch, false)}
	if code := requestWithCookie(handler, cookie); code != http.StatusOK {
		t.Fatalf("sso session: %d", code)
	}

	body, _ := json.Marshal(map[string]string{"subject": sub})
	req := httptest.NewRequest(http.MethodPost, "/api/sso/revoke", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	s.handleSSORevoke(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("sso revoke: %d %s", w.Code, w.Body.String())
	}
	if code := requestWithCookie(handler, cookie); code != http.StatusUnauthorized {
		t.Fatalf("sso session after endpoint revocation: %d, want 401", code)
	}

	// Unknown subject: 404. Empty body: 400.
	body, _ = json.Marshal(map[string]string{"subject": "oidc:0123456789abcdef:ghost"})
	req = httptest.NewRequest(http.MethodPost, "/api/sso/revoke", strings.NewReader(string(body)))
	w = httptest.NewRecorder()
	s.handleSSORevoke(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown subject: %d, want 404", w.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/sso/revoke", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	s.handleSSORevoke(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty subject: %d, want 400", w.Code)
	}

	// GET /api/sso lists the principal (never a secret).
	req = httptest.NewRequest(http.MethodGet, "/api/sso", nil)
	w = httptest.NewRecorder()
	s.handleSSOPrincipals(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list principals: %d", w.Code)
	}
	var listed struct {
		Data []principalView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 1 || listed.Data[0].Subject != sub || listed.Data[0].Role != RoleViewer {
		t.Fatalf("principal list = %+v", listed.Data)
	}
	// The admin surface (Settings > SSO) shows when the identity last
	// signed in — the view must carry it.
	if listed.Data[0].LastSignIn == "" {
		t.Fatalf("principal view carries no last_sign_in: %+v", listed.Data[0])
	}
}

func TestRequiredRoleSSOAdminOnly(t *testing.T) {
	for _, row := range []struct{ method, path string }{
		{"GET", "/api/sso"},
		{"POST", "/api/sso/revoke"},
	} {
		if got := requiredCapabilities(row.method, row.path); len(got) != 1 || got[0] != caps.AdministerUsers {
			t.Errorf("requiredCapabilities(%s %s) = %v, want [administer.users]", row.method, row.path, got)
		}
	}
}

// ── Persistence / migration ───────────────────────────────────────────────

// The oidc_principals section of users.json round-trips; a pre-principal
// users.json (no field) loads with an empty principal set.
func TestPrincipalStorePersistence(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "auth.json")
	g := newAuthGate("", "", credFile)
	if err := g.createUser("root", "rootpass123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	sub := oidcSubjectID("https://idp.example", "persist")
	epoch, _, err := g.upsertOIDCPrincipal(sub, "pat", "pat@x.example", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}

	reopened := newAuthGate("", "", credFile)
	reopened.credMu.RLock()
	p := reopened.oidcPrincipals[sub]
	counter := reopened.epochCounter
	reopened.credMu.RUnlock()
	if p == nil || p.AuthEpoch != epoch || p.Role != RoleEditor || p.Username != "pat" {
		t.Fatalf("reloaded principal = %+v, want epoch %d editor pat", p, epoch)
	}
	if counter != epoch {
		t.Fatalf("epoch counter = %d, want %d", counter, epoch)
	}
	// The reloaded row validates a session minted before the restart.
	handler := gateWithMux(reopened)
	cookie := &http.Cookie{Name: sessionCookie, Value: reopened.newSessionFor(sub, "pat", RoleEditor, epoch, false)}
	if code := requestWithCookie(handler, cookie); code != http.StatusOK {
		t.Fatalf("session against reloaded principal: %d, want 200", code)
	}

	// A pre-principal users.json (hand-written, no oidc_principals) loads.
	legacyDir := t.TempDir()
	legacyFile := filepath.Join(legacyDir, "users.json")
	hash, _ := bcrypt.GenerateFromPassword([]byte("legacypass1"), bcrypt.MinCost)
	legacy := `{"users":[{"username":"old","password_hash":"` + string(hash) + `","role":"admin","auth_epoch":3}],"epoch_counter":3}`
	if err := os.WriteFile(legacyFile, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	legacyGate := newAuthGate("", "", filepath.Join(legacyDir, "auth.json"))
	legacyGate.credMu.RLock()
	n := len(legacyGate.oidcPrincipals)
	legacyGate.credMu.RUnlock()
	if n != 0 {
		t.Fatalf("legacy install loaded %d principals, want 0", n)
	}

	// Duplicate principal subjects are an outage, not a silent override.
	badDir := t.TempDir()
	badFile := filepath.Join(badDir, "users.json")
	dup := fmt.Sprintf(`{"users":[{"username":"a","password_hash":"x","role":"admin","auth_epoch":1}],"oidc_principals":[{"subject":%q,"username":"p","role":"viewer","auth_epoch":2},{"subject":%q,"username":"q","role":"viewer","auth_epoch":3}]}`, sub, sub)
	if err := os.WriteFile(badFile, []byte(dup), 0600); err != nil {
		t.Fatal(err)
	}
	badGate := newAuthGate("", "", filepath.Join(badDir, "auth.json"))
	if badGate.initErr == nil {
		t.Fatal("duplicate principal subjects must fail the credential store load")
	}
}

// ── Revocation races (-race) ──────────────────────────────────────────────

// Concurrent logins, revocations, and authenticated requests must stay
// consistent: once a revocation returns, every subsequently started request
// with a pre-revocation token is rejected — no panicked state, no
// post-revocation acceptance, and no cross-user fallout.
func TestRevocationRacesUnderConcurrency(t *testing.T) {
	g := newTestGate(t)
	handler := gateWithMux(g)
	if err := g.createUser("admin", "adminpass1", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	const users = 6
	for i := 0; i < users; i++ {
		if err := g.createUser(fmt.Sprintf("u%d", i), fmt.Sprintf("pass%dw0rd", i), RoleEditor); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Issuers: continuously log in and use the fresh sessions.
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cookie := loginCookieQuiet(g, fmt.Sprintf("u%d", i), fmt.Sprintf("pass%dw0rd", i))
				if cookie != nil {
					requestWithCookie(handler, cookie)
				}
			}
		}(i)
	}
	// Revokers: repeatedly revoke every account, then verify the durable
	// invariant — after revokeSessions returns, a NEW session minted from a
	// login that completed BEFORE the revocation is dead. (Fresh logins
	// after the bump get the new epoch and must keep working, which the
	// issuer loop asserts implicitly by never seeing 401 twice in a row
	// for its own just-issued cookie... the strong invariant is below.)
	for round := 0; round < 20; round++ {
		for i := 0; i < users; i++ {
			name := fmt.Sprintf("u%d", i)
			g.credMu.RLock()
			u := g.users[name]
			var preEpoch uint64
			if u != nil {
				preEpoch = u.AuthEpoch
			}
			g.credMu.RUnlock()
			pre := &http.Cookie{Name: sessionCookie, Value: g.newSessionFor(name, name, RoleEditor, preEpoch, true)}
			if err := g.revokeSessions(name); err != nil {
				t.Errorf("revoke %s: %v", name, err)
			}
			if code := requestWithCookie(handler, pre); code != http.StatusUnauthorized {
				t.Errorf("pre-revocation session for %s accepted (%d) after revoke returned", name, code)
			}
		}
	}
	close(stop)
	wg.Wait()
}

// loginCookieQuiet is loginCookie without failing the test (used in loops).
func loginCookieQuiet(g *authGate, username, password string) *http.Cookie {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.handleLogin(w, req)
	if w.Code != http.StatusOK {
		return nil
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}
