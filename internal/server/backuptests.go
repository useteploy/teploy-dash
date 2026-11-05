package server

// backuptests.go — automatic restore tests (plan P2.6). One dash-owned test
// per covered database, run weekly in the off-hours window through the CLI's
// `accessory verify-backup` (the same engine the manual Restore Tests page
// uses): the latest object is restored into a scratch container on the server
// and proven usable. The running database is never touched.
//
// Guards, because 4 GB hosts run these beside production:
//   - one test per server at a time;
//   - only inside the window (UTC hours; Dash runs on one clock and servers
//     do not agree on theirs — set it where Dash runs);
//   - a free RAM / free disk read over SSH before launch: a test needs the
//     object's size twice (download + restored copy) plus room to boot the
//     scratch engine.
//
// Records are dash-owned (ID prefix backups-, dash_owned set): created only
// for databases the policy engine already covers, retired only on a complete
// fresh read that no longer lists them. History is kept (capped) — the
// migration gate (P2.9) needs a green test of the correct engine kind.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/incident"
	"github.com/useteploy/teploy-dash/internal/remote"
	"github.com/useteploy/teploy-dash/internal/restoretest"
	sshclient "github.com/useteploy/teploy-dash/internal/ssh"
)

const (
	backupTestTick     = 5 * time.Minute
	backupTestWeekly   = 7 * 24 * time.Hour
	backupTestRetry    = time.Hour // an unsuitable host retries within (or at) the next window
	backupTestTimeout  = 4 * time.Hour
	backupTestHistory  = 20
	backupTestIDPrefix = "backups-"

	// Resource guard floors. Disk needed is the greater of the floor and
	// twice the object's size (the download plus the restored copy).
	backupTestMinFreeMemMB  = 1024
	backupTestMinFreeDiskKB = 2 << 20 // 2 GiB
	backupTestDiskPerByte   = 2
)

// backupAutoTest is one dash-owned weekly verification. Subject identity is
// the status entry's container (accessory or adopted name) plus, when the
// engine addresses databases separately, the database's own objects.
type backupAutoTest struct {
	ID     string `json:"id"`
	Server string `json:"server"`
	// Container is the status entry's name (the container): the stable
	// identity reconcile keys on. Accessory is what verify-backup addresses
	// (the teploy accessory name, or the docker name for adopted containers).
	Container string          `json:"container"`
	App       string          `json:"app,omitempty"`
	Accessory string          `json:"accessory,omitempty"`
	Database  string          `json:"database,omitempty"`
	Adopted   bool            `json:"adopted,omitempty"`
	Engine    string          `json:"engine,omitempty"`
	DashOwned bool            `json:"dash_owned"`
	CreatedAt time.Time       `json:"created_at"`
	NextDueAt time.Time       `json:"next_due_at"`
	LastRun   *backupTestRun  `json:"last_run,omitempty"`
	History   []backupTestRun `json:"history,omitempty"`
	// RetiredAt is set when a complete fresh read no longer lists the
	// subject; the history stays, and a returning subject resumes it.
	RetiredAt *time.Time `json:"retired_at,omitempty"`
}

// Subject is the entry name the policy engine knows this test by.
func (t backupAutoTest) Subject() string {
	if t.Adopted {
		return t.Container
	}
	if t.App != "" && t.Accessory != "" {
		return t.App + "/" + t.Accessory
	}
	return t.Accessory
}

