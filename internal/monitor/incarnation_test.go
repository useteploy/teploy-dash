package monitor

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/store"
)

// casMockStore mirrors the store-side incarnation CAS so the runner's
// behavior on applied=false can be tested deterministically. saveEntered /
// saveRelease let a test hold a check inside SaveCheck while an edit or
// delete lands — the exact D08 interleaving.
type casMockStore struct {
	mu          sync.Mutex
	monitors    map[string]store.Monitor
	clock       uint64
	checks      []store.CheckResult
	blockSave   bool
	saveEntered chan struct{}
	saveRelease chan struct{}

	saveMonitor func(*store.Monitor) // optional hook for store-side bumps
}

func newCasMockStore() *casMockStore {
	return &casMockStore{
		monitors:    make(map[string]store.Monitor),
		saveEntered: make(chan struct{}),
		saveRelease: make(chan struct{}),
	}
}

func (m *casMockStore) ListMonitors() ([]store.Monitor, error) { return nil, nil }
func (m *casMockStore) GetMonitor(id string) (*store.Monitor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mon, ok := m.monitors[id]; ok {
		cp := mon
		return &cp, nil
	}
	return nil, fmt.Errorf("monitor %q: %w", id, os.ErrNotExist)
}
func (m *casMockStore) SaveMonitor(mon *store.Monitor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.monitors[mon.ID]
	next := cur.Incarnation + 1
	if m.clock >= next {
		next = m.clock + 1
	}
	m.clock = next
	mon.Incarnation = next
	m.monitors[mon.ID] = *mon
	return nil
}
func (m *casMockStore) DeleteMonitor(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.monitors, id)
	return nil
}
func (m *casMockStore) SaveCheck(r store.CheckResult) (bool, error) {
	if m.blockSave {
		m.saveEntered <- struct{}{}
		<-m.saveRelease
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.monitors[r.MonitorID]
	if !ok || cur.Incarnation != r.Incarnation {
		return false, nil
	}
	m.checks = append(m.checks, r)
	return true, nil
}
func (m *casMockStore) GetChecks(string, time.Time, int) ([]store.CheckResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.CheckResult(nil), m.checks...), nil
}
func (m *casMockStore) GetStats(string, time.Time) (*store.UptimeStats, error) { return nil, nil }
func (m *casMockStore) ListRestoreTests() ([]store.RestoreTest, error)         { return nil, nil }
func (m *casMockStore) GetRestoreTest(string) (*store.RestoreTest, error)      { return nil, nil }
func (m *casMockStore) SaveRestoreTest(store.RestoreTest) error                { return nil }
func (m *casMockStore) SaveRestoreTestResult(string, store.RestoreTest) (bool, error) {
	return true, nil
}
func (m *casMockStore) DeleteRestoreTest(string) error { return nil }
func (m *casMockStore) Close() error                   { return nil }
func (m *casMockStore) Cleanup() error                 { return nil }

func (m *casMockStore) saved() []store.CheckResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.CheckResult(nil), m.checks...)
}

// recordingAlerter captures Send calls so tests can assert no spurious alert
// fired off a dropped check.
type recordingAlerter struct {
	mu     sync.Mutex
	events []alert.Event
}

func (a *recordingAlerter) Send(ev alert.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

func (a *recordingAlerter) sent() []alert.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]alert.Event(nil), a.events...)
}

