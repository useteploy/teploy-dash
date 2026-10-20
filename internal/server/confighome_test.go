package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/cli"
)

// configRunner fakes the CLI for the server-authority P2 endpoints: it
// answers the version handshake with the given capability tokens, resolves
// `server list`, and hands every other argv (with stdin, for the stdin
// runner) to reply.
type configRunner struct {
	mu     sync.Mutex
	tokens []string
	calls  []configCall
	reply  func(args []string, stdin string) (*cli.Result, error)
}

type configCall struct {
	args  []string
	stdin string
}

func (c *configRunner) handle(args []string, stdin string) (*cli.Result, error) {
	joined := strings.Join(args, " ")
	switch {
	case joined == "version --json":
		tokens, _ := json.Marshal(c.tokens)
		return &cli.Result{Stdout: `{"version":"teploy v0.1.44","machine_interface":2,"capabilities":` + string(tokens) + `}`}, nil
	case strings.HasPrefix(joined, "server list"):
		return &cli.Result{Stdout: `{"machine_interface":2,"servers":[{"name":"prod","host":"192.0.2.10","user":"deploy"}]}`}, nil
	}
	c.mu.Lock()
	c.calls = append(c.calls, configCall{args: append([]string(nil), args...), stdin: stdin})
	c.mu.Unlock()
	if c.reply != nil {
		return c.reply(args, stdin)
	}
	return &cli.Result{Stdout: "{}"}, nil
}

func (c *configRunner) run(_ context.Context, args ...string) (*cli.Result, error) {
	return c.handle(args, "")
}

// runStdin mirrors cli.RunWithStdin: a non-zero exit is an ExitStatusError.
func (c *configRunner) runStdin(_ context.Context, stdin string, args ...string) (*cli.Result, error) {
	res, err := c.handle(args, stdin)
	if err != nil {
		return res, err
	}
	return res, cli.CheckExit(res)
}

func (c *configRunner) last(prefix string) *configCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.calls) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.Join(c.calls[i].args, " "), prefix) {
			call := c.calls[i]
			return &call
		}
	}
	return nil
}

var allConfigTokens = []string{"app-list-machine", "server-status-machine", "events-v1", "actor-flag-v1", "config-home-v1", "config-merge-v1", "config-unprivileged-v1", "deploy-home-v1"}

func newConfigServer(t *testing.T, c *configRunner) *Server {
	t.Helper()
	return New(Config{
		DataDir:        t.TempDir(),
		NoAuth:         true,
		CLIInstalled:   func() bool { return true },
		CLIRunner:      c.run,
		CLIStdinRunner: c.runStdin,
		CLIIdentity:    func() string { return "test-cli" },
	})
}

func serve(s *Server, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.handleAppAction(rec, req)
	return rec
}

