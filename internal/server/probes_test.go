package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/store"
)

// A39/A47: liveness is cheap and dependency-free; readiness checks the store
// and reports degraded persistence without leaving rotation.

func probeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	return body
}

func TestHealthzIsCheapOK(t *testing.T) {
	s := New(Config{DataDir: t.TempDir(), NoAuth: true})
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := probeBody(t, rec)
	if body["status"] != "ok" {
		t.Fatalf("healthz body = %v", body)
	}
}

func TestReadyzReadyAndStoreUnavailable(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	s := New(Config{DataDir: t.TempDir(), NoAuth: true, Store: store.NewFileStore(dir)})

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := probeBody(t, rec)
	if body["status"] != "ready" {
		t.Fatalf("readyz body = %v", body)
	}

	// Break the store directory: readiness must fail (503) so orchestrators
	// pull the instance from rotation while liveness stays green.
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d body=%s, want 503", rec.Code, rec.Body.String())
	}
	body = probeBody(t, rec)
	if body["status"] != "unavailable" {
		t.Fatalf("readyz body = %v", body)
	}
	// Liveness is still OK — restarting the process would not help.
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d after store outage", rec.Code)
	}
}

func TestReadyzDegradedWhenOperationsUnavailable(t *testing.T) {
	dir := t.TempDir()
	// Block operation-service initialization: <dataDir>/operations exists
	// as a plain file, so the records/events dirs cannot be created.
	if err := os.WriteFile(filepath.Join(dir, "operations"), []byte("not a dir"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{DataDir: dir, NoAuth: true, CLIInstalled: func() bool { return cli.IsInstalled() }})
	if s.operationInitErr == nil {
		t.Fatal("test precondition: operation service should fail to init")
	}
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("degraded readiness status = %d body=%s, want 200 (stays in rotation)", rec.Code, rec.Body.String())
	}
	body := probeBody(t, rec)
	if body["status"] != "degraded" {
		t.Fatalf("readyz body = %v", body)
	}
}

// The probes bypass the session gate (orchestrators have no login) and are
// reachable in setup mode without redirecting to /setup.
func TestProbesBypassAuthAndSetup(t *testing.T) {
	s := New(Config{DataDir: t.TempDir()}) // no credentials -> setup mode
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s, want 200 (no redirect to setup)", path, rec.Code, rec.Body.String())
		}
	}
}
