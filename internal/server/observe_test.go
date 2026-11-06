package server

// observe_test.go — Programme B items 2-4: the Observe connect flow against
// a mock Observe (discovery doc + service API), the never-guess rules, and
// the token's secrecy in every stored and answered record.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/useteploy/teploy-dash/internal/operation"
)

const observeDoc = `{"product":"observe","version":"0.2.0","instance_id":"9f2c0c0e6b6d4f5a8e1d3b7a2c4f6e80","dashboard_url":"https://observe.example.org","ingest_url":"https://ingest.example.org","capabilities":["service_tokens","sites_external_ref","otlp_http"]}`

// mockObserve is the Observe surface dash talks to: the unauthenticated
// discovery doc and the service API's alerts read (with per-token verdicts).
type mockObserve struct {
	*httptest.Server
	docBody      atomic.Value // string
	tokenVerdict atomic.Value // string: "ok" | "refused" | "busy"
	siteCalls    atomic.Int32
	keyMints     atomic.Int32
	siteCreated  atomic.Value // string JSON of last create-site body
	lastAuth     atomic.Value // string
}

func newMockObserve(t *testing.T) *mockObserve {
	t.Helper()
	m := &mockObserve{}
	m.docBody.Store(observeDoc)
	m.tokenVerdict.Store("ok")
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/teploy.json":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(m.docBody.Load().(string)))
		case r.URL.Path == "/api/v1/service/sites" && r.Method == http.MethodPost:
			m.lastAuth.Store(r.Header.Get("Authorization"))
			var body struct {
				ExternalRef string `json:"external_ref"`
				Name        string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			raw, _ := json.Marshal(body)
			m.siteCreated.Store(string(raw))
			m.siteCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"created":true,"site":{"site_id":"st_123","external_ref":"` + body.ExternalRef + `","managed_by":"teploy-dash"}}`))
		case r.URL.Path == "/api/v1/service/sites/st_123/keys" && r.Method == http.MethodPost:
			m.lastAuth.Store(r.Header.Get("Authorization"))
			m.keyMints.Add(1)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"key":"obs_live_secret_key","info":{"key_id":"k_1","site_id":"st_123","label":"dash-telemetry"}}`))
		case strings.Contains(r.URL.Path, "/alerts/rules"):
			m.lastAuth.Store(r.Header.Get("Authorization"))
			switch m.tokenVerdict.Load().(string) {
			case "refused":
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"invalid or revoked service token"}`))
			case "busy":
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`{"error":"authentication temporarily unavailable"}`))
			default:
				// A random probe site is never this manager's: the honest
				// answer is 404, which still proves the token authenticated.
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"site not found"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.Close)
	return m
}

// observeTestServer bundles the server with its runner so tests can read
// back every CLI call (argv + stdin).
type observeTestServer struct {
	*Server
	runner *configRunner
}

// observeCLIWrites calls fn for every CLI invocation the test runner saw.
func (o *observeTestServer) observeCLIWrites(fn func(args []string, stdin string)) {
	o.runner.mu.Lock()
	defer o.runner.mu.Unlock()
	for _, c := range o.runner.calls {
		fn(c.args, c.stdin)
	}
}

func newObserveTestServer(t *testing.T, mock *mockObserve) *observeTestServer {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	runner := &configRunner{tokens: []string{"siblings-v1", "secret-stdin-v1", "redeploy-v1"}}
	s := New(Config{
		DataDir:         t.TempDir(),
		NoAuth:          true,
		CLIInstalled:    func() bool { return true },
		CLIRunner:       runner.run,
		CLIStdinRunner:  runner.runStdin,
		CLIIdentity:     func() string { return "test-cli" },
		EnvStdinSupport: func() (bool, error) { return true, nil },
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: "192.0.2.10", User: "deploy"}, nil
		},
		OperationExecutor: func(_ context.Context, command operation.Command, emit func(operation.Stream, string)) (int, error) {
			return 0, nil
		},
	})
	t.Cleanup(func() { s.DrainOperations(context.Background()) })
	_ = mock
	return &observeTestServer{Server: s, runner: runner}
}

// connectedObserve returns a server already connected to the mock.
func connectedObserve(t *testing.T, mock *mockObserve) *observeTestServer {
	t.Helper()
	s := newObserveTestServer(t, mock)
	body := `{"base_url":"` + mock.URL + `","token":"obs_svc_live_token_value","confirm_instance_id":"9f2c0c0e6b6d4f5a8e1d3b7a2c4f6e80","allow_internal":true}`
	if rec := postObserve(t, s.Server, "/api/observe/connect", body); rec.Code != http.StatusOK {
		t.Fatalf("connect = %d: %s", rec.Code, rec.Body.String())
	}
	return s
}

func postObserve(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func deleteObserve(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))
	return rec
}

func observeBody(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/observe", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/observe = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Data
}

func TestObserveConnectFullFlow(t *testing.T) {
	mock := newMockObserve(t)
	s := newObserveTestServer(t, mock)

	// Preview: the doc answers before any token moves.
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/observe/preview?url="+mock.URL+"&allow_internal=true", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "9f2c0c0e6b6d4f5a8e1d3b7a2c4f6e80") {
		t.Fatalf("preview = %d: %s", rec.Code, rec.Body.String())
	}

	// Connect: wrong instance confirmation refuses.
	body := `{"base_url":"` + mock.URL + `","token":"obs_svc_live_token_value","confirm_instance_id":"deadbeef","allow_internal":true}`
	if rec := postObserve(t, s.Server, "/api/observe/connect", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched confirm = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.observe.connection(); ok {
		t.Fatal("a mismatched confirmation must store nothing")
	}

	// Connect: the confirmed id matches the doc.
	body = `{"base_url":"` + mock.URL + `","token":"obs_svc_live_token_value","confirm_instance_id":"9f2c0c0e6b6d4f5a8e1d3b7a2c4f6e80","allow_internal":true}`
	rec = postObserve(t, s.Server, "/api/observe/connect", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "obs_svc_live_token_value") {
		t.Fatal("the connect response must never carry the token")
	}
	if !strings.Contains(rec.Body.String(), "token_fp") {
		t.Fatal("the response carries the fingerprint instead")
	}

	// Stored at ~/.teploy/observe.json, 0600, token present only there.
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".teploy", "observe.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the connection must persist: %v", err)
	}
	if strings.Contains(string(raw), "obs_svc_live_token_value") != true {
		t.Fatal("the token must persist (it needs reuse) — only in this 0600 file")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("observe.json mode = %v, want 0600", info.Mode().Perm())
	}

	// The view shows the connection, never the token.
	view := observeBody(t, s.Server)
	if view["connected"] != true {
		t.Fatalf("view = %v", view)
	}
	conn := view["connection"].(map[string]any)
	if conn["instance_id"] != "9f2c0c0e6b6d4f5a8e1d3b7a2c4f6e80" || conn["base_url"] != mock.URL {
		t.Fatalf("connection view = %v", conn)
	}

	// The verification read hit the service API with the Bearer token.
	if got := mock.lastAuth.Load(); got != "Bearer obs_svc_live_token_value" {
		t.Fatalf("verify Authorization = %v", got)
	}

	// Disconnect forgets it and says where to revoke.
	if rec := deleteObserve(t, s.Server, "/api/observe/connection"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Revoke") {
		t.Fatalf("disconnect = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.observe.connection(); ok {
		t.Fatal("disconnect must clear the connection")
	}
	if view := observeBody(t, s.Server); view["connected"] != false {
		t.Fatal("view after disconnect must read disconnected")
	}
}

func TestObserveConnectRefusals(t *testing.T) {
	mock := newMockObserve(t)
	s := newObserveTestServer(t, mock)
	const confirm = `"confirm_instance_id":"9f2c0c0e6b6d4f5a8e1d3b7a2c4f6e80","allow_internal":true}`

	cases := []struct {
		name, body, want string
	}{
		{"refused token", `{"base_url":"` + mock.URL + `","token":"obs_svc_revoked"` + "," + confirm, "refused"},
		{"not a service token", `{"base_url":"` + mock.URL + `","token":"jwt-or-junk",` + confirm, "obs_svc_"},
		{"bad url", `{"base_url":"ftp://nope","token":"obs_svc_x",` + confirm, "base URL"},
		{"no confirm", `{"base_url":"` + mock.URL + `","token":"obs_svc_x","allow_internal":true}`, "confirm"},
	}
	for _, tc := range cases {
		mock.tokenVerdict.Store("ok")
		if tc.name == "refused token" {
			mock.tokenVerdict.Store("refused")
		}
		rec := postObserve(t, s.Server, "/api/observe/connect", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d: %s", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s message = %s, want it to mention %q", tc.name, rec.Body.String(), tc.want)
		}
		if strings.Contains(rec.Body.String(), "obs_svc_revoked") {
			t.Fatalf("%s leaked the token value: %s", tc.name, rec.Body.String())
		}
		if _, ok := s.observe.connection(); ok {
			t.Fatalf("%s must not store a connection", tc.name)
		}
	}
}

// No discovery doc (a Ship-style 404, or a plain web server): the connect
// refuses — the instance is never guessed from a label or a URL.
func TestObserveConnectWithoutDiscoveryDocRefuses(t *testing.T) {
	none := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(none.Close)
	s := newObserveTestServer(t, nil)
	body := `{"base_url":"` + none.URL + `","token":"obs_svc_x","confirm_instance_id":"whatever","allow_internal":true}`
	if rec := postObserve(t, s.Server, "/api/observe/connect", body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "discovery") {
		t.Fatalf("no-doc connect = %d: %s", rec.Code, rec.Body.String())
	}
}

// The instance fence: a doc that answers with a DIFFERENT instance id (a
// restored copy behind the same URL) refuses writes until reconnect.
func TestObserveInstanceFence(t *testing.T) {
	mock := newMockObserve(t)
	s := newObserveTestServer(t, mock)
	body := `{"base_url":"` + mock.URL + `","token":"obs_svc_live_token_value","confirm_instance_id":"9f2c0c0e6b6d4f5a8e1d3b7a2c4f6e80","allow_internal":true}`
	if rec := postObserve(t, s.Server, "/api/observe/connect", body); rec.Code != http.StatusOK {
		t.Fatalf("connect = %d: %s", rec.Code, rec.Body.String())
	}
	mock.docBody.Store(`{"product":"observe","version":"0.2.0","instance_id":"00000000000000000000000000000000","dashboard_url":"https://observe.example.org"}`)
	conn, _ := s.observe.connection()
	if _, err := observeDocStillCurrent(context.Background(), &conn); err == nil {
		t.Fatal("a changed instance id must refuse further writes")
	}
}

func TestScrubSecretRemovesTokenFromErrors(t *testing.T) {
	out := scrubSecret("POST https://x/keys failed with obs_svc_live_token_value in flight", "obs_svc_live_token_value")
	if strings.Contains(out, "obs_svc_live_token_value") {
		t.Fatalf("scrubbed text still carries the token: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("scrubbed text = %s", out)
	}
}

// The connect surface is version-gated on the siblings capability like the
// rest of Programme B.
func TestObserveConnectGatedOnSiblingsCapability(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &configRunner{tokens: []string{}} // no siblings-v1
	_ = runner
	s := New(Config{DataDir: t.TempDir(), NoAuth: true, CLIRunner: runner.run})
	rec := postObserve(t, s, "/api/observe/connect", `{"base_url":"http://x","token":"obs_svc_x","confirm_instance_id":"y"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("gated connect = %d, want 501: %s", rec.Code, rec.Body.String())
	}
}

// ── per-app telemetry (plan item 4) ──────────────────────────────────────────

func getJSONObserve(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var out map[string]any
	if rec.Body.Len() > 0 {
		var env struct {
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &env) == nil && env.Data != nil {
			out = env.Data
		}
	}
	return rec.Code, out
}

func TestObserveTelemetryPlanFirstThenApply(t *testing.T) {
	mock := newMockObserve(t)
	s := connectedObserve(t, mock)

	// Plan: no side effects — no Observe site call, no CLI write.
	code, plan := getJSONObserve(t, s.Server, "/api/observe/telemetry/plan?server=prod&app=web")
	if code != http.StatusOK {
		t.Fatalf("plan = %d: %v", code, plan)
	}
	if plan["external_ref"] != "dash/prod/web" || plan["ingest_url"] != "https://ingest.example.org" {
		t.Fatalf("plan = %v", plan)
	}
	env := plan["env"].([]any)
	if len(env) != 7 {
		t.Fatalf("plan env writes = %d, want 7", len(env))
	}
	if fp, _ := plan["fingerprint"].(string); fp == "" {
		t.Fatal("plan must carry a fingerprint")
	}
	if mock.siteCalls.Load() != 0 {
		t.Fatal("the plan must not touch Observe")
	}

	// Apply with a stale fingerprint refuses before any write.
	stale := `{"server":"prod","app":"web","fingerprint":"00000000000000000000000000000000"}`
	if rec := postObserve(t, s.Server, "/api/observe/telemetry", stale); rec.Code != http.StatusConflict {
		t.Fatalf("stale fingerprint = %d: %s", rec.Code, rec.Body.String())
	}
	if mock.siteCalls.Load() != 0 {
		t.Fatal("a refused apply must write nothing")
	}

	// Apply with the plan's fingerprint: site ensured, key minted, env and
	// secrets written via the CLI, redeploy queued.
	apply := `{"server":"prod","app":"web","fingerprint":"` + plan["fingerprint"].(string) + `"}`
	rec := postObserve(t, s.Server, "/api/observe/telemetry", apply)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "obs_live_secret_key") {
		t.Fatal("the minted telemetry key must never appear in a response")
	}
	if mock.siteCalls.Load() != 1 || mock.keyMints.Load() != 1 {
		t.Fatalf("site calls = %d, key mints = %d", mock.siteCalls.Load(), mock.keyMints.Load())
	}
	if got := mock.siteCreated.Load(); got != `{"external_ref":"dash/prod/web","name":"web"}` {
		t.Fatalf("site create body = %v", got)
	}

	// The CLI wrote 5 env vars (values on stdin) and 2 secrets.
	var envSets, secretSets int
	s.observeCLIWrites(func(args []string, stdin string) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "env set "):
			envSets++
		case strings.HasPrefix(joined, "secret set "):
			secretSets++
			if !strings.Contains(stdin, "obs_live_secret_key") {
				t.Fatalf("secret value must ride stdin, got %q for %v", stdin, args)
			}
		}
	})
	if envSets != 5 || secretSets != 2 {
		t.Fatalf("env writes = %d, secret writes = %d, want 5 and 2", envSets, secretSets)
	}

	// The redeploy was queued as a durable operation.
	var out struct {
		Data struct {
			Operation operation.Operation `json:"operation"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding the apply response: %v", err)
	}
	if out.Data.Operation.ID == "" || out.Data.Operation.Request.Kind != operation.KindRedeploy {
		t.Fatalf("apply must return the redeploy operation: %s", rec.Body.String())
	}

	// The stored record names the site and key id, never the key.
	home, _ := os.UserHomeDir()
	raw, _ := os.ReadFile(filepath.Join(home, ".teploy", "observe.json"))
	if strings.Contains(string(raw), "obs_live_secret_key") {
		t.Fatal("the telemetry key must not be stored")
	}
	if !strings.Contains(string(raw), "st_123") || !strings.Contains(string(raw), "k_1") {
		t.Fatalf("the record must name site and key ids: %s", raw)
	}

	// The view lists the opt-in.
	if _, view := getJSONObserve(t, s.Server, "/api/observe"); code != http.StatusOK || len(view["apps"].([]any)) != 1 {
		t.Fatalf("view apps = %v", view["apps"])
	}

	// Disable: exactly the recorded keys go, a redeploy is queued, the
	// record clears.
	rec = postObserve(t, s.Server, "/api/observe/telemetry/disable", `{"server":"prod","app":"web"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", rec.Code, rec.Body.String())
	}
	var unsets, secretRms int
	s.observeCLIWrites(func(args []string, stdin string) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "env unset ") {
			unsets++
		}
		if strings.HasPrefix(joined, "secret rm ") {
			secretRms++
		}
	})
	if unsets != 5 || secretRms != 2 {
		t.Fatalf("disable env unsets = %d, secret rm = %d, want 5 and 2", unsets, secretRms)
	}
	_, view := getJSONObserve(t, s.Server, "/api/observe")
	if len(view["apps"].([]any)) != 0 {
		t.Fatalf("opt-in must be gone after disable: %v", view["apps"])
	}
}

