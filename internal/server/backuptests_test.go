package server

// backuptests_test.go — the automatic restore tests (P2.6): reconcile creates
// one dash-owned test per covered database and retires on complete evidence,
// the scheduler obeys the window / one-per-server / resource guard, and a run
// records its verdict and history.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/remote"
)

func TestParseBackupResources(t *testing.T) {
	// `free -m | awk '/^Mem:/{print "MEM",$2,$3,$7}'` → total used available.
	res, err := parseBackupResources("MEM 7976 3200 4776\nDISK 27284900")
	if err != nil {
		t.Fatal(err)
	}
	if res.FreeMemMB != 4776 || res.FreeDiskKB != 27284900 {
		t.Fatalf("res = %+v", res)
	}
	// Without an available column (busybox free), total-used.
	res, err = parseBackupResources("MEM 7976 3200\nDISK 500")
	if err != nil {
		t.Fatal(err)
	}
	if res.FreeMemMB != 4776 {
		t.Fatalf("without an available column, total-used: %+v", res)
	}
	if _, err := parseBackupResources("DISK 500"); err == nil {
		t.Fatal("a reply without memory must be refused")
	}
}

func recordFreshStatus(t *testing.T, s *Server, server string) *backupStatusEnvelope {
	t.Helper()
	return recordFreshStatusAt(t, s, server, backupsNow)
}

func recordFreshStatusAt(t *testing.T, s *Server, server string, at time.Time) *backupStatusEnvelope {
	t.Helper()
	env := mustDecodeStatus(t, "status-fleet.json")
	env.Host = server
	s.recordBackupStatus(server, env, nil, at)
	return env
}

func testsOf(t *testing.T, s *Server) map[string]*backupAutoTest {
	t.Helper()
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	out := map[string]*backupAutoTest{}
	for id, test := range s.backups.rec.Tests {
		out[id] = test
	}
	return out
}

// From the corpus envelope: blog-db (covered, primary) and
// wordpress-shared-mysql (adopted, database wp_blog) get tests; omni_postgres
// (failing), shop-db (stale), the unmanaged pair and the excluded volumes do
// not — dash only creates tests for what the policy already covers.
func TestReconcileBackupTests_CreatesPerCoveredDatabase(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	recordFreshStatus(t, s, "prod")

	s.reconcileBackupTests(backupsNow)
	tests := testsOf(t, s)
	if len(tests) != 2 {
		for id, test := range tests {
			t.Logf("%s: %+v", id, test)
		}
		t.Fatalf("tests = %d, want 2", len(tests))
	}
	var blog, wp *backupAutoTest
	for _, test := range tests {
		switch test.Subject() {
		case "blog/db":
			blog = test
		case "wordpress-shared-mysql":
			wp = test
		}
	}
	if blog == nil || wp == nil {
		t.Fatalf("missing subjects: %+v", tests)
	}
	if !blog.DashOwned || blog.Database != "" || blog.Adopted || blog.Engine != "postgres" || blog.App != "blog" {
		t.Errorf("blog test = %+v", blog)
	}
	if !wp.Adopted || wp.Database != "wp_blog" || wp.App != "" || wp.Engine != "mysql" {
		t.Errorf("wordpress test = %+v", wp)
	}
	// Idempotent: a second reconcile rebuilds nothing.
	s.reconcileBackupTests(backupsNow.Add(time.Minute))
	if len(testsOf(t, s)) != 2 {
		t.Fatal("reconcile must be idempotent")
	}
}

