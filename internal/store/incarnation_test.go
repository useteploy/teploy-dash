package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// D08 / A19+R34 (store side): SaveCheck is a compare-and-swap against the
// monitor's current incarnation. A check that was in flight when the monitor
// was deleted must not resurrect its history (the pre-fix SaveCheck's O_CREATE
// re-created history/m1.jsonl and landed the dead incarnation's row, which a
// same-ID recreate then inherited as a phantom transition baseline).
func TestSaveCheckDeletedMonitorDoesNotResurrectHistory(t *testing.T) {
	fs := NewFileStore(t.TempDir())
	m := Monitor{ID: "m1", Type: "http", Target: "https://example.com", Interval: time.Minute, Timeout: 10 * time.Second, Enabled: true}
	if err := fs.SaveMonitor(&m); err != nil {
		t.Fatalf("save monitor: %v", err)
	}
	if err := fs.DeleteMonitor("m1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	res := CheckResult{MonitorID: "m1", Incarnation: m.Incarnation, Status: "down", CheckedAt: time.Now()}
	applied, err := fs.SaveCheck(res)
	if err != nil {
		t.Fatalf("save check: %v", err)
	}
	if applied {
		t.Fatal("check for a deleted monitor must be dropped, not applied")
	}
	if _, err := os.Stat(filepath.Join(fs.dir, "history", "m1.jsonl")); !os.IsNotExist(err) {
		t.Fatal("deleted monitor's history was resurrected by an in-flight check")
	}
}

// D08 / A19+R34: an edit bumps the incarnation; a check that ran under the
// pre-edit configuration must not land on the edited monitor's history.
func TestSaveCheckRequiresCurrentIncarnation(t *testing.T) {
	fs := NewFileStore(t.TempDir())
	m := Monitor{ID: "m1", Type: "http", Target: "https://old.example.com", Interval: time.Minute, Timeout: 10 * time.Second, Enabled: true}
	if err := fs.SaveMonitor(&m); err != nil {
		t.Fatalf("save monitor: %v", err)
	}

	stale := CheckResult{MonitorID: "m1", Incarnation: m.Incarnation, Status: "down", CheckedAt: time.Now()}

	// The edit lands while the check is in flight.
	m.Target = "https://new.example.com"
	if err := fs.SaveMonitor(&m); err != nil {
		t.Fatalf("edit monitor: %v", err)
	}

	applied, err := fs.SaveCheck(stale)
	if err != nil {
		t.Fatalf("save check: %v", err)
	}
	if applied {
		t.Fatal("stale-incarnation check must be dropped by the CAS")
	}
	checks, err := fs.GetChecks("m1", time.Time{}, 0)
	if err != nil {
		t.Fatalf("get checks: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("stale check landed on the edited monitor's history: %d rows", len(checks))
	}

	// The new incarnation's checks still apply.
	fresh := CheckResult{MonitorID: "m1", Incarnation: m.Incarnation, Status: "up", CheckedAt: time.Now()}
	applied, err = fs.SaveCheck(fresh)
	if err != nil || !applied {
		t.Fatalf("fresh-incarnation check must apply: applied=%v err=%v", applied, err)
	}
}

// D08 / F037-R37 family (applied to monitors): incarnations are monotonic per
// store, so a delete+recreate with the same ID can never reuse the deleted
// incarnation's number (no ABA: an in-flight check from the old incarnation
// can never CAS-match the recreated monitor).
func TestSaveMonitorIncarnationMonotonicAcrossDeleteRecreate(t *testing.T) {
	fs := NewFileStore(t.TempDir())
	m := Monitor{ID: "m1", Type: "http", Target: "https://example.com", Interval: time.Minute, Timeout: 10 * time.Second, Enabled: true}
	if err := fs.SaveMonitor(&m); err != nil {
		t.Fatalf("create: %v", err)
	}
	first := m.Incarnation
	if first == 0 {
		t.Fatal("SaveMonitor must assign a nonzero incarnation")
	}

	m.Interval = 2 * time.Minute
	if err := fs.SaveMonitor(&m); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if m.Incarnation != first+1 {
		t.Fatalf("edit must bump the incarnation: got %d, want %d", m.Incarnation, first+1)
	}

	if err := fs.DeleteMonitor("m1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	recreated := Monitor{ID: "m1", Type: "http", Target: "https://other.example.com", Interval: time.Minute, Timeout: 10 * time.Second, Enabled: true}
	if err := fs.SaveMonitor(&recreated); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if recreated.Incarnation <= m.Incarnation {
		t.Fatalf("recreate must not reuse an incarnation (ABA): got %d, want > %d", recreated.Incarnation, m.Incarnation)
	}

	// A client-supplied incarnation is never trusted — the store assigns.
	hijack := Monitor{ID: "m2", Type: "http", Target: "https://example.com", Interval: time.Minute, Timeout: 10 * time.Second, Enabled: true, Incarnation: 1 << 40}
	if err := fs.SaveMonitor(&hijack); err != nil {
		t.Fatalf("save m2: %v", err)
	}
	if hijack.Incarnation == 1<<40 || hijack.Incarnation == 0 {
		t.Fatalf("store must assign the incarnation, not trust the payload: got %d", hijack.Incarnation)
	}
}