type backupTestRun struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	Detail     string    `json:"detail,omitempty"`
	Metric     string    `json:"metric,omitempty"`
	Date       string    `json:"date,omitempty"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	// Skipped says why a due run did not launch (resource guard, window).
	Skipped string `json:"skipped,omitempty"`
}

type backupAutoTestView struct {
	ID        string          `json:"id"`
	Server    string          `json:"server"`
	App       string          `json:"app,omitempty"`
	Accessory string          `json:"accessory,omitempty"`
	Database  string          `json:"database,omitempty"`
	Adopted   bool            `json:"adopted,omitempty"`
	Engine    string          `json:"engine,omitempty"`
	NextDueAt time.Time       `json:"next_due_at"`
	LastRun   *backupTestRun  `json:"last_run,omitempty"`
	History   int             `json:"history"`
	Runs      []backupTestRun `json:"runs,omitempty"`
	Retired   bool            `json:"retired,omitempty"`
}

func (t *backupAutoTest) view() backupAutoTestView {
	return backupAutoTestView{ID: t.ID, Server: t.Server, App: t.App, Accessory: t.Accessory,
		Database: t.Database, Adopted: t.Adopted, Engine: t.Engine, NextDueAt: t.NextDueAt,
		LastRun: t.LastRun, History: len(t.History), Runs: t.History, Retired: t.RetiredAt != nil}
}

func (t *backupAutoTest) record(r backupTestRun) {
	t.LastRun = &r
	t.History = append(t.History, r)
	if len(t.History) > backupTestHistory {
		t.History = t.History[len(t.History)-backupTestHistory:]
	}
}

func (t *backupAutoTest) matches(server, entry string, e *backupEntry) bool {
	if t.Server != server || t.RetiredAt != nil {
		return false
	}
	if t.Container != entry {
		return false
	}
	if e.Kind == "adopted" || t.Adopted {
		return t.Adopted == (e.Kind == "adopted")
	}
	if e.Kind != "accessory" {
		return false
	}
	return t.App == e.App
}

// ── reconcile: dash-owned records follow what the policy covers ──────────────

// backupTestSubjects lists the databases a fresh status covers: one subject
// per database the engine addresses separately, else the entry's primary.
func backupTestSubjects(st *backupStatusEnvelope) map[string]string {
	out := map[string]string{}
	for i := range st.Entries {
		e := &st.Entries[i]
		if e.Kind != "accessory" && e.Kind != "adopted" {
			continue
		}
		switch e.Coverage {
		case "covered", "adopted":
		default:
			continue // failing/stale/uncovered: not dash's to create; existing tests stay
		}
		if len(e.Databases) > 0 {
			for _, db := range e.Databases {
				out[e.Name+"\x00"+db.Name] = e.Engine
			}
			continue
		}
		out[e.Name+"\x00"] = e.Engine
	}
	return out
}

// reconcileBackupTests creates the tests a covered fleet needs and retires the
// ones whose subject a complete fresh read no longer lists. Idempotent: an
// existing record (including its history) is never rebuilt.
func (s *Server) reconcileBackupTests(now time.Time) {
	b := &s.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, rec := range b.rec.Servers {
		if rec.Status == nil || !backupServerFresh(rec, now) {
			continue
		}
		want := backupTestSubjects(rec.Status)
		for _, t := range b.rec.Tests {
			if t.Server != name {
				continue
			}
			key := t.Container + "\x00" + t.Database
			if _, ok := want[key]; !ok {
				// Complete evidence: this server was just read fresh and the
				// subject is gone. Retire the dash-owned record, keep history.
				if t.RetiredAt == nil {
					at := now
					t.RetiredAt = &at
				}
				continue
			}
			if t.RetiredAt != nil {
				t.RetiredAt = nil // back, without losing its history
			}
			if t.Engine == "" {
				t.Engine = want[key]
			}
			delete(want, key)
		}
		for key, engine := range want {
			accessory, database, _ := strings.Cut(key, "\x00")
			var e *backupEntry
			for i := range rec.Status.Entries {
				if rec.Status.Entries[i].Name == accessory {
					e = &rec.Status.Entries[i]
				}
			}
			if e == nil {
				continue
			}
			// verify-backup addresses an accessory by its teploy name and an
			// adopted container by its docker name.
			accessoryName := e.Name
			if e.Kind == "accessory" && e.Accessory != "" {
				accessoryName = e.Accessory
			}
			t := &backupAutoTest{ID: backupTestIDPrefix + randomHex(8), Server: name,
				Container: e.Name, App: e.App, Accessory: accessoryName, Database: database, Adopted: e.Kind == "adopted",
				Engine: engine, DashOwned: true, CreatedAt: now, NextDueAt: now}
			b.rec.Tests[t.ID] = t
		}
	}
	b.saveLocked()
}

// ── the scheduler ────────────────────────────────────────────────────────────

// tickBackupTests launches the due runs that fit the window, the one-per-server
// bound and the resource guard. It returns quickly; runs continue on the
// backups wait group.
func (s *Server) tickBackupTests(ctx context.Context, now time.Time) {
	b := &s.backups
	b.mu.Lock()
	window := b.rec.Window
	if !window.Contains(now.Hour()) {
		b.mu.Unlock()
		return
	}
	due := make([]*backupAutoTest, 0, len(b.rec.Tests))
	for _, t := range b.rec.Tests {
		if t.RetiredAt == nil && !t.NextDueAt.After(now) {
			due = append(due, t)
		}
	}
	// Oldest due first, and only one per server.
	sort.Slice(due, func(i, j int) bool { return due[i].NextDueAt.Before(due[j].NextDueAt) })
	chosen := map[string]*backupAutoTest{}
	for _, t := range due {
		if chosen[t.Server] == nil && !b.running[t.Server] {
			chosen[t.Server] = t
		}
	}
	for _, t := range chosen {
		b.running[t.Server] = true
	}
	b.mu.Unlock()
	if len(chosen) == 0 {
		return
	}
	for _, t := range chosen {
		t := t
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer func() {
				b.mu.Lock()
				delete(b.running, t.Server)
				b.mu.Unlock()
			}()
			s.runBackupTest(ctx, t, now)
		}()
	}
}

// runBackupTest runs one verification to completion and records the outcome.
func (s *Server) runBackupTest(ctx context.Context, t *backupAutoTest, now time.Time) {
	b := &s.backups
	// Evidence check: the subject must still be on the server's last status.
	var sizeBytes int64
	fresh := false
	b.mu.Lock()
	if rec := b.rec.Servers[t.Server]; rec != nil && rec.Status != nil && backupServerFresh(rec, now) {
		fresh = true
		for i := range rec.Status.Entries {
			e := &rec.Status.Entries[i]
			if !t.matches(t.Server, e.Name, e) {
				continue
			}
			if t.Database == "" {
				if e.Newest != nil {
					sizeBytes = e.Newest.SizeBytes
				}
				if sizeBytes == 0 {
					sizeBytes = e.SizeBytes
				}
			}
			for _, db := range e.Databases {
				if db.Name == t.Database && db.Newest != nil {
					sizeBytes = db.Newest.SizeBytes
				}
			}
		}
	}
	b.mu.Unlock()
	if !fresh {
		s.deferBackupTest(t, now, "waiting: this server's status is too old to launch against")
		return
	}

	// Resource guard: the host must have room beside production.
	srv, err := s.lookupServerStrict(ctx, t.Server)
	if err != nil {
		s.deferBackupTest(t, now, fmt.Sprintf("waiting: the server could not be resolved (%v)", err))
		return
	}
	res, resErr := s.backups.resources(ctx, srv)
	needKB := int64(backupTestMinFreeDiskKB)
	if sizeBytes > 0 {
		if extra := (sizeBytes * backupTestDiskPerByte) / 1024; extra > needKB {
			needKB = extra
		}
	}
	if resErr != nil || res.FreeMemMB < backupTestMinFreeMemMB || res.FreeDiskKB < needKB {
		why := fmt.Sprintf("waiting: the resource read failed (%v)", resErr)
		if resErr == nil {
			why = fmt.Sprintf("waiting: %d MB free RAM (< %d) or %d MB free disk (< %d)",
				res.FreeMemMB, backupTestMinFreeMemMB, res.FreeDiskKB/1024, needKB/1024)
		}
		s.deferBackupTest(t, now, why)
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, backupTestTimeout)
	defer cancel()
	args := []string{"accessory", "verify-backup", t.Accessory, "--host", t.Server, "--json"}
	if t.Adopted {
		args = append(args, "--adopted")
	} else {
		args = append(args, "--app", t.App)
	}
	if t.Database != "" {
		args = append(args, "--database", t.Database)
	}
	result, runErr := s.runCLI(runCtx, args...)
	outcome := parseVerifyRun(result, runErr)

	b.mu.Lock()
	t.record(outcome)
	t.NextDueAt = now.Add(backupTestWeekly)
	b.saveLocked()
	b.mu.Unlock()

	if tr := s.alerts.tracker; tr != nil {
		// Same source and policy as the manual Restore Tests page: one failed
		// run opens one incident, the next pass recovers it.
		subject := t.Subject()
		if t.Database != "" {
			subject += " db " + t.Database
		}
		rule := incident.Rule{Key: restoretest.RuleKey(t.ID), Source: alert.SourceRestoreTest, ID: t.ID,
			Name: "restore test " + subject + " on " + t.Server, App: t.App, Server: t.Server,
			Target: subject, Severity: alert.SeverityWarning, Path: "/backups",
			Policy: incident.Policy{FailAfter: 1, RecoverAfter: 1}}
		msg := outcome.Detail
		if outcome.OK {
			msg = fmt.Sprintf("backup %s verified (%s)", outcome.Date, outcome.Metric)
		}
		if outcome.Skipped == "" {
			tr.Observe(rule, outcome.OK, outcome.At, msg)
		}
	}
}

// deferBackupTest records why a due run did not launch and pushes the retry
// out by an hour (within the window, or into the next one).
func (s *Server) deferBackupTest(t *backupAutoTest, now time.Time, why string) {
	b := &s.backups
	why = sanitizeDomainError(why)
	b.mu.Lock()
	t.record(backupTestRun{At: now, Skipped: why})
	t.NextDueAt = now.Add(backupTestRetry)
	b.saveLocked()
	b.mu.Unlock()
	log.Printf("[backups] restore test %s on %s not launched: %s", t.ID, t.Server, why)
}

// parseVerifyRun reads the CLI's verdict. verify-backup semantics (as the
// restore-test runner documents): a NON-ZERO exit still carries the JSON
// verdict on stdout; only a transport failure (no result at all) overrides it.
func parseVerifyRun(result *cli.Result, err error) backupTestRun {
	at := time.Now().UTC()
	if result == nil {
		return backupTestRun{At: at, Detail: fmt.Sprintf("verify-backup did not run: %v", err)}
	}
	var res restoretest.VerifyResult
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &res); jsonErr != nil {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		if err != nil {
			detail = err.Error()
		}
		return backupTestRun{At: at, Detail: "no verification result: " + sanitizeDomainError(detail)}
	}
	return backupTestRun{At: at, OK: res.OK, Detail: sanitizeDomainError(res.Detail), Metric: res.Metric,
		Date: res.Date, DurationMs: res.DurationMs}
}

// ── the resource read ────────────────────────────────────────────────────────

type backupResources struct {
	FreeMemMB  int64 `json:"free_mem_mb"`
	FreeDiskKB int64 `json:"free_disk_kb"`
}

// readBackupResources reads free memory and the root filesystem's free space
// over SSH: `free -m` (procps' available column when present, else total-used)
// and `df -k /`.
func (s *Server) readBackupResources(ctx context.Context, srv remote.ServerConn) (backupResources, error) {
	c, err := sshclient.Connect(ctx, srv.Host, srv.User, srv.KeyPath)
	if err != nil {
		return backupResources{}, fmt.Errorf("connecting to %s: %w", srv.Name, err)
	}
	defer c.Close()
	out, err := c.Run(ctx, "free -m | awk '/^Mem:/{print \"MEM\",$2,$3,$7}'; df -k / | awk 'NR==2{print \"DISK\",$4}'")
	if err != nil {
		return backupResources{}, fmt.Errorf("reading free memory and disk on %s: %w", srv.Name, err)
	}
	res, err := parseBackupResources(out)
	if err != nil {
		return backupResources{}, fmt.Errorf("reading free memory and disk on %s: %w", srv.Name, err)
	}
	return res, nil
}

func parseBackupResources(out string) (backupResources, error) {
	var res backupResources
	var sawMem, sawDisk bool
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MEM":
			if len(f) < 3 {
				return res, fmt.Errorf("short free line %q", line)
			}
			total, _ := strconv.ParseInt(f[1], 10, 64)
			used, _ := strconv.ParseInt(f[2], 10, 64)
			res.FreeMemMB = total - used
			if len(f) >= 4 {
				if avail, err := strconv.ParseInt(f[3], 10, 64); err == nil && avail > 0 {
					res.FreeMemMB = avail
				}
			}
			sawMem = true
		case "DISK":
			free, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return res, fmt.Errorf("short df line %q", line)
			}
			res.FreeDiskKB = free
			sawDisk = true
		}
	}
	if !sawMem || !sawDisk {
		return res, fmt.Errorf("the reply named neither memory nor disk (%q)", strings.TrimSpace(out))
	}
	return res, nil
}

// ── the page join ────────────────────────────────────────────────────────────

// manualRestoreTests is what the person-made Restore Tests page last proved,
// keyed server/app/accessory. It joins the fleet rows so a person's test is
// not invisible next to dash's.
func (s *Server) manualRestoreTests() map[string]backupRowTest {
	out := map[string]backupRowTest{}
	if s.store == nil {
		return out
	}
	tests, err := s.store.ListRestoreTests()
	if err != nil {
		return out
	}
	for _, t := range tests {
		if t.LastRunAt.IsZero() {
			continue
		}
		out[t.Server+"\x00"+t.App+"\x00"+t.Accessory] = backupRowTest{At: t.LastRunAt, OK: t.LastOK, Metric: t.LastMetric}
	}
	return out
}

// lastRestoreTestLocked is what one fleet row shows: the newest FAILED run
// among its tests (a broken chain of trust outweighs a fresher green one),
// else the oldest of the latest runs (the weakest link — a row whose newest
// database has not been tested in a while says so). Manual Restore Tests
// page results join in.
func (b *backupState) lastRestoreTestLocked(server string, e *backupEntry, manual map[string]backupRowTest) *backupRowTest {
	var oldest, latestFailed *backupRowTest
	consider := func(candidate backupRowTest) {
		c := candidate
		if oldest == nil || c.At.Before(oldest.At) {
			oldest = &c
		}
		if !c.OK && (latestFailed == nil || c.At.After(latestFailed.At)) {
			latestFailed = &c
		}
	}
	if e.Kind == "accessory" && e.App != "" && e.Accessory != "" {
		if m, ok := manual[server+"\x00"+e.App+"\x00"+e.Accessory]; ok {
			consider(m)
		}
	}
	subjects := map[string]bool{"": true}
	for _, db := range e.Databases {
		subjects[db.Name] = true
	}
	for _, t := range b.rec.Tests {
		if !t.matches(server, e.Name, e) || t.LastRun == nil || t.LastRun.Skipped != "" {
			continue
		}
		if !subjects[t.Database] {
			continue
		}
		consider(backupRowTest{At: t.LastRun.At, OK: t.LastRun.OK, Metric: t.LastRun.Metric})
	}
	if latestFailed != nil {
		return latestFailed
	}
	return oldest
}

func (b *backupState) testViewsLocked(now time.Time) []backupAutoTestView {
	out := make([]backupAutoTestView, 0, len(b.rec.Tests))
	for _, t := range b.rec.Tests {
		if t.RetiredAt != nil {
			continue
		}
		out = append(out, t.view())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].NextDueAt.Before(out[j].NextDueAt)
	})
	return out
}
