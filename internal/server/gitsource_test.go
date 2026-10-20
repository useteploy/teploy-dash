package server

// gitsource_test.go — the D4 app-scoped source routes: gating on the
// git-source/trigger-poll tokens, the argv each CLI call builds (tokens
// never on argv), routing-hint maintenance, and the register flow against
// a fake forge.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/source"
)

var gitSourceTokens = []string{"app-list-machine", "server-status-machine", "events-v1", "actor-flag-v1", "config-home-v1", "config-merge-v1", "config-unprivileged-v1", "deploy-home-v1", "git-source-v1", "trigger-poll-v1"}

func newGitSourceServer(t *testing.T, reply func(args []string, stdin string) (*cli.Result, error)) (*Server, *configRunner) {
	t.Helper()
	runner := &configRunner{tokens: gitSourceTokens, reply: reply}
	server := newConfigServer(t, runner)
	if server.routingInitErr != nil {
		t.Fatalf("routing init: %v", server.routingInitErr)
	}
	return server, runner
}

func TestGitSourceRoutesHiddenWithOlderCLI(t *testing.T) {
	runner := &configRunner{tokens: []string{"app-list-machine", "config-home-v1"}}
	s := newConfigServer(t, runner)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/apps/prod/web/source", ""},
		{"GET", "/api/apps/prod/web/source/timer", ""},
		{"POST", "/api/apps/prod/web/source/key", `{}`},
		{"POST", "/api/apps/prod/web/source/host-key", `{}`},
		{"POST", "/api/apps/prod/web/source/register", `{}`},
		{"POST", "/api/apps/prod/web/source/reset-watermark", `{"ref":"refs/heads/main","confirm":"refs/heads/main"}`},
		{"POST", "/api/apps/prod/web/source/retry", `{}`},
		{"POST", "/api/apps/prod/web/source/install-timer", `{}`},
	} {
		response := serve(s, tc.method, tc.path, tc.body)
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s status=%d body=%s, want 501", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
	// A CLI with git-source but without trigger-poll keeps the read/mutate
	// routes but hides the trigger ones.
	runner.tokens = []string{"app-list-machine", "server-status-machine", "config-home-v1", "git-source-v1"}
	s.capabilitiesCache.invalidate()
	if response := serve(s, "GET", "/api/apps/prod/web/source", ""); response.Code == http.StatusNotImplemented {
		t.Fatal("source show hidden with git-source present")
	}
	resetBody := `{"ref":"refs/heads/main","confirm":"refs/heads/main"}`
	for _, path := range []string{"/api/apps/prod/web/source/retry", "/api/apps/prod/web/source/reset-watermark", "/api/apps/prod/web/source/install-timer"} {
		body := `{}`
		if path == "/api/apps/prod/web/source/reset-watermark" {
			body = resetBody
		}
		if response := serve(s, "POST", path, body); response.Code != http.StatusNotImplemented {
			t.Fatalf("%s status=%d, want 501", path, response.Code)
		}
	}
}

