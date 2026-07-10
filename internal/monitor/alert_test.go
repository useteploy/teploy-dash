package monitor

import (
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/store"
)

// mockStore is a minimal in-memory Store for testing check persistence
// doesn't affect the state transition logic we care about.
type mockStore struct{ checks []store.CheckResult }

func (m *mockStore) ListMonitors() ([]store.Monitor, error)       { return nil, nil }
func (m *mockStore) GetMonitor(id string) (*store.Monitor, error) { return nil, nil }
func (m *mockStore) SaveMonitor(store.Monitor) error              { return nil }
func (m *mockStore) DeleteMonitor(string) error                   { return nil }
func (m *mockStore) SaveCheck(c store.CheckResult) error {
	m.checks = append(m.checks, c)
	return nil
}
func (m *mockStore) GetChecks(string, time.Time, int) ([]store.CheckResult, error) {
	return nil, nil
}
func (m *mockStore) GetStats(string, time.Time) (*store.UptimeStats, error) { return nil, nil }
func (m *mockStore) ListRestoreTests() ([]store.RestoreTest, error)          { return nil, nil }
func (m *mockStore) GetRestoreTest(string) (*store.RestoreTest, error)       { return nil, nil }
func (m *mockStore) SaveRestoreTest(store.RestoreTest) error                 { return nil }
func (m *mockStore) DeleteRestoreTest(string) error                          { return nil }
func (m *mockStore) Close() error                                           { return nil }
func (m *mockStore) Cleanup() error                                         { return nil }

// newNoopAlerter returns a Dispatcher with no channels configured so
// Send() short-circuits to a no-op. Sufficient for verifying the
// runCheck transition guard is exercised without hitting a real webhook.
func newNoopAlerter() *alert.Dispatcher {
	return alert.New(alert.Config{})
}

// Exercises the runCheck flow: two identical results should NOT produce
// an alert, but a change should. We can't easily count dispatches without
// a real network target, so we verify state tracking behaviour directly.
func TestRunner_TracksLastStatus(t *testing.T) {
	r := New(&mockStore{})

	m := store.Monitor{ID: "m1", Name: "t", Type: "http", Target: "http://127.0.0.1:1"}

	// First run — no previous state, no transition yet.
	r.runCheck(m)
	r.mu.Lock()
	first := r.lastStat[m.ID]
	r.mu.Unlock()
	if first != "down" {
		// http://127.0.0.1:1 is guaranteed unreachable
		t.Errorf("expected first status 'down', got %q", first)
	}

	// Second run with same result — lastStat stays the same.
	r.runCheck(m)
	r.mu.Lock()
	second := r.lastStat[m.ID]
	r.mu.Unlock()
	if second != first {
		t.Errorf("status shouldn't change between identical checks, got %q -> %q", first, second)
	}
}

// Alert-on-transition test: seed lastStat to "up", then run a check that
// produces "down" — and verify the transition is detected. We don't
// actually verify alert.Send is called because Dispatcher fires goroutines
// and swallows errors from its webhook; the guard in runCheck is what we
// care about (`alerter != nil && prev != "" && prev != result.Status`).
func TestRunner_DetectsTransitionFromSeededState(t *testing.T) {
	r := New(&mockStore{})
	r.SetAlerter(newNoopAlerter())

	m := store.Monitor{ID: "m1", Name: "t", Type: "http", Target: "http://127.0.0.1:1"}

	// Seed: previous status was "up"
	r.mu.Lock()
	r.lastStat[m.ID] = "up"
	r.mu.Unlock()

	r.runCheck(m)

	r.mu.Lock()
	after := r.lastStat[m.ID]
	r.mu.Unlock()
	if after != "down" {
		t.Errorf("expected status to transition to 'down', got %q", after)
	}
}
