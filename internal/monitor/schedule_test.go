package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/store"
)

// anchorStore is a file store wrapper exposing when checks land, so the
// schedule tests can assert TIMING (whether a check fired inside a window)
// without polling the check rows for their own sake.
type anchorStore struct {
	*store.FileStore
	saved   chan store.CheckResult
	stopped bool
}

func (a *anchorStore) SaveCheck(c store.CheckResult) (bool, error) {
	applied, err := a.FileStore.SaveCheck(c)
	select {
	case a.saved <- c:
	default:
	}
	return applied, err
}

func (a *anchorStore) waitCheck(t *testing.T, within time.Duration) bool {
	t.Helper()
	select {
	case <-a.saved:
		return true
	case <-time.After(within):
		return false
	}
}

// drain discards buffered check events (the seeds written before the runner
// started) so waitCheck observes only checks the SCHEDULER fires.
func (a *anchorStore) drain() {
	for {
		select {
		case <-a.saved:
		default:
			return
		}
	}
}

func newAnchorStore(t *testing.T) *anchorStore {
	t.Helper()
	return &anchorStore{FileStore: store.NewFileStore(t.TempDir()), saved: make(chan store.CheckResult, 16)}
}

// D08 (durable next_due_at, pass-7 A25 residual): a monitor with a RECENT
// persisted check waits the true remainder of anchor+interval after a
// restart — restarting dash more often than the interval must not multiply
// checks.
func TestStart_FreshAnchorWaitsTrueRemainder(t *testing.T) {
	as := newAnchorStore(t)
	m := store.Monitor{ID: "m", Name: "m", Type: "http", Target: "http://127.0.0.1:1", Interval: time.Minute, Timeout: time.Second, AllowInternal: true, Enabled: true}
	if err := as.SaveMonitor(&m); err != nil {
		t.Fatal(err)
	}
	// A check landed 5s ago on a 60s interval: 55s remain.
	if applied, err := as.SaveCheck(store.CheckResult{MonitorID: "m", Incarnation: m.Incarnation, Status: "down", CheckedAt: time.Now().Add(-5 * time.Second)}); err != nil || !applied {
		t.Fatalf("seed check: applied=%v err=%v", applied, err)
	}

	as.drain()
	r := New(as)
	r.Start()
	defer r.Stop(context.Background())

	if as.waitCheck(t, 2*time.Second) {
		t.Fatal("restarted monitor with a fresh anchor checked immediately — schedule did not survive the restart")
	}
}

// D08: the overdue and no-history cases still fire promptly — the remainder
// wait must never postpone an already-due (or never-run) monitor.
func TestStart_OverdueAndFreshMonitorsCheckImmediately(t *testing.T) {
	as := newAnchorStore(t)
	overdue := store.Monitor{ID: "overdue", Name: "o", Type: "http", Target: "http://127.0.0.1:1", Interval: time.Minute, Timeout: time.Second, AllowInternal: true, Enabled: true}
	if err := as.SaveMonitor(&overdue); err != nil {
		t.Fatal(err)
	}
	// Last check 2 intervals ago: due the moment the scheduler starts.
	if applied, err := as.SaveCheck(store.CheckResult{MonitorID: "overdue", Incarnation: overdue.Incarnation, Status: "down", CheckedAt: time.Now().Add(-2 * time.Minute)}); err != nil || !applied {
		t.Fatalf("seed overdue check: applied=%v err=%v", applied, err)
	}
	neverRun := store.Monitor{ID: "never", Name: "n", Type: "http", Target: "http://127.0.0.1:1", Interval: time.Minute, Timeout: time.Second, AllowInternal: true, Enabled: true}
	if err := as.SaveMonitor(&neverRun); err != nil {
		t.Fatal(err)
	}

	as.drain()
	r := New(as)
	r.Start()
	defer r.Stop(context.Background())

	if !as.waitCheck(t, 10*time.Second) {
		t.Fatal("overdue monitor did not check promptly after restart")
	}
	if !as.waitCheck(t, 10*time.Second) {
		t.Fatal("never-run monitor did not check immediately")
	}
	// Exactly the two due checks — no third (the remainder wait holds for
	// the overdue monitor's NEXT fire, one full interval later).
	if as.waitCheck(t, 2*time.Second) {
		t.Fatal("a third check fired inside the interval — cadence broken")
	}
}

// D08: a reload (edit) keeps the cadence — the anchor is the last persisted
// check, not the reload time, so saving a config edit no longer triggers an
// extra check ahead of schedule.
func TestReload_KeepsCadenceFromAnchor(t *testing.T) {
	as := newAnchorStore(t)
	m := store.Monitor{ID: "m", Name: "m", Type: "http", Target: "http://127.0.0.1:1", Interval: time.Minute, Timeout: time.Second, AllowInternal: true, Enabled: true}
	if err := as.SaveMonitor(&m); err != nil {
		t.Fatal(err)
	}
	if applied, err := as.SaveCheck(store.CheckResult{MonitorID: "m", Incarnation: m.Incarnation, Status: "down", CheckedAt: time.Now().Add(-10 * time.Second)}); err != nil || !applied {
		t.Fatalf("seed check: applied=%v err=%v", applied, err)
	}

	as.drain()
	r := New(as)
	edited := m
	edited.Timeout = 2 * time.Second
	if err := as.SaveMonitor(&edited); err != nil {
		t.Fatal(err)
	}
	r.Reload(edited)
	defer r.Stop(context.Background())

	if as.waitCheck(t, 2*time.Second) {
		t.Fatal("reload triggered an immediate check — edits must derive from the persisted anchor")
	}
}

// The shared delay helper: same contract as the restore runner's.
func TestInitialDelay(t *testing.T) {
	now := time.Now()
	if d := initialDelay(time.Time{}, time.Minute, now); d != 0 {
		t.Errorf("zero anchor must mean immediate, got %v", d)
	}
	if d := initialDelay(now.Add(-time.Minute), time.Minute, now); d != 0 {
		t.Errorf("overdue anchor must mean immediate, got %v", d)
	}
	if d := initialDelay(now.Add(-10*time.Second), time.Minute, now); d <= 40*time.Second || d > 50*time.Second {
		t.Errorf("fresh anchor must wait the true remainder (~50s), got %v", d)
	}
}