// A doc with no ingest URL (separate listener, OBSERVE_INGEST_URL unset)
// refuses the plan — the ingest address is never guessed.
func TestObserveTelemetryPlanNeedsIngestURL(t *testing.T) {
	mock := newMockObserve(t)
	mock.docBody.Store(`{"product":"observe","version":"0.2.0","instance_id":"9f2c0c0e6b6d4f5a8e1d3b7a2c4f6e80","dashboard_url":"https://observe.example.org"}`)
	s := connectedObserve(t, mock)
	rec := httptest.NewRecorder()
	s.Server.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/observe/telemetry/plan?server=prod&app=web", nil))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "ingest") {
		t.Fatalf("no-ingest plan = %d: %s", rec.Code, rec.Body.String())
	}
}

// Re-applying an opted-in app is a rotation: the plan says so, and the new
// apply replaces the record.
func TestObserveTelemetryRotation(t *testing.T) {
	mock := newMockObserve(t)
	s := connectedObserve(t, mock)
	_, plan := getJSONObserve(t, s.Server, "/api/observe/telemetry/plan?server=prod&app=web")
	if plan["rotation"] != nil {
		t.Fatal("first apply must not read as a rotation")
	}
	apply := `{"server":"prod","app":"web","fingerprint":"` + plan["fingerprint"].(string) + `"}`
	if rec := postObserve(t, s.Server, "/api/observe/telemetry", apply); rec.Code != http.StatusOK {
		t.Fatalf("apply = %d: %s", rec.Code, rec.Body.String())
	}
	_, plan2 := getJSONObserve(t, s.Server, "/api/observe/telemetry/plan?server=prod&app=web")
	rot := plan2["rotation"]
	if rot == nil {
		t.Fatal("a second plan must carry the rotation note")
	}
	if plan2["fingerprint"] == plan["fingerprint"] {
		t.Fatal("the rotation plan must have its own fingerprint")
	}
}