func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func TestConfigRoutesHiddenWithOlderCLI(t *testing.T) {
	c := &configRunner{tokens: []string{"app-list-machine", "server-status-machine"}}
	s := newConfigServer(t, c)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/apps/prod/web/config", ""},
		{"PUT", "/api/apps/prod/web/config", `{"content":"x: 1\n","expect":"` + strings.Repeat("a", 40) + `"}`},
		{"GET", "/api/apps/prod/web/config/history", ""},
		{"POST", "/api/apps/prod/web/config/home", `{"home":"teploy"}`},
		{"GET", "/api/apps/prod/web/events", ""},
		{"POST", "/api/apps/prod/web/deploy", ""},
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

func TestConfigGetForwardsCLIEnvelope(t *testing.T) {
	c := &configRunner{tokens: allConfigTokens, reply: func(args []string, _ string) (*cli.Result, error) {
		return &cli.Result{Stdout: `{"machine_interface":2,"home":"teploy","commit":"` + strings.Repeat("b", 40) + `","content":"app: web\n"}`}, nil
	}}
	s := newConfigServer(t, c)
	rec := serve(s, "GET", "/api/apps/prod/web/config?at=abcdef1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("config get = %d: %s", rec.Code, rec.Body.String())
	}
	call := c.last("config get")
	if call == nil || indexOfArg(call.args, "--at") < 0 || call.args[indexOfArg(call.args, "--at")+1] != "abcdef1" {
		t.Fatalf("config get argv = %v", call)
	}
	if i := indexOfArg(call.args, "--host"); i < 0 || call.args[i+1] != "192.0.2.10" {
		t.Fatalf("config get must target the resolved host: %v", call.args)
	}
	if rec := serve(s, "GET", "/api/apps/prod/web/config?at=$(id)", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("a non-commit at= must be refused, got %d", rec.Code)
	}
}

func TestConfigPutIsCASOverStdinWithActor(t *testing.T) {
	expect := strings.Repeat("c", 40)
	c := &configRunner{tokens: allConfigTokens, reply: func(args []string, _ string) (*cli.Result, error) {
		return &cli.Result{Stdout: `{"machine_interface":2,"home":"teploy","commit":"` + strings.Repeat("d", 40) + `"}`}, nil
	}}
	s := newConfigServer(t, c)
	rec := serve(s, "PUT", "/api/apps/prod/web/config", `{"content":"app: web\nimage: nginx:2\n","expect":"`+expect+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("config put = %d: %s", rec.Code, rec.Body.String())
	}
	call := c.last("config set")
	if call == nil {
		t.Fatal("config set not run")
	}
	if call.stdin != "app: web\nimage: nginx:2\n" {
		t.Fatalf("content must travel on stdin, got %q", call.stdin)
	}
	if strings.Contains(strings.Join(call.args, " "), "nginx:2") {
		t.Fatalf("content must never be on the argv: %v", call.args)
	}
	if i := indexOfArg(call.args, "--expect"); i < 0 || call.args[i+1] != expect {
		t.Fatalf("save must be compare-and-swap on the loaded commit: %v", call.args)
	}
	if i := indexOfArg(call.args, "--actor"); i < 0 || !strings.HasPrefix(call.args[i+1], "dash:") {
		t.Fatalf("save must name the dashboard actor: %v", call.args)
	}
	// Auth disabled: there is no principal to narrow, so no --unprivileged.
	if indexOfArg(call.args, "--unprivileged") >= 0 {
		t.Fatalf("no-auth save should not be narrowed: %v", call.args)
	}

	for _, body := range []string{
		`{"content":"x: 1\n","expect":"HEAD"}`,
		`{"content":"   ","expect":"` + expect + `"}`,
		`{"content":"x: 1\n","expect":"` + expect + `","extra":1}`,
	} {
		if rec := serve(s, "PUT", "/api/apps/prod/web/config", body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s = %d, want 400", body, rec.Code)
		}
	}
}

func TestConfigPutConflictReturnsMerge(t *testing.T) {
	expect := strings.Repeat("1", 40)
	current := strings.Repeat("3", 40)
	c := &configRunner{tokens: allConfigTokens}
	c.reply = func(args []string, _ string) (*cli.Result, error) {
		switch args[1] {
		case "set":
			return &cli.Result{ExitCode: 1, Stderr: `{"machine_interface":2,"code":"conflict","message":"conflict","detail":"config moved to ` + current + `"}`}, nil
		case "merge":
			return &cli.Result{Stdout: `{"machine_interface":2,"base":"` + expect + `","current":"` + current + `","merged":"image: nginx:2\n","conflicts":[{"key":"image","mine":"nginx:2","current":"nginx:3"}]}`}, nil
		}
		return &cli.Result{Stdout: "{}"}, nil
	}
	s := newConfigServer(t, c)
	rec := serve(s, "PUT", "/api/apps/prod/web/config", `{"content":"image: nginx:2\n","expect":"`+expect+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
		Merge struct {
			Current   string `json:"current"`
			Merged    string `json:"merged"`
			Conflicts []struct {
				Key string `json:"key"`
			} `json:"conflicts"`
		} `json:"merge"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Merge.Current != current || body.Merge.Merged != "image: nginx:2\n" || len(body.Merge.Conflicts) != 1 {
		t.Fatalf("conflict must carry the CLI merge: %+v", body)
	}
	merge := c.last("config merge")
	if merge == nil || merge.stdin != "image: nginx:2\n" || merge.args[indexOfArg(merge.args, "--base")+1] != expect {
		t.Fatalf("merge must run against the edit's base with the draft on stdin: %+v", merge)
	}
}

func TestConfigPutValidationIs422(t *testing.T) {
	c := &configRunner{tokens: allConfigTokens, reply: func(args []string, _ string) (*cli.Result, error) {
		return &cli.Result{ExitCode: 1, Stderr: `{"machine_interface":2,"code":"config-invalid","message":"invalid","detail":"env_files are not allowed in Teploy home"}`}, nil
	}}
	s := newConfigServer(t, c)
	rec := serve(s, "PUT", "/api/apps/prod/web/config", `{"content":"env_files: [a]\n","expect":"`+strings.Repeat("1", 40)+`"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "env_files are not allowed") {
		t.Fatalf("validation = %d %s, want 422 with the CLI detail", rec.Code, rec.Body.String())
	}
}

func TestDeployPendingRunsServerConfigDeploy(t *testing.T) {
	c := &configRunner{tokens: allConfigTokens}
	s := newConfigServer(t, c)
	if s.operations == nil {
		t.Skip("operations unavailable in this environment")
	}
	rec := serve(s, "POST", "/api/apps/prod/web/deploy", "")
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("deploy pending = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"kind":"deploy"`) {
		t.Fatalf("deploy pending must enqueue a deploy operation: %s", rec.Body.String())
	}
}

func TestConfigOwnedEnvKeyRefused(t *testing.T) {
	c := &configRunner{tokens: allConfigTokens, reply: func(args []string, _ string) (*cli.Result, error) {
		if len(args) > 1 && args[0] == "config" && args[1] == "get" {
			return &cli.Result{Stdout: `{"machine_interface":2,"home":"teploy","commit":"` + strings.Repeat("e", 40) + `","content":"app: web\nimage: nginx\nenv:\n  LOG_LEVEL: info\n"}`}, nil
		}
		return &cli.Result{}, nil
	}}
	s := newConfigServer(t, c)
	rec := serve(s, "POST", "/api/apps/prod/web/env", `{"key":"LOG_LEVEL","value":"debug"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Settings") {
		t.Fatalf("config-owned key edit = %d %s, want 409 pointing to Settings", rec.Code, rec.Body.String())
	}
	if call := c.last("env set"); call != nil {
		t.Fatalf("a config-owned key must not reach env set: %v", call.args)
	}
	rec = serve(s, "DELETE", "/api/apps/prod/web/env/LOG_LEVEL", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("config-owned key unset = %d, want 409", rec.Code)
	}
}

func TestConfigAuthorityCarriesHome(t *testing.T) {
	commit, applied := strings.Repeat("f", 40), strings.Repeat("0", 39)+"1"
	c := &configRunner{tokens: allConfigTokens, reply: func(args []string, _ string) (*cli.Result, error) {
		return &cli.Result{Stdout: `{"machine_interface":2,"home":"teploy","commit":"` + commit + `","applied_commit":"` + applied + `","content":"app: web\nimage: nginx\nenv:\n  LOG_LEVEL: info\n"}`}, nil
	}}
	s := newConfigServer(t, c)
	rec := serve(s, "GET", "/api/apps/prod/web/config-authority", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("config-authority = %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data configAuthorityResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Home != "teploy" || env.Data.ConfigCommit != commit || env.Data.AppliedCommit != applied {
		t.Fatalf("home/commits not surfaced: %+v", env.Data)
	}
	if len(env.Data.DeclaredEnvKeys) != 1 || env.Data.DeclaredEnvKeys[0] != "LOG_LEVEL" {
		t.Fatalf("teploy-home declared keys = %v", env.Data.DeclaredEnvKeys)
	}
}

func TestCapabilityCacheFollowsCLIIdentity(t *testing.T) {
	identity := "v1"
	probes := 0
	s := New(Config{
		DataDir:      t.TempDir(),
		NoAuth:       true,
		CLIInstalled: func() bool { return true },
		CLIIdentity:  func() string { return identity },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			if strings.Join(args, " ") == "version --json" {
				probes++
				return &cli.Result{Stdout: `{"version":"teploy v0.1.44","machine_interface":2,"capabilities":["config-home-v1"]}`}, nil
			}
			return &cli.Result{}, nil
		},
	})
	ctx := context.Background()
	s.capabilities(ctx)
	s.capabilities(ctx)
	if probes != 1 {
		t.Fatalf("same binary must hit the cache, probes = %d", probes)
	}
	identity = "v2"
	if !s.capabilities(ctx).Features.ConfigHome || probes != 2 {
		t.Fatalf("a replaced binary must re-probe, probes = %d", probes)
	}
	s.capabilitiesCache.invalidate()
	s.capabilities(ctx)
	if probes != 3 {
		t.Fatalf("invalidate must force a re-probe, probes = %d", probes)
	}
}

// Gate 8: a session without the admin capability saves with --unprivileged,
// so the CLI refuses a change that reaches the host; an admin does not.
func TestConfigWritesNarrowNonAdmins(t *testing.T) {
	c := &configRunner{tokens: allConfigTokens, reply: func(args []string, _ string) (*cli.Result, error) {
		return &cli.Result{Stdout: `{"machine_interface":2,"home":"teploy","commit":"` + strings.Repeat("d", 40) + `"}`}, nil
	}}
	s := newConfigServer(t, c)
	expect := strings.Repeat("c", 40)
	for _, tc := range []struct {
		role         string
		unprivileged bool
	}{{"editor", true}, {"admin", false}} {
		for _, req := range []struct{ method, path, body, prefix string }{
			{"PUT", "/api/apps/prod/web/config", `{"content":"app: web\n","expect":"` + expect + `"}`, "config set"},
			{"POST", "/api/apps/prod/web/config/restore", `{"commit":"` + expect + `"}`, "config restore"},
		} {
			r := httptest.NewRequest(req.method, req.path, strings.NewReader(req.body))
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(context.WithValue(r.Context(), userCtxKey, &sessionInfo{user: "u", role: tc.role, caps: caps.PresetForRole(tc.role)}))
			rec := httptest.NewRecorder()
			s.handleAppAction(rec, r)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s as %s = %d: %s", req.method, req.path, tc.role, rec.Code, rec.Body.String())
			}
			call := c.last(req.prefix)
			if got := indexOfArg(call.args, "--unprivileged") >= 0; got != tc.unprivileged {
				t.Fatalf("%s as %s: --unprivileged = %v, want %v (%v)", req.prefix, tc.role, got, tc.unprivileged, call.args)
			}
			if i := indexOfArg(call.args, "--actor"); i < 0 || call.args[i+1] != "dash:u" {
				t.Fatalf("%s as %s must name the user: %v", req.prefix, tc.role, call.args)
			}
		}
	}
}
