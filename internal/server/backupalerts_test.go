package server

// backupalerts_test.go — Dash-owned backup incidents (P2.8): one incident per
// cause from the fleet status, adoption on the first complete read, recovery
// only on fresh evidence, retirement when a subject disappears.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/incident"
	"github.com/useteploy/teploy-dash/internal/remote"
	"github.com/useteploy/teploy-dash/internal/store"
)

func newFileStoreForTest(t *testing.T) store.Store {
	t.Helper()
	return store.NewFileStore(t.TempDir())
}

func cliExit(msg string) error {
	return fmt.Errorf("exit 1: %s", msg)
}

func openIncidents(t *testing.T, s *Server) map[string]string {
	t.Helper()
	out := map[string]string{}
	if s.alerts.tracker == nil {
		t.Fatal("no tracker")
	}
	for _, inc := range s.alerts.tracker.Open() {
		out[inc.Key] = inc.Message
	}
	return out
}

// The corpus envelope: omni_postgres failing, shop-db stale, redis-cache and
// analytics-ch uncovered, target and tooling fine, blog-db and the adopted
// mysql covered, volumes excluded.
func TestBackupIncidents_OnePerCause(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	unreachable := func() (*cli.Result, error) { return nil, cliExit("ssh: connect refused") }
	c.status["stage"] = unreachable
	s := newBackupsServer(t, c)
	s.sweepBackups(context.Background())

	inc := openIncidents(t, s)
	want := map[string]bool{
		"backup:prod:fail:omni_postgres":     true,
		"backup:prod:stale:shop-db":          true,
		"backup:prod:uncovered:redis-cache":  true,
		"backup:prod:uncovered:analytics-ch": true,
	}
	for key := range want {
		if inc[key] == "" {
			t.Errorf("missing incident %s; have %v", key, inc)
		}
	}
	for _, forbidden := range []string{"backup:prod:fail:blog-db", "backup:prod:stale:blog-db", "backup:prod:target", "backup:prod:fail:blog/volumes"} {
		if inc[forbidden] != "" {
			t.Errorf("a healthy or excluded subject must not open: %s", forbidden)
		}
	}
	if len(inc) != len(want) {
		t.Fatalf("exactly one incident per cause: %v", inc)
	}
	if !strings.Contains(inc["backup:prod:fail:omni_postgres"], "the last backup failed") {
		t.Errorf("message = %q", inc["backup:prod:fail:omni_postgres"])
	}
}

// A later good read recovers every open rule and retires the ones whose
// subject disappeared.
func TestBackupIncidents_RecoverAndRetire(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	c.status["stage"] = func() (*cli.Result, error) { return nil, cliExit("ssh: connect refused") }
	s := newBackupsServer(t, c)
	s.sweepBackups(context.Background())
	if len(openIncidents(t, s)) != 4 {
		t.Fatalf("baseline incidents: %v", openIncidents(t, s))
	}

	// Everything healthy now, and shop-db left the server entirely.
	fixed := mustDecodeStatus(t, "status-fleet.json")
	fixed.Entries = fixed.Entries[:1] // blog-db only, covered
	if fixed.Entries[0].Name != "blog-db" || fixed.Entries[0].Coverage != "covered" {
		t.Fatal("fixture order changed")
	}
	now := backupsNow.Add(backupSweepInterval)
	fixed.Host = "prod"
	s.recordBackupStatus("prod", fixed, nil, now)
	s.evaluateBackupIncidents("prod", now)

	inc := openIncidents(t, s)
	if len(inc) != 0 {
		t.Fatalf("recovered and retired: %v", inc)
	}
}

// Stale evidence keeps a failing incident open but never recovers one: a
// server Dash cannot read has proved nothing since its last good status.
func TestBackupIncidents_StaleEvidenceRecoversNothing(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	// First read: everything healthy except one failing entry.
	env := mustDecodeStatus(t, "status-fleet.json")
	env.Entries = env.Entries[:1]
	env.Entries[0].Coverage = "failing"
	s.recordBackupStatus("prod", env, nil, backupsNow)
	s.evaluateBackupIncidents("prod", backupsNow)
	if len(openIncidents(t, s)) != 1 {
		t.Fatalf("one failing incident: %v", openIncidents(t, s))
	}
	// The next read fails: the incident stays open...
	s.recordBackupStatus("prod", nil, context.DeadlineExceeded, backupsNow.Add(backupSweepInterval))
	s.evaluateBackupIncidents("prod", backupsNow.Add(backupSweepInterval))
	if len(openIncidents(t, s)) != 1 {
		t.Fatalf("an unread server recovers nothing: %v", openIncidents(t, s))
	}
	// ...and a STALE good status (older than the trust bound) recovers
	// nothing either.
	s.recordBackupStatus("prod", env, nil, backupsNow.Add(-3*backupSweepInterval-2*backupServerTimeout))
	env.Entries[0].Coverage = "covered"
	s.evaluateBackupIncidents("prod", backupsNow.Add(2*backupSweepInterval))
	if len(openIncidents(t, s)) != 1 {
		t.Fatalf("stale evidence recovers nothing: %v", openIncidents(t, s))
	}
}

