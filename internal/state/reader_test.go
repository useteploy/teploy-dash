package state

import (
	"os"
	"path/filepath"
	"testing"
)

// stateFixture is the exact format teploy-cli writes (matches
// useteploy/teploy-cli internal/state/state.go Write).
const stateFixture = `current_port=49153
current_hash=v4
previous_port=49152
previous_hash=v3
domain=smoke-kv.local
`

func TestParseStateFile_CLIFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	if err := os.WriteFile(path, []byte(stateFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := parseStateFile(path)
	if err != nil {
		t.Fatalf("parseStateFile: %v", err)
	}
	if s.CurrentHash != "v4" {
		t.Errorf("CurrentHash = %q, want v4", s.CurrentHash)
	}
	if s.PreviousHash != "v3" {
		t.Errorf("PreviousHash = %q, want v3", s.PreviousHash)
	}
	if s.Port != 49153 {
		t.Errorf("Port = %d, want 49153", s.Port)
	}
	if s.Domain != "smoke-kv.local" {
		t.Errorf("Domain = %q, want smoke-kv.local", s.Domain)
	}
}

func TestParseStateFile_IgnoresUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	if err := os.WriteFile(path, []byte("current_hash=x\nfuture_field=ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := parseStateFile(path)
	if err != nil {
		t.Fatalf("parseStateFile: %v", err)
	}
	if s.CurrentHash != "x" {
		t.Errorf("CurrentHash = %q, want x", s.CurrentHash)
	}
}

func TestParseStateFile_MissingFile(t *testing.T) {
	_, err := parseStateFile(filepath.Join(t.TempDir(), "nonexistent"))
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestParseStateFile_HandlesBlankAndMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	content := "\ncurrent_hash=ok\n\nno-equals-sign-here\ncurrent_port=8080\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := parseStateFile(path)
	if err != nil {
		t.Fatalf("parseStateFile: %v", err)
	}
	if s.CurrentHash != "ok" || s.Port != 8080 {
		t.Errorf("got CurrentHash=%q Port=%d, want ok/8080", s.CurrentHash, s.Port)
	}
}

func TestReader_ListApps(t *testing.T) {
	root := t.TempDir()

	// app1 with valid state.
	app1Dir := filepath.Join(root, "app1")
	if err := os.MkdirAll(app1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app1Dir, "state"), []byte(stateFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	// app2 with state but no domain (current_port + hash only).
	app2Dir := filepath.Join(root, "app2")
	if err := os.MkdirAll(app2Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app2Dir, "state"), []byte("current_hash=abc\ncurrent_port=3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// app3 directory with no state file — should be skipped silently.
	if err := os.MkdirAll(filepath.Join(root, "app3"), 0o755); err != nil {
		t.Fatal(err)
	}

	// stray file at the root — should be ignored (not a directory).
	if err := os.WriteFile(filepath.Join(root, "teploy.log"), []byte("log line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	apps, err := NewReader(root).ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("len(apps) = %d, want 2 (got: %+v)", len(apps), apps)
	}
	// Sorted alphabetically.
	if apps[0].App != "app1" || apps[1].App != "app2" {
		t.Errorf("unexpected order: %s, %s", apps[0].App, apps[1].App)
	}
	if apps[0].Domain != "smoke-kv.local" {
		t.Errorf("app1.Domain = %q, want smoke-kv.local", apps[0].Domain)
	}
	if apps[1].Port != 3000 {
		t.Errorf("app2.Port = %d, want 3000", apps[1].Port)
	}
}

func TestReader_ListApps_NonexistentDir(t *testing.T) {
	apps, err := NewReader("/nonexistent/path").ListApps()
	if err != nil {
		t.Errorf("expected nil error for missing dir, got %v", err)
	}
	if len(apps) != 0 {
		t.Errorf("expected 0 apps, got %d", len(apps))
	}
}

func TestReader_GetApp(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "myapp")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "state"), []byte(stateFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := NewReader(root).GetApp("myapp")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if app.App != "myapp" {
		t.Errorf("App = %q, want myapp", app.App)
	}
	if app.CurrentHash != "v4" {
		t.Errorf("CurrentHash = %q, want v4", app.CurrentHash)
	}
}
