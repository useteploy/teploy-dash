package store

import (
	"os"
	"path/filepath"
	"strings"
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

func TestFileStore_RestoreTestRoundTrip(t *testing.T) {
	store := NewFileStore(t.TempDir())

	rt := RestoreTest{
		ID: "rt1", Server: "prod", App: "myapp", Accessory: "db",
		Bucket: "backups", Region: "us-east-1", IntervalHours: 24, Enabled: true,
		LastRunAt: time.Now().Truncate(time.Second), LastOK: true,
		LastMetric: "tables=42", LastDate: "20260710-040000", LastDurationMs: 9500,
	}
	if err := store.SaveRestoreTest(rt); err != nil {
		t.Fatalf("SaveRestoreTest: %v", err)
	}

	list, err := store.ListRestoreTests()
	if err != nil {
		t.Fatalf("ListRestoreTests: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 restore test, got %d", len(list))
	}

	got, err := store.GetRestoreTest("rt1")
	if err != nil {
		t.Fatalf("GetRestoreTest: %v", err)
	}
	if got.LastMetric != "tables=42" || !got.LastOK || got.IntervalHours != 24 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	if err := store.DeleteRestoreTest("rt1"); err != nil {
		t.Fatalf("DeleteRestoreTest: %v", err)
	}
	list, _ = store.ListRestoreTests()
	if len(list) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(list))
	}

	// Path-traversal guard, same rule as monitors.
	if err := store.SaveRestoreTest(RestoreTest{ID: "../evil"}); err == nil {
		t.Error("expected invalid-id rejection")
	}
}

// A31: an oversized record in a history file must abort that file's cleanup
// rewrite and PRESERVE the original, not silently replace it with a
// truncated copy.
func TestCleanup_OversizedRecordPreservesOriginal(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	histDir := filepath.Join(dir, "history")
	if err := os.MkdirAll(histDir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(histDir, "m1.jsonl")

	goodOld := `{"monitor_id":"m1","status":"up","checked_at":"2020-01-01T00:00:00Z"}` + "\n"
	goodNew := `{"monitor_id":"m1","status":"up","checked_at":"2999-01-01T00:00:00Z"}` + "\n"
	oversized := `{"monitor_id":"m1","status":"up","message":"` + strings.Repeat("x", 2*maxRecordBytes) + `"}` + "\n"
	original := goodOld + oversized + goodNew
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if err := s.Cleanup(); err == nil {
		t.Fatal("expected Cleanup to report the unreadable history file")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("oversized record rewrote the file:\n%d bytes before, %d after", len(original), len(after))
	}

	// Leftover temp files must not accumulate next to the preserved original.
	entries, _ := os.ReadDir(histDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cleanup-") {
			t.Fatalf("cleanup left temp file behind: %s", e.Name())
		}
	}
}

// A31/A32: a healthy cleanup drops expired records and keeps fresh ones.
func TestCleanup_DropsExpiredKeepsFresh(t *testing.T) {
	s := NewFileStore(t.TempDir())
	old := time.Now().AddDate(0, 0, -(RetentionDays + 1))
	fresh := time.Now()
	if err := s.SaveCheck(CheckResult{MonitorID: "m1", Status: "up", CheckedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheck(CheckResult{MonitorID: "m1", Status: "up", CheckedAt: fresh}); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	checks, err := s.GetChecks("m1", time.Time{}, 0)
	if err != nil {
		t.Fatalf("GetChecks: %v", err)
	}
	if len(checks) != 1 || !checks[0].CheckedAt.Equal(fresh) {
		t.Fatalf("expected only the fresh check, got %d", len(checks))
	}
}

// A32: a data directory that cannot be created surfaces through InitErr
// instead of producing a silently broken store.
func TestNewFileStore_UnwritableDirReportsInitErr(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission-based failure does not trigger")
	}
	s := NewFileStore("/proc/definitely/not/writable")
	if s.InitErr() == nil {
		t.Fatal("expected InitErr for an unwritable data dir")
	}
}

// A15: persisting a result must not resurrect a deleted test, and must not
// overwrite configuration fields edited while the run was in flight.
func TestSaveRestoreTestResult_ResultOnly(t *testing.T) {
	s := NewFileStore(t.TempDir())
	if err := s.SaveRestoreTest(RestoreTest{ID: "t1", Server: "prod", App: "web", Accessory: "pg", Bucket: "b", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// A run against the CURRENT configuration applies its result.
	run := RestoreTest{ID: "t1", Server: "prod", App: "web", Accessory: "pg", Bucket: "b", LastOK: true, LastDetail: "ok", LastMetric: "checksum", LastRunAt: time.Now()}
	applied, err := s.SaveRestoreTestResult("t1", run)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("matching-target result not applied")
	}
	got, err := s.GetRestoreTest("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastOK != true || got.LastMetric != "checksum" {
		t.Errorf("result fields not applied: %+v", got)
	}

	// A24: the operator retargets the test (new bucket) while a run against
	// the OLD bucket is still in flight. The late result describes a
	// different target and must be dropped, not attached to the new config —
	// while the config edit itself survives untouched.
	if err := s.SaveRestoreTest(RestoreTest{ID: "t1", Server: "prod", App: "web", Accessory: "pg", Bucket: "NEW", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	stale := RestoreTest{ID: "t1", Server: "prod", App: "web", Accessory: "pg", Bucket: "b", LastOK: false, LastDetail: "stale", LastRunAt: time.Now()}
	applied, err = s.SaveRestoreTestResult("t1", stale)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("retargeted result reported as applied")
	}
	// F037: a region-only change is also a retarget.
	if err := s.SaveRestoreTest(RestoreTest{ID: "t1", Server: "prod", App: "web", Accessory: "pg", Bucket: "NEW", Region: "eu-west-1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	sameBucketOtherRegion := RestoreTest{ID: "t1", Server: "prod", App: "web", Accessory: "pg", Bucket: "NEW", Region: "us-east-1", LastOK: true, LastRunAt: time.Now()}
	applied, err = s.SaveRestoreTestResult("t1", sameBucketOtherRegion)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("region-only retarget inherited the old region's result")
	}
	got, _ = s.GetRestoreTest("t1")
	if got.Bucket != "NEW" {
		t.Errorf("config edit overwritten by late result: bucket=%q", got.Bucket)
	}
	if got.LastDetail == "stale" {
		t.Errorf("retargeted test inherited the old target's result: %+v", got)
	}

	// Deleted mid-run: no resurrect.
	if err := s.DeleteRestoreTest("t1"); err != nil {
		t.Fatal(err)
	}
	if applied, err := s.SaveRestoreTestResult("t1", run); err != nil || applied {
		t.Fatalf("deleted-mid-run save: applied=%v err=%v", applied, err)
	}
	if _, err := s.GetRestoreTest("t1"); err == nil {
		t.Error("deleted restore test resurrected by its own result")
	}
}