func TestReconcileBackupTests_RetiresOnCompleteEvidenceAndResumes(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	recordFreshStatus(t, s, "prod")
	s.reconcileBackupTests(backupsNow)

	// A stale server read retires nothing.
	s.backups.mu.Lock()
	s.backups.rec.Servers["prod"].LastSuccessAt = backupsNow.Add(-3 * backupSweepInterval)
	s.backups.mu.Unlock()
	s.reconcileBackupTests(backupsNow)
	if len(testsOf(t, s)) != 2 {
		t.Fatal("a stale read must not retire anything")
	}

	// A complete read without blog-db retires its test, keeping the record.
	env := mustDecodeStatus(t, "status-fleet.json")
	env.Host = "prod"
	kept := []backupEntry{}
	for _, e := range env.Entries {
		if e.Name != "blog-db" {
			kept = append(kept, e)
		}
	}
	env.Entries = kept
	s.recordBackupStatus("prod", env, nil, backupsNow.Add(time.Minute))
	s.reconcileBackupTests(backupsNow.Add(time.Minute))
	tests := testsOf(t, s)
	if len(tests) != 2 {
		t.Fatal("the record is kept, retired")
	}
	retired := 0
	for _, test := range tests {
		if test.Subject() == "blog/db" {
			if test.RetiredAt == nil {
				t.Fatal("blog/db must be retired")
			}
			retired++
		}
	}
	if retired != 1 {
		t.Fatalf("retired = %d", retired)
	}

	// The subject returns: the same record resumes without losing history.
	recordFreshStatus(t, s, "prod")
	s.reconcileBackupTests(backupsNow.Add(2 * time.Minute))
	for _, test := range testsOf(t, s) {
		if test.RetiredAt != nil {
			t.Fatalf("%s still retired", test.ID)
		}
	}
}

func TestTickBackupTests_WindowGate(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	recordFreshStatus(t, s, "prod")
	s.reconcileBackupTests(backupsNow)
	// Midday is outside the default 02:00-06:00 UTC window: nothing launches.
	s.tickBackupTests(context.Background(), backupsNow)
	if n := c.countOf("verify-backup"); n != 0 {
		t.Fatalf("outside the window nothing launches: %d", n)
	}
	// Inside the window it does.
	night := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	s.backups.mu.Lock()
	for _, test := range s.backups.rec.Tests {
		test.NextDueAt = night
	}
	s.backups.mu.Unlock()
	s.backups.resources = func(context.Context, remote.ServerConn) (backupResources, error) {
		return backupResources{FreeMemMB: 8192, FreeDiskKB: 100 << 20}, nil
	}
	s.tickBackupTests(context.Background(), night)
	s.backups.wg.Wait()
	if n := c.countOf("verify-backup"); n != 1 {
		t.Fatalf("one per server per tick: %d", n)
	}
	// The second due test launches on the next tick once the first finished.
	s.tickBackupTests(context.Background(), night)
	s.backups.wg.Wait()
	if n := c.countOf("verify-backup"); n != 2 {
		t.Fatalf("inside the window both due tests launch: %d", n)
	}
}

func TestTickBackupTests_ResourceGuardDefers(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	recordFreshStatus(t, s, "prod")
	s.reconcileBackupTests(backupsNow)
	night := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	s.backups.mu.Lock()
	for _, test := range s.backups.rec.Tests {
		test.NextDueAt = night
	}
	s.backups.mu.Unlock()
	s.backups.resources = func(context.Context, remote.ServerConn) (backupResources, error) {
		return backupResources{FreeMemMB: 512, FreeDiskKB: 100 << 20}, nil // too little RAM
	}
	s.tickBackupTests(context.Background(), night)
	s.backups.wg.Wait()
	if n := c.countOf("verify-backup"); n != 0 {
		t.Fatalf("a tight host must not launch: %d", n)
	}
	deferred := 0
	for _, test := range testsOf(t, s) {
		if test.LastRun == nil {
			continue // not this tick's pick: one launch slot per server
		}
		if !strings.Contains(test.LastRun.Skipped, "free RAM") {
			t.Fatalf("the deferral must be recorded: %+v", test.LastRun)
		}
		if !test.NextDueAt.Equal(night.Add(backupTestRetry)) {
			t.Fatalf("a deferred test retries in an hour: %v", test.NextDueAt)
		}
		deferred++
	}
	if deferred != 1 {
		t.Fatalf("deferred = %d, want the one launched test", deferred)
	}
}

