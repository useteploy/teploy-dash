package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
)

// D-08: with no teploy CLI on the dashboard host every route that needs it
// answers the same 503 and the same message naming the install location —
// not a mix of 400/502/503 and a raw exec error.
func TestNoCLIAnswersOneConsistentError(t *testing.T) {
	// Routes that call the cli package directly (registries) find no binary;
	// the rest consult the injected CLIInstalled.
	t.Setenv("PATH", t.TempDir())
	s := New(Config{DataDir: t.TempDir(), NoAuth: true, CLIInstalled: func() bool { return false }})
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/api/servers", ""},
		{"GET", "/api/servers/x", ""},
		{"GET", "/api/registries", ""},
		{"POST", "/api/registries", `{"server":"ghcr.io","username":"u","password":"p"}`},
		{"GET", "/api/apps/prod/web/env", ""},
		{"POST", "/api/apps/prod/web/restart", ""},
		{"GET", "/api/logs/prod/web", ""},
	} {
		req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
		if rt.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, req)
		var env struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if rec.Code != http.StatusServiceUnavailable || env.Error != cli.ErrNotInstalled.Error() {
			t.Errorf("%s %s = %d %q, want 503 %q", rt.method, rt.path, rec.Code, rec.Body.String(), cli.ErrNotInstalled.Error())
		}
	}
}
