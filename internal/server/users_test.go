package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// Hash at the cheapest cost in tests: keeps the auth suite fast and avoids
// loading the CPU enough to perturb timing-sensitive neighbors.
func init() { bcryptCost = bcrypt.MinCost }

// newTestGate returns a multi-user gate backed by a temp users.json, with no
// env-var bootstrap credential (pure stored-user mode).
func newTestGate(t *testing.T) *authGate {
	t.Helper()
	dir := t.TempDir()
	return newAuthGate("", "", filepath.Join(dir, "auth.json"))
}

// sessionReq builds a request carrying a live session for the given user/role.
func sessionReq(g *authGate, method, target, user, role string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: g.newSession(user, role)})
	return req
}

func TestRequiredRole(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{"GET", "/api/apps", RoleViewer},
		{"GET", "/api/servers", RoleViewer},
		{"POST", "/api/deploy", RoleEditor},
		{"POST", "/api/apps/prod/web/rollback", RoleEditor},
		{"DELETE", "/api/apps/prod/web/env/FOO", RoleEditor},
		// kv: reads are viewer, writes are editor. No kv-specific code produces
		// this — requiredRole already fails closed on any unclassified mutating
		// route. These rows pin the contract so an adminOnlyPrefixes edit or a
		// new special case can't move it silently.
		{"GET", "/api/apps/prod/web/kv", RoleViewer},
		{"GET", "/api/apps/prod/web/kv/value", RoleViewer},
		{"POST", "/api/apps/prod/web/kv", RoleEditor},
		{"DELETE", "/api/apps/prod/web/kv", RoleEditor},
		{"GET", "/api/users", RoleAdmin},
		{"POST", "/api/users", RoleAdmin},
		{"DELETE", "/api/users/jane", RoleAdmin},
		{"GET", "/api/mcp-tokens", RoleAdmin},
		{"GET", "/api/config/servers", RoleAdmin},
		{"POST", "/api/registries", RoleAdmin},
		{"GET", "/api/notifications", RoleAdmin},
		{"POST", "/api/auth/password", RoleViewer}, // self-service
	}
	for _, c := range cases {
		if got := requiredRole(c.method, c.path); got != c.want {
			t.Errorf("requiredRole(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// The gate must enforce the role hierarchy: a viewer can read but not mutate,
// an editor can mutate app routes but not touch admin routes, an admin can do
// both. A missing/unknown role gets the least privilege.
func TestRoleGateEnforcement(t *testing.T) {
	g := newTestGate(t)
	// Seed one account per role so every session validates against a live
	// principal row (the unconditional epoch check retires sessions with no
	// row — A02). The "garbage" role normalizes to viewer at creation, and
	// validation re-reads the role live, so an unknown issued role can never
	// exceed the account's real privilege.
	for _, u := range []struct {
		name, pass, role string
	}{
		{"admin", "adminpass1", RoleAdmin},
		{"v", "viewerpass1", RoleViewer},
		{"e", "editorpass1", RoleEditor},
		{"a", "admin2pass1", RoleAdmin},
		{"x", "garbagepass", "garbage"},
	} {
		if err := g.createUser(u.name, u.pass, u.role); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }
	mux.HandleFunc("/api/apps", ok)   // read
	mux.HandleFunc("/api/deploy", ok) // editor mutation
	mux.HandleFunc("/api/users", ok)  // admin
	h := g.wrap(mux)

	cases := []struct {
		name, user, role, method, path string
		want                           int
	}{
		{"viewer reads", "v", RoleViewer, "GET", "/api/apps", 200},
		{"viewer cannot deploy", "v", RoleViewer, "POST", "/api/deploy", http.StatusForbidden},
		{"viewer cannot manage users", "v", RoleViewer, "GET", "/api/users", http.StatusForbidden},
		{"editor deploys", "e", RoleEditor, "POST", "/api/deploy", 200},
		{"editor cannot manage users", "e", RoleEditor, "GET", "/api/users", http.StatusForbidden},
		{"admin deploys", "a", RoleAdmin, "POST", "/api/deploy", 200},
		{"admin manages users", "a", RoleAdmin, "GET", "/api/users", 200},
		{"unknown role denied mutation", "x", "garbage", "POST", "/api/deploy", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := sessionReq(g, c.method, "http://dash.local"+c.path, c.user, c.role)
			req.Host = "dash.local"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != c.want {
				t.Errorf("%s %s as %s: got %d, want %d", c.method, c.path, c.role, w.Code, c.want)
			}
		})
	}
}

func TestCreateUserAndAuthenticate(t *testing.T) {
	g := newTestGate(t)
	if err := g.createUser("jane", "hunter2pw", RoleEditor); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	// Correct credentials authenticate with the stored role.
	u, ok := g.authenticate("jane", "hunter2pw")
	if !ok || u.Role != RoleEditor {
		t.Fatalf("authenticate jane = %+v, %v; want editor, true", u, ok)
	}
	// Wrong password fails.
	if _, ok := g.authenticate("jane", "wrong"); ok {
		t.Error("wrong password authenticated")
	}
	// Unknown user fails (and must not panic on the dummy-hash timing path).
	if _, ok := g.authenticate("ghost", "whatever"); ok {
		t.Error("unknown user authenticated")
	}
	// Duplicate username rejected.
	if err := g.createUser("jane", "another8x", RoleViewer); err == nil {
		t.Error("expected duplicate-username error")
	}
	// Short password rejected.
	if err := g.createUser("bob", "short", RoleViewer); err == nil {
		t.Error("expected short-password error")
	}
}

func TestLastAdminGuards(t *testing.T) {
	g := newTestGate(t)
	if err := g.createUser("root", "adminpass1", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	// Cannot demote the only admin.
	if err := g.setRole("root", RoleEditor); err == nil {
		t.Error("expected error demoting the last admin")
	}
	// Cannot delete the only admin.
	if err := g.deleteUser("root"); err == nil {
		t.Error("expected error deleting the last admin")
	}
	// With a second admin, demotion of the first is allowed.
	if err := g.createUser("root2", "adminpass2", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := g.setRole("root", RoleEditor); err != nil {
		t.Errorf("demoting one of two admins should succeed: %v", err)
	}
	// Now root2 is the last admin again — delete must fail.
	if err := g.deleteUser("root2"); err == nil {
		t.Error("expected error deleting the last admin after demotion")
	}
}

func TestSetPasswordPersists(t *testing.T) {
	g := newTestGate(t)
	if err := g.createUser("kate", "initial8x", RoleViewer); err != nil {
		t.Fatal(err)
	}
	if err := g.setPassword("kate", "rotated8x"); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.authenticate("kate", "rotated8x"); !ok {
		t.Error("new password should authenticate")
	}
	if _, ok := g.authenticate("kate", "initial8x"); ok {
		t.Error("old password should no longer authenticate")
	}
}

// A pre-RBAC single-user auth.json must migrate into the multi-user store as an
// admin, and the migration must be persisted to users.json.
func TestLegacyAuthJSONMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "auth.json")
	hash, _ := bcrypt.GenerateFromPassword([]byte("legacypass"), bcrypt.DefaultCost)
	blob, _ := json.Marshal(map[string]string{"username": "operator", "password_hash": string(hash)})
	if err := os.WriteFile(legacy, blob, 0600); err != nil {
		t.Fatal(err)
	}

	g := newAuthGate("", "", legacy)
	if g.setupRequired {
		t.Fatal("migrated gate must not require setup")
	}
	u, ok := g.authenticate("operator", "legacypass")
	if !ok || u.Role != RoleAdmin {
		t.Fatalf("migrated user = %+v, %v; want admin, true", u, ok)
	}
	// users.json should now exist with the migrated account.
	if _, err := os.Stat(filepath.Join(dir, "users.json")); err != nil {
		t.Errorf("expected users.json written on migration: %v", err)
	}
}

// The env-var bootstrap credential authenticates as an implicit admin when no
// users file exists (the Docker TEPLOY_DASH_PASSWORD path).
func TestEnvBootstrapAdmin(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("admin", "bootpass", filepath.Join(dir, "auth.json"))
	if g.setupRequired {
		t.Fatal("env bootstrap should not require setup")
	}
	u, ok := g.authenticate("", "bootpass")
	if !ok || u.Role != RoleAdmin {
		t.Fatalf("env bootstrap auth = %+v, %v; want admin, true", u, ok)
	}
	if _, ok := g.authenticate("admin", "wrong"); ok {
		t.Error("wrong env password authenticated")
	}
}

// A02: the env bootstrap credential is materialized into a STORED account at
// startup. Once the operator changes that password (or deletes the account
// after adding another admin), the original env password is gone for good —
// there is no implicit fallback authentication path left to re-enable.
func TestEnvBootstrapShadowedByStoredAccount(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("admin", "bootpass", filepath.Join(dir, "auth.json"))
	if g.setupRequired {
		t.Fatal("env bootstrap should not require setup")
	}
	// Materialized at startup: the account exists, as an admin.
	if _, ok := g.authenticate("admin", "bootpass"); !ok {
		t.Fatal("materialized env account must authenticate with the env password")
	}
	if err := g.setPassword("admin", "storedpass1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.authenticate("admin", "bootpass"); ok {
		t.Error("old env password still authenticates after a password change")
	}
	// Deleting the account (possible once a second admin exists) must NOT
	// resurrect the env password — the pre-A02 failure mode.
	if err := g.createUser("admin2", "secondpass1", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := g.deleteUser("admin"); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.authenticate("admin", "bootpass"); ok {
		t.Error("retired env password re-enabled after deleting the materialized account")
	}
	// The store is canonical: a restart keeps the deleted account deleted
	// and keeps the second admin.
	g2 := newAuthGate("admin", "bootpass", filepath.Join(dir, "auth.json"))
	if _, ok := g2.authenticate("admin2", "secondpass1"); !ok {
		t.Error("second admin lost after reload")
	}
	if _, ok := g2.authenticate("admin", "bootpass"); ok {
		t.Error("retired env password re-enabled after restart")
	}
}

// A02: an environment username that collides with an EXISTING store never
// authenticates with the env password — the store wins once it exists.
func TestEnvIgnoredWhenStoreExists(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "auth.json")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	g := newAuthGate("", "", credFile)
	if err := g.createUser("admin", "storedpass1", RoleEditor); err != nil {
		t.Fatal(err)
	}
	// Restart with an env password set for a name that already exists.
	g2 := newAuthGate("admin", "envpass123", credFile)
	if _, ok := g2.authenticate("admin", "envpass123"); ok {
		t.Error("env password authenticated against an existing stored account")
	}
	if u, ok := g2.authenticate("admin", "storedpass1"); !ok || u.Role != RoleEditor {
		t.Fatalf("stored account = %+v, %v; want editor, true", u, ok)
	}
}

// A03: a session issued before a password reset (or role change / deletion /
// same-name recreation) stops authorizing on its next request — the epoch
// embedded at issuance no longer matches the account.
func TestSessionEpochInvalidation(t *testing.T) {
	g := newTestGate(t)
	if err := g.createUser("alice", "alicepass1", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := g.createUser("bob", "bobpass123", RoleEditor); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := g.wrap(mux)

	// Role read is live: a session issued as editor upgrades immediately
	// after an admin promotes the account, without re-login.
	promote := func() {
		if err := g.setRole("bob", RoleAdmin); err != nil {
			t.Fatal(err)
		}
	}

	do := func(token string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/users", nil) // admin-only route
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		h.ServeHTTP(w, req)
		return w.Code
	}

	// Issue bob's session the way login does: epoch captured from authenticate.
	bob, ok := g.authenticate("bob", "bobpass123")
	if !ok {
		t.Fatal("bob must authenticate")
	}
	token := g.newSessionFor("bob", "bob", "bobpass123", bob.AuthEpoch, true)
	if code := do(token); code != http.StatusForbidden {
		t.Fatalf("editor session on admin route = %d, want 403", code)
	}
	// A role change bumps the epoch too: the demotion/promotion takes effect
	// on the account's very next request — the old session is revoked, not
	// merely re-rated.
	promote()
	if code := do(token); code != http.StatusUnauthorized {
		t.Fatalf("session after role change = %d, want 401 (revoked)", code)
	}

	// Password reset invalidates sessions issued against the old credential.
	if err := g.setPassword("bob", "newpass123"); err != nil {
		t.Fatal(err)
	}
	if code := do(token); code != http.StatusUnauthorized {
		t.Fatalf("session after password reset = %d, want 401", code)
	}

	// Deletion, then recreation under the SAME name: the new account's epoch
	// is strictly higher, so no earlier session can ride the identity.
	token2 := g.newSessionFor("bob", "bob", "newpass123", 0, true)
	if err := g.deleteUser("bob"); err != nil {
		t.Fatal(err)
	}
	if code := do(token2); code != http.StatusUnauthorized {
		t.Fatalf("session after deletion = %d, want 401", code)
	}
	if err := g.createUser("bob", "bobpass999", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if code := do(token2); code != http.StatusUnauthorized {
		t.Fatalf("stale-epoch session on recreated account = %d, want 401", code)
	}
}

// A03: an admin password reset against a nonexistent username must fail and
// create nothing (setPassword used to synthesize an admin account).
func TestSetPasswordUnknownUserCreatesNothing(t *testing.T) {
	g := newTestGate(t)
	if err := g.setPassword("ghost", "whatever123"); err == nil {
		t.Fatal("expected error resetting an unknown user")
	}
	if _, ok := g.authenticate("ghost", "whatever123"); ok {
		t.Error("unknown user authenticates after a failed reset")
	}
	if len(g.listUsers()) != 0 {
		t.Error("an account was created by a failed password reset")
	}
}

// A04: an unreadable/corrupt users.json must NOT fall back to legacy
// auth.json credentials, and the gate must refuse requests instead of
// opening setup mode.
func TestCorruptUsersFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	hash, _ := bcrypt.GenerateFromPassword([]byte("legacypass"), bcrypt.DefaultCost)
	blob, _ := json.Marshal(map[string]string{"username": "operator", "password_hash": string(hash)})
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), blob, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))
	if g.initErr == nil {
		t.Fatal("expected initErr for a corrupt users.json")
	}
	if g.setupRequired {
		t.Error("corrupt users.json must not open setup mode")
	}
	if _, ok := g.authenticate("operator", "legacypass"); ok {
		t.Error("legacy credential reactivated by a corrupt users.json")
	}

	mux := http.NewServeMux()
	reached := false
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) { reached = true })
	h := g.wrap(mux)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/apps", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("gate should 503 during a credential outage, got %d", w.Code)
	}
	if reached {
		t.Error("request reached the handler during a credential outage")
	}
}

