package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/incident"
	"github.com/useteploy/teploy-dash/internal/store"
)

type syncAlerter struct {
	mu     sync.Mutex
	events []alert.Event
}

func (a *syncAlerter) Send(ev alert.Event) {
	a.mu.Lock()
	a.events = append(a.events, ev)
	a.mu.Unlock()
}

func (a *syncAlerter) kinds() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var k []string
	for _, ev := range a.events {
		k = append(k, ev.Kind+"/"+ev.Reason)
	}
	return strings.Join(k, ",")
}

func downMonitor(t *testing.T, st *store.FileStore, id string) store.Monitor {
	t.Helper()
	m := store.Monitor{ID: id, Name: id, Type: "tcp", Target: "127.0.0.1:1", AllowInternal: true,
		Interval: time.Hour, Timeout: time.Second, Enabled: true, App: "shop", Server: "deploy-1"}
	if err := st.SaveMonitor(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Default policy through the runner: 2 failed checks are silent, the 3rd
// opens one incident carrying the monitor's app/server and a v2 shape.
func TestRunnerDefaultDebounce(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	a := &syncAlerter{}
	r := New(st)
	r.SetAlerter(a)
	m := downMonitor(t, st, "m1")
	r.runCheck(m, 0)
	r.runCheck(m, 0)
	if a.kinds() != "" {
		t.Fatalf("2 failures alerted: %s", a.kinds())
	}
	r.runCheck(m, 0)
	r.runCheck(m, 0)
	if a.kinds() != alert.KindOpened+"/" {
		t.Fatalf("got %s, want exactly one open", a.kinds())
	}
	ev := a.events[0]
	if ev.App != "shop" || ev.Server != "deploy-1" || ev.Source != alert.SourceMonitor || ev.RuleID != "m1" || ev.ConsecutiveFailures != 3 {
		t.Fatalf("open event = %+v", ev)
	}
}

// Restart: the streak is restored from check history, so two failures
// before a restart plus one after open the incident.
func TestRunnerRestoresStreakFromHistory(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	m := downMonitor(t, st, "m1")
	r1 := New(st)
	r1.SetAlerter(&syncAlerter{})
	r1.runCheck(m, 0)
	r1.runCheck(m, 0)

	a := &syncAlerter{}
	tr, _ := incident.New(incident.Options{Alerter: a})
	r2 := New(st)
	r2.SetTracker(tr)
	r2.startMonitor(m) // seeds from history, schedules (interval 1h, anchor fresh)
	defer r2.Stop(context.Background())
	if f, _ := tr.Counts(RuleKey("m1")); f != 2 {
		t.Fatalf("restored fails = %d, want 2", f)
	}
	r2.mu.Lock()
	gen := r2.generations["m1"]
	r2.mu.Unlock()
	r2.runCheck(m, gen)
	if a.kinds() != alert.KindOpened+"/" {
		t.Fatalf("third failure across restart: %s", a.kinds())
	}
}

// Deleting or disabling a monitor with an announced incident retires it.
func TestRunnerRetiresOnRemoveAndDisable(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	a := &syncAlerter{}
	r := New(st)
	r.SetAlerter(a)
	m := downMonitor(t, st, "m1")
	m.FailAfter = 1
	r.runCheck(m, 0)
	r.Remove("m1")
	if a.kinds() != "incident_opened/,incident_recovered/retired" {
		t.Fatalf("remove: %s", a.kinds())
	}

	m2 := downMonitor(t, st, "m2")
	m2.FailAfter = 1
	r.runCheck(m2, 0)
	m2.Enabled = false
	r.Reload(m2)
	if !strings.HasSuffix(a.kinds(), "incident_opened/,incident_recovered/retired") || len(a.events) != 4 {
		t.Fatalf("disable: %s", a.kinds())
	}
}

func TestPushVerdictLateness(t *testing.T) {
	m := store.Monitor{ID: "p", Type: "push", Interval: 10 * time.Minute, Grace: 2 * time.Minute}
	last := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if v := pushVerdict(m, last, last.Add(12*time.Minute)); v.Status != "up" {
		t.Fatalf("inside interval+grace: %+v", v)
	}
	v := pushVerdict(m, last, last.Add(12*time.Minute+time.Second))
	if v.Status != "down" || !strings.Contains(v.Message, "no ping for") {
		t.Fatalf("late: %+v", v)
	}
}

// A push monitor goes down when no ping arrives within interval + grace,
// and a ping through its token recovers it. Unknown tokens are refused.
func TestPushMonitorLateThenPing(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	a := &syncAlerter{}
	r := New(st)
	r.SetAlerter(a)
	m := store.Monitor{ID: "p1", Name: "nightly backup", Type: "push", Interval: 10 * time.Second, Grace: 0,
		Enabled: true, PushToken: strings.Repeat("ab", 32)}
	if err := st.SaveMonitor(&m); err != nil {
		t.Fatal(err)
	}
	// Seed an old ping so the first deadline is already past.
	r.mu.Lock()
	r.lastPing["p1"] = time.Now().Add(-time.Minute)
	r.mu.Unlock()
	r.startMonitor(m)
	defer r.Stop(context.Background())

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && a.kinds() == "" {
		time.Sleep(20 * time.Millisecond)
	}
	if a.kinds() != alert.KindOpened+"/" || a.events[0].Source != alert.SourceHeartbeat || a.events[0].Target == m.PushToken {
		t.Fatalf("late push: %s %+v", a.kinds(), a.events)
	}
	if strings.Contains(a.events[0].Target, m.PushToken) {
		t.Fatal("push token leaked into the alert target")
	}
	if _, err := r.Ping("nope", true, ""); err != ErrUnknownPushToken {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := r.Ping(m.PushToken, true, "backup ok"); err != nil {
		t.Fatal(err)
	}
	if a.kinds() != "incident_opened/,incident_recovered/" {
		t.Fatalf("ping recovery: %s", a.kinds())
	}
	checks, _ := st.GetChecks("p1", time.Time{}, 1)
	if len(checks) != 1 || checks[0].Status != "up" || checks[0].Message != "backup ok" {
		t.Fatalf("ping check = %+v", checks)
	}
	// An explicit failure report records down.
	if _, err := r.Ping(m.PushToken, false, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(a.kinds(), ",incident_opened/") {
		t.Fatalf("reported failure: %s", a.kinds())
	}
}

// First start with incident tracking: a monitor whose history is already
// failing is adopted unannounced, and EndAdoption announces it.
func TestRunnerAdoptsExistingOutageOnFirstStart(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	m := downMonitor(t, st, "m1")
	r1 := New(st)
	r1.SetAlerter(&syncAlerter{})
	for i := 0; i < 4; i++ {
		r1.runCheck(m, 0)
	}
	a := &syncAlerter{}
	tr, err := incident.New(incident.Options{Dir: t.TempDir(), Alerter: a})
	if err != nil || !tr.Adopting() {
		t.Fatalf("want a first-run tracker: %v", err)
	}
	r2 := New(st)
	r2.SetTracker(tr)
	r2.startMonitor(m)
	defer r2.Stop(context.Background())
	if open := tr.Open(); len(open) != 1 || open[0].Notified || open[0].ConsecutiveFailures != 4 {
		t.Fatalf("adopted = %+v", open)
	}
	if a.kinds() != "" {
		t.Fatalf("adoption alerted: %s", a.kinds())
	}
	tr.EndAdoption()
	if a.kinds() != alert.KindOpened+"/" {
		t.Fatalf("after adoption: %s", a.kinds())
	}
}