func TestTickBackupTests_OnePerServerAtATime(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	// Two databases on one server: only one launches per tick.
	env := mustDecodeStatus(t, "status-fleet.json")
	for i := range env.Entries {
		if env.Entries[i].Name == "omni_postgres" {
			env.Entries[i].Coverage = "covered"
		}
	}
	s.recordBackupStatus("prod", env, nil, backupsNow)
	s.reconcileBackupTests(backupsNow)
	n := 0
	for _, test := range testsOf(t, s) {
		if test.Server == "prod" {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("need two tests on one server, have %d", n)
	}
	night := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	s.backups.mu.Lock()
	for _, test := range s.backups.rec.Tests {
		test.NextDueAt = night
	}
	s.backups.mu.Unlock()
	release := make(chan struct{})
	s.backups.resources = func(context.Context, remote.ServerConn) (backupResources, error) {
		<-release
		return backupResources{FreeMemMB: 8192, FreeDiskKB: 100 << 20}, nil
	}
	s.tickBackupTests(context.Background(), night)
	// While the first is still prechecking, a second tick launches nothing.
	s.tickBackupTests(context.Background(), night)
	close(release)
	s.backups.wg.Wait()
	if got := c.countOf("verify-backup"); got != 1 {
		t.Fatalf("one test per server per tick: %d", got)
	}
}

func TestRunBackupTest_VerdictAndWeeklyCadence(t *testing.T) {
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
		if test.Subject() == "wordpress-shared-mysql" {
			wp = test
		}
	}
	if wp == nil {
		t.Fatal("no wordpress test")
	}
	s.runBackupTest(context.Background(), wp, night)
	if !wp.LastRun.OK || wp.LastRun.Skipped != "" {
		t.Fatalf("last run = %+v", wp.LastRun)
	}
	if !wp.NextDueAt.Equal(night.Add(backupTestWeekly)) {
		t.Fatalf("weekly cadence: %v", wp.NextDueAt)
	}
	// The adopted per-database run addressed the container and its database.
	if n := c.countOf("verify-backup", "--adopted", "--database", "wp_blog"); n != 1 {
		t.Fatalf("adopted database run: %d", n)
	}

	// A failed verification (non-zero exit, JSON verdict on stdout) records
	// a failed run — the same semantics the manual runner documents.
	recordFreshStatusAt(t, s, "prod", night.Add(backupTestWeekly))
	c.verify = map[string]func([]string) (*cli.Result, error){
		"wordpress-shared-mysql": func([]string) (*cli.Result, error) {
			return &cli.Result{ExitCode: 1, Stdout: `{"app":"","accessory":"wordpress-shared-mysql","ok":false,"detail":"restored copy had no tables"}`}, nil
		},
	}
	s.runBackupTest(context.Background(), wp, night.Add(backupTestWeekly))
	if wp.LastRun.OK || wp.LastRun.Detail == "" {
		t.Fatalf("failed run = %+v", wp.LastRun)
	}
	// History is kept and capped.
	for i := 0; i < backupTestHistory+5; i++ {
		recordFreshStatusAt(t, s, "prod", night.Add(time.Duration(i)*time.Hour))
		s.runBackupTest(context.Background(), wp, night.Add(time.Duration(i)*time.Hour))
	}
	if len(wp.History) != backupTestHistory {
		t.Fatalf("history = %d, want %d", len(wp.History), backupTestHistory)
	}
}