// A03: two concurrent setup requests holding the valid bootstrap token can
// create only one initial account.
func TestSetupRaceCreatesSingleAccount(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))
	token := g.bootstrapToken

	do := func(username string) int {
		b, _ := json.Marshal(map[string]string{
			"bootstrap_token": token,
			"username":        username,
			"password":        "adminpassword", "confirm_password": "adminpassword",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(b))
		w := httptest.NewRecorder()
		g.handleSetup(w, req)
		return w.Code
	}

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, name := range []string{"alice", "bob"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			codes <- do(name)
		}(name)
	}
	wg.Wait()
	close(codes)
	okCount := 0
	for c := range codes {
		if c == http.StatusOK {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("expected exactly one setup to succeed, got %d", okCount)
	}
	if n := len(g.listUsers()); n != 1 {
		t.Errorf("expected 1 account after racing setups, got %d", n)
	}
}

// Changing password should only invalidate the changing user's sessions, not
// everyone else's.
func TestDeleteUserSessionsScoped(t *testing.T) {
	g := newTestGate(t)
	tokAlice := g.newSession("alice", RoleEditor)
	tokBob := g.newSession("bob", RoleViewer)

	g.deleteUserSessions("alice")

	if _, ok := g.lookupSession(tokAlice); ok {
		t.Error("alice's session should be gone")
	}
	if _, ok := g.lookupSession(tokBob); !ok {
		t.Error("bob's session should survive alice's password change")
	}
}

