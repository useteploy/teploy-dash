package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// R45: in no-auth mode the operator IS the admin — managing internal
// monitors from the loopback no-auth dashboard previously 403'd because
// there is no session to check.
func TestMonitorAllowInternalWorksInNoAuthMode(t *testing.T) {
	s := &Server{store: store.NewFileStore(t.TempDir()), config: Config{NoAuth: true}}
	internal := `{"id":"m1","name":"M1","type":"http","target":"http://10.0.0.5/up","interval":60000000000,"timeout":10000000000,"allow_internal":true}`
	if w := postMonitor(t, s, "", internal); w.Code != 200 {
		t.Fatalf("no-auth operator enabling allow_internal: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// R46: TCP monitor targets need a nonempty host and a numeric 1-65535 port —
// SplitHostPort alone accepted "host:" and service names.
func TestMonitorTCPTargetValidation(t *testing.T) {
	s := monitorsTestServer(t)
	cases := []struct{ name, target string }{
		{"empty host", ":5432"},
		{"service port", "db.example:postgres"},
		{"zero port", "db.example:0"},
		{"out of range port", "db.example:70000"},
	}
	for _, tc := range cases {
		body := `{"id":"m","name":"M","type":"tcp","target":"` + tc.target + `","interval":60000000000,"timeout":10000000000}`
		if w := postMonitor(t, s, RoleAdmin, body); w.Code != 400 {
			t.Fatalf("%s (%s): got %d, want 400", tc.name, tc.target, w.Code)
		}
	}
	good := `{"id":"m","name":"M","type":"tcp","target":"db.example:5432","interval":60000000000,"timeout":10000000000}`
	if w := postMonitor(t, s, RoleAdmin, good); w.Code != 200 {
		t.Fatalf("valid tcp target: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// The monitor list carries each monitor's newest check, so the list dot can
// show the current state rather than the 24-hour uptime.
func TestMonitorListCarriesLastCheck(t *testing.T) {
	s := monitorsTestServer(t)
	m := &store.Monitor{ID: "m1", Name: "M1", Type: "http", Target: "https://example.com", Interval: time.Minute, Timeout: 10 * time.Second, Enabled: true}
	if err := s.store.SaveMonitor(m); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, c := range []store.CheckResult{
		{MonitorID: "m1", Status: "down", Message: "refused", CheckedAt: now, Incarnation: m.Incarnation},
		{MonitorID: "m1", Status: "up", CheckedAt: now.Add(-time.Minute), Incarnation: m.Incarnation},
	} {
		if ok, err := s.store.SaveCheck(c); err != nil || !ok {
			t.Fatalf("save check: %v %v", ok, err)
		}
	}
	w := httptest.NewRecorder()
	s.handleMonitors(w, httptest.NewRequest("GET", "/api/monitors", nil))
	var list []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %s (%v)", w.Body.String(), err)
	}
	last, ok := list[0]["last_check"].(map[string]any)
	if !ok || last["status"] != "down" || last["message"] != "refused" {
		t.Fatalf("last_check = %v, want the newest (down) check", list[0]["last_check"])
	}

	fresh := monitorsTestServer(t)
	if err := fresh.store.SaveMonitor(&store.Monitor{ID: "m2", Name: "M2", Type: "http", Target: "https://example.com", Interval: time.Minute, Timeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	fresh.handleMonitors(w, httptest.NewRequest("GET", "/api/monitors", nil))
	if strings.Contains(w.Body.String(), "last_check") {
		t.Fatalf("a monitor with no checks must not carry last_check: %s", w.Body.String())
	}
}
