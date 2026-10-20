package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
)

// Without a CLI that reports a config home, the authority answer is empty
// and no env key is refused.
func TestConfigAuthority_NoConfigHome(t *testing.T) {
	s := newConfigServer(t, &configRunner{tokens: []string{"app-list-machine"}})
	rec := serve(s, "GET", "/api/apps/prod/web/config-authority", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"declared_env_keys":[]`) || strings.Contains(rec.Body.String(), `"home"`) {
		t.Fatalf("config-authority = %d %s", rec.Code, rec.Body.String())
	}
	if err := s.configOwnedEnvKeyError(context.Background(), "prod", "web", "RAILS_ENV"); err != nil {
		t.Fatalf("env edit refused without a config home: %v", err)
	}
}

// A folder-home app never refuses env edits, whatever its config declares.
func TestConfigAuthority_FolderHomeNeverRefuses(t *testing.T) {
	c := &configRunner{tokens: allConfigTokens, reply: func(args []string, _ string) (*cli.Result, error) {
		return &cli.Result{Stdout: `{"machine_interface":2,"home":"folder","content":"app: web\nenv:\n  RAILS_ENV: production\n"}`}, nil
	}}
	s := newConfigServer(t, c)
	if err := s.configOwnedEnvKeyError(context.Background(), "prod", "web", "RAILS_ENV"); err != nil {
		t.Fatalf("folder-home env edit refused: %v", err)
	}
}

func TestConfigAuthority_RouteClassifiedAsMetadata(t *testing.T) {
	required := requiredCapabilities(http.MethodGet, "/api/apps/prod/web/config-authority")
	if len(required) != 1 || required[0] != "view.metadata" {
		t.Fatalf("config-authority requires %v, want [view.metadata]", required)
	}
}

// HTTP and MCP env edits share the Teploy-home guard, including env_literal
// keys, aliases and merges; undeclared keys stay settable.
func TestTeployHomeEnvGuardHTTPAndMCP(t *testing.T) {
	for _, fields := range []string{"env: {RAILS_ENV: production}\n", "env_literal: {RAILS_ENV: production}\n", "base: &base {RAILS_ENV: production}\nenv_literal: {<<: *base}\n"} {
		t.Run(fields, func(t *testing.T) {
			content := strings.ReplaceAll("app: web\nimage: example/web:1\n"+fields, "\n", `\n`)
			c := &configRunner{tokens: allConfigTokens, reply: func(args []string, _ string) (*cli.Result, error) {
				if len(args) > 1 && args[0] == "config" && args[1] == "get" {
					return &cli.Result{Stdout: `{"machine_interface":2,"home":"teploy","commit":"` + strings.Repeat("e", 40) + `","content":"` + content + `"}`}, nil
				}
				return &cli.Result{}, nil
			}}
			s := newConfigServer(t, c)
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				path := "/api/apps/prod/web/env"
				payload := `{"key":"RAILS_ENV","value":"changed"}`
				if method == http.MethodDelete {
					path += "/RAILS_ENV"
					payload = ""
				}
				if w := serve(s, method, path, payload); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Settings") {
					t.Fatalf("HTTP %s declared key %d %s", method, w.Code, w.Body.String())
				}
			}
			backend := mcpBackend{s: s}
			want := "environment variable RAILS_ENV is set in this app's Teploy settings; change it in Settings"
			if _, err := backend.SetEnv(context.Background(), "prod", "web", "RAILS_ENV", "changed"); err == nil || err.Error() != want {
				t.Fatalf("MCP set: %v", err)
			}
			if _, err := backend.UnsetEnv(context.Background(), "prod", "web", "RAILS_ENV"); err == nil || err.Error() != want {
				t.Fatalf("MCP unset: %v", err)
			}
			if err := s.configOwnedEnvKeyError(context.Background(), "prod", "web", "UNDECLARED_INPUT"); err != nil {
				t.Fatalf("undeclared key refused: %v", err)
			}
		})
	}
}

// The manifests API is gone: every route answers 410 with the reason.
func TestManifestsAPIGone(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/manifests"},
		{http.MethodGet, "/api/manifests/prod/web"},
		{http.MethodPut, "/api/manifests/prod/web"},
		{http.MethodPost, "/api/manifests/prod/web/apply"},
		{http.MethodDelete, "/api/manifests/prod/web"},
	} {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), `"error":"the manifests API was removed; app settings live on the app's server (Settings tab / teploy config)"`) {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