// ── DASH-005 / DASH-006: copy-on-write persistence ───────────────────────
//
// createUser/setPassword/setRole/deleteUser used to mutate the live g.users
// map (and, for createUser, flip g.setupRequired) BEFORE persisting, so a
// failed write left the running process and users.json silently diverged —
// a failed role change still took effect until restart, a failed first
// account left setup mode disabled with nothing durable behind it. These
// tests force the persist step to fail (an unwritable users.json directory)
// and assert live state is byte-for-byte what it was before the call.

// unwritableDir makes dir's contents unwritable so saveUsersFile's
// MkdirAll/WriteFile/Rename fails, and registers a cleanup to restore
// permissions (t.TempDir() cleanup requires the dir be writable again).
func unwritableDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
}

func TestCreateUser_PersistFailureLeavesLiveStateUnchanged(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))

	if err := g.createUser("existing", "existingpassword", RoleViewer); err != nil {
		t.Fatalf("seed createUser: %v", err)
	}
	before := g.listUsers()

	unwritableDir(t, dir)

	if err := g.createUser("newuser", "newpassword123", RoleViewer); err == nil {
		t.Fatal("expected error when persistence fails")
	}

	after := g.listUsers()
	if len(after) != len(before) {
		t.Fatalf("live user count changed after failed persist: before=%d after=%d", len(before), len(after))
	}
	for _, u := range after {
		if u.Username == "newuser" {
			t.Error("newuser is live in memory despite failed persistence")
		}
	}
}

