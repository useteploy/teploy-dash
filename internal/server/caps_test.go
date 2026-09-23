package server

// caps_test.go — X03 first slice: roles backed by resource-scoped
// capabilities. The tests here pin, in order of load:
//
//  1. the ROUTE TABLE — which capability each API route class requires
//     (TestRequiredCapabilitiesRouteMatrix);
//  2. the PRESET matrix per role class through the real middleware chain —
//     viewer lists metadata but env/KV VALUES and logs are refused with the
//     missing capability named; operator deploys and mutates but cannot
//     reveal; admin holds everything;
//  3. the LEGACY PROFILE — accounts from pre-X03 installs keep today's
//     effective permissions exactly (including viewer value reads), across
//     restarts, until an admin clicks the narrowing;
//  4. the NARROWING CLICK — one explicit request moves an account onto its
//     role's preset and takes effect on the very next request of the SAME
//     live session (capabilities are read live, like roles);
//  5. CUSTOM capability storage — additive per-account sets that persist;
//  6. the MCP token surface (mint defaults, per-tool enforcement).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/operation"
)

// ── Test doubles ──────────────────────────────────────────────────────────

// capsEnvJSON is the env payload the stubbed CLI returns for `env list`.
const capsEnvJSON = `{"API_KEY":"hunter2-beta","COUNT":"7"}`

// newCapsRunner stubs the CLI delegate: `server list` answers one registered
// server; `env list` answers a two-variable env (values included, exactly
// like the real CLI's --reveal); `kv get` answers one value; everything else
// succeeds with empty output.
func newCapsRunner() func(context.Context, ...string) (*cli.Result, error) {
	return func(_ context.Context, args ...string) (*cli.Result, error) {
		switch {
		case len(args) >= 2 && args[0] == "server" && args[1] == "list":
			return &cli.Result{Stdout: `{"prod":{"host":"192.0.2.10","user":"deploy"}}`}, nil
		case len(args) >= 2 && args[0] == "env" && args[1] == "list":
			return &cli.Result{Stdout: capsEnvJSON}, nil
		case len(args) >= 2 && args[0] == "kv" && args[1] == "get":
			return &cli.Result{Stdout: "on\n"}, nil
		default:
			return &cli.Result{}, nil
		}
	}
}

// newCapsServer builds a full Server WITH the auth gate, a stubbed CLI, and
// a no-op operation executor (deploys enqueue but never shell out). The
// registered cleanup drains the operation manager BEFORE t.TempDir removes
// the data dir (cleanup is LIFO and TempDir registered first) — otherwise a
// terminal-state persist racing RemoveAll fails the test with "directory
// not empty" (same class as the pass-7 TestMCPMutationsRouteThroughOperations
// flake).
func newCapsServer(t *testing.T, dir string) *Server {
	t.Helper()
	s := New(Config{
		DataDir:      dir,
		CLIInstalled: func() bool { return true },
		CLIRunner:    newCapsRunner(),
		OperationExecutor: func(context.Context, operation.Command, func(operation.Stream, string)) (int, error) {
			return 0, nil
		},
	})
	t.Cleanup(func() { s.DrainOperations(context.Background()) })
	return s
}

// writeLegacyUsers writes a pre-X03 users.json: roles but no capability
// profile fields. Loading it must map every account to the LEGACY profile.
func writeLegacyUsers(t *testing.T, dir string, users map[string]string) {
	t.Helper()
	var f usersFileFormat
	var epoch uint64
	for name, role := range users {
		epoch++
		f.Users = append(f.Users, dashUser{Username: name, PasswordHash: dummyBcryptHash, Role: role, AuthEpoch: epoch})
	}
	f.EpochCounter = epoch
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// seedPresetUsers creates post-X03 accounts through the gate's own create
// path (new accounts default to the role presets).
func seedPresetUsers(t *testing.T, s *Server) {
	t.Helper()
	for _, u := range []struct{ name, role string }{
		{"admin", RoleAdmin},
		{"op", RoleEditor},
		{"v", RoleViewer},
	} {
		if err := s.gate.createUser(u.name, u.name+"pass123", u.role); err != nil {
			t.Fatal(err)
		}
	}
}

// capDo drives one authenticated request through the FULL middleware chain
// (baselineHeaders → body limits → origin guard → session gate with
// capability enforcement → routes).
func capDo(t *testing.T, s *Server, user, role, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.gate.newSession(user, role)})
	req.Host = "dash.local"
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func capError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding error body %q: %v", rec.Body.String(), err)
	}
	return env.Error
}

