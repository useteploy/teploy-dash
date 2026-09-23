package mcp

// caps_test.go — X03: MCP tokens carry an explicit capability set at mint;
// every tool declares the capability it requires and the handler enforces it
// (listing hides unauthorized tools; calls are refused with the missing
// capability named). Legacy tokens (pre-X03 files, no capabilities field)
// keep exactly their old read_only-derived behavior.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/caps"
)

// toolCall runs one tools/call through the real HTTP handler.
func toolCall(t *testing.T, url, token, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	return rpc(t, url, token, "tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	})
}

func isErrorResult(t *testing.T, out map[string]interface{}) (bool, string) {
	t.Helper()
	result, ok := out["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("no result object: %v", out)
	}
	text := ""
	if content, ok := result["content"].([]interface{}); ok && len(content) > 0 {
		if c, ok := content[0].(map[string]interface{}); ok {
			text, _ = c["text"].(string)
		}
	}
	return result["isError"] == true, text
}

func TestTokenMintDefaultsToOperatorMinusSecrets(t *testing.T) {
	store, err := NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := store.Create("agent", false)
	if err != nil {
		t.Fatal(err)
	}
	// Default = the operator preset, which excludes reveal.secrets (no MCP
	// tool reveals secret values anyway), administer.*, and restore.data.
	want := caps.OperatorDefault().Sorted()
	if strings.Join(tok.Capabilities, ",") != strings.Join(want, ",") {
		t.Fatalf("minted capabilities = %v, want %v", tok.Capabilities, want)
	}
	_, ro, err := store.Create("reader", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ro.Capabilities, ",") != caps.ViewMetadata {
		t.Fatalf("read-only mint capabilities = %v, want [view.metadata]", ro.Capabilities)
	}
}

func TestCreateWithCapabilitiesValidates(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateWithCapabilities("empty", nil); err == nil {
		t.Fatal("empty capability set accepted — it would round-trip to the legacy full set")
	}
	if _, _, err := store.CreateWithCapabilities("bad", []string{"view.metadata", "execute.everything"}); err == nil {
		t.Fatal("unknown capability accepted")
	}
	plain, tok, err := store.CreateWithCapabilities("scoped", []string{caps.ViewMetadata, caps.RevealSecrets})
	if err != nil {
		t.Fatal(err)
	}
	if !tok.CapabilitySet().Allow(caps.RevealSecrets) || tok.CapabilitySet().Allow(caps.ExecuteDeploy) {
		t.Fatalf("scoped token set wrong: %v", tok.Capabilities)
	}
	// Persistence: the explicit set round-trips.
	store2, err := NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := store2.Verify(plain); !ok || !got.CapabilitySet().Allow(caps.RevealSecrets) {
		t.Fatal("explicit capabilities lost across reload")
	}
}

func TestToolEnforcementNamesMissingCapability(t *testing.T) {
	store, err := NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	viewerPlain, _, err := store.Create("viewer", true)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	h := httptest.NewServer(NewHandler(store, Tools(b), "test"))
	defer h.Close()

	errResult, text := isErrorResult(t, toolCall(t, h.URL, viewerPlain, "teploy_deploy", map[string]interface{}{
		"server": "s1", "app": "web", "image": "nginx:latest",
	}))
	if !errResult || !strings.Contains(text, caps.ExecuteDeploy) {
		t.Fatalf("viewer-default deploy = (%v, %q), want error naming execute.deploy", errResult, text)
	}
	errResult, text = isErrorResult(t, toolCall(t, h.URL, viewerPlain, "teploy_app_logs", map[string]interface{}{
		"server": "s1", "app": "web",
	}))
	if !errResult || !strings.Contains(text, caps.ViewLogs) {
		t.Fatalf("viewer-default app_logs = (%v, %q), want error naming view.logs", errResult, text)
	}
	if len(b.calls) != 0 {
		t.Fatalf("backend reached without capability: %v", b.calls)
	}
}

func TestDefaultTokenDeploysMutatesAndReadsLogs(t *testing.T) {
	store, err := NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := store.Create("agent", false)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	h := httptest.NewServer(NewHandler(store, Tools(b), "test"))
	defer h.Close()

	for _, call := range []struct {
		name string
		args map[string]interface{}
	}{
		{"teploy_deploy", map[string]interface{}{"server": "s1", "app": "web", "image": "nginx:latest"}},
		{"teploy_set_env", map[string]interface{}{"server": "s1", "app": "web", "key": "K", "value": "V"}},
		{"teploy_app_logs", map[string]interface{}{"server": "s1", "app": "web"}},
	} {
		errResult, text := isErrorResult(t, toolCall(t, h.URL, plain, call.name, call.args))
		if errResult {
			t.Fatalf("default token %s refused: %s", call.name, text)
		}
	}
}

// Pre-X03 token files (no capabilities field) keep their read_only-derived
// behavior: a full token could use the entire MCP surface, a read-only token
// only the read tools.
func TestLegacyTokensKeepExactPriorBehavior(t *testing.T) {
	dir := t.TempDir()
	legacy := `[` +
		`{"id":"aaa111","name":"old-full","hash":"` + hashToken("tpd_oldfull") + `","read_only":false,"created_at":"2026-01-01T00:00:00Z"},` +
		`{"id":"bbb222","name":"old-ro","hash":"` + hashToken("tpd_oldro") + `","read_only":true,"created_at":"2026-01-01T00:00:00Z"}` +
		`]`
	if err := os.WriteFile(filepath.Join(dir, "mcp-tokens.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	h := httptest.NewServer(NewHandler(store, Tools(b), "test"))
	defer h.Close()

	if errResult, text := isErrorResult(t, toolCall(t, h.URL, "tpd_oldfull", "teploy_deploy", map[string]interface{}{
		"server": "s1", "app": "web", "image": "nginx:latest",
	})); errResult {
		t.Fatalf("legacy full token deploy refused: %s", text)
	}
	if errResult, text := isErrorResult(t, toolCall(t, h.URL, "tpd_oldfull", "teploy_set_env", map[string]interface{}{
		"server": "s1", "app": "web", "key": "K", "value": "V",
	})); errResult {
		t.Fatalf("legacy full token set_env refused: %s", text)
	}
	errResult, _ := isErrorResult(t, toolCall(t, h.URL, "tpd_oldro", "teploy_rollback", map[string]interface{}{
		"server": "s1", "app": "web",
	}))
	if !errResult {
		t.Fatal("legacy read-only token allowed a mutation")
	}
}

func TestToolsListHidesUnauthorizedTools(t *testing.T) {
	store, err := NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := store.CreateWithCapabilities("meta", []string{caps.ViewMetadata})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(NewHandler(store, Tools(&fakeBackend{}), "test"))
	defer h.Close()

	list := rpc(t, h.URL, plain, "tools/list", nil)["result"].(map[string]interface{})["tools"].([]interface{})
	if len(list) == 0 {
		t.Fatal("metadata token sees no tools")
	}
	for _, tl := range list {
		name := tl.(map[string]interface{})["name"].(string)
		if name != "teploy_list_apps" && name != "teploy_get_app" && name != "teploy_list_servers" &&
			name != "teploy_list_monitors" && name != "teploy_list_env_keys" {
			t.Errorf("metadata-only token sees mutating/log tool %q", name)
		}
	}
}

// mintedTokenView checks the management API shape indirectly: the token
// record's capabilities survive the JSON round trip.
func TestTokenCapabilitiesRoundTrip(t *testing.T) {
	store, err := NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := store.CreateWithCapabilities("scoped", []string{caps.ViewMetadata, caps.ViewLogs})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store.Verify(plain)
	if !ok {
		t.Fatal("verify failed")
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "view.logs") {
		t.Fatalf("capabilities missing from token JSON: %s", data)
	}
}