func TestCreateUser_FirstUserPersistFailureKeepsSetupRequired(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))
	if !g.setupRequired {
		t.Fatal("expected setupRequired=true for a fresh gate with no users")
	}

	unwritableDir(t, dir)

	if err := g.createUser("admin", "adminpassword", RoleAdmin); err == nil {
		t.Fatal("expected error when persistence fails")
	}
	if !g.setupRequired {
		t.Error("setupRequired flipped to false despite the first account never being durably saved")
	}
	if len(g.users) != 0 {
		t.Errorf("expected zero live users after failed first-account persist, got %d", len(g.users))
	}
}

func TestSetPassword_PersistFailureLeavesHashUnchanged(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))
	if err := g.createUser("alice", "originalpassword", RoleEditor); err != nil {
		t.Fatalf("seed createUser: %v", err)
	}

	unwritableDir(t, dir)

	if err := g.setPassword("alice", "newpassword123"); err == nil {
		t.Fatal("expected error when persistence fails")
	}
	if _, ok := g.authenticate("alice", "originalpassword"); !ok {
		t.Error("original password stopped working after a failed setPassword")
	}
	if _, ok := g.authenticate("alice", "newpassword123"); ok {
		t.Error("new password works despite the change never being persisted")
	}
}

func TestSetRole_PersistFailureLeavesRoleUnchanged(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))
	if err := g.createUser("alice", "alicepassword", RoleViewer); err != nil {
		t.Fatalf("seed createUser: %v", err)
	}

	unwritableDir(t, dir)

	if err := g.setRole("alice", RoleAdmin); err == nil {
		t.Fatal("expected error when persistence fails")
	}
	g.credMu.RLock()
	role := g.users["alice"].Role
	g.credMu.RUnlock()
	if role != RoleViewer {
		t.Errorf("role = %q after failed setRole, want unchanged %q", role, RoleViewer)
	}
}

