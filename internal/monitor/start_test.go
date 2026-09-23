package monitor

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/store"
)

// TestStart_LogsOnlyEnabledMonitorCount verifies that Start() logs the count
// of monitors it actually started (i.e. those with Enabled: true), not the
// total number of monitors returned by the store.
func TestStart_LogsOnlyEnabledMonitorCount(t *testing.T) {
	ms := &mockStore{
		monitors: []store.Monitor{
			{ID: "m1", Name: "enabled", Type: "http", Target: "http://127.0.0.1:1", Enabled: true},
			{ID: "m2", Name: "disabled-1", Type: "http", Target: "http://127.0.0.1:1", Enabled: false},
			{ID: "m3", Name: "disabled-2", Type: "http", Target: "http://127.0.0.1:1", Enabled: false},
		},
	}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	r := New(ms)
	r.Start()
	r.Stop(context.Background())

	if !strings.Contains(buf.String(), "Started 1 monitors") {
		t.Errorf("expected log to contain %q, got %q", "Started 1 monitors", buf.String())
	}
}

// D08 (restart preservation, piece c): the intended monitor set survives a
// backend restart exactly. A deleted monitor stays deleted (no scheduler, no
// resurrected checks — including nothing seeded from its old history), an
// edited monitor keeps its edit, and the restarted scheduler stamps checks
// with the monitor's CURRENT incarnation (the store CAS then guards the new
// schedule against any stale writer).
func TestStart_PreservesIntendedSetAcrossRestart(t *testing.T) {
	fs := store.NewFileStore(t.TempDir())

	kept := store.Monitor{ID: "kept", Name: "Kept", Type: "http", Target: "http://127.0.0.1:1", Interval: time.Minute, Timeout: time.Second, AllowInternal: true, Enabled: true}
	if err := fs.SaveMonitor(&kept); err != nil {
		t.Fatal(err)
	}
	deleted := store.Monitor{ID: "gone", Name: "Gone", Type: "http", Target: "http://127.0.0.1:1", Interval: time.Minute, Timeout: time.Second, AllowInternal: true, Enabled: true}
	if err := fs.SaveMonitor(&deleted); err != nil {
		t.Fatal(err)
	}
	if applied, err := fs.SaveCheck(store.CheckResult{MonitorID: "gone", Incarnation: deleted.Incarnation, Status: "down", CheckedAt: time.Now()}); err != nil || !applied {
		t.Fatalf("seed check for gone: applied=%v err=%v", applied, err)
	}
	if err := fs.DeleteMonitor("gone"); err != nil {
		t.Fatal(err)
	}
	// The edit that landed before the restart.
	edited := kept
	edited.Interval = 2 * time.Minute
	if err := fs.SaveMonitor(&edited); err != nil {
		t.Fatal(err)
	}
	if edited.Incarnation <= kept.Incarnation {
		t.Fatal("edit must bump the incarnation")
	}

	// "Restart": a fresh runner over the same persisted state.
	r := New(fs)
	r.Start()
	defer r.Stop(context.Background())

	r.mu.Lock()
	_, keptScheduled := r.stopChs["kept"]
	_, goneScheduled := r.stopChs["gone"]
	r.mu.Unlock()
	if !keptScheduled {
		t.Fatal("edited monitor must be scheduled after restart")
	}
	if goneScheduled {
		t.Fatal("deleted monitor must not be scheduled after restart")
	}

	// The kept monitor's immediate first check lands under the EDITED
	// incarnation; the deleted monitor gains nothing.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if checks, _ := fs.GetChecks("kept", time.Time{}, 1); len(checks) == 1 {
			if checks[0].Incarnation != edited.Incarnation {
				t.Fatalf("restarted check carries incarnation %d, want the edited %d", checks[0].Incarnation, edited.Incarnation)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if checks, _ := fs.GetChecks("kept", time.Time{}, 1); len(checks) != 1 {
		t.Fatal("restarted monitor's first check never landed")
	}
	if checks, _ := fs.GetChecks("gone", time.Time{}, 0); len(checks) != 0 {
		t.Fatalf("deleted monitor gained %d checks after restart", len(checks))
	}
}