func TestGitSourceShowPassesEnvelopeAndNotesRoute(t *testing.T) {
	s, runner := newGitSourceServer(t, func(args []string, stdin string) (*cli.Result, error) {
		if strings.HasPrefix(strings.Join(args, " "), "config source show") {
			return &cli.Result{Stdout: `{"machine_interface":2,"host":"192.0.2.10","app":"web","repository":"git@github.com:owner/repo.git","branch":"main"}`}, nil
		}
		return &cli.Result{Stdout: "{}"}, nil
	})
	response := serve(s, "GET", "/api/apps/prod/web/source", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if call := runner.last("config source show"); call == nil || indexOfArg(call.args, "--json") < 0 ||
		indexOfArg(call.args, "--host") < 0 || indexOfArg(call.args, "web") < 0 {
		t.Fatalf("show argv = %v", call)
	}
	if routes := s.routing.RoutesFor("https://github.com/owner/repo"); len(routes) != 1 || routes[0].App != "web" || routes[0].Server != "prod" {
		t.Fatalf("route not noted: %+v", routes)
	}
	// An app with no git: block clears its hint.
	runner.reply = func(args []string, stdin string) (*cli.Result, error) {
		return &cli.Result{Stdout: `{"machine_interface":2,"host":"192.0.2.10","app":"web"}`}, nil
	}
	serve(s, "GET", "/api/apps/prod/web/source", "")
	if routes := s.routing.RoutesFor("https://github.com/owner/repo"); len(routes) != 0 {
		t.Fatalf("route survived a disconnected app: %+v", routes)
	}
}

func TestGitSourceKeyBuildsDeployKeyArgv(t *testing.T) {
	s, runner := newGitSourceServer(t, nil)
	response := serve(s, "POST", "/api/apps/prod/web/source/key", `{"type":"ssh"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	call := runner.last("config source key")
	if call == nil {
		t.Fatal("config source key never ran")
	}
	joined := strings.Join(call.args, " ")
	if !strings.Contains(joined, "config source key --type ssh --json") || !strings.Contains(joined, "--actor dash:") {
		t.Fatalf("key argv = %v", call.args)
	}
	if response := serve(s, "POST", "/api/apps/prod/web/source/key", `{"type":"token"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("token type status=%d, want 400", response.Code)
	}
	// A CLI refusal (non-zero exit, plain stderr) answers 422 with the text.
	runner.reply = func(args []string, stdin string) (*cli.Result, error) {
		return &cli.Result{ExitCode: 1, Stderr: "web has no git: source in its config"}, &cli.ExitStatusError{Code: 1}
	}
	if response := serve(s, "POST", "/api/apps/prod/web/source/key", `{}`); response.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(response.Body.String(), "no git: source") {
		t.Fatalf("refusal status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestGitSourceHostKeyValidatesFingerprint(t *testing.T) {
	s, runner := newGitSourceServer(t, nil)
	body := `{"expected_fingerprint":"SHA256:abcdefghijklmnopqrstuvwxyz012345678901234567890123456789012="}`
	response := serve(s, "POST", "/api/apps/prod/web/source/host-key", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if call := runner.last("config source host-key"); call == nil || indexOfArg(call.args, "--expected-fingerprint") < 0 {
		t.Fatalf("host-key argv = %v", call)
	}
	if response := serve(s, "POST", "/api/apps/prod/web/source/host-key", `{"expected_fingerprint":"md5:deadbeef"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("bad fingerprint status=%d, want 400", response.Code)
	}
}

func TestGitSourceResetWatermarkRequiresTypedRef(t *testing.T) {
	s, runner := newGitSourceServer(t, nil)
	if response := serve(s, "POST", "/api/apps/prod/web/source/reset-watermark", `{"ref":"refs/heads/main","confirm":"main"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed reset status=%d, want 400", response.Code)
	}
	if response := serve(s, "POST", "/api/apps/prod/web/source/reset-watermark", `{"ref":"refs/heads/main","confirm":"refs/heads/main"}`); response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	call := runner.last("trigger reset")
	if call == nil || !strings.Contains(strings.Join(call.args, " "), "--ref refs/heads/main") {
		t.Fatalf("reset argv = %v", call)
	}
	if response := serve(s, "POST", "/api/apps/prod/web/source/retry", `{}`); response.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", response.Code, response.Body.String())
	}
	if runner.last("trigger retry") == nil {
		t.Fatal("trigger retry never ran")
	}
}

func TestGitSourceInstallTimerIsHostScoped(t *testing.T) {
	s, runner := newGitSourceServer(t, nil)
	response := serve(s, "POST", "/api/apps/prod/web/source/install-timer", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	call := runner.last("trigger install-timer")
	if call == nil {
		t.Fatal("trigger install-timer never ran")
	}
	joined := strings.Join(call.args, " ")
	if !strings.Contains(joined, "--host 192.0.2.10") || strings.Contains(joined, "--app") {
		t.Fatalf("install-timer argv = %v", call.args)
	}
}

func TestGitSourceTimerReadsDoctor(t *testing.T) {
	s, runner := newGitSourceServer(t, func(args []string, stdin string) (*cli.Result, error) {
		if strings.HasPrefix(strings.Join(args, " "), "doctor") {
			// Exit 1 (a failing check elsewhere) still carries the report.
			return &cli.Result{ExitCode: 1, Stdout: `{"machine_interface":2,"checks":[{"name":"disk","result":"fail"},{"name":"git-trigger","result":"warn","detail":"git-source apps (web) but no teploy-trigger-poll.timer"}]}`}, &cli.ExitStatusError{Code: 1}
		}
		return &cli.Result{Stdout: "{}"}, nil
	})
	response := serve(s, "GET", "/api/apps/prod/web/source/timer", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Installed bool   `json:"installed"`
			Known     bool   `json:"known"`
			Detail    string `json:"detail"`
		} `json:"data"`
	}
	json.Unmarshal(response.Body.Bytes(), &envelope)
	if envelope.Data.Installed || !envelope.Data.Known || !strings.Contains(envelope.Data.Detail, "timer") {
		t.Fatalf("timer verdict = %+v", envelope.Data)
	}
	if call := runner.last("doctor"); call == nil || indexOfArg(call.args, "--json") < 0 {
		t.Fatalf("doctor argv = %v", call)
	}
}

func TestGitSourceRegisterWithoutCredentialReturnsPasteKit(t *testing.T) {
	s, _ := newGitSourceServer(t, nil)
	body := `{"forge":"github","repository":"git@github.com:owner/repo.git","branch":"main","webhook_base":"https://dash.example","public_key":"ssh-ed25519 AAAA key"}`
	response := serve(s, "POST", "/api/apps/prod/web/source/register", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Registered    bool   `json:"registered"`
			SourceID      string `json:"source_id"`
			WebhookURL    string `json:"webhook_url"`
			PublicKey     string `json:"public_key"`
			WebhookSecret string `json:"webhook_secret"`
		} `json:"data"`
	}
	json.Unmarshal(response.Body.Bytes(), &envelope)
	if envelope.Data.Registered || envelope.Data.PublicKey != "ssh-ed25519 AAAA key" {
		t.Fatalf("register response = %+v", envelope.Data)
	}
	if !strings.HasSuffix(envelope.Data.WebhookURL, "/hooks/sources/"+envelope.Data.SourceID) ||
		!strings.HasPrefix(envelope.Data.WebhookURL, "https://dash.example") {
		t.Fatalf("webhook url = %q", envelope.Data.WebhookURL)
	}
	secret, err := s.sources.WebhookSecret(envelope.Data.SourceID)
	if err != nil || secret == "" || secret != envelope.Data.WebhookSecret {
		t.Fatalf("paste-kit secret mismatch: %q vs %q (%v)", envelope.Data.WebhookSecret, secret, err)
	}
	if routes := s.routing.RoutesFor("https://github.com/owner/repo"); len(routes) != 1 || routes[0].App != "web" {
		t.Fatalf("route not noted by register: %+v", routes)
	}
	for _, bad := range []string{
		`{"forge":"generic","repository":"https://forge.example/owner/repo","branch":"main","webhook_base":"https://dash.example","public_key":"k"}`,
		`{"forge":"github","repository":"not a url","branch":"main","webhook_base":"https://dash.example","public_key":"k"}`,
		`{"forge":"github","repository":"https://github.com/owner/repo","branch":"","webhook_base":"https://dash.example","public_key":"k"}`,
		`{"forge":"github","repository":"https://github.com/owner/repo","branch":"main","webhook_base":"https://dash.example/hooks/x","public_key":"k"}`,
		`{"forge":"github","repository":"https://github.com/owner/repo","branch":"main","webhook_base":"ftp://dash.example","public_key":"k"}`,
		`{"forge":"github","repository":"https://github.com/owner/repo","branch":"main","webhook_base":"https://dash.example","public_key":""}`,
	} {
		if response := serve(s, "POST", "/api/apps/prod/web/source/register", bad); response.Code != http.StatusBadRequest {
			t.Fatalf("bad register body %s status=%d, want 400", bad, response.Code)
		}
	}
}

func TestGitSourceRegisterThroughForgeAPI(t *testing.T) {
	var mu sync.Mutex
	var posts []map[string]interface{}
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			json.NewEncoder(w).Encode([]interface{}{})
			return
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		posts = append(posts, body)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{"id": len(posts)})
	}))
	defer forge.Close()
	s, _ := newGitSourceServer(t, nil)
	// A dash-held credential for exactly this repository: the real
	// provider chain (private-file read, scope check, admitted origin).
	providerPath := filepath.Join(t.TempDir(), "providers.json")
	os.WriteFile(providerPath, []byte(`{"repo":{"forge":"forgejo","repository":"https://forge.example/owner/repo","api_url":`+fmt.Sprintf("%q", forge.URL)+`,"token_env":"TEST_FORGE_TOKEN","allow_private":true}}`), 0600)
	s.sourceProviders = &source.Providers{Path: providerPath}
	t.Setenv("TEST_FORGE_TOKEN", "test-token")
	body := `{"forge":"forgejo","repository":"https://forge.example/owner/repo","branch":"main","webhook_base":"https://dash.example","public_key":"ssh-ed25519 AAAA key"}`
	response := serve(s, "POST", "/api/apps/prod/web/source/register", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Registered bool   `json:"registered"`
			SourceID   string `json:"source_id"`
		} `json:"data"`
	}
	json.Unmarshal(response.Body.Bytes(), &envelope)
	if !envelope.Data.Registered {
		t.Fatalf("register response = %s", response.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	var keySeen, hookSeen bool
	for _, post := range posts {
		if _, ok := post["key"]; ok {
			keySeen = true
			if post["key"] != "ssh-ed25519 AAAA key" || post["read_only"] != true || !strings.HasPrefix(post["title"].(string), "teploy web") {
				t.Fatalf("deploy key post = %v", post)
			}
		}
		if config, ok := post["config"].(map[string]interface{}); ok {
			hookSeen = true
			if config["url"] != "https://dash.example/hooks/sources/"+envelope.Data.SourceID || config["secret"] == "" {
				t.Fatalf("webhook post = %v", post)
			}
			if post["branch_filter"] != "main" || post["type"] != "forgejo" {
				t.Fatalf("webhook post = %v", post)
			}
		}
	}
	if !keySeen || !hookSeen {
		t.Fatalf("forge posts = %+v", posts)
	}
	// The created source watches the branch while this app is its only
	// route, and re-registering widens nothing.
	if src, err := s.sources.Get(envelope.Data.SourceID); err != nil || src.DefaultBranch != "main" {
		t.Fatalf("created source = %+v (%v)", src, err)
	}
}

func TestGitSourceRegisterWidensWatchForSecondApp(t *testing.T) {
	s, _ := newGitSourceServer(t, nil)
	if err := s.routing.Link("prod", "first", "https://github.com/owner/repo"); err != nil {
		t.Fatal(err)
	}
	body := `{"forge":"github","repository":"https://github.com/owner/repo","branch":"develop","webhook_base":"https://dash.example","public_key":"k"}`
	response := serve(s, "POST", "/api/apps/prod/second/source/register", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	sources, err := s.sources.List()
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %+v (%v)", sources, err)
	}
	if sources[0].DefaultBranch != "" {
		t.Fatalf("second app kept a single-branch watch: %+v", sources[0])
	}
	if routes := s.routing.RoutesFor("https://github.com/owner/repo"); len(routes) != 2 {
		t.Fatalf("routes = %+v", routes)
	}
}

func TestGitSourceRegisterRefusedForgeAnswersBadGateway(t *testing.T) {
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer forge.Close()
	s, _ := newGitSourceServer(t, nil)
	s.sourceAccessResolver = func(ctx context.Context, src *source.Source) (*source.Access, error) {
		return &source.Access{Provider: source.Provider{Forge: src.Forge, APIURL: forge.URL}, Token: "t", Client: forge.Client()}, nil
	}
	// Without a stored credential reference the register endpoint cannot
	// resolve access, so seed one through the API first.
	id, _ := createSource(t, s, "github", "https://github.com/owner/repo", "")
	if _, err := s.sources.Update(id, source.UpdateInput{CredentialRef: strPtr("provider-a")}); err != nil {
		t.Fatal(err)
	}
	body := `{"forge":"github","repository":"https://github.com/owner/repo","branch":"main","webhook_base":"https://dash.example","public_key":"ssh-ed25519 AAAA key"}`
	response := serve(s, "POST", "/api/apps/prod/web/source/register", body)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func strPtr(v string) *string { return &v }