func TestDeleteUser_PersistFailureLeavesUserPresent(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))
	if err := g.createUser("admin", "adminpassword", RoleAdmin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := g.createUser("alice", "alicepassword", RoleViewer); err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	unwritableDir(t, dir)

	if err := g.deleteUser("alice"); err == nil {
		t.Fatal("expected error when persistence fails")
	}
	g.credMu.RLock()
	_, stillPresent := g.users["alice"]
	g.credMu.RUnlock()
	if !stillPresent {
		t.Error("alice removed from live state despite failed persistence")
	}
}

// A01: a stored user (setup-created admin, ordinary editor) can change their
// OWN password through the real handler; previously every change routed
// through the env-migration helper and failed with "user not found" whenever
// no env credential was configured. The change must invalidate other
// sessions, accept the new password, and reject the old one.
func TestChangePasswordStoredUserHTTTP(t *testing.T) {
	g := newTestGate(t)
	if err := g.createUser("alice", "alicepass1", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/password", g.handleChangePassword)
	h := g.wrap(mux)

	post := func(token, current, next string) int {
		body := fmt.Sprintf(`{"current_password":%q,"new_password":%q,"confirm_password":%q}`, current, next, next)
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/auth/password", strings.NewReader(body))
		if token != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		}
		h.ServeHTTP(w, req)
		return w.Code
	}

	alice, ok := g.authenticate("alice", "alicepass1")
	if !ok {
		t.Fatal("alice must authenticate")
	}
	token := g.newSessionFor("alice", "alice", "alicepass1", alice.AuthEpoch, true)
	if code := post(token, "alicepass1", "newpass123"); code != http.StatusOK {
		t.Fatalf("self password change = %d, want 200", code)
	}
	if _, ok := g.authenticate("alice", "newpass123"); !ok {
		t.Error("new password rejected after change")
	}
	if _, ok := g.authenticate("alice", "alicepass1"); ok {
		t.Error("old password accepted after change")
	}
	// The session used for the change was invalidated by it.
	if code := post(token, "newpass123", "another123"); code != http.StatusUnauthorized {
		t.Fatalf("pre-change session still valid = %d, want 401", code)
	}
}

