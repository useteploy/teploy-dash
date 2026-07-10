package restoretest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/store"
)

func newTestRunner(t *testing.T, out string, cliErr error) (*Runner, store.Store) {
	t.Helper()
	st := store.NewFileStore(t.TempDir())
	r := New(st)
	r.runCLI = func(server, user, app, accessory, bucket, region string) (string, string, error) {
		return out, "boom-stderr", cliErr
	}
	return r, st
}

func seedTest(t *testing.T, st store.Store) store.RestoreTest {
	t.Helper()
	rt := store.RestoreTest{
		ID: "rt1", Server: "prod", App: "myapp", Accessory: "db",
		Bucket: "backups", Region: "us-east-1", IntervalHours: 24, Enabled: true,
	}
	if err := st.SaveRestoreTest(rt); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return rt
}

func TestRunNow_ParsesAndPersistsSuccess(t *testing.T) {
	out := `{"app":"myapp","accessory":"db","kind":"postgres","date":"20260710-040000","metric":"tables=42","duration_ms":9500,"ok":true}`
	r, st := newTestRunner(t, out, nil)
	rt := seedTest(t, st)

	got := r.RunNow(rt)
	if !got.LastOK {
		t.Fatalf("expected LastOK, detail=%s", got.LastDetail)
	}
	if got.LastMetric != "tables=42" || got.LastDate != "20260710-040000" || got.LastDurationMs != 9500 {
		t.Errorf("result fields not mapped: %+v", got)
	}
	if got.LastRunAt.IsZero() {
		t.Error("LastRunAt not stamped")
	}

	// Persisted, not just returned.
	saved, err := st.GetRestoreTest("rt1")
	if err != nil || !saved.LastOK || saved.LastMetric != "tables=42" {
		t.Fatalf("result not persisted: %+v err=%v", saved, err)
	}
}

func TestRunNow_FailedVerificationIsResult(t *testing.T) {
	// verify-backup exits non-zero on a failed verification but still prints
	// the JSON result — the runner must use the result, not the exit error.
	out := `{"app":"myapp","accessory":"db","kind":"postgres","date":"20260710-040000","ok":false,"detail":"restored database has zero tables"}`
	r, st := newTestRunner(t, out, fmt.Errorf("exit status 1"))
	rt := seedTest(t, st)

	got := r.RunNow(rt)
	if got.LastOK {
		t.Fatal("expected LastOK=false")
	}
	if got.LastDetail != "restored database has zero tables" {
		t.Errorf("detail should come from the JSON result, got %q", got.LastDetail)
	}
}

func TestRunNow_OperationalFailureWithoutResult(t *testing.T) {
	// SSH failure / bad flags: non-JSON output. Detail should carry stderr.
	r, st := newTestRunner(t, "usage: teploy accessory verify-backup", nil)
	rt := seedTest(t, st)

	got := r.RunNow(rt)
	if got.LastOK {
		t.Fatal("expected LastOK=false")
	}
	if !strings.Contains(got.LastDetail, "boom-stderr") {
		t.Errorf("detail should carry stderr, got %q", got.LastDetail)
	}
}

func TestRunNow_TracksOutcomeTransitions(t *testing.T) {
	out := `{"ok":false,"detail":"nope"}`
	r, st := newTestRunner(t, out, nil)
	rt := seedTest(t, st)

	r.RunNow(rt)
	r.mu.Lock()
	first := r.lastOK["rt1"]
	r.mu.Unlock()
	if first {
		t.Fatal("expected lastOK=false after failed run")
	}

	r.mu.Lock()
	r.runCLI = func(server, user, app, accessory, bucket, region string) (string, string, error) {
		return `{"ok":true,"metric":"tables=7","date":"20260710-050000"}`, "", nil
	}
	r.mu.Unlock()

	cur, _ := st.GetRestoreTest("rt1")
	r.RunNow(*cur)
	r.mu.Lock()
	second := r.lastOK["rt1"]
	r.mu.Unlock()
	if !second {
		t.Fatal("expected lastOK=true after recovery")
	}
}

func TestStartSeedsBaselineAndSkipsImmediateRunForKnownTests(t *testing.T) {
	// A test that has already run must NOT re-run on dash restart (runs are
	// expensive); its persisted outcome seeds the transition baseline.
	ran := 0
	st := store.NewFileStore(t.TempDir())
	r := New(st)
	r.runCLI = func(server, user, app, accessory, bucket, region string) (string, string, error) {
		ran++
		return `{"ok":true}`, "", nil
	}
	rt := store.RestoreTest{
		ID: "rt2", Server: "prod", App: "a", Accessory: "db",
		Bucket: "b", IntervalHours: 24, Enabled: true,
		LastRunAt: time.Now().Add(-time.Hour), LastOK: false,
	}
	if err := st.SaveRestoreTest(rt); err != nil {
		t.Fatal(err)
	}

	r.Start()
	defer r.Stop()
	time.Sleep(50 * time.Millisecond)

	if ran != 0 {
		t.Errorf("previously-run test must not execute on Start, ran %d times", ran)
	}
	r.mu.Lock()
	seeded, ok := r.lastOK["rt2"]
	r.mu.Unlock()
	if !ok || seeded {
		t.Errorf("expected baseline seeded to false, got ok=%v val=%v", ok, seeded)
	}
}
