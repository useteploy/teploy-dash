package restoretest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/store"
)

func newTestRunner(t *testing.T, out string, cliErr error) (*Runner, store.Store) {
	t.Helper()
	st := store.NewFileStore(t.TempDir())
	r := New(st)
	r.runCLI = func(server, user, app, accessory, bucket, region string) (string, string, error, error) {
		// cliErr models the CLI's non-zero EXIT (verdict-bearing per the
		// documented verify-backup semantics), not a transport failure.
		return out, "boom-stderr", cliErr, nil
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

	got, err := r.RunNow(rt)
	if err != nil {
		t.Fatal(err)
	}
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

	got, err := r.RunNow(rt)
	if err != nil {
		t.Fatal(err)
	}
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

	got, err := r.RunNow(rt)
	if err != nil {
		t.Fatal(err)
	}
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

	_, _ = r.RunNow(rt)
	r.mu.Lock()
	first := r.lastOK["rt1"]
	r.mu.Unlock()
	if first {
		t.Fatal("expected lastOK=false after failed run")
	}

	r.mu.Lock()
	r.runCLI = func(server, user, app, accessory, bucket, region string) (string, string, error, error) {
		return `{"ok":true,"metric":"tables=7","date":"20260710-050000"}`, "", nil, nil
	}
	r.mu.Unlock()

	cur, _ := st.GetRestoreTest("rt1")
	_, _ = r.RunNow(*cur)
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
	r.runCLI = func(server, user, app, accessory, bucket, region string) (string, string, error, error) {
		ran++
		return `{"ok":true}`, "", nil, nil
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
	defer r.Stop(context.Background())
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

// A26: a TRANSPORT failure (timeout, missing binary) is a failed
// verification even when the child managed to print a parseable {"ok":true}
// before hanging — and failure branches must not inherit the previous run's
// metric/date/duration fields.
func TestRunNow_TransportErrorBeatsLyingStdout(t *testing.T) {
	out := `{"app":"myapp","ok":true,"metric":"tables=42","date":"20260710-040000","duration_ms":9500}`
	st := store.NewFileStore(t.TempDir())
	r := New(st)
	var calls int32
	r.runCLI = func(server, user, app, accessory, bucket, region string) (string, string, error, error) {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&calls) == 1 {
			// A clean, persisted success first, so stale fields exist.
			return out, "", nil, nil
		}
		// Second run: timeout, but the CLI already printed its (stale) JSON.
		return out, "", nil, context.DeadlineExceeded
	}
	rt := seedTest(t, st)
	first, err := r.RunNow(rt)
	if err != nil {
		t.Fatal(err)
	}
	if !first.LastOK || first.LastMetric != "tables=42" {
		t.Fatalf("first run = %+v", first)
	}

	second, err := r.RunNow(first)
	if err != nil {
		t.Fatal(err)
	}
	if second.LastOK {
		t.Fatal("transport timeout with ok:true stdout reported as successful verification")
	}
	if second.LastMetric != "" || second.LastDate != "" || second.LastDurationMs != 0 {
		t.Fatalf("failure branch inherited stale metric fields: %+v", second)
	}
	if !strings.Contains(second.LastDetail, "did not complete") {
		t.Fatalf("detail = %q", second.LastDetail)
	}

	// A transport failure whose result cannot be persisted must not alert
	// either — swap in a failing store wrapper.
	bad := &failingResultStore{Store: st}
	r2 := New(bad)
	r2.runCLI = func(server, user, app, accessory, bucket, region string) (string, string, error, error) {
		return `{"ok":false,"detail":"broken backup"}`, "", nil, nil
	}
	_, _ = r2.RunNow(seedTest(t, st))
	if bad.calls != 1 {
		t.Fatalf("SaveRestoreTestResult calls = %d", bad.calls)
	}
}

type failingResultStore struct {
	store.Store
	calls int
}

func (f *failingResultStore) SaveRestoreTestResult(id string, result store.RestoreTest) (bool, error) {
	f.calls++
	return false, errors.New("disk full")
}

// F036: a second RunNow while one is in flight returns the UNCHANGED record
// plus ErrAlreadyRunning — the caller can no longer mistake the previous
// run's verdict for the outcome of the requested verification.
func TestRunNow_ConcurrentClaimReturnsBusyError(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	release := make(chan struct{})
	r := New(st)
	r.runCLI = func(server, user, app, accessory, bucket, region string) (string, string, error, error) {
		<-release
		return `{"ok":true}`, "", nil, nil
	}
	rt := seedTest(t, st)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.RunNow(rt)
	}()
	// Wait for the claim, then collide.
	deadline := time.After(2 * time.Second)
	for {
		r.mu.Lock()
		claimed := r.running[rt.ID]
		r.mu.Unlock()
		if claimed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("run never claimed")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	got, err := r.RunNow(rt)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second concurrent run: err=%v, want ErrAlreadyRunning", err)
	}
	if !got.LastRunAt.Equal(rt.LastRunAt) {
		t.Fatal("busy response must return the unchanged record")
	}
	close(release)
	<-done
}