// D08 race (edit): a check is in flight when the monitor is edited. The
// store's incarnation CAS drops the stale row; the runner must not touch the
// transition baseline or alert off it, and the NEW incarnation's schedule
// continues exactly (its own check persists under the new incarnation).
func TestRunCheck_EditDuringCheck_DropsStaleRowAndKeepsNewSchedule(t *testing.T) {
	ms := newCasMockStore()
	al := &recordingAlerter{}
	r := New(ms)
	r.SetAlerter(al)

	old := store.Monitor{ID: "m1", Name: "t", Type: "http", Target: "http://127.0.0.1:1", Interval: time.Minute, Timeout: time.Second, AllowInternal: true}
	if err := ms.SaveMonitor(&old); err != nil {
		t.Fatalf("save monitor: %v", err)
	}

	// Seed a baseline so a transition would fire if the runner misbehaved.
	r.mu.Lock()
	r.generations["m1"] = 1
	r.lastStat["m1"] = "up"
	r.mu.Unlock()

	ms.blockSave = true
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runCheck(old, 1) // scheduler of the OLD incarnation (gen 1)
	}()
	<-ms.saveEntered // the check is now inside SaveCheck

	// The edit lands mid-save: new config, new incarnation, generation bump.
	edited := old
	edited.Target = "http://127.0.0.2:1"
	if err := ms.SaveMonitor(&edited); err != nil {
		t.Fatalf("edit monitor: %v", err)
	}
	r.mu.Lock()
	r.generations["m1"] = 2
	r.mu.Unlock()

	ms.blockSave = false
	close(ms.saveRelease)
	<-done

	if got := ms.saved(); len(got) != 0 {
		t.Fatalf("stale check must not persist, got %d rows", len(got))
	}

	// The stale check must not fire a transition alert (baseline was "up",
	// the stale result is "down").
	if evs := al.sent(); len(evs) != 0 {
		t.Fatalf("stale check must not alert, got %d events", len(evs))
	}

	// The new incarnation's schedule continues: its check persists.
	r.runCheck(edited, 2)
	saved := ms.saved()
	if len(saved) != 1 {
		t.Fatalf("new incarnation's check must persist, got %d rows", len(saved))
	}
	if saved[0].Incarnation != edited.Incarnation {
		t.Fatalf("persisted check carries incarnation %d, want %d", saved[0].Incarnation, edited.Incarnation)
	}

	// And the new incarnation's baseline was not corrupted by the stale one.
	r.mu.Lock()
	base := r.lastStat["m1"]
	r.mu.Unlock()
	if base != "down" {
		t.Fatalf("baseline after new-incarnation check = %q, want down (the new check's verdict)", base)
	}
}

// D08 race (delete): a check is in flight when the monitor is deleted. The
// store drops the row (monitor gone); the runner must not write a baseline
// for the deleted ID and must not alert. No further checks happen (the
// generation fence already stops the scheduler; this pins the persistence
// half).
func TestRunCheck_DeleteDuringCheck_NoResurrectedRowOrBaseline(t *testing.T) {
	ms := newCasMockStore()
	al := &recordingAlerter{}
	r := New(ms)
	r.SetAlerter(al)

	old := store.Monitor{ID: "m1", Name: "t", Type: "http", Target: "http://127.0.0.1:1", Interval: time.Minute, Timeout: time.Second, AllowInternal: true}
	if err := ms.SaveMonitor(&old); err != nil {
		t.Fatalf("save monitor: %v", err)
	}

	r.mu.Lock()
	r.generations["m1"] = 1
	r.lastStat["m1"] = "up"
	r.mu.Unlock()

	ms.blockSave = true
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runCheck(old, 1)
	}()
	<-ms.saveEntered

	ms.DeleteMonitor("m1")
	r.Remove("m1") // bumps generation, clears lastStat

	ms.blockSave = false
	close(ms.saveRelease)
	<-done

	if got := ms.saved(); len(got) != 0 {
		t.Fatalf("deleted monitor must not gain a check row, got %d", len(got))
	}
	r.mu.Lock()
	_, hasBase := r.lastStat["m1"]
	r.mu.Unlock()
	if hasBase {
		t.Fatal("deleted monitor must not have a transition baseline after the in-flight check")
	}
	if evs := al.sent(); len(evs) != 0 {
		t.Fatalf("deleted monitor's in-flight check must not alert, got %d events", len(evs))
	}
}

// The runner honors applied=false even when the generation still matches
// (the store is the authority on what landed): no baseline mutation, no
// alert, no error surfaced as a store failure.
func TestRunCheck_StoreDroppedCheck_DoesNotTouchBaseline(t *testing.T) {
	ms := newCasMockStore()
	al := &recordingAlerter{}
	r := New(ms)
	r.SetAlerter(al)

	m := store.Monitor{ID: "m1", Name: "t", Type: "http", Target: "http://127.0.0.1:1", Interval: time.Minute, Timeout: time.Second, AllowInternal: true}
	if err := ms.SaveMonitor(&m); err != nil {
		t.Fatalf("save monitor: %v", err)
	}

	r.mu.Lock()
	r.generations["m1"] = 7
	r.lastStat["m1"] = "up"
	r.mu.Unlock()

	// Run with the CURRENT generation but a stale incarnation in the config:
	// the store CAS is the only thing standing between this row and history.
	staleCfg := m
	staleCfg.Incarnation = m.Incarnation - 1
	r.runCheck(staleCfg, 7)

	if got := ms.saved(); len(got) != 0 {
		t.Fatalf("CAS-dropped check must not persist, got %d", len(got))
	}
	r.mu.Lock()
	base := r.lastStat["m1"]
	r.mu.Unlock()
	if base != "up" {
		t.Fatalf("baseline must stay 'up' when the store dropped the check, got %q", base)
	}
	if evs := al.sent(); len(evs) != 0 {
		t.Fatalf("dropped check must not alert, got %d events", len(evs))
	}
}

