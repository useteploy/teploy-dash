package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	// Seed an account so the gate leaves setup mode; the sessions below are
	// independent of it and exercise each role directly.
	if err := g.createUser("admin", "adminpass1", RoleAdmin); err != nil {
		t.Fatal(err)
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
