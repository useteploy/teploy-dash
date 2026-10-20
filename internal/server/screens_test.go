package server

// screens_test.go — plan D5 surfaces: secrets (write-only), the releases
// list with rollback-to, redeploy, backups, and the env-guard deferral to
// the CLI (env-config-guard-v1). Same injected-runner seam as
// confighome_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/operation"
)

// allScreenTokens is every token the D5 surfaces gate on, on top of the P2
// set. env-config-guard-v1 is included only by tests that exercise the
// deferral, because it switches the env guard from dash's local pre-check to
// the CLI's refusal.
var screenTokens = []string{"secret-stdin-v1", "releases-json-v1", "rollback-json-v1", "redeploy-v1", "backup-json-v1", "backup-target-v1"}

func withScreenTokens(extra ...string) []string {
	return append(append([]string{}, allConfigTokens...), append(append([]string{}, screenTokens...), extra...)...)
}

// newScreenOpServer is newConfigServer plus an instant operation executor
// and resolver: the enqueue tests' operations complete synchronously, so
// draining in cleanup stops every worker before t.TempDir removal (a live
// executor's late record writes race the cleanup on CI).
type screenOpServer struct {
	*Server
	screenRunner *configRunner
}

func newScreenOpServer(t *testing.T) *screenOpServer {
	t.Helper()
	c := &configRunner{tokens: withScreenTokens()}
	s := New(Config{
		DataDir:         t.TempDir(),
		NoAuth:          true,
		CLIInstalled:    func() bool { return true },
		CLIRunner:       c.run,
		CLIStdinRunner:  c.runStdin,
		CLIIdentity:     func() string { return "test-cli" },
		EnvStdinSupport: func() (bool, error) { return true, nil },
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: "192.0.2.10", User: "deploy"}, nil
		},
		OperationExecutor: func(_ context.Context, command operation.Command, _ func(operation.Stream, string)) (int, error) {
			return 0, nil
		},
	})
	t.Cleanup(func() { s.DrainOperations(context.Background()) })
	return &screenOpServer{Server: s, screenRunner: c}
}

// Every D5 surface answers 501 with the remedy when the CLI lacks its token,
// and the CLI is not run at all.
func TestScreensHiddenWithOlderCLI(t *testing.T) {
	c := &configRunner{tokens: allConfigTokens}
	s := newConfigServer(t, c)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/apps/prod/web/releases", ""},
		{"GET", "/api/apps/prod/web/backups", ""},
		{"GET", "/api/apps/prod/web/secrets", ""},
		{"POST", "/api/apps/prod/web/redeploy", ""},
		{"POST", "/api/apps/prod/web/rollback", `{"to":"20261019-120000"}`},
		{"POST", "/api/apps/prod/web/secrets", `{"key":"API_KEY","value":"v"}`},
		{"DELETE", "/api/apps/prod/web/secrets/API_KEY", ""},
		{"POST", "/api/apps/prod/web/backups/create", `{}`},
		{"POST", "/api/apps/prod/web/backups/restore", `{"id":"20261019-120000"}`},
	} {
		rec := serve(s, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s with an older CLI = %d, want 501: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "newer teploy CLI") {
			t.Errorf("%s %s: refusal must name the remedy: %s", tc.method, tc.path, rec.Body.String())
		}
	}
	if len(c.calls) != 0 {
		t.Fatalf("an unsupported surface must not run the CLI: %v", c.calls)
	}
}