// ── 1. The route table ────────────────────────────────────────────────────

func TestRequiredCapabilitiesRouteMatrix(t *testing.T) {
	cases := []struct {
		method, path string
		want         []string
	}{
		// Reads default to metadata visibility.
		{"GET", "/api/apps", []string{caps.ViewMetadata}},
		{"GET", "/api/fleet", []string{caps.ViewMetadata}},
		{"GET", "/api/servers", []string{caps.ViewMetadata}},
		{"GET", "/api/servers/prod", []string{caps.ViewMetadata}},
		{"GET", "/api/apps/prod/web/status", []string{caps.ViewMetadata}},
		{"GET", "/api/apps/prod/web/drift", []string{caps.ViewMetadata}},
		{"GET", "/api/apps/prod/web/stats", []string{caps.ViewMetadata}},
		{"GET", "/api/apps/prod/web/health", []string{caps.ViewMetadata}},
		{"GET", "/api/apps/prod/web/accessories", []string{caps.ViewMetadata}},
		{"GET", "/api/onboarding/preflight", []string{caps.ViewMetadata}},
		{"POST", "/api/onboarding/preflight", []string{caps.ViewMetadata}}, // probe, not a mutation
		{"GET", "/api/onboarding/entry", []string{caps.ViewMetadata}},
		{"GET", "/api/operations", []string{caps.ViewMetadata}},
		{"GET", "/api/operations/op1", []string{caps.ViewMetadata}},
		{"GET", "/api/operations/op1/events", []string{caps.ViewMetadata}},
		{"GET", "/api/monitors", []string{caps.ViewMetadata}},
		{"GET", "/api/restore-tests", []string{caps.ViewMetadata}},
		{"GET", "/api/groups", []string{caps.ViewMetadata}},
		{"GET", "/api/templates", []string{caps.ViewMetadata}},
		{"GET", "/api/manifests", []string{caps.ViewMetadata}},
		{"GET", "/api/nav", []string{caps.ViewMetadata}},

		// Secret VALUE reads are the load-bearing split: metadata variants
		// stay viewer-visible, values need reveal.secrets.
		{"GET", "/api/apps/prod/web/env", []string{caps.RevealSecrets}},
		{"GET", "/api/apps/prod/web/env/keys", []string{caps.ViewMetadata}},
		{"GET", "/api/apps/prod/web/kv", []string{caps.ViewMetadata}},
		{"GET", "/api/apps/prod/web/kv/value", []string{caps.RevealSecrets}},

		// Logs and replay are deliberate access (X03).
		{"GET", "/api/apps/prod/web/log", []string{caps.ViewLogs}},
		{"GET", "/api/logs/prod/web", []string{caps.ViewLogs}},

		// Deploy-class mutations.
		{"POST", "/api/deploy", []string{caps.ExecuteDeploy}},
		{"POST", "/api/templates/install", []string{caps.ExecuteDeploy}},
		{"POST", "/api/operations", []string{caps.ExecuteDeploy}},
		{"POST", "/api/operations/op1/cancel", []string{caps.ExecuteDeploy}},
		{"POST", "/api/operations/op1/retry", []string{caps.ExecuteDeploy}},
		{"POST", "/api/apps/prod/web/stop", []string{caps.ExecuteDeploy}},
		{"POST", "/api/apps/prod/web/rollback", []string{caps.ExecuteDeploy}},
		{"POST", "/api/apps/prod/web/remove", []string{caps.ExecuteDeploy}},
		{"POST", "/api/apps/prod/web/maintenance/on", []string{caps.ExecuteDeploy}},
		{"POST", "/api/apps/prod/web/accessories/redis/stop", []string{caps.ExecuteDeploy}},
		{"POST", "/api/apps/prod/web/accessories/redis/logs", []string{caps.ViewLogs}},

		// env/kv writes.
		{"POST", "/api/apps/prod/web/env", []string{caps.ExecuteMutate}},
		{"DELETE", "/api/apps/prod/web/env/FOO", []string{caps.ExecuteMutate}},
		{"POST", "/api/apps/prod/web/kv", []string{caps.ExecuteMutate}},
		{"DELETE", "/api/apps/prod/web/kv", []string{caps.ExecuteMutate}},

		// Restore/cutover.
		{"POST", "/api/restore-tests", []string{caps.RestoreData}},
		{"PUT", "/api/restore-tests/rt1", []string{caps.RestoreData}},
		{"DELETE", "/api/restore-tests/rt1", []string{caps.RestoreData}},
		{"POST", "/api/restore-tests/rt1/run", []string{caps.RestoreData}},

		// User and credential administration.
		{"GET", "/api/users", []string{caps.AdministerUsers}},
		{"POST", "/api/users", []string{caps.AdministerUsers}},
		{"DELETE", "/api/users/jane", []string{caps.AdministerUsers}},
		{"PUT", "/api/users/jane", []string{caps.AdministerUsers}},
		{"POST", "/api/users/jane/revoke-sessions", []string{caps.AdministerUsers}},
		{"GET", "/api/sso", []string{caps.AdministerUsers}},
		{"POST", "/api/sso/revoke", []string{caps.AdministerUsers}},
		{"GET", "/api/mcp-tokens", []string{caps.AdministerCredentials}},
		{"POST", "/api/mcp-tokens", []string{caps.AdministerCredentials}},
		{"DELETE", "/api/mcp-tokens/abc", []string{caps.AdministerCredentials}},
		{"GET", "/api/config/servers", []string{caps.AdministerCredentials}},
		{"POST", "/api/config/servers", []string{caps.AdministerCredentials}},
		{"PUT", "/api/config/servers/prod", []string{caps.AdministerCredentials}},
		{"DELETE", "/api/config/servers/prod", []string{caps.AdministerCredentials}},
		{"GET", "/api/registries", []string{caps.AdministerCredentials}},
		{"POST", "/api/registries", []string{caps.AdministerCredentials}},
		{"GET", "/api/notifications", []string{caps.AdministerCredentials}},
		{"PATCH", "/api/notifications", []string{caps.AdministerCredentials}},
		// D04 sources carry webhook secrets and credential references —
		// the same credential-administration class as registries.
		{"GET", "/api/sources", []string{caps.AdministerCredentials}},
		{"POST", "/api/sources", []string{caps.AdministerCredentials}},
		{"GET", "/api/sources/src-0123456789abcdef", []string{caps.AdministerCredentials}},
		{"PATCH", "/api/sources/src-0123456789abcdef", []string{caps.AdministerCredentials}},
		{"DELETE", "/api/sources/src-0123456789abcdef", []string{caps.AdministerCredentials}},
		{"POST", "/api/sources/src-0123456789abcdef/verify", []string{caps.AdministerCredentials}},
		{"POST", "/api/sources/src-0123456789abcdef/rotate-secret", []string{caps.AdministerCredentials}},

		// Dashboard-config mutations (monitors, groups, homepage,
		// manifests) are execute.mutate.
		{"POST", "/api/monitors", []string{caps.ExecuteMutate}},
		{"PUT", "/api/monitors/m1", []string{caps.ExecuteMutate}},
		{"DELETE", "/api/monitors/m1", []string{caps.ExecuteMutate}},
		{"POST", "/api/monitors/m1/run", []string{caps.ExecuteMutate}},
		{"POST", "/api/groups", []string{caps.ExecuteMutate}},
		{"DELETE", "/api/groups/g1/projects/p1/apps/a1", []string{caps.ExecuteMutate}},
		{"PUT", "/api/homepage", []string{caps.ExecuteMutate}},
		{"PUT", "/api/manifests/prod/web", []string{caps.ExecuteMutate}},
		{"DELETE", "/api/manifests/prod/web", []string{caps.ExecuteMutate}},

		// Self-service identity is available to every authenticated
		// principal — no capability gates knowing who you are or changing
		// your own password.
		{"GET", "/api/auth/me", nil},
		{"POST", "/api/auth/password", nil},

		// Fail closed: unclassified mutations require execute.mutate,
		// unclassified reads stay metadata-visible.
		{"POST", "/api/nonsense", []string{caps.ExecuteMutate}},
		{"GET", "/api/nonsense", []string{caps.ViewMetadata}},
		{"PUT", "/api/apps/prod/web", []string{caps.ExecuteMutate}},

		// Non-API paths (the SPA shell) carry no capability requirement;
		// their data comes from the API routes above.
		{"GET", "/", nil},
	}
	for _, c := range cases {
		got := requiredCapabilities(c.method, c.path)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("requiredCapabilities(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// ── 2. The preset matrix through the real chain ──────────────────────────

// THE mutation anchor: skip the gate's capability check and this fails —
// a preset viewer would receive env values.
func TestViewerCannotRevealSecretValues(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)

	for _, row := range []struct {
		target, cap string
	}{
		{"/api/apps/prod/web/env", caps.RevealSecrets},
		{"/api/apps/prod/web/kv/value?key=flags/beta", caps.RevealSecrets},
		{"/api/logs/prod/web?lines=10", caps.ViewLogs},
	} {
		rec := capDo(t, s, "v", RoleViewer, http.MethodGet, row.target, "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer GET %s = %d, want 403", row.target, rec.Code)
			continue
		}
		if msg := capError(t, rec); !strings.Contains(msg, row.cap) {
			t.Errorf("viewer GET %s error %q does not name %s", row.target, msg, row.cap)
		}
	}
	rec := capDo(t, s, "v", RoleViewer, http.MethodPost, "/api/operations", `{"kind":"deploy","server":"prod","app":"web","image":"nginx:latest"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.ExecuteDeploy) {
		t.Fatalf("viewer deploy = %d (%s), want 403 naming execute.deploy", rec.Code, capError(t, rec))
	}
}

func TestViewerSeesMetadataVariants(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)

	// env/keys is the viewer-visible metadata variant: names only.
	rec := capDo(t, s, "v", RoleViewer, http.MethodGet, "/api/apps/prod/web/env/keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer env/keys = %d (%s), want 200", rec.Code, capError(t, rec))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env.Data), "hunter2-beta") {
		t.Fatalf("env/keys leaked a value: %s", env.Data)
	}
	var keys struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(env.Data, &keys); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keys.Keys) != fmt.Sprint([]string{"API_KEY", "COUNT"}) {
		t.Fatalf("env/keys keys = %v, want [API_KEY COUNT]", keys.Keys)
	}

	// kv listing (metadata) stays viewer-visible; the value read does not.
	rec = capDo(t, s, "v", RoleViewer, http.MethodGet, "/api/apps/prod/web/kv?pattern=*", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer kv list = %d (%s), want 200", rec.Code, capError(t, rec))
	}
}

func TestOperatorExecutesButCannotReveal(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)

	rec := capDo(t, s, "op", RoleEditor, http.MethodPost, "/api/operations", `{"kind":"deploy","server":"prod","app":"web","image":"nginx:latest"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("operator deploy = %d (%s), want 202", rec.Code, capError(t, rec))
	}
	// POST /api/monitors reaches the handler (store nil answers 200 []) —
	// proof the execute.mutate gate passed.
	rec = capDo(t, s, "op", RoleEditor, http.MethodPost, "/api/monitors", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator monitor mutation = %d (%s), want 200 (store-nil early return)", rec.Code, capError(t, rec))
	}
	rec = capDo(t, s, "op", RoleEditor, http.MethodGet, "/api/apps/prod/web/env", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.RevealSecrets) {
		t.Fatalf("operator env GET = %d (%s), want 403 naming reveal.secrets", rec.Code, capError(t, rec))
	}
	// Operator cannot administer users.
	rec = capDo(t, s, "op", RoleEditor, http.MethodGet, "/api/users", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.AdministerUsers) {
		t.Fatalf("operator users GET = %d (%s), want 403 naming administer.users", rec.Code, capError(t, rec))
	}
	// Operator cannot run restore verifications.
	rec = capDo(t, s, "op", RoleEditor, http.MethodPost, "/api/restore-tests/rt1/run", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.RestoreData) {
		t.Fatalf("operator restore run = %d (%s), want 403 naming restore.data", rec.Code, capError(t, rec))
	}
}

func TestOperatorRevealsWhenGranted(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)

	rec := capDo(t, s, "admin", RoleAdmin, http.MethodPut, "/api/users/op", `{"capabilities":["view.metadata","reveal.secrets"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant capabilities = %d (%s)", rec.Code, capError(t, rec))
	}
	rec = capDo(t, s, "op", RoleEditor, http.MethodGet, "/api/apps/prod/web/env", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("granted operator env GET = %d (%s), want 200", rec.Code, capError(t, rec))
	}
	if !strings.Contains(rec.Body.String(), "hunter2-beta") {
		t.Fatalf("granted operator env GET did not return values: %s", rec.Body.String())
	}
}

func TestAdminHoldsAllCapabilities(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)

	rec := capDo(t, s, "admin", RoleAdmin, http.MethodGet, "/api/apps/prod/web/env", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hunter2-beta") {
		t.Fatalf("admin env GET = %d, want 200 with values", rec.Code)
	}
	rec = capDo(t, s, "admin", RoleAdmin, http.MethodGet, "/api/apps/prod/web/kv/value?key=flags/beta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin kv value = %d (%s), want 200", rec.Code, capError(t, rec))
	}
	rec = capDo(t, s, "admin", RoleAdmin, http.MethodGet, "/api/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin users GET = %d (%s), want 200", rec.Code, capError(t, rec))
	}
	rec = capDo(t, s, "admin", RoleAdmin, http.MethodPost, "/api/restore-tests/rt1/run", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin restore run = %d, want 404 (no store configured — capability passed)", rec.Code)
	}
}

// ── 3. The legacy profile ────────────────────────────────────────────────

func TestLegacyProfilePreservesPriorBehaviorExactly(t *testing.T) {
	dir := t.TempDir()
	writeLegacyUsers(t, dir, map[string]string{
		"lv": RoleViewer,
		"le": RoleEditor,
		"la": RoleAdmin,
	})
	s := newCapsServer(t, dir)

	// Legacy viewer: today's viewer read env AND kv values.
	for _, target := range []string{
		"/api/apps/prod/web/env",
		"/api/apps/prod/web/kv/value?key=flags/beta",
	} {
		rec := capDo(t, s, "lv", RoleViewer, http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Errorf("legacy viewer GET %s = %d (%s), want 200 — never silently narrowed", target, rec.Code, capError(t, rec))
		}
	}
	// Legacy viewer still cannot mutate (parity with the old role gate).
	rec := capDo(t, s, "lv", RoleViewer, http.MethodPost, "/api/operations", `{"kind":"deploy","server":"prod","app":"web","image":"nginx:latest"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("legacy viewer deploy = %d, want 403", rec.Code)
	}

	// Legacy editor: deploy, mutate, restore — exactly as before.
	rec = capDo(t, s, "le", RoleEditor, http.MethodPost, "/api/operations", `{"kind":"deploy","server":"prod","app":"web","image":"nginx:latest"}`)
	if rec.Code != http.StatusAccepted {
		t.Errorf("legacy editor deploy = %d (%s), want 202", rec.Code, capError(t, rec))
	}
	rec = capDo(t, s, "le", RoleEditor, http.MethodPost, "/api/monitors", `{}`)
	if rec.Code != http.StatusOK {
		t.Errorf("legacy editor monitor mutation = %d, want 200", rec.Code)
	}
	rec = capDo(t, s, "le", RoleEditor, http.MethodPost, "/api/restore-tests/rt1/run", "")
	if rec.Code == http.StatusForbidden {
		t.Error("legacy editor restore run was narrowed — legacy profile must keep restore.data")
	}
	// Legacy editor also reads values (today's contract).
	rec = capDo(t, s, "le", RoleEditor, http.MethodGet, "/api/apps/prod/web/env", "")
	if rec.Code != http.StatusOK {
		t.Errorf("legacy editor env GET = %d, want 200", rec.Code)
	}

	// Legacy admin: everything, including administration.
	rec = capDo(t, s, "la", RoleAdmin, http.MethodGet, "/api/users", "")
	if rec.Code != http.StatusOK {
		t.Errorf("legacy admin users GET = %d, want 200", rec.Code)
	}
}

func TestLegacyProfileSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	writeLegacyUsers(t, dir, map[string]string{"lv": RoleViewer})
	s := newCapsServer(t, dir)
	if rec := capDo(t, s, "lv", RoleViewer, http.MethodGet, "/api/apps/prod/web/env", ""); rec.Code != http.StatusOK {
		t.Fatalf("pre-restart legacy viewer env GET = %d, want 200", rec.Code)
	}

	// A fresh process over the same store keeps the legacy profile.
	s2 := newCapsServer(t, dir)
	if rec := capDo(t, s2, "lv", RoleViewer, http.MethodGet, "/api/apps/prod/web/env", ""); rec.Code != http.StatusOK {
		t.Fatalf("post-restart legacy viewer env GET = %d (%s), want 200 — migration must never narrow", rec.Code, capError(t, rec))
	}
}

// ── 4. The narrowing click ───────────────────────────────────────────────

func TestNarrowToPresetTakesEffectImmediately(t *testing.T) {
	dir := t.TempDir()
	writeLegacyUsers(t, dir, map[string]string{
		"lv": RoleViewer,
		"la": RoleAdmin,
	})
	s := newCapsServer(t, dir)

	if rec := capDo(t, s, "lv", RoleViewer, http.MethodGet, "/api/apps/prod/web/env", ""); rec.Code != http.StatusOK {
		t.Fatalf("legacy viewer env GET before narrowing = %d, want 200", rec.Code)
	}

	// The explicit act: an admin clicks "narrow to preset".
	rec := capDo(t, s, "la", RoleAdmin, http.MethodPost, "/api/users/lv/narrow-to-preset", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("narrow-to-preset = %d (%s), want 200", rec.Code, capError(t, rec))
	}

	// The SAME live session is narrowed on its next request — capabilities
	// are read from the principal row on every request, like roles.
	rec = capDo(t, s, "lv", RoleViewer, http.MethodGet, "/api/apps/prod/web/env", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.RevealSecrets) {
		t.Fatalf("post-narrowing env GET = %d (%s), want 403 naming reveal.secrets", rec.Code, capError(t, rec))
	}
	// Metadata stays visible after narrowing.
	rec = capDo(t, s, "lv", RoleViewer, http.MethodGet, "/api/apps/prod/web/env/keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("post-narrowing env/keys = %d, want 200", rec.Code)
	}
}

// ── 5. Settings surface + custom storage ─────────────────────────────────

func TestUsersListingExposesProfilesAndCapabilities(t *testing.T) {
	dir := t.TempDir()
	writeLegacyUsers(t, dir, map[string]string{
		"lv": RoleViewer,
		"la": RoleAdmin,
	})
	s := newCapsServer(t, dir)
	seedPresetUsers(t, s)

	rec := capDo(t, s, "la", RoleAdmin, http.MethodGet, "/api/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users listing = %d", rec.Code)
	}
	var env struct {
		Data []struct {
			Username          string   `json:"username"`
			Role              string   `json:"role"`
			CapabilityProfile string   `json:"capability_profile"`
			Capabilities      []string `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	profiles := map[string]string{}
	capsets := map[string][]string{}
	for _, u := range env.Data {
		profiles[u.Username] = u.CapabilityProfile
		capsets[u.Username] = u.Capabilities
	}
	if profiles["lv"] != "legacy" {
		t.Errorf("legacy account profile = %q, want \"legacy\" (visible, flagged)", profiles["lv"])
	}
	if profiles["v"] != "preset" {
		t.Errorf("new account profile = %q, want \"preset\"", profiles["v"])
	}
	if fmt.Sprint(capsets["v"]) != fmt.Sprint([]string{caps.ViewMetadata}) {
		t.Errorf("preset viewer capabilities = %v, want [view.metadata]", capsets["v"])
	}
	if fmt.Sprint(capsets["lv"]) != fmt.Sprint([]string{caps.RevealSecrets, caps.ViewLogs, caps.ViewMetadata}) {
		t.Errorf("legacy viewer capabilities = %v, want the recorded legacy set", capsets["lv"])
	}
}

func TestCustomCapabilityValidation(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)

	rec := capDo(t, s, "admin", RoleAdmin, http.MethodPut, "/api/users/v", `{"capabilities":["execute.everything"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown capability = %d, want 400", rec.Code)
	}
	// Role and capabilities are separate explicit acts; mixing them in one
	// request is rejected rather than guessed.
	rec = capDo(t, s, "admin", RoleAdmin, http.MethodPut, "/api/users/v", `{"role":"editor","capabilities":["view.metadata"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("role+capabilities in one request = %d, want 400", rec.Code)
	}
}

func TestCustomCapabilitiesPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	writeLegacyUsers(t, dir, map[string]string{
		"lv": RoleViewer,
		"la": RoleAdmin,
	})
	s := newCapsServer(t, dir)

	rec := capDo(t, s, "la", RoleAdmin, http.MethodPut, "/api/users/lv", `{"capabilities":["view.metadata","reveal.secrets"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("custom grant = %d (%s)", rec.Code, capError(t, rec))
	}
	s2 := newCapsServer(t, dir)
	rec = capDo(t, s2, "lv", RoleViewer, http.MethodGet, "/api/apps/prod/web/env", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("post-restart custom-granted viewer env GET = %d (%s), want 200", rec.Code, capError(t, rec))
	}
}

// ── 6. Whoami ────────────────────────────────────────────────────────────

func TestWhoamiExposesCapabilities(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)

	rec := capDo(t, s, "v", RoleViewer, http.MethodGet, "/api/auth/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami = %d", rec.Code)
	}
	var env struct {
		Data struct {
			Mode string `json:"mode"`
			User struct {
				Username     string   `json:"username"`
				Role         string   `json:"role"`
				Capabilities []string `json:"capabilities"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(env.Data.User.Capabilities) != fmt.Sprint([]string{caps.ViewMetadata}) {
		t.Fatalf("whoami capabilities = %v, want [view.metadata]", env.Data.User.Capabilities)
	}
}
