package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/store"
)

func statusTestServer(t *testing.T, public bool) *Server {
	t.Helper()
	st := store.NewFileStore(t.TempDir())

	mustSave := func(m store.Monitor) {
		if err := st.SaveMonitor(m); err != nil {
			t.Fatalf("SaveMonitor: %v", err)
		}
	}
	mustSave(store.Monitor{ID: "web", Name: "Web", Type: "http", Target: "https://internal.example.com/health", Enabled: true})
	mustSave(store.Monitor{ID: "api", Name: "API", Type: "http", Target: "http://10.0.0.5:8080", Enabled: true})
	mustSave(store.Monitor{ID: "hidden", Name: "Hidden", Type: "tcp", Target: "10.0.0.9:5432", Enabled: false})

	now := time.Now()
	if err := st.SaveCheck(store.CheckResult{MonitorID: "web", Status: "up", CheckedAt: now}); err != nil {
		t.Fatalf("SaveCheck: %v", err)
	}
	if err := st.SaveCheck(store.CheckResult{MonitorID: "api", Status: "down", CheckedAt: now}); err != nil {
		t.Fatalf("SaveCheck: %v", err)
	}

	return &Server{config: Config{PublicStatus: public}, store: st}
}

func TestStatusAPI_DisabledReturns404(t *testing.T) {
	s := statusTestServer(t, false)
	w := httptest.NewRecorder()
	s.handleStatusAPI(w, httptest.NewRequest("GET", "/api/status", nil))
	if w.Code != 404 {
		t.Fatalf("expected 404 when disabled, got %d", w.Code)
	}
}

func TestStatusAPI_EnabledOnlyPublicFields(t *testing.T) {
	s := statusTestServer(t, true)
	w := httptest.NewRecorder()
	s.handleStatusAPI(w, httptest.NewRequest("GET", "/api/status", nil))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// The raw body must not leak targets/IPs even though the monitors have them.
	body := w.Body.String()
	for _, leak := range []string{"internal.example.com", "10.0.0.5", "10.0.0.9", "target", "Hidden"} {
		if strings.Contains(body, leak) {
			t.Errorf("public status leaked %q: %s", leak, body)
		}
	}

	var resp statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Monitors) != 2 {
		t.Fatalf("expected 2 enabled monitors, got %d", len(resp.Monitors))
	}
	// One up + one down => degraded overall.
	if resp.Status != "degraded" {
		t.Errorf("expected degraded, got %q", resp.Status)
	}

	byName := map[string]statusMonitor{}
	for _, m := range resp.Monitors {
		byName[m.Name] = m
	}
	if byName["Web"].Status != "up" {
		t.Errorf("Web should be up, got %q", byName["Web"].Status)
	}
	if byName["API"].Status != "down" {
		t.Errorf("API should be down, got %q", byName["API"].Status)
	}
}

func TestStatusPage_Toggle(t *testing.T) {
	off := statusTestServer(t, false)
	w := httptest.NewRecorder()
	off.handleStatusPage(w, httptest.NewRequest("GET", "/status", nil))
	if w.Code != 404 {
		t.Fatalf("disabled status page should 404, got %d", w.Code)
	}

	on := statusTestServer(t, true)
	w = httptest.NewRecorder()
	on.handleStatusPage(w, httptest.NewRequest("GET", "/status", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Service status") {
		t.Fatalf("enabled status page should render, got %d", w.Code)
	}
}

func TestAppNamePatternMatchesCLIAppNames(t *testing.T) {
	for _, name := range []string{"api", "my-app", "my_app", "my.app", "release-1.2"} {
		if !validAppName(name) {
			t.Errorf("expected %q to be a valid app name", name)
		}
	}
	for _, name := range []string{"", ".", "..", "../app", "my app", "app;rm"} {
		if validAppName(name) {
			t.Errorf("expected %q to be rejected", name)
		}
	}
}
