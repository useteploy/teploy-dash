package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── Token store ──────────────────────────────────────────────────────────

func TestTokenLifecycle(t *testing.T) {
	dir := t.TempDir()
	s, err := NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	plain, tok, err := s.Create("ci-bot", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "tpd_") {
		t.Fatalf("token missing prefix: %s", plain)
	}
	if tok.Hash == plain || strings.Contains(tok.Hash, plain) {
		t.Fatal("plaintext must not be stored")
	}

	got, ok := s.Verify(plain)
	if !ok || got.Name != "ci-bot" {
		t.Fatalf("verify failed: %v %v", got, ok)
	}
	if _, ok := s.Verify("tpd_wrong"); ok {
		t.Fatal("wrong token verified")
	}

	// Persistence across reload.
	s2, err := NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Verify(plain); !ok {
		t.Fatal("token lost after reload")
	}

	if err := s2.Delete(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Verify(plain); ok {
		t.Fatal("deleted token still verifies")
	}
}

// ── Protocol ─────────────────────────────────────────────────────────────

type fakeBackend struct{ calls []string }

func (f *fakeBackend) rec(s string) (string, error) {
	f.calls = append(f.calls, s)
	return "ok:" + s, nil
}

func (f *fakeBackend) ListApps(ctx context.Context) (string, error) { return f.rec("list_apps") }
func (f *fakeBackend) GetApp(ctx context.Context, server, app string) (string, error) {
	return f.rec("get_app " + server + "/" + app)
}
func (f *fakeBackend) AppLogs(ctx context.Context, server, app string, lines int) (string, error) {
	return f.rec(fmt.Sprintf("logs %s/%s %d", server, app, lines))
}
func (f *fakeBackend) ListServers(ctx context.Context) (string, error) { return f.rec("list_servers") }
func (f *fakeBackend) ListMonitors(ctx context.Context) (string, error) {
	return f.rec("list_monitors")
}
func (f *fakeBackend) ListEnvKeys(ctx context.Context, server, app string) (string, error) {
	return f.rec("env_keys " + server + "/" + app)
}
func (f *fakeBackend) Deploy(ctx context.Context, server, app, image, domain string, port int) (string, error) {
	return f.rec("deploy " + app + " " + image)
}
func (f *fakeBackend) Rollback(ctx context.Context, server, app string) (string, error) {
	return f.rec("rollback " + app)
}
func (f *fakeBackend) AppAction(ctx context.Context, server, app, action string) (string, error) {
	return f.rec("action " + action + " " + app)
}
func (f *fakeBackend) SetEnv(ctx context.Context, server, app, key, value string) (string, error) {
	return f.rec("set_env " + key)
}
func (f *fakeBackend) UnsetEnv(ctx context.Context, server, app, key string) (string, error) {
	return f.rec("unset_env " + key)
}

func testServer(t *testing.T) (*httptest.Server, string, string, *fakeBackend) {
	t.Helper()
	store, err := NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := store.Create("full", false)
	if err != nil {
		t.Fatal(err)
	}
	ro, _, err := store.Create("reader", true)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	h := NewHandler(store, Tools(b), "test")
	return httptest.NewServer(h), full, ro, b
}

func rpc(t *testing.T, url, token, method string, params interface{}) map[string]interface{} {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d for %s", resp.StatusCode, method)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUnauthorized(t *testing.T) {
	srv, _, _, _ := testServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("expected WWW-Authenticate header")
	}
}

func TestGetMethodNotAllowed(t *testing.T) {
	srv, _, _, _ := testServer(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("expected 405 on GET, got %d", resp.StatusCode)
	}
}

func TestInitializeAndToolsList(t *testing.T) {
	srv, full, ro, _ := testServer(t)
	defer srv.Close()

	out := rpc(t, srv.URL, full, "initialize", map[string]interface{}{"protocolVersion": "2025-06-18"})
	result := out["result"].(map[string]interface{})
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocol echo failed: %v", result)
	}

	fullList := rpc(t, srv.URL, full, "tools/list", nil)["result"].(map[string]interface{})["tools"].([]interface{})
	roList := rpc(t, srv.URL, ro, "tools/list", nil)["result"].(map[string]interface{})["tools"].([]interface{})
	if len(roList) >= len(fullList) {
		t.Fatalf("read-only list (%d) should be smaller than full (%d)", len(roList), len(fullList))
	}
	for _, tl := range roList {
		ann := tl.(map[string]interface{})["annotations"].(map[string]interface{})
		if ann["readOnlyHint"] != true {
			t.Fatalf("read-only token saw mutating tool: %v", tl)
		}
	}
}

func TestToolCallAndReadOnlyEnforcement(t *testing.T) {
	srv, full, ro, b := testServer(t)
	defer srv.Close()

	out := rpc(t, srv.URL, full, "tools/call", map[string]interface{}{
		"name":      "teploy_rollback",
		"arguments": map[string]interface{}{"server": "s1", "app": "web"},
	})
	result := out["result"].(map[string]interface{})
	if result["isError"] == true {
		t.Fatalf("rollback call errored: %v", result)
	}
	if len(b.calls) != 1 || b.calls[0] != "rollback web" {
		t.Fatalf("backend not called correctly: %v", b.calls)
	}

	// A read-only token calling a mutating tool must be denied WITHOUT
	// reaching the backend.
	out = rpc(t, srv.URL, ro, "tools/call", map[string]interface{}{
		"name":      "teploy_rollback",
		"arguments": map[string]interface{}{"server": "s1", "app": "web"},
	})
	result = out["result"].(map[string]interface{})
	if result["isError"] != true {
		t.Fatalf("read-only mutation should be an error result: %v", result)
	}
	if len(b.calls) != 1 {
		t.Fatalf("backend must not have been called again: %v", b.calls)
	}

	// Read tools work for read-only tokens.
	out = rpc(t, srv.URL, ro, "tools/call", map[string]interface{}{"name": "teploy_list_apps"})
	if out["result"].(map[string]interface{})["isError"] == true {
		t.Fatalf("read tool failed for read-only token")
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	srv, full, _, _ := testServer(t)
	defer srv.Close()

	out := rpc(t, srv.URL, full, "tools/call", map[string]interface{}{"name": "nope"})
	if out["result"].(map[string]interface{})["isError"] != true {
		t.Fatal("unknown tool should be an isError result")
	}

	out = rpc(t, srv.URL, full, "wat/method", nil)
	if out["error"] == nil {
		t.Fatal("unknown method should be a protocol error")
	}
}

func TestNotificationAccepted(t *testing.T) {
	srv, full, _, _ := testServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+full)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("expected 202 for notification, got %d", resp.StatusCode)
	}
}

func TestMissingArgs(t *testing.T) {
	srv, full, _, _ := testServer(t)
	defer srv.Close()

	out := rpc(t, srv.URL, full, "tools/call", map[string]interface{}{
		"name":      "teploy_get_app",
		"arguments": map[string]interface{}{"server": "s1"},
	})
	result := out["result"].(map[string]interface{})
	if result["isError"] != true {
		t.Fatal("missing app arg should be an error result")
	}
	text := result["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "app") {
		t.Fatalf("error should name the missing arg: %s", text)
	}
}
