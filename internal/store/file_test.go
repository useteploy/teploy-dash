package store

import (
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
