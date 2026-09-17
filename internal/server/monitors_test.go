package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/store"
)

// monitorsTestServer builds a Server with a file store for monitor-route tests.
func monitorsTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{store: store.NewFileStore(t.TempDir())}
}

func postMonitor(t *testing.T, s *Server, role string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/monitors", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req = withUser(req, &sessionInfo{user: "u", role: role})
	}
	w := httptest.NewRecorder()
	s.handleMonitors(w, req)
	return w
}

// A13: enabling internal-network monitoring is an admin-only choice.
func TestMonitorAllowInternalRequiresAdmin(t *testing.T) {
	s := monitorsTestServer(t)

	internal := `{"id":"m1","name":"M1","type":"http","target":"http://10.0.0.5/up","interval":60000000000,"timeout":10000000000,"allow_internal":true}`
	if w := postMonitor(t, s, RoleEditor, internal); w.Code != 403 {
		t.Fatalf("editor enabling allow_internal: got %d, want 403", w.Code)
	}
	if w := postMonitor(t, s, RoleAdmin, internal); w.Code != 200 {
		t.Fatalf("admin enabling allow_internal: got %d, want 200 (%s)", w.Code, w.Body.String())
	}

	// An editor must not be able to flip the flag off either.
	flip := `{"id":"m1","name":"M1","type":"http","target":"http://10.0.0.5/up","interval":60000000000,"timeout":10000000000,"allow_internal":false}`
	if w := postMonitor(t, s, RoleEditor, flip); w.Code != 403 {
		t.Fatalf("editor disabling allow_internal: got %d, want 403", w.Code)
	}
	if w := postMonitor(t, s, RoleAdmin, flip); w.Code != 200 {
		t.Fatalf("admin disabling allow_internal: got %d, want 200", w.Code)
	}
}

// A14: boundary validation rejects out-of-range bounds and bare ping targets,
// and normalizes methods.
func TestMonitorValidationBounds(t *testing.T) {
	s := monitorsTestServer(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"interval too fast", `{"id":"x","type":"http","target":"https://example.com","interval":1000000000,"timeout":1000000000}`, 400},
		{"interval too slow", `{"id":"x","type":"http","target":"https://example.com","interval":90000000000000,"timeout":1000000000}`, 400},
		{"timeout exceeds interval", `{"id":"x","type":"http","target":"https://example.com","interval":5000000000,"timeout":6000000000}`, 400},
		{"expected status out of range", `{"id":"x","type":"http","target":"https://example.com","interval":60000000000,"timeout":5000000000,"expected_status":42}`, 400},
		{"bare ping host", `{"id":"x","type":"ping","target":"example.com","interval":60000000000,"timeout":5000000000}`, 400},
		{"ping with port ok", `{"id":"x","type":"ping","target":"example.com:22","interval":60000000000,"timeout":5000000000}`, 200},
		{"defaults applied", `{"id":"x","type":"http","target":"https://example.com"}`, 200},
	}
	for _, c := range cases {
		if w := postMonitor(t, s, RoleAdmin, c.body); w.Code != c.want {
			t.Errorf("%s: got %d, want %d (%s)", c.name, w.Code, c.want, w.Body.String())
		}
	}

	// Lowercase method normalizes to upper in storage.
	w := postMonitor(t, s, RoleAdmin, `{"id":"m","type":"http","target":"https://example.com","method":"head"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"method":"HEAD"`) {
		t.Errorf("method normalization: got %d %s", w.Code, w.Body.String())
	}
}