// A03: a self-service password change racing an administrative reset fails
// closed — the CAS on the captured epoch returns a conflict instead of
// silently overwriting the newer credential. The interleave is the window
// inside the handler (verification done, commit not yet run), so it is
// simulated at the unit boundary the handler drives.
func TestChangePasswordCASConflict(t *testing.T) {
	g := newTestGate(t)
	if err := g.createUser("alice", "alicepass1", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	// Handler step 1: authenticate captures the account and its epoch.
	alice, ok := g.authenticate("alice", "alicepass1")
	if !ok {
		t.Fatal("alice must authenticate")
	}
	// ...an administrative reset lands inside the window...
	if err := g.setPassword("alice", "adminreset1"); err != nil {
		t.Fatal(err)
	}
	// Handler step 2: the self-change commits against the stale epoch.
	err := g.setPasswordCAS("alice", alice.AuthEpoch, "selfnewpass1", true)
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale self-change err = %v, want ErrStaleEpoch", err)
	}
	// The admin reset survives untouched.
	if _, ok := g.authenticate("alice", "adminreset1"); !ok {
		t.Error("admin reset was overwritten by the stale self-change")
	}
	// A matching epoch still succeeds.
	if err := g.setPasswordCAS("alice", 0, "freshpass1", false); err != nil {
		t.Fatalf("unconditional reset failed: %v", err)
	}
}
