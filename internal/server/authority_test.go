package server

// authority_test.go — D07 config-authority indicators: the per-app answer
// of which side owns the configuration (git-managed manifest / dash-managed
// manifest / unregistered), and the explicit refusal of dash-side edits to
// source-owned env keys (never a silent competing source of truth).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/manifest"
)

// newAuthorityServer seeds one admin account (clears first-run setup) and
// returns the server plus a logged-in admin cookie.
func newAuthorityServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)
	cookie := loginCookie(t, s.gate, "admin", "adminpass123")
	return s, cookie
}

func authorityDo(t *testing.T, s *Server, cookie *http.Cookie, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(cookie)
	req.Host = "dash.local"
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func putManifest(t *testing.T, s *Server, cookie *http.Cookie, server, app, body string) {
	t.Helper()
	rec := authorityDo(t, s, cookie, http.MethodPut, "/api/manifests/"+server+"/"+app, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("put manifest: %d %s", rec.Code, rec.Body.String())
	}
}

func getAuthority(t *testing.T, s *Server, cookie *http.Cookie, server, app string) configAuthorityResponse {
	t.Helper()
	rec := authorityDo(t, s, cookie, http.MethodGet, "/api/apps/"+server+"/"+app+"/config-authority", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("config-authority: %d %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data configAuthorityResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

// An app with NO registered manifest answers registered=false — the UI
// treats every setting as local (the pre-manifest truth).
func TestConfigAuthority_UnregisteredApp(t *testing.T) {
	s, cookie := newAuthorityServer(t)
	a := getAuthority(t, s, cookie, "prod", "web")
	if a.Registered || a.Mode != "" || len(a.DeclaredEnvKeys) != 0 {
		t.Fatalf("unregistered authority = %+v, want registered=false with an empty key list", a)
	}
}

// A git-managed manifest answers the mode, the git reference, the pinned
// revision, the normalized (browsable) repository URL, and the declared env
// keys.
func TestConfigAuthority_GitManagedManifest(t *testing.T) {
	s, cookie := newAuthorityServer(t)
	putManifest(t, s, cookie, "prod", "web", `{"mode":"git-managed","git":{"repository":"git@github.com:acme/web.git","revision":"0123456789abcdef0123456789abcdef01234567","manifest_path":"teploy.yml"},"manifest":"app: web\nimage: example/web:1\nenv:\n  RAILS_ENV: production\n  LOG_LEVEL: info\n"}`)

	a := getAuthority(t, s, cookie, "prod", "web")
	if !a.Registered || a.Mode != string(manifest.ModeGitManaged) {
		t.Fatalf("authority = %+v, want registered git-managed", a)
	}
	if a.Git == nil || a.Git.Repository != "git@github.com:acme/web.git" || a.Git.ManifestPath != "teploy.yml" || a.Git.Revision != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("git reference = %+v", a.Git)
	}
	// ManifestRevision is the content hash of dash's pinned copy (the git
	// commit lives in Git.Revision); it must be present.
	if len(a.ManifestRevision) != 64 {
		t.Fatalf("manifest revision = %q, want a sha256 content revision", a.ManifestRevision)
	}
	// The scp-like clone URL normalizes to the browsable https form — the
	// "create a source change" affordance links here.
	if a.RepositoryURL != "https://github.com/acme/web" {
		t.Fatalf("repository url = %q, want https://github.com/acme/web", a.RepositoryURL)
	}
	if strings.Join(a.DeclaredEnvKeys, ",") != "LOG_LEVEL,RAILS_ENV" {
		t.Fatalf("declared keys = %v", a.DeclaredEnvKeys)
	}
}

// A dash-managed manifest answers its mode with no git reference.
func TestConfigAuthority_DashManagedManifest(t *testing.T) {
	s, cookie := newAuthorityServer(t)
	putManifest(t, s, cookie, "prod", "web", `{"mode":"dash-managed","manifest":"app: web\nimage: example/web:1\n"}`)
	a := getAuthority(t, s, cookie, "prod", "web")
	if !a.Registered || a.Mode != string(manifest.ModeDashManaged) || a.Git != nil {
		t.Fatalf("authority = %+v, want registered dash-managed with no git ref", a)
	}
}

// The guard: setting or deleting a key DECLARED by a git-managed manifest
// refuses with 409 and names the repository (the remedy: change it at the
// source). The refusal fires before any CLI call.
func TestEnvMutation_RefusesSourceOwnedKey(t *testing.T) {
	s, cookie := newAuthorityServer(t)
	putManifest(t, s, cookie, "prod", "web", `{"mode":"git-managed","git":{"repository":"https://github.com/acme/web","revision":"0123456789abcdef0123456789abcdef01234567"},"manifest":"app: web\nimage: example/web:1\nenv:\n  RAILS_ENV: production\n"}`)

	rec := authorityDo(t, s, cookie, http.MethodPost, "/api/apps/prod/web/env", `{"key":"RAILS_ENV","value":"development"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("source-owned env POST = %d %s, want 409", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"RAILS_ENV", "git-managed", "github.com/acme/web"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("409 body missing %q: %s", want, rec.Body.String())
		}
	}

	rec2 := authorityDo(t, s, cookie, http.MethodDelete, "/api/apps/prod/web/env/RAILS_ENV", "")
	if rec2.Code != http.StatusConflict || !strings.Contains(rec2.Body.String(), "RAILS_ENV") {
		t.Fatalf("source-owned env DELETE = %d %s, want 409 naming the key", rec2.Code, rec2.Body.String())
	}
}

// Keys NOT declared by the manifest stay dash-settable (secret values cannot
// live in a manifest — the secret: inputs a git-managed manifest references
// are set here on purpose), and dash-managed manifests never trip the guard.
func TestEnvKeySourceOwned_Scoping(t *testing.T) {
	s, cookie := newAuthorityServer(t)
	putManifest(t, s, cookie, "prod", "web", `{"mode":"git-managed","git":{"repository":"https://github.com/acme/web","revision":"0123456789abcdef0123456789abcdef01234567"},"manifest":"app: web\nimage: example/web:1\nenv:\n  RAILS_ENV: production\n"}`)
	putManifest(t, s, cookie, "prod", "api", `{"mode":"dash-managed","manifest":"app: api\nimage: example/api:1\nenv:\n  RAILS_ENV: production\n"}`)

	if owned, repo := s.envKeySourceOwned("prod", "web", "API_TOKEN"); owned {
		t.Fatalf("undeclared key must stay dash-settable (repo=%q)", repo)
	}
	if owned, _ := s.envKeySourceOwned("prod", "api", "RAILS_ENV"); owned {
		t.Fatal("dash-managed declared key must not trip the git guard (the manifest is dash-side)")
	}
	if owned, _ := s.envKeySourceOwned("prod", "nother", "RAILS_ENV"); owned {
		t.Fatal("unregistered app must not trip the guard")
	}
}

// The route table classifies the endpoint as metadata (viewer-visible:
// names and repo references, never values).
func TestConfigAuthority_RouteClassifiedAsMetadata(t *testing.T) {
	required := requiredCapabilities(http.MethodGet, "/api/apps/prod/web/config-authority")
	if len(required) != 1 || required[0] != "view.metadata" {
		t.Fatalf("config-authority requires %v, want [view.metadata]", required)
	}
}
