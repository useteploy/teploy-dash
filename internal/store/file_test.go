package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStore_SaveListDeleteMonitor(t *testing.T) {
	store := NewFileStore(t.TempDir())

	m := Monitor{
		ID: "m1", Name: "homepage", Type: "http",
		Target: "https://example.com", Interval: 60 * time.Second, Enabled: true,
	}
	if err := store.SaveMonitor(m); err != nil {
		t.Fatalf("SaveMonitor: %v", err)
	}

	list, err := store.ListMonitors()
	if err != nil {
		t.Fatalf("ListMonitors: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 monitor, got %d", len(list))
	}
	if list[0].Name != "homepage" {
		t.Errorf("expected name 'homepage', got %q", list[0].Name)
	}

	got, err := store.GetMonitor("m1")
	if err != nil {
		t.Fatalf("GetMonitor: %v", err)
	}
	if got.Target != m.Target {
		t.Errorf("expected target %q, got %q", m.Target, got.Target)
	}

	if err := store.DeleteMonitor("m1"); err != nil {
		t.Fatalf("DeleteMonitor: %v", err)
	}

	list, _ = store.ListMonitors()
	if len(list) != 0 {
		t.Errorf("expected 0 monitors after delete, got %d", len(list))
	}
}

func TestFileStore_SaveAndReadChecks(t *testing.T) {
	store := NewFileStore(t.TempDir())

	now := time.Now()
	for i := 0; i < 5; i++ {
		err := store.SaveCheck(CheckResult{
			MonitorID:    "m1",
			Status:       "up",
			StatusCode:   200,
			ResponseTime: 100 * time.Millisecond,
			CheckedAt:    now.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("SaveCheck: %v", err)
		}
	}

	checks, err := store.GetChecks("m1", now.Add(-1*time.Hour), 100)
	if err != nil {
		t.Fatalf("GetChecks: %v", err)
	}
	if len(checks) != 5 {
		t.Errorf("expected 5 checks, got %d", len(checks))
	}
}

func TestFileStore_StatsComputation(t *testing.T) {
	store := NewFileStore(t.TempDir())

	now := time.Now()
	for i := 0; i < 10; i++ {
		status := "up"
		if i%3 == 0 {
			status = "down"
		}
		store.SaveCheck(CheckResult{
			MonitorID:    "m1",
			Status:       status,
			ResponseTime: 100 * time.Millisecond,
			CheckedAt:    now.Add(time.Duration(i) * time.Second),
		})
	}

	stats, err := store.GetStats("m1", now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.TotalChecks != 10 {
		t.Errorf("expected 10 total checks, got %d", stats.TotalChecks)
	}
	// 4 down (indices 0, 3, 6, 9), 6 up = 60% uptime
	if stats.UpChecks != 6 || stats.DownChecks != 4 {
		t.Errorf("expected 6 up / 4 down, got %d up / %d down", stats.UpChecks, stats.DownChecks)
	}
}

func TestValidID(t *testing.T) {
	valid := []string{"abc123", "mon_1", "a-b-c", "X9"}
	for _, id := range valid {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}
	bad := []string{"", "../../etc/passwd", "a/b", "a..b", "a b", "x.json", "a;b", "héllo"}
	for _, id := range bad {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}

// A path-traversal monitor ID must be rejected by the file store, not written
// outside its directory.
func TestFileStore_RejectsTraversalID(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)

	if err := s.SaveMonitor(Monitor{ID: "../../pwned", Name: "x", Type: "http", Target: "http://x"}); err == nil {
		t.Fatal("SaveMonitor accepted a traversal ID")
	}
	if err := s.SaveCheck(CheckResult{MonitorID: "../../pwned"}); err == nil {
		t.Fatal("SaveCheck accepted a traversal ID")
	}
	if _, err := s.GetMonitor("../../etc/passwd"); err == nil {
		t.Fatal("GetMonitor accepted a traversal ID")
	}
	if err := s.DeleteMonitor("../x"); err == nil {
		t.Fatal("DeleteMonitor accepted a traversal ID")
	}
	// Nothing should have been created outside the monitors dir.
	if _, err := os.Stat(filepath.Join(dir, "pwned.json")); err == nil {
		t.Fatal("a file was written outside the monitors directory")
	}
}