func TestRunBackupTest_TransportFailureIsAFailedRun(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	recordFreshStatus(t, s, "prod")
	test := &backupAutoTest{ID: "backups-x", Server: "prod", Container: "blog-db", App: "blog", Accessory: "db", Engine: "postgres", DashOwned: true, NextDueAt: backupsNow}
	s.backups.mu.Lock()
	s.backups.rec.Tests[test.ID] = test
	s.backups.mu.Unlock()
	s.backups.resources = func(context.Context, remote.ServerConn) (backupResources, error) {
		return backupResources{FreeMemMB: 8192, FreeDiskKB: 100 << 20}, nil
	}
	c.verify = map[string]func([]string) (*cli.Result, error){
		"db": func([]string) (*cli.Result, error) { return nil, fmt.Errorf("cli vanished") },
	}
	s.runBackupTest(context.Background(), test, backupsNow)
	if test.LastRun.OK || !strings.Contains(test.LastRun.Detail, "cli vanished") {
		t.Fatalf("run = %+v", test.LastRun)
	}
}

// The fleet row shows the weakest link: the oldest of its databases' latest
// tests, and a manual Restore Test page result joins in.
func TestLastRestoreTestLocked_WeakestLink(t *testing.T) {
	b := &backupState{rec: backupRecord{Tests: map[string]*backupAutoTest{}}}
	entry := &backupEntry{Name: "mysql-shared", Kind: "adopted", Engine: "mysql",
		Databases: []backupEntryDB{{Name: "wp_a"}, {Name: "wp_b"}}}
	older := backupsNow.Add(-72 * time.Hour)
	newer := backupsNow.Add(-24 * time.Hour)
	b.rec.Tests["t1"] = &backupAutoTest{ID: "t1", Server: "prod", Container: "mysql-shared", Accessory: "mysql-shared", Database: "wp_a", Adopted: true,
		LastRun: &backupTestRun{At: newer, OK: true}}
	b.rec.Tests["t2"] = &backupAutoTest{ID: "t2", Server: "prod", Container: "mysql-shared", Accessory: "mysql-shared", Database: "wp_b", Adopted: true,
		LastRun: &backupTestRun{At: older, OK: true}}
	got := b.lastRestoreTestLocked("prod", entry, map[string]backupRowTest{})
	if got == nil || !got.At.Equal(older) {
		t.Fatalf("got = %+v, want the older", got)
	}
	// A failed run is the row's verdict, whatever its age.
	b.rec.Tests["t2"].LastRun = &backupTestRun{At: newer.Add(time.Hour), OK: false}
	got = b.lastRestoreTestLocked("prod", entry, map[string]backupRowTest{})
	if got == nil || got.OK || !got.At.Equal(newer.Add(time.Hour)) {
		t.Fatalf("got = %+v", got)
	}
	// A manual test joins for accessory entries.
	manual := &backupEntry{Name: "db", Kind: "accessory", App: "blog", Accessory: "db"}
	got = b.lastRestoreTestLocked("prod", manual, map[string]backupRowTest{
		"prod\x00blog\x00db": {At: backupsNow.Add(-2 * time.Hour), OK: true}})
	if got == nil || !got.At.Equal(backupsNow.Add(-2*time.Hour)) {
		t.Fatalf("manual join = %+v", got)
	}
}

// ── migration evidence (P2.9) ────────────────────────────────────────────────

func TestMigrationEvidence_CountsDistinctGoodObjects(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	recordFreshStatus(t, s, "prod")

	// Baseline read: the current object is the starting point, not a count.
	s.backups.mu.Lock()
	m := s.backups.rec.Servers["prod"].Migration["blog-db"]
	s.backups.mu.Unlock()
	if m == nil || m.ConsecutiveGood != 0 {
		t.Fatalf("baseline = %+v", m)
	}
	// A new object arrives: one good backup watched.
	for i, key := range []string{"s3://teploy-backups/prod/blog/20261105-04.age", "s3://teploy-backups/prod/blog/20261105-05.age"} {
		next := mustDecodeStatus(t, "status-fleet.json")
		for j := range next.Entries {
			if next.Entries[j].Name == "blog-db" {
				next.Entries[j].Newest.Key = key
			}
		}
		s.recordBackupStatus("prod", next, nil, backupsNow.Add(time.Duration(i+1)*time.Hour))
	}
	s.backups.mu.Lock()
	m = s.backups.rec.Servers["prod"].Migration["blog-db"]
	s.backups.mu.Unlock()
	if m.ConsecutiveGood != 2 {
		t.Fatalf("consecutive good = %d, want 2", m.ConsecutiveGood)
	}
	// A failing read resets the chain.
	failing := mustDecodeStatus(t, "status-fleet.json")
	for j := range failing.Entries {
		if failing.Entries[j].Name == "blog-db" {
			failing.Entries[j].Coverage = "failing"
		}
	}
	s.recordBackupStatus("prod", failing, nil, backupsNow.Add(4*time.Hour))
	s.backups.mu.Lock()
	m = s.backups.rec.Servers["prod"].Migration["blog-db"]
	s.backups.mu.Unlock()
	if m.ConsecutiveGood != 0 {
		t.Fatalf("a failure resets: %+v", m)
	}
}

