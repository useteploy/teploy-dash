package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// DASH-003: a Nucleus-connect failure silently falls back to the file store
// while still reporting healthy — the active backend must be visible via
// /api/health so the fallback isn't invisible after the startup log scrolls.
func TestHandleHealth_ReportsBackend(t *testing.T) {
	cases := []struct {
		configured, want string
	}{
		{"", "file"}, // zero-value Config.Backend defaults to "file"
		{"file", "file"},
		{"nucleus", "nucleus"},
	}
	for _, c := range cases {
		s := &Server{config: Config{Backend: c.configured}}
		req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		w := httptest.NewRecorder()
		s.handleHealth(w, req)

		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("configured=%q: invalid JSON body: %v", c.configured, err)
		}
		if body["backend"] != c.want {
			t.Errorf("configured=%q: backend = %q, want %q", c.configured, body["backend"], c.want)
		}
		if body["status"] != "ok" {
			t.Errorf("configured=%q: status = %q, want ok", c.configured, body["status"])
		}
	}
}
