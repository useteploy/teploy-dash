package server

// D05 templates-as-reviewed-versioned-packages: handler tests for catalog
// validation/rendering, version pinning on instantiate, and upgrade-notes
// surfacing. The teploy CLI is faked on PATH (echoing a catalog JSON file
// the test can rewrite between calls); operation execution is injected so
// installs succeed without a real CLI.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/operation"
)

// fakeTemplateCLI writes a teploy shell script on PATH that prints the
// catalog file's current contents for any invocation, and returns the
// catalog path so tests can rewrite it.
func fakeTemplateCLI(t *testing.T, catalog string) string {
	t.Helper()
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	cliPath := filepath.Join(dir, "teploy")
	script := "#!/bin/sh\ncat " + catalogPath + "\n"
	if err := os.WriteFile(cliPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return catalogPath
}

func templatesTestServer(t *testing.T) *Server {
	t.Helper()
	return New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: name + ".example", User: "deploy"}, nil
		},
		OperationExecutor: func(_ context.Context, _ operation.Command, emit func(operation.Stream, string)) (int, error) {
			emit(operation.StreamStdout, "installed")
			return 0, nil
		},
	})
}

const todayCatalog = `[{"name":"postgres-admin","description":"PostgreSQL 16 with Adminer","accessories":["postgres"],"variables":["domain","db_password"]}]`

func TestTemplatesCatalogValidatedAndRendered(t *testing.T) {
	fakeTemplateCLI(t, todayCatalog)
	s := templatesTestServer(t)

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/templates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data) != 1 {
		t.Fatalf("entries = %#v", envelope.Data)
	}
	entry := envelope.Data[0]
	if entry["version_state"] != "unversioned" {
		t.Fatalf("version_state = %v, want unversioned (today's catalog carries no versions)", entry["version_state"])
	}
	if v, invented := entry["version"]; invented && v != "" {
		t.Fatalf("version field = %v, dash must not invent one", v)
	}
}

func TestTemplatesCatalogInvalidPayloadIsDependencyFailure(t *testing.T) {
	fakeTemplateCLI(t, `[{"name":"a","version":"latest"}]`)
	s := templatesTestServer(t)

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/templates", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s — a catalog that fails validation must not render", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not semver") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestTemplatesInstallUnpinnedStillWorks(t *testing.T) {
	fakeTemplateCLI(t, todayCatalog)
	s := templatesTestServer(t)

	body := bytes.NewBufferString(`{"template":"postgres-admin","domain":"db.example","server":"prod"}`)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/templates/install", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Request.TemplateVersion != "" {
		t.Fatalf("TemplateVersion = %q, want empty on an unpinned install", envelope.Data.Request.TemplateVersion)
	}
	waitSucceeded(t, s, envelope.Data.ID)
}

func TestTemplatesInstallVersionPinEnforcedAtAdmission(t *testing.T) {
	versioned := `[{"name":"postgres-admin","description":"PostgreSQL 16 with Adminer","version":"1.4.0","variables":["domain"],"upgrade_notes":"dump/restore across majors","backup_scope":"accessory dump"}]`
	fakeTemplateCLI(t, versioned)
	s := templatesTestServer(t)

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/templates/install", bytes.NewBufferString(body)))
		return rec
	}

	rec := post(`{"template":"postgres-admin","domain":"db.example","server":"prod","template_version":"1.3.0"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "moved from v1.3.0 (selected) to v1.4.0 (current)") {
		t.Fatalf("stale pin: status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = post(`{"template":"postgres-admin","domain":"db.example","server":"prod","template_version":"1.4.0"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("current pin: status = %d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Request.TemplateVersion != "1.4.0" {
		t.Fatalf("TemplateVersion = %q, want the pin recorded on the request", envelope.Data.Request.TemplateVersion)
	}
	waitSucceeded(t, s, envelope.Data.ID)
}

func TestTemplatesInstallPinAgainstUnversionedCatalogRefused(t *testing.T) {
	fakeTemplateCLI(t, todayCatalog)
	s := templatesTestServer(t)

	body := bytes.NewBufferString(`{"template":"postgres-admin","domain":"db.example","server":"prod","template_version":"1.0.0"}`)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/templates/install", body))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "unversioned") {
		t.Fatalf("status = %d body=%s — a pin against a version-less catalog pretends a guarantee neither side has", rec.Code, rec.Body.String())
	}
}

func TestTemplatesUpgradeSurfacingWhenCatalogAdvances(t *testing.T) {
	catalogPath := fakeTemplateCLI(t, `[{"name":"postgres-admin","description":"PostgreSQL 16 with Adminer","version":"1.4.0","variables":["domain"],"upgrade_notes":"pg16 requires dump/restore","backup_scope":"db accessory dump + volume copy"}]`)
	s := templatesTestServer(t)

	// Install at 1.4.0 and let it succeed.
	body := bytes.NewBufferString(`{"template":"postgres-admin","domain":"db.example","server":"prod","template_version":"1.4.0"}`)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/templates/install", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("install status = %d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, s, envelope.Data.ID)

	// Catalog advances to 1.5.0.
	if err := os.WriteFile(catalogPath, []byte(`[{"name":"postgres-admin","description":"PostgreSQL 16 with Adminer","version":"1.5.0","variables":["domain"],"upgrade_notes":"pg17 major: dump then restore","backup_scope":"db accessory dump + volume copy"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/templates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []struct {
			Installed *struct {
				Server  string `json:"server"`
				Version string `json:"version"`
			} `json:"installed"`
			Upgrade *struct {
				From        string `json:"from"`
				To          string `json:"to"`
				Notes       string `json:"notes"`
				BackupScope string `json:"backup_scope"`
			} `json:"upgrade"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0].Installed == nil {
		t.Fatalf("entries = %#v", list.Data)
	}
	if list.Data[0].Installed.Server != "prod" || list.Data[0].Installed.Version != "1.4.0" {
		t.Fatalf("installed = %#v", list.Data[0].Installed)
	}
	up := list.Data[0].Upgrade
	if up == nil || up.From != "1.4.0" || up.To != "1.5.0" || !strings.Contains(up.Notes, "dump then restore") || up.BackupScope == "" {
		t.Fatalf("upgrade = %#v, want 1.4.0 -> 1.5.0 with the package's own notes and backup scope", up)
	}
}

func waitSucceeded(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		op, err := s.operations.Get(id)
		if err == nil && op.Status == operation.StatusSucceeded {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("operation %s did not succeed in time", id)
}
