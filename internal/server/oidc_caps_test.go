package server

// oidc_caps_test.go — D06 tail: the capability matrix driven through OIDC
// (SSO) principals. local principals are pinned in caps_test.go and machine
// principals in internal/mcp/caps_test.go; this file closes the third leg:
//
//  1. a principal minted NOW starts on the role's PRESET (never the wider
//     legacy profile), whatever the IdP role claim says;
//  2. allow/deny through the REAL middleware chain: a preset SSO viewer is
//     refused secret values with the capability named while the metadata
//     variant answers; a preset SSO editor deploys but cannot reveal; an
//     admin principal holds everything;
//  3. a PRE-MATRIX principal row (no profile field on disk) keeps its
//     recorded legacy permissions across restarts — the migration promise;
//  4. a username-claim rename keeps the same principal, profile, and
//     effective capabilities — a rename never widens or narrows access.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/caps"
)

// ssoDo drives one request through the FULL middleware chain with a session
// minted for an OIDC principal row (newSessionFor with local=false — the
// same session shape the OIDC callback issues).
func ssoDo(t *testing.T, s *Server, subject, user, role, method, target, body string, epoch uint64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.gate.newSessionFor(subject, user, role, epoch, false)})
	req.Host = "dash.local"
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

// mintSSO records a first SSO sign-in through the gate's own upsert path.
func mintSSO(t *testing.T, s *Server, subject, user, role string) uint64 {
	t.Helper()
	epoch, _, err := s.gate.upsertOIDCPrincipal(subject, user, user+"@x.example", role)
	if err != nil {
		t.Fatal(err)
	}
	return epoch
}

// 1. A principal minted after X03 starts on the role's preset — minting on
// the legacy profile granted a fresh viewer reveal.secrets on first sign-in.
func TestNewOIDCPrincipalMintsOnPresetProfile(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	sub := oidcSubjectID("https://idp.example", "u-new")
	mintSSO(t, s, sub, "nora", RoleViewer)

	s.gate.credMu.RLock()
	p := s.gate.oidcPrincipals[sub]
	s.gate.credMu.RUnlock()
	if p == nil {
		t.Fatal("principal row missing after first sign-in")
	}
	if p.CapabilityProfile != profilePreset {
		t.Fatalf("new principal profile = %q, want %q", p.CapabilityProfile, profilePreset)
	}
	if got := capabilitiesForProfile(p.CapabilityProfile, p.Capabilities, p.Role); !got.Allow(caps.ViewMetadata) || got.Allow(caps.RevealSecrets) {
		t.Fatalf("new viewer principal caps = %v, want the viewer preset (metadata, no reveal)", got.Sorted())
	}
}

// 2. The preset matrix through the real chain for OIDC principals.
func TestOIDCPrincipalCapabilityAllowDeny(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s) // a local account clears first-run setup mode
	vSub := oidcSubjectID("https://idp.example", "u-v")
	eSub := oidcSubjectID("https://idp.example", "u-e")
	aSub := oidcSubjectID("https://idp.example", "u-a")
	vEpoch := mintSSO(t, s, vSub, "vera", RoleViewer)
	eEpoch := mintSSO(t, s, eSub, "emy", RoleEditor)
	aEpoch := mintSSO(t, s, aSub, "ada", RoleAdmin)

	// Preset SSO viewer: metadata yes, values no — the deny names the
	// capability, and the metadata variant still answers.
	rec := ssoDo(t, s, vSub, "vera", RoleViewer, http.MethodGet, "/api/apps/prod/web/env", "", vEpoch)
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.RevealSecrets) {
		t.Fatalf("sso viewer env GET = %d (%s), want 403 naming reveal.secrets", rec.Code, capError(t, rec))
	}
	rec = ssoDo(t, s, vSub, "vera", RoleViewer, http.MethodGet, "/api/apps/prod/web/env/keys", "", vEpoch)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "hunter2-beta") {
		t.Fatalf("sso viewer env/keys = %d, want 200 names-only", rec.Code)
	}
	rec = ssoDo(t, s, vSub, "vera", RoleViewer, http.MethodPost, "/api/operations", `{"kind":"deploy","server":"prod","app":"web","image":"nginx:latest"}`, vEpoch)
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.ExecuteDeploy) {
		t.Fatalf("sso viewer deploy = %d (%s), want 403 naming execute.deploy", rec.Code, capError(t, rec))
	}

	// Preset SSO editor: deploys and mutates, but secret values and user
	// administration are still denied.
	rec = ssoDo(t, s, eSub, "emy", RoleEditor, http.MethodPost, "/api/operations", `{"kind":"deploy","server":"prod","app":"web","image":"nginx:latest"}`, eEpoch)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("sso editor deploy = %d (%s), want 202", rec.Code, capError(t, rec))
	}
	rec = ssoDo(t, s, eSub, "emy", RoleEditor, http.MethodGet, "/api/apps/prod/web/env", "", eEpoch)
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.RevealSecrets) {
		t.Fatalf("sso editor env GET = %d (%s), want 403 naming reveal.secrets", rec.Code, capError(t, rec))
	}
	rec = ssoDo(t, s, eSub, "emy", RoleEditor, http.MethodGet, "/api/users", "", eEpoch)
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.AdministerUsers) {
		t.Fatalf("sso editor users GET = %d (%s), want 403 naming administer.users", rec.Code, capError(t, rec))
	}

	// SSO admin: everything, including values and administration.
	rec = ssoDo(t, s, aSub, "ada", RoleAdmin, http.MethodGet, "/api/apps/prod/web/env", "", aEpoch)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hunter2-beta") {
		t.Fatalf("sso admin env GET = %d, want 200 with values", rec.Code)
	}
	rec = ssoDo(t, s, aSub, "ada", RoleAdmin, http.MethodGet, "/api/users", "", aEpoch)
	if rec.Code != http.StatusOK {
		t.Fatalf("sso admin users GET = %d, want 200", rec.Code)
	}
}

