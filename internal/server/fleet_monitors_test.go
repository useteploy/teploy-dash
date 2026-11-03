package server

import (
	"testing"

	"github.com/useteploy/teploy-dash/internal/remote"
	"github.com/useteploy/teploy-dash/internal/store"
)

func fleetEnv(server, errMsg string, apps ...remote.AppState) ServerObservation {
	return ServerObservation{Server: server, Error: errMsg, Apps: apps}
}

func TestFleetMonitorHostsFiltersUnprobeable(t *testing.T) {
	got := fleetMonitorHosts(" App.Acme.dev, bugs.acme.io ,dash.example.com,x.local,omni-api.internal,box.tail1.ts.net,100.1.2.3,*.wild.org,host:8080,localhost,")
	want := []string{"app.acme.dev", "bugs.acme.io"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
}

func TestReconcileFleetMonitors(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	s := &Server{store: st}
	manual := store.Monitor{ID: "manual", Name: "m", Type: "http", Target: "https://x.acme.dev/", Enabled: true}
	if err := st.SaveMonitor(&manual); err != nil {
		t.Fatal(err)
	}

	sweep := []ServerObservation{
		fleetEnv("a", "", remote.AppState{App: "akiroo", Domain: "app.acme.dev, bugs.acme.io"}),
		fleetEnv("b", "", remote.AppState{App: "site", Domain: "site.acme.dev"}),
	}
	added, removed, err := s.reconcileFleetMonitors(sweep)
	if err != nil || added != 3 || removed != 0 {
		t.Fatalf("first sweep: added=%d removed=%d err=%v", added, removed, err)
	}
	m, err := st.GetMonitor(fleetMonitorID("bugs.acme.io"))
	if err != nil || m.App != "akiroo" || !m.Reachable || m.Target != "https://bugs.acme.io/" || !m.Enabled {
		t.Fatalf("fleet monitor = %+v, %v", m, err)
	}

	// A person disables one: the reconciler must not re-enable it.
	m.Enabled = false
	if err := st.SaveMonitor(m); err != nil {
		t.Fatal(err)
	}
	if added, _, _ := s.reconcileFleetMonitors(sweep); added != 0 {
		t.Fatalf("second sweep re-added %d", added)
	}
	if m2, _ := st.GetMonitor(m.ID); m2.Enabled {
		t.Fatal("reconciler re-enabled a monitor a person disabled")
	}

	// Server b unreachable and its app gone from a: nothing is retired.
	partial := []ServerObservation{
		fleetEnv("a", "", remote.AppState{App: "akiroo", Domain: "app.acme.dev"}),
		fleetEnv("b", "ssh: timeout"),
	}
	if _, removed, _ := s.reconcileFleetMonitors(partial); removed != 0 {
		t.Fatalf("incomplete sweep retired %d monitors", removed)
	}

	// Complete sweep without bugs.acme.io and site: both retired, the
	// manual monitor untouched.
	complete := []ServerObservation{
		fleetEnv("a", "", remote.AppState{App: "akiroo", Domain: "app.acme.dev"}),
		fleetEnv("b", ""),
	}
	if _, removed, _ := s.reconcileFleetMonitors(complete); removed != 2 {
		t.Fatalf("complete sweep retired %d, want 2", removed)
	}
	list, _ := st.ListMonitors()
	ids := map[string]bool{}
	for _, m := range list {
		ids[m.ID] = true
	}
	if len(list) != 2 || !ids["manual"] || !ids[fleetMonitorID("app.acme.dev")] {
		t.Fatalf("remaining monitors = %v", ids)
	}
}