// D08 race (recreate): a check is in flight when its monitor is deleted and
// RECREATED under the same ID. The store's monotonic incarnation clock means
// the recreated monitor never reuses the deleted incarnation's number, so the
// in-flight save cannot CAS-match it: the stale row drops, no transition
// fires off the dead incarnation, and the recreated incarnation owns the
// schedule and the baseline from its own first check onward.
func TestRunCheck_RecreateDuringCheck_NoPhantomRowOrStolenBaseline(t *testing.T) {
	ms := newCasMockStore()
	al := &recordingAlerter{}
	r := New(ms)
	r.SetAlerter(al)

	old := store.Monitor{ID: "m1", Name: "t", Type: "http", Target: "http://127.0.0.1:1", Interval: time.Minute, Timeout: time.Second, AllowInternal: true}
	if err := ms.SaveMonitor(&old); err != nil {
		t.Fatalf("save monitor: %v", err)
	}
	oldIncarnation := old.Incarnation

	// Seed a baseline so a misbehaving stale check would fire a transition.
	r.mu.Lock()
	r.generations["m1"] = 1
	r.lastStat["m1"] = "up"
	r.mu.Unlock()

	ms.blockSave = true
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runCheck(old, 1) // scheduler of the DELETED incarnation
	}()
	<-ms.saveEntered

	// Delete + recreate under the same ID lands mid-save. The mock's store
	// clock mirrors the real one: the recreated incarnation is strictly
	// greater than the deleted one's.
	if err := ms.DeleteMonitor("m1"); err != nil {
		t.Fatalf("delete monitor: %v", err)
	}
	recreated := old
	recreated.Target = "http://127.0.0.3:1"
	if err := ms.SaveMonitor(&recreated); err != nil {
		t.Fatalf("recreate monitor: %v", err)
	}
	if recreated.Incarnation <= oldIncarnation {
		t.Fatalf("recreated incarnation %d must exceed the deleted incarnation %d (ABA guard)", recreated.Incarnation, oldIncarnation)
	}
	// The runner tears the old scheduler down and starts the new one, as
	// the delete/create API path does (Remove + startMonitor).
	r.Remove("m1")
	r.mu.Lock()
	r.generations["m1"] = 2
	r.mu.Unlock()

	ms.blockSave = false
	close(ms.saveRelease)
	<-done

	// The dead incarnation's in-flight result must not persist and must not
	// fire a transition alert.
	if got := ms.saved(); len(got) != 0 {
		t.Fatalf("deleted incarnation's check must not persist, got %d rows", len(got))
	}
	if evs := al.sent(); len(evs) != 0 {
		t.Fatalf("deleted incarnation's check must not alert, got %d events", len(evs))
	}

	// The recreated incarnation owns the schedule from here: its checks
	// persist under ITS incarnation, and the baseline it writes is its own
	// verdict — not inherited from the dead one (Remove cleared the old
	// baseline; the mock store holds no history to re-seed from).
	r.runCheck(recreated, 2)
	saved := ms.saved()
	if len(saved) != 1 {
		t.Fatalf("recreated incarnation's check must persist, got %d rows", len(saved))
	}
	if saved[0].Incarnation != recreated.Incarnation {
		t.Fatalf("persisted check carries incarnation %d, want the recreated %d", saved[0].Incarnation, recreated.Incarnation)
	}
	r.mu.Lock()
	base := r.lastStat["m1"]
	r.mu.Unlock()
	if base != "down" {
		t.Fatalf("baseline after recreated check = %q, want down (its own verdict)", base)
	}
	if evs := al.sent(); len(evs) != 0 {
		t.Fatalf("first check of a fresh incarnation must not alert without a prior baseline, got %d events", len(evs))
	}
}