// 3. A principal row written before the capability matrix (no profile field
// on disk) keeps the recorded legacy permissions across restarts — the same
// migration promise local accounts have.
func TestLegacyOIDCPrincipalKeepsPreMatrixCapabilitiesAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	sub := oidcSubjectID("https://idp.example", "u-old")
	file := usersFileFormat{
		Users:        []dashUser{{Username: "root", PasswordHash: dummyBcryptHash, Role: RoleAdmin, AuthEpoch: 1, CapabilityProfile: profileLegacy}},
		EpochCounter: 1,
		OIDCPrincipals: []dashPrincipal{{
			Subject: sub, Username: "old-timer", Role: RoleViewer, AuthEpoch: 1,
		}},
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	s := newCapsServer(t, dir)
	rec := ssoDo(t, s, sub, "old-timer", RoleViewer, http.MethodGet, "/api/apps/prod/web/env", "", 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-restart legacy sso viewer env GET = %d (%s), want 200 — never silently narrowed", rec.Code, capError(t, rec))
	}

	s2 := newCapsServer(t, dir)
	rec = ssoDo(t, s2, sub, "old-timer", RoleViewer, http.MethodGet, "/api/apps/prod/web/env", "", 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-restart legacy sso viewer env GET = %d (%s), want 200", rec.Code, capError(t, rec))
	}
}

// 4. A username-claim rename keeps the same principal row: profile and
// effective capabilities are untouched — a rename neither widens nor
// narrows access (D06 acceptance).
func TestOIDCUsernameRenameDoesNotChangeCapabilities(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s) // a local account clears first-run setup mode
	sub := oidcSubjectID("https://idp.example", "u-ren")
	epoch := mintSSO(t, s, sub, "ray", RoleViewer)

	ssoDo(t, s, sub, "ray", RoleViewer, http.MethodGet, "/api/apps/prod/web/env", "", epoch) // sanity: 403

	// The IdP renames the user; the next sign-in upserts the same subject.
	if _, _, err := s.gate.upsertOIDCPrincipal(sub, "raymond", "ray@x.example", RoleViewer); err != nil {
		t.Fatal(err)
	}
	s.gate.credMu.RLock()
	p := s.gate.oidcPrincipals[sub]
	s.gate.credMu.RUnlock()
	if p.Username != "raymond" {
		t.Fatalf("rename not recorded: %q", p.Username)
	}
	if p.CapabilityProfile != profilePreset {
		t.Fatalf("rename changed the profile: %q", p.CapabilityProfile)
	}
	if got := capabilitiesForProfile(p.CapabilityProfile, p.Capabilities, p.Role); len(got) != 1 || !got.Allow(caps.ViewMetadata) {
		t.Fatalf("rename changed capabilities: %v", got.Sorted())
	}
	// And the live session under the renamed row still enforces the same
	// matrix — denied exactly as before.
	rec := ssoDo(t, s, sub, "ray", RoleViewer, http.MethodGet, "/api/apps/prod/web/env", "", epoch)
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.RevealSecrets) {
		t.Fatalf("post-rename env GET = %d (%s), want 403 naming reveal.secrets", rec.Code, capError(t, rec))
	}
	rec = ssoDo(t, s, sub, "ray", RoleViewer, http.MethodGet, "/api/apps/prod/web/env/keys", "", epoch)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-rename metadata GET = %d, want 200", rec.Code)
	}
}