// The first complete read of a server adopts what was already broken into one
// summary instead of an alert per cause.
func TestBackupIncidents_FirstReadAdopts(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	recordFreshStatus(t, s, "prod")
	s.evaluateBackupIncidents("prod", backupsNow)
	inc := openIncidents(t, s)
	if len(inc) != 4 {
		t.Fatalf("adopted as open, unannounced: %v", inc)
	}
	// Adopted incidents carry no individual announcement (no channel is
	// configured here): they wait for the summary path.
	for _, i := range s.alerts.tracker.Open() {
		if i.Notified {
			t.Fatalf("an adopted incident must not have alerted individually: %+v", i)
		}
	}
	// The server is marked adopted: a NEW failure after this opens its own
	// incident (it was not among the adopted four).
	env := mustDecodeStatus(t, "status-fleet.json")
	env.Entries = env.Entries[:1]
	env.Entries[0].Coverage = "failing"
	now := backupsNow.Add(backupSweepInterval)
	s.recordBackupStatus("prod", env, nil, now)
	s.evaluateBackupIncidents("prod", now)
	if openIncidents(t, s)["backup:prod:fail:blog-db"] == "" {
		t.Fatalf("a new failure after adoption opens normally: %v", openIncidents(t, s))
	}
}

// A target that went unreachable opens the target rule; a server whose
// tooling broke opens the tools rule.
func TestBackupIncidents_TargetAndTools(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	env := mustDecodeStatus(t, "status-fleet.json")
	env.Target.Reachable = false
	env.Target.Error = "could not list the bucket: 403"
	env.AWSCLIProblem = "the aws CLI is missing on the server"
	env.Entries = nil
	s.recordBackupStatus("prod", env, nil, backupsNow)
	s.evaluateBackupIncidents("prod", backupsNow)
	inc := openIncidents(t, s)
	if inc["backup:prod:target:"] == "" || !strings.Contains(inc["backup:prod:target:"], "403") {
		t.Fatalf("target incident = %q", inc["backup:prod:target:"])
	}
	if inc["backup:prod:tools:"] == "" {
		t.Fatalf("tools incident missing: %v", inc)
	}
}

// The automatic restore tests alert through the restore-test source (one
// owner per signal): a failed run opens, the next pass recovers.
func TestBackupAutoTestIncident_FailThenRecover(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	recordFreshStatus(t, s, "prod")
	s.reconcileBackupTests(backupsNow)
	night := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	s.backups.resources = func(context.Context, remote.ServerConn) (backupResources, error) {
		return backupResources{FreeMemMB: 8192, FreeDiskKB: 100 << 20}, nil
	}
	var wp *backupAutoTest
	for _, test := range testsOf(t, s) {
		if test.Container == "wordpress-shared-mysql" {
			wp = test
		}
	}
	if wp == nil {
		t.Fatal("no wordpress test")
	}
	c.verify = map[string]func([]string) (*cli.Result, error){
		"wordpress-shared-mysql": func([]string) (*cli.Result, error) {
			return &cli.Result{ExitCode: 1, Stdout: `{"accessory":"wordpress-shared-mysql","ok":false,"detail":"restored copy had no tables"}`}, nil
		},
	}
	recordFreshStatusAt(t, s, "prod", night)
	s.runBackupTest(context.Background(), wp, night)
	inc := openIncidents(t, s)
	if inc["restore-test:"+wp.ID] == "" {
		t.Fatalf("restore-test incident missing: %v", inc)
	}
	for key := range inc {
		if !strings.HasPrefix(key, "restore-test:") {
			t.Fatalf("the failing verify is the restore-test source's to report, not %s", key)
		}
	}
	// Green next week: recovered.
	c.verify = nil
	recordFreshStatusAt(t, s, "prod", night.Add(backupTestWeekly))
	s.runBackupTest(context.Background(), wp, night.Add(backupTestWeekly))
	if len(openIncidents(t, s)) != 0 {
		t.Fatalf("recovered: %v", openIncidents(t, s))
	}
}