func TestReleasesListForwardsCLIEnvelope(t *testing.T) {
	c := &configRunner{tokens: withScreenTokens(), reply: func(_ []string, _ string) (*cli.Result, error) {
		return &cli.Result{Stdout: `{"machine_interface":2,"host":"h","app":"web","deployment_type":"container","current":"b","releases":[{"release":"b","current":true,"rollback_available":false},{"release":"a","image":"nginx:1","current":false,"rollback_available":true}]}`}, nil
	}}
	s := newConfigServer(t, c)
	rec := serve(s, "GET", "/api/apps/prod/web/releases", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("releases = %d: %s", rec.Code, rec.Body.String())
	}
	call := c.last("releases")
	if call == nil || indexOfArg(call.args, "--json") < 0 || indexOfArg(call.args, "--app") < 0 {
		t.Fatalf("releases argv = %v", call)
	}
	var body struct {
		Data struct {
			Current string `json:"current"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Data.Current != "b" {
		t.Fatalf("releases envelope not forwarded: %s (%v)", rec.Body.String(), err)
	}
}

func TestSecretsListSetRemove(t *testing.T) {
	c := &configRunner{tokens: withScreenTokens(), reply: func(args []string, _ string) (*cli.Result, error) {
		if args[0] == "secret" && args[1] == "list" {
			return &cli.Result{Stdout: `[{"key":"API_KEY","value":"***","provider":"local"}]`}, nil
		}
		return &cli.Result{Stdout: `  Set secret API_KEY`}, nil
	}}
	s := newConfigServer(t, c)
	rec := serve(s, "GET", "/api/apps/prod/web/secrets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("secrets list = %d: %s", rec.Code, rec.Body.String())
	}
	list := c.last("secret list")
	if list == nil || indexOfArg(list.args, "--json") < 0 {
		t.Fatalf("secret list argv = %v", list)
	}

	rec = serve(s, "POST", "/api/apps/prod/web/secrets", `{"key":"API_KEY","value":"s3cr3t-value"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("secrets set = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cr3t-value") {
		t.Fatalf("the value must never come back: %s", rec.Body.String())
	}
	set := c.last("secret set")
	if set == nil {
		t.Fatal("secret set not run")
	}
	if set.stdin != "s3cr3t-value" {
		t.Fatalf("value must travel verbatim on stdin, got %q", set.stdin)
	}
	if strings.Contains(strings.Join(set.args, " "), "s3cr3t-value") {
		t.Fatalf("the value must never be on the argv: %v", set.args)
	}
	if indexOfArg(set.args, "--stdin") < 0 {
		t.Fatalf("set must use the stdin contract: %v", set.args)
	}

	rec = serve(s, "DELETE", "/api/apps/prod/web/secrets/API_KEY", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("secrets delete = %d: %s", rec.Code, rec.Body.String())
	}
	if call := c.last("secret rm"); call == nil || indexOfArg(call.args, "API_KEY") < 0 {
		t.Fatalf("secret rm argv = %v", call)
	}
	// Flag-shaped keys never reach the CLI's argv.
	if rec := serve(s, "DELETE", "/api/apps/prod/web/secrets/--host", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("flag-shaped key = %d, want 400", rec.Code)
	}
}

// rollback --to goes through the durable operation path; an invalid target
// is refused at admission and never enqueued.
func TestRollbackToEnqueuesOperation(t *testing.T) {
	s := newScreenOpServer(t)
	rec := serve(s.Server, "POST", "/api/apps/prod/web/rollback", `{"to":"20261019-120000"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("rollback-to = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"kind":"rollback"`) || !strings.Contains(rec.Body.String(), `"to":"20261019-120000"`) {
		t.Fatalf("rollback-to must enqueue a rollback operation with its target: %s", rec.Body.String())
	}
	for _, body := range []string{`{"to":"--image"}`, `{"to":"a; rm -rf /"}`, `{"unexpected":1}`} {
		if rec := serve(s.Server, "POST", "/api/apps/prod/web/rollback", body); rec.Code != http.StatusBadRequest {
			t.Errorf("rollback body %s = %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestRedeployEnqueuesOperation(t *testing.T) {
	s := newScreenOpServer(t)
	rec := serve(s.Server, "POST", "/api/apps/prod/web/redeploy", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("redeploy = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"kind":"redeploy"`) {
		t.Fatalf("redeploy must be its own operation kind: %s", rec.Body.String())
	}
}

func TestBackupsListAndOperations(t *testing.T) {
	s := newScreenOpServer(t)
	c := s.screenRunner
	c.reply = func(_ []string, _ string) (*cli.Result, error) {
		return &cli.Result{Stdout: `{"machine_interface":2,"host":"h","app":"web","target":{"bucket":"b","region":"us-east-1","has_credentials":true},"backups":[{"id":"20261019-120000","kind":"volumes","size_bytes":10,"created_at":"2026-10-19T12:00:00Z","key":"s3://b/web","restorable":true}],"schedules":[]}`}, nil
	}
	rec := serve(s.Server, "GET", "/api/apps/prod/web/backups", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("backups = %d: %s", rec.Code, rec.Body.String())
	}
	if call := c.last("backup list"); call == nil || indexOfArg(call.args, "--json") < 0 {
		t.Fatalf("backup list argv = %v", call)
	}

	rec = serve(s.Server, "POST", "/api/apps/prod/web/backups/create", `{"accessory":"postgres"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("backup create = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"kind":"backup_create"`) {
		t.Fatalf("backup create must be a durable operation: %s", rec.Body.String())
	}

	rec = serve(s.Server, "POST", "/api/apps/prod/web/backups/restore", `{"id":"20261019-120000-0123456789abcdef"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("backup restore = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"kind":"backup_restore"`) {
		t.Fatalf("backup restore must be a durable operation: %s", rec.Body.String())
	}
	for _, body := range []string{`{"id":"../../etc"}`, `{"id":""}`, `{"id":"not-a-date"}`, `{"other":1}`} {
		if rec := serve(s.Server, "POST", "/api/apps/prod/web/backups/restore", body); rec.Code != http.StatusBadRequest {
			t.Errorf("restore body %s = %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
}

// With the CLI's own env guard (env-config-guard-v1) dash drops its local
// pre-check: the edit reaches the CLI, whose refusal is surfaced verbatim as
// a 409 naming the key and the remedy.
func TestEnvGuardDefersToCLI(t *testing.T) {
	c := &configRunner{tokens: withScreenTokens("env-config-guard-v1"), reply: func(args []string, _ string) (*cli.Result, error) {
		if args[0] == "config" && args[1] == "get" {
			return &cli.Result{Stdout: `{"machine_interface":2,"home":"teploy","commit":"` + strings.Repeat("e", 40) + `","content":"app: web\nenv:\n  LOG_LEVEL: info\n"}`}, nil
		}
		return &cli.Result{ExitCode: 1, Stderr: "env operation refused (nothing written):\n  LOG_LEVEL is declared by web's Teploy config (edited in Teploy) — change it there (`teploy config set --app web env.LOG_LEVEL=...`, or --unset env.LOG_LEVEL) or in dash Settings; a .env value would be shadowed at the next deploy"}, nil
	}}
	s := newConfigServer(t, c)
	rec := serve(s, "POST", "/api/apps/prod/web/env", `{"key":"LOG_LEVEL","value":"debug"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("CLI-guarded env set = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "LOG_LEVEL is declared") || !strings.Contains(rec.Body.String(), "Settings") {
		t.Fatalf("the CLI's refusal message must surface: %s", rec.Body.String())
	}
	if call := c.last("env set"); call == nil {
		t.Fatal("with the CLI guard the edit must reach the CLI (its refusal is the answer)")
	} else if call.stdin != "debug" {
		t.Fatalf("env value must ride stdin: %+v", call)
	}
}

// Without the token, dash's local guard still refuses (unchanged behavior).
func TestEnvGuardLocalStillRefusesWithoutToken(t *testing.T) {
	c := &configRunner{tokens: allConfigTokens, reply: func(args []string, _ string) (*cli.Result, error) {
		if args[0] == "config" && args[1] == "get" {
			return &cli.Result{Stdout: `{"machine_interface":2,"home":"teploy","commit":"` + strings.Repeat("e", 40) + `","content":"app: web\nenv:\n  LOG_LEVEL: info\n"}`}, nil
		}
		return &cli.Result{}, nil
	}}
	s := newConfigServer(t, c)
	rec := serve(s, "POST", "/api/apps/prod/web/env", `{"key":"LOG_LEVEL","value":"debug"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Settings") {
		t.Fatalf("local guard = %d %s, want 409 pointing to Settings", rec.Code, rec.Body.String())
	}
}

func TestScreenRouteClassification(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         string
	}{
		{http.MethodGet, "/api/apps/prod/web/releases", "view.metadata"},
		{http.MethodGet, "/api/apps/prod/web/backups", "view.metadata"},
		{http.MethodGet, "/api/apps/prod/web/secrets", "view.metadata"},
		{http.MethodPost, "/api/apps/prod/web/secrets", "execute.mutate"},
		{http.MethodDelete, "/api/apps/prod/web/secrets/KEY", "execute.mutate"},
		{http.MethodPost, "/api/apps/prod/web/redeploy", "execute.deploy"},
		{http.MethodPost, "/api/apps/prod/web/backups/create", "execute.deploy"},
		{http.MethodPost, "/api/apps/prod/web/backups/restore", "execute.deploy"},
	} {
		if got := requiredCapabilities(tc.method, tc.path); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s %s requires %v, want [%s]", tc.method, tc.path, got, tc.want)
		}
	}
}