func TestMigrationView_ReadyNeedsTwoGoodAndAGreenTestOfTheEngine(t *testing.T) {
	b := &backupState{rec: backupRecord{Tests: map[string]*backupAutoTest{}}}
	rec := &backupServerRecord{Server: "prod", Migration: map[string]*backupMigrateEntry{
		"blog-db": {Engine: "postgres", ConsecutiveGood: 2},
	}}
	entry := &backupEntry{Name: "blog-db", Kind: "accessory", App: "blog", Accessory: "db", Engine: "postgres", Coverage: "covered"}
	if v := b.migrationViewLocked(rec, entry); v == nil || v.Ready || !strings.Contains(v.Detail, "restore test") {
		t.Fatalf("view = %+v", v)
	}
	// A green test of the WRONG engine does not open the gate.
	b.rec.Tests["t1"] = &backupAutoTest{ID: "t1", Server: "prod", Container: "blog-db", App: "blog", Accessory: "db", Engine: "mysql",
		LastRun: &backupTestRun{At: backupsNow, OK: true}}
	if v := b.migrationViewLocked(rec, entry); v.Ready {
		t.Fatalf("wrong engine must not be ready: %+v", v)
	}
	// A skipped run is not a green test.
	b.rec.Tests["t1"] = &backupAutoTest{ID: "t1", Server: "prod", Container: "blog-db", App: "blog", Accessory: "db", Engine: "postgres",
		LastRun: &backupTestRun{At: backupsNow, OK: true, Skipped: "waiting"}}
	if v := b.migrationViewLocked(rec, entry); v.Ready {
		t.Fatalf("a skipped run is not a green test: %+v", v)
	}
	b.rec.Tests["t1"] = &backupAutoTest{ID: "t1", Server: "prod", Container: "blog-db", App: "blog", Accessory: "db", Engine: "postgres",
		LastRun: &backupTestRun{At: backupsNow, OK: true}}
	if v := b.migrationViewLocked(rec, entry); !v.Ready {
		t.Fatalf("view = %+v", v)
	}
	// Excluded and uncovered rows carry no gate.
	excluded := &backupEntry{Name: "blog/volumes", Kind: "volumes", Coverage: "excluded"}
	if v := b.migrationViewLocked(rec, excluded); v != nil {
		t.Fatalf("excluded = %+v", v)
	}
}

// The page carries the hand-written job list and the rule (report-only).
func TestBackupsPage_HandwrittenSurface(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	recordFreshStatus(t, s, "prod")
	view := s.backupsView(context.Background(), backupsNow)
	if len(view.Handwritten) == 0 || view.HandwrittenRule == "" {
		t.Fatal("the migration surface must be reported")
	}
	var raw []byte
	raw, _ = json.Marshal(view)
	for _, name := range []string{"omni-backup", "dash-state-backup", "teploy-nucleus-backup"} {
		if !strings.Contains(string(raw), name) {
			t.Errorf("the known job list must name %s", name)
		}
	}
	if !strings.Contains(view.HandwrittenRule, "human action") {
		t.Error("the rule must say removal stays a human action")
	}
}