// The heartbeat monitor dash owns for a server: interval from the policy,
// stale grace of one interval, and its failure is the missed-heartbeat alert
// (source heartbeat, not backup).
func TestBackupHeartbeatMonitor_OwnedRecord(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	s.store = newFileStoreForTest(t)
	writeNotifications(t, `{"channels":[],"public_url":"https://dash.example.internal"}`)
	m, err := s.ensureBackupHeartbeat("prod", "1h")
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != "push" || m.Interval != time.Hour || m.Grace != time.Hour || m.Server != "prod" {
		t.Fatalf("monitor = %+v", m)
	}
	if !strings.HasPrefix(m.ID, "backups-") || m.PushToken == "" {
		t.Fatalf("monitor = %+v", m)
	}
	// Idempotent: the same record updates, a second one is not created.
	again, err := s.ensureBackupHeartbeat("prod", "12h")
	if err != nil || again.ID != m.ID || again.Interval != 12*time.Hour {
		t.Fatalf("again = %+v err = %v", again, err)
	}
	if monitors, err := s.store.ListMonitors(); err != nil || len(monitors) != 1 {
		t.Fatalf("monitors = %d err = %v", len(monitors), err)
	}
	if !s.isBackupHeartbeatToken(m.PushToken) {
		t.Fatal("the token must be recognized as dash-owned")
	}
	if s.isBackupHeartbeatToken("not-a-token") {
		t.Fatal("an unknown token is not dash's")
	}
	// Removal only touches dash-owned monitors.
	s.removeBackupHeartbeat(m.ID)
	if monitors, _ := s.store.ListMonitors(); len(monitors) != 0 {
		t.Fatal("the owned monitor is removed")
	}
}

// A server removed from the fleet: its rules and owned records retire.
func TestRetireRemovedBackupServers(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	s.store = newFileStoreForTest(t)
	writeNotifications(t, `{"channels":[],"public_url":"https://dash.example.internal"}`)
	s.sweepBackups(context.Background())
	s.reconcileBackupTests(backupsNow)
	s.backups.mu.Lock()
	s.backups.rec.Servers["ghost"] = &backupServerRecord{Server: "ghost", State: backupServerOK, LastSuccessAt: backupsNow,
		RuleKeys: []string{"backup:ghost:target"}}
	ghostTest := &backupAutoTest{ID: "backups-ghost", Server: "ghost", Container: "db", NextDueAt: backupsNow}
	s.backups.rec.Tests["backups-ghost"] = ghostTest
	s.backups.mu.Unlock()

	servers, err := s.resolveServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.retireRemovedBackupServers(servers)
	s.backups.mu.Lock()
	_, hasServer := s.backups.rec.Servers["ghost"]
	_, hasTest := s.backups.rec.Tests["backups-ghost"]
	s.backups.mu.Unlock()
	if hasServer || hasTest {
		t.Fatal("the ghost server and its test must be retired")
	}
	for _, inc := range s.alerts.tracker.Open() {
		if inc.Key == "backup:ghost:target" || inc.Key == "restore-test:backups-ghost" {
			t.Fatalf("the ghost's incidents retire: %+v", inc)
		}
	}
}

// Event v2 shape: a backup incident carries source backup, an incident id and
// the page link — the wire contract the receivers build against.
func TestBackupIncident_EventShape(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	writeNotifications(t, `{"channels":[{"id":"ntfy","type":"ntfy","url":"https://ntfy.example/topic","token":"t"}],"public_url":"https://dash.example.internal"}`)
	var got []alert.Event
	tr, err := incident.New(incident.Options{Alerter: eventCollector{&got}, PublicURL: s.alertPublicURL})
	if err != nil {
		t.Fatal(err)
	}
	s.alerts.tracker = tr
	s.sweepBackups(context.Background())
	if len(got) == 0 {
		t.Fatal("no events sent")
	}
	perIncident := 0
	for _, ev := range got {
		if ev.Source != alert.SourceBackup {
			t.Fatalf("source = %q", ev.Source)
		}
		if ev.URL == "" || !strings.Contains(ev.URL, "/backups") {
			t.Fatalf("event = %+v", ev)
		}
		if ev.Kind == alert.KindSummary {
			continue // one message for the batch, no single incident id
		}
		if ev.IncidentID == "" {
			t.Fatalf("event = %+v", ev)
		}
		perIncident++
	}
	if perIncident == 0 {
		t.Fatal("the machine-audience opens must carry incident ids")
	}
}

type eventCollector struct{ events *[]alert.Event }

func (e eventCollector) Send(ev alert.Event) { *e.events = append(*e.events, ev) }
