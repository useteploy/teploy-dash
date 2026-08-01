package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// DASH-001: a fresh, remotely-reachable instance in setup mode used to let
// ANY visitor claim the first (admin) account — no credential was required
// beyond reaching /api/setup before the legitimate operator did. A bootstrap
// token generated at startup and required on the setup request closes that.

func doSetup(t *testing.T, g *authGate, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(b))
	w := httptest.NewRecorder()
	g.handleSetup(w, req)
	return w
}

func TestHandleSetup_RequiresBootstrapToken(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))
	if g.bootstrapToken == "" {
		t.Fatal("expected a bootstrap token to be generated for a fresh setup-required gate")
	}

	w := doSetup(t, g, map[string]string{
		"username": "admin", "password": "adminpassword", "confirm_password": "adminpassword",
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (missing token)", w.Code)
	}
	if len(g.users) != 0 {
		t.Error("account was created despite a missing bootstrap token")
	}
}

func TestHandleSetup_RejectsWrongToken(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))

	w := doSetup(t, g, map[string]string{
		"bootstrap_token": "not-the-real-token",
		"username":        "admin", "password": "adminpassword", "confirm_password": "adminpassword",
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (wrong token)", w.Code)
	}
	if len(g.users) != 0 {
		t.Error("account was created despite a wrong bootstrap token")
	}
}

func TestHandleSetup_CorrectTokenSucceeds(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))

	w := doSetup(t, g, map[string]string{
		"bootstrap_token": g.bootstrapToken,
		"username":        "admin", "password": "adminpassword", "confirm_password": "adminpassword",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	if _, ok := g.authenticate("admin", "adminpassword"); !ok {
		t.Error("account was not actually created")
	}
}

func TestHandleSetup_TokenIsSingleUse(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))
	token := g.bootstrapToken

	w := doSetup(t, g, map[string]string{
		"bootstrap_token": token,
		"username":        "admin", "password": "adminpassword", "confirm_password": "adminpassword",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("first setup: status = %d, body = %s", w.Code, w.Body.String())
	}

	// setupRequired is now false, so a second attempt with the SAME token
	// must be rejected — handleSetup's own "already configured" check fires
	// first, which is fine: the point is the token can never create a second
	// account.
	w2 := doSetup(t, g, map[string]string{
		"bootstrap_token": token,
		"username":        "attacker", "password": "attackerpassword", "confirm_password": "attackerpassword",
	})
	if w2.Code == http.StatusOK {
		t.Error("a second account was created by replaying the bootstrap token")
	}
	if _, ok := g.authenticate("attacker", "attackerpassword"); ok {
		t.Error("attacker account exists — bootstrap token was not single-use")
	}
}

func TestHandleSetup_ExpiredTokenRejected(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))
	g.credMu.Lock()
	g.bootstrapTokenExpiry = g.bootstrapTokenExpiry.Add(-2 * bootstrapTokenTTL) // force expiry
	g.credMu.Unlock()

	w := doSetup(t, g, map[string]string{
		"bootstrap_token": g.bootstrapToken,
		"username":        "admin", "password": "adminpassword", "confirm_password": "adminpassword",
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (expired token)", w.Code)
	}
}

// Env-var bootstrap mode (TEPLOY_DASH_PASSWORD) never enters setup mode, so
// no bootstrap token is generated — confirm that's still true.
func TestEnvBootstrap_NoSetupTokenGenerated(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("admin", "bootpass", filepath.Join(dir, "auth.json"))
	if g.bootstrapToken != "" {
		t.Error("bootstrap token generated even though env-var credentials were supplied")
	}
}
