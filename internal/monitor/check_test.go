package monitor

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/store"
)

// checkHTTP must honor a configured ExpectedStatus exactly. Before the fix it
// computed expectedStatus but always judged up/down on the 2xx-3xx range, so a
// monitor expecting e.g. 401 or 201 was scored wrong.
func TestCheckHTTP_ExpectedStatusExactMatch(t *testing.T) {
	cases := []struct {
		name       string
		serverCode int
		expected   int
		wantStatus string
	}{
		{"expect 200 get 200", 200, 200, "up"},
		{"expect 201 get 201", 201, 201, "up"},
		{"expect 401 get 401", 401, 401, "up"},   // would be "down" under old 2xx/3xx rule
		{"expect 200 get 500", 500, 200, "down"},
		{"expect 201 get 200", 200, 201, "down"}, // would be "up" under old 2xx/3xx rule
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.serverCode)
			}))
			defer srv.Close()

			r := New(&mockStore{})
			got := r.checkHTTP(store.Monitor{
				ID: "m", Type: "http", Target: srv.URL, ExpectedStatus: tc.expected,
			})
			if got.Status != tc.wantStatus {
				t.Errorf("status %d expect %d: got %q want %q (%s)",
					tc.serverCode, tc.expected, got.Status, tc.wantStatus, got.Message)
			}
		})
	}
}

// With no ExpectedStatus set, any 2xx/3xx is up and everything else is down.
func TestCheckHTTP_DefaultRange(t *testing.T) {
	cases := []struct {
		code       int
		wantStatus string
	}{
		{200, "up"}, {204, "up"}, {301, "up"}, {399, "up"},
		{400, "down"}, {404, "down"}, {500, "down"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.code)
		}))
		r := New(&mockStore{})
		got := r.checkHTTP(store.Monitor{ID: "m", Type: "http", Target: srv.URL})
		if got.Status != tc.wantStatus {
			t.Errorf("code %d: got %q want %q", tc.code, got.Status, tc.wantStatus)
		}
		srv.Close()
	}
}

// checkHTTP must apply the monitor's own Timeout, not a hardcoded client
// timeout. A server that sleeps longer than the monitor timeout is "down".
func TestCheckHTTP_PerMonitorTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	r := New(&mockStore{})
	got := r.checkHTTP(store.Monitor{
		ID: "m", Type: "http", Target: srv.URL, Timeout: 50 * time.Millisecond,
	})
	if got.Status != "down" {
		t.Errorf("expected timeout -> down, got %q (%s)", got.Status, got.Message)
	}
}
