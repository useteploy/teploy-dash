package server

// backupfleet.go — Dash Backups (plan P2, Slice 4). The CLI is the engine on each
// server: Dash reads `teploy backup status --json` per server at a slow cadence,
// joins it with what only Dash knows (restore tests, the recovery drill), and
// shows one page. A server that could not be read reads "unknown", with the age
// of what it last said; it is neither covered nor failing.
//
// State is one file-store record, <data>/backups.json, 0600 (plan principle 9).
// It holds the public recovery key and bucket names, never a credential: storage
// keys are forgotten once pushed, and the private recovery key is never held.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/durable"
	"github.com/useteploy/teploy-dash/internal/remote"
	"github.com/useteploy/teploy-dash/internal/restoretest"
	"github.com/useteploy/teploy-dash/internal/s3probe"
)

const (
	backupSweepInterval    = 30 * time.Minute
	backupServerTimeout    = 12 * time.Minute
	backupSweepConcurrency = 2
	// backupStaleAfter: evidence older than two missed sweeps reads unknown.
	backupStaleAfter = 2*backupSweepInterval + backupServerTimeout
)

// Row and server states beyond the CLI's coverage values.
const (
	backupStateUnknown = "unknown"
	backupServerOK     = "ok"
	backupServerFailed = "unreachable"
)

// ── the CLI's envelope (contracts/schema/backup-status-envelope.schema.json) ─

type backupStatusEnvelope struct {
	MachineInterface   int              `json:"machine_interface"`
	Host               string           `json:"host"`
	ObservedAt         time.Time        `json:"observed_at"`
	Target             backupStatTarget `json:"target"`
	Policy             backupStatPolicy `json:"policy"`
	Encrypted          bool             `json:"encrypted"`
	AWSCLIProblem      string           `json:"aws_cli_problem,omitempty"`
	AWSCLI             bool             `json:"aws_cli"`
	AgeCLI             bool             `json:"age_cli"`
	LastRun            *backupStatRun   `json:"last_run,omitempty"`
	Entries            []backupEntry    `json:"entries"`
	UncoveredDatabases []string         `json:"uncovered_databases"`
	LeftoverWorkspaces []string         `json:"leftover_workspaces,omitempty"`
	Warnings           []string         `json:"warnings,omitempty"`
	LeftoverUploads    []string         `json:"leftover_uploads,omitempty"`
}

type backupStatTarget struct {
	Configured        bool     `json:"configured"`
	Bucket            string   `json:"bucket,omitempty"`
	Region            string   `json:"region,omitempty"`
	Endpoint          string   `json:"endpoint,omitempty"`
	Prefix            string   `json:"prefix,omitempty"`
	HasCredentials    bool     `json:"has_credentials"`
	Reachable         bool     `json:"reachable"`
	Error             string   `json:"error,omitempty"`
	IncomingLifecycle string   `json:"incoming_lifecycle,omitempty"`
	OtherServers      []string `json:"other_servers,omitempty"`
}

type backupRetention struct {
	KeepLast    int `json:"keep_last,omitempty"`
	KeepDaily   int `json:"keep_daily,omitempty"`
	KeepWeekly  int `json:"keep_weekly,omitempty"`
	KeepMonthly int `json:"keep_monthly,omitempty"`
	MaxAgeDays  int `json:"max_age_days,omitempty"`
}

type backupStatPolicy struct {
	Configured     bool            `json:"configured"`
	Interval       string          `json:"interval,omitempty"`
	CronInstalled  bool            `json:"cron_installed"`
	Retention      backupRetention `json:"retention"`
	Excludes       []string        `json:"excludes,omitempty"`
	Volumes        bool            `json:"volumes"`
	VolumeMaxGB    float64         `json:"volume_max_gb,omitempty"`
	HeartbeatCount int             `json:"heartbeat_count"`
	HeartbeatHosts []string        `json:"heartbeat_hosts,omitempty"`
	Recipient      string          `json:"recipient,omitempty"`
}

type backupStatRun struct {
	At            time.Time `json:"at"`
	Status        string    `json:"status"`
	TargetsOK     int       `json:"targets_ok"`
	TargetsFailed int       `json:"targets_failed"`
	Omissions     []string  `json:"omissions,omitempty"`
	Error         string    `json:"error,omitempty"`
	Stale         bool      `json:"stale"`
	Warnings      []string  `json:"warnings,omitempty"`
}

type backupObject struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt string `json:"created_at"`
	Encrypted bool   `json:"encrypted"`
}

type backupEntryDB struct {
	Name   string        `json:"name"`
	Newest *backupObject `json:"newest,omitempty"`
}

type backupVerify struct {
	At       time.Time `json:"at"`
	OK       bool      `json:"ok"`
	ID       string    `json:"id,omitempty"`
	Metric   string    `json:"metric,omitempty"`
	Database string    `json:"database,omitempty"`
}

type backupEntry struct {
	Name               string          `json:"name"`
	Kind               string          `json:"kind"`
	App                string          `json:"app,omitempty"`
	Accessory          string          `json:"accessory,omitempty"`
	Engine             string          `json:"engine,omitempty"`
	State              string          `json:"state,omitempty"`
	Unmanaged          bool            `json:"unmanaged"`
	Adopted            bool            `json:"adopted"`
	Coverage           string          `json:"coverage"`
	Reason             string          `json:"reason,omitempty"`
	Schedule           string          `json:"schedule"`
	ScheduleForm       string          `json:"schedule_form,omitempty"`
	LastSuccess        *time.Time      `json:"last_success,omitempty"`
	LastFailure        *time.Time      `json:"last_failure,omitempty"`
	LastError          string          `json:"last_error,omitempty"`
	Newest             *backupObject   `json:"newest,omitempty"`
	Databases          []backupEntryDB `json:"databases,omitempty"`
	LastVerify         *backupVerify   `json:"last_verify,omitempty"`
	UncoveredDatabases []string        `json:"uncovered_databases,omitempty"`
	SizeBytes          int64           `json:"size_bytes,omitempty"`
	Encrypted          bool            `json:"encrypted"`
}

// ── persisted state ──────────────────────────────────────────────────────────

type backupServerRecord struct {
	Server        string                `json:"server"`
	State         string                `json:"state"`
	Error         string                `json:"error,omitempty"`
	LastAttemptAt time.Time             `json:"last_attempt_at"`
	LastSuccessAt time.Time             `json:"last_success_at,omitempty"`
	Status        *backupStatusEnvelope `json:"status,omitempty"`
	// Adopted: this server's first result has been taken in, and what was already
	// wrong then became one summary rather than an alert each.
	Adopted bool `json:"adopted,omitempty"`
	// RuleKeys are the incident rules the last read evidenced, so a rule whose
	// subject is gone is retired on complete evidence only.
	RuleKeys []string `json:"rule_keys,omitempty"`
	// PolicyAppliedAt is when Dash last wrote the policy here: a first run that has
	// not happened yet is only late after two intervals from this.
	PolicyAppliedAt time.Time `json:"policy_applied_at,omitempty"`
	// HeartbeatMonitorID is the push monitor Dash owns for this server.
	HeartbeatMonitorID string `json:"heartbeat_monitor_id,omitempty"`
	HeartbeatNote      string `json:"heartbeat_note,omitempty"`
	// Migration is the per-entry evidence for retiring hand-written backup
	// jobs (plan P2.9): distinct good policy backups seen and their keys.
	Migration map[string]*backupMigrateEntry `json:"migration,omitempty"`
}

// backupMigrateEntry is one entry's migration evidence. ConsecutiveGood counts
// DISTINCT good objects dash has watched arrive (a lower bound on consecutive
// successful policy runs: a read between two runs sees one), reset by any
// failing read.
type backupMigrateEntry struct {
	Engine          string    `json:"engine,omitempty"`
	LastKey         string    `json:"last_key,omitempty"`
	ConsecutiveGood int       `json:"consecutive_good"`
	LastGoodAt      time.Time `json:"last_good_at,omitempty"`
}

type backupRecord struct {
	Storage         *backupStorage                 `json:"storage,omitempty"`
	Policy          *backupPolicySpec              `json:"policy,omitempty"`
	Recovery        *backupRecovery                `json:"recovery,omitempty"`
	Window          backupWindow                   `json:"window"`
	ChecklistDoneAt *time.Time                     `json:"checklist_done_at,omitempty"`
	Servers         map[string]*backupServerRecord `json:"servers"`
	Tests           map[string]*backupAutoTest     `json:"tests"`
	LastSweepAt     time.Time                      `json:"last_sweep_at,omitempty"`
	Unsupported     bool                           `json:"unsupported,omitempty"`
	SweepError      string                         `json:"sweep_error,omitempty"`
}

type backupState struct {
	path string
	mu   sync.Mutex
	rec  backupRecord
	// kick asks the loop for a sweep now (Refresh, a storage change).
	kick chan struct{}
	// sweepMu keeps sweeps single-flight.
	sweepMu sync.Mutex
	// running holds the servers with an automatic restore test in flight.
	running map[string]bool
	wg      sync.WaitGroup
	// pendingKey is a generated recovery key's PUBLIC half, awaiting the owner's
	// "I saved it". Memory only: a restart makes them generate again.
	pendingKey string
	// probe is the storage probe (a seam for tests).
	probe func(context.Context, s3probe.Config) s3probe.Result
	// resources is the pre-launch free RAM/disk read (a seam for tests).
	resources func(context.Context, remote.ServerConn) (backupResources, error)
}

// serverName aliases the fleet's server connection type: the backup files
// speak of servers by name and this keeps their signatures short.
type serverName = remote.ServerConn

func backupServerNames(servers []serverName) []string {
	names := make([]string, 0, len(servers))
	for _, srv := range servers {
		names = append(names, srv.Name)
	}
	sort.Strings(names)
	return names
}

func (s *Server) initBackups(dataDir string) {
	b := &s.backups
	b.rec = backupRecord{Servers: map[string]*backupServerRecord{}, Tests: map[string]*backupAutoTest{}, Window: defaultBackupWindow}
	b.kick = make(chan struct{}, 1)
	b.running = map[string]bool{}
	b.probe = s3probe.Run
	b.resources = s.readBackupResources
	if dataDir == "" {
		return
	}
	b.path = filepath.Join(dataDir, "backups.json")
	raw, err := os.ReadFile(b.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var rec backupRecord
	if err == nil {
		err = json.Unmarshal(raw, &rec)
	}
	if err != nil {
		// Unreadable state must not stop the watching: set aside and start over.
		if aside := setAside(b.path, err); aside != nil {
			log.Printf("[backups] %s is unreadable (%v) and could not be set aside: %v", b.path, err, aside)
			b.path = ""
		}
		return
	}
	if rec.Servers == nil {
		rec.Servers = map[string]*backupServerRecord{}
	}
	if rec.Tests == nil {
		rec.Tests = map[string]*backupAutoTest{}
	}
	if rec.Window == (backupWindow{}) {
		rec.Window = defaultBackupWindow
	}
	b.rec = rec
}

func (b *backupState) saveLocked() {
	if b.path == "" {
		return
	}
	raw, err := json.Marshal(b.rec)
	if err == nil {
		err = durable.Replace(b.path, raw, 0600)
	}
	if err != nil {
		log.Printf("[backups] persist failed (state in memory until the next change): %v", err)
	}
}

func (b *backupState) serverLocked(name string) *backupServerRecord {
	rec := b.rec.Servers[name]
	if rec == nil {
		rec = &backupServerRecord{Server: name}
		b.rec.Servers[name] = rec
	}
	return rec
}

// ── reading a server ─────────────────────────────────────────────────────────

func (s *Server) readBackupStatus(ctx context.Context, server string) (*backupStatusEnvelope, error) {
	args := []string{"backup", "status", "--host", server, "--json"}
	result, err := s.runCLI(ctx, args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, commandFailure(args, result)
	}
	return decodeBackupStatus(result.Stdout)
}

func decodeBackupStatus(stdout string) (*backupStatusEnvelope, error) {
	// The central decode refuses an envelope from a newer machine interface.
	if _, err := cli.ParseJSON(stdout); err != nil {
		return nil, err
	}
	var env backupStatusEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		return nil, fmt.Errorf("decoding backup status: %w", err)
	}
	if env.Host == "" || env.ObservedAt.IsZero() || env.Entries == nil || env.UncoveredDatabases == nil {
		return nil, errors.New("decoding backup status: incomplete machine response")
	}
	return &env, nil
}

// recordBackupStatus stores one read. A failed read keeps the last status, with
// its age, and says it failed.
func (s *Server) recordBackupStatus(server string, env *backupStatusEnvelope, err error, now time.Time) {
	b := &s.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.serverLocked(server)
	rec.LastAttemptAt = now
	if err != nil {
		rec.State, rec.Error = backupServerFailed, sanitizeDomainError(err.Error())
		return
	}
	rec.State, rec.Error = backupServerOK, ""
	rec.LastSuccessAt = now
	rec.Status = sanitizeBackupStatus(env)
	updateBackupMigration(rec, env, now)
}

// updateBackupMigration folds one fresh status into the migration evidence:
// a new good object counts, a failing read resets the chain. Caller holds mu.
func updateBackupMigration(rec *backupServerRecord, env *backupStatusEnvelope, now time.Time) {
	if rec.Migration == nil {
		rec.Migration = map[string]*backupMigrateEntry{}
	}
	seen := map[string]bool{}
	for i := range env.Entries {
		e := &env.Entries[i]
		if e.Kind != "accessory" && e.Kind != "adopted" {
			continue
		}
		key := e.Name
		seen[key] = true
		m := rec.Migration[key]
		if m == nil {
			m = &backupMigrateEntry{Engine: e.Engine}
			rec.Migration[key] = m
		}
		if m.Engine == "" {
			m.Engine = e.Engine
		}
		switch e.Coverage {
		case "covered", "adopted":
			if e.Newest != nil && e.Newest.Key != m.LastKey {
				if m.LastKey != "" {
					m.ConsecutiveGood++
				}
				m.LastKey, m.LastGoodAt = e.Newest.Key, now
			}
		case "failing":
			m.ConsecutiveGood = 0
		}
	}
	for name := range rec.Migration {
		if !seen[name] {
			delete(rec.Migration, name)
		}
	}
}

// sanitizeBackupStatus clears control characters and URL credentials from what
// the page serves to viewers and alerts send out.
func sanitizeBackupStatus(env *backupStatusEnvelope) *backupStatusEnvelope {
	env.Target.Error = sanitizeDomainError(env.Target.Error)
	env.AWSCLIProblem = sanitizeDomainError(env.AWSCLIProblem)
	if env.LastRun != nil {
		env.LastRun.Error = sanitizeDomainError(env.LastRun.Error)
	}
	for i := range env.Entries {
		env.Entries[i].Reason = sanitizeDomainError(env.Entries[i].Reason)
		env.Entries[i].LastError = sanitizeDomainError(env.Entries[i].LastError)
	}
	for i := range env.Warnings {
		env.Warnings[i] = sanitizeDomainError(env.Warnings[i])
	}
	return env
}

// ── the sweep ────────────────────────────────────────────────────────────────

// RunBackups sweeps on the slow cadence and runs the automatic restore tests'
// scheduler until ctx ends. Nothing runs at all on a CLI without the fleet engine.
func (s *Server) RunBackups(ctx context.Context) {
	wait := 2 * time.Minute // let the fleet read settle after a start
	s.backups.mu.Lock()
	if last := s.backups.rec.LastSweepAt; !last.IsZero() {
		if remaining := backupSweepInterval - time.Since(last); remaining > 0 {
			wait = remaining
		}
	}
	s.backups.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	tests := time.NewTicker(backupTestTick)
	defer tests.Stop()
	defer s.backups.wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.sweepBackups(ctx)
			timer.Reset(backupSweepInterval)
		case <-s.backups.kick:
			s.sweepBackups(ctx)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(backupSweepInterval)
		case <-tests.C:
			s.tickBackupTests(ctx, time.Now().UTC())
		}
	}
}

func (s *Server) kickBackupSweep() {
	select {
	case s.backups.kick <- struct{}{}:
	default:
	}
}

func (s *Server) sweepBackups(ctx context.Context) {
	b := &s.backups
	b.sweepMu.Lock()
	defer b.sweepMu.Unlock()
	supported := s.cliInstalled() && s.capabilities(ctx).Features.BackupFleet
	var sweepErr string
	var servers []remote.ServerConn
	listed := false
	if supported {
		var err error
		if servers, err = s.resolveServers(ctx); err != nil {
			sweepErr = sanitizeDomainError("server discovery failed: " + err.Error())
			log.Printf("[backups] %s", sweepErr)
		} else {
			listed = true
			s.readBackupServers(ctx, servers)
		}
	}
	if ctx.Err() != nil {
		return // shutting down: not evidence about anything
	}
	b.mu.Lock()
	b.rec.LastSweepAt = time.Now().UTC()
	b.rec.Unsupported = !supported
	b.rec.SweepError = sweepErr
	b.mu.Unlock()
	if listed {
		s.retireRemovedBackupServers(servers)
		s.reconcileBackupTests(time.Now().UTC())
		for _, srv := range servers {
			s.evaluateBackupIncidents(srv.Name, time.Now().UTC())
		}
		if s.alerts.tracker != nil {
			s.alerts.tracker.AnnounceUnannounced()
		}
	}
	b.mu.Lock()
	b.saveLocked()
	b.mu.Unlock()
}

// readBackupServers reads every server under bounded concurrency with a
// per-server deadline: one bad server costs its own budget.
func (s *Server) readBackupServers(ctx context.Context, servers []remote.ServerConn) {
	sem := make(chan struct{}, backupSweepConcurrency)
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			readCtx, cancel := context.WithTimeout(ctx, backupServerTimeout)
			defer cancel()
			env, err := s.readBackupStatus(readCtx, name)
			if ctx.Err() != nil {
				return
			}
			s.recordBackupStatus(name, env, err, time.Now().UTC())
		}(srv.Name)
	}
	wg.Wait()
}

// retireRemovedBackupServers forgets servers no longer in the fleet: their
// incident rules, their automatic restore tests (and those tests' rules) and
// the heartbeat monitor Dash made for them. servers is a complete list
// (discovery succeeded).
func (s *Server) retireRemovedBackupServers(servers []remote.ServerConn) {
	names := make(map[string]bool, len(servers))
	for _, srv := range servers {
		names[srv.Name] = true
	}
	b := &s.backups
	var keys, monitors, testIDs []string
	b.mu.Lock()
	for name, rec := range b.rec.Servers {
		if names[name] {
			continue
		}
		keys = append(keys, rec.RuleKeys...)
		if rec.HeartbeatMonitorID != "" {
			monitors = append(monitors, rec.HeartbeatMonitorID)
		}
		delete(b.rec.Servers, name)
	}
	for id, t := range b.rec.Tests {
		if !names[t.Server] {
			testIDs = append(testIDs, id)
			keys = append(keys, restoretest.RuleKey(id))
			delete(b.rec.Tests, id)
		}
	}
	b.saveLocked()
	b.mu.Unlock()
	if s.alerts.tracker != nil {
		for _, key := range keys {
			s.alerts.tracker.Retire(key, "server removed")
		}
	}
	for _, id := range monitors {
		s.removeBackupHeartbeat(id)
	}
}

// ── the page ─────────────────────────────────────────────────────────────────

type backupRowTest struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Metric string    `json:"metric,omitempty"`
}

// backupMigrateView is the P2.9 readiness gate for one row: a hand-written
// backup job may be removed only after the policy engine has demonstrably
// produced two good backups in a row AND a restore test of the same engine
// kind has passed.
type backupMigrateView struct {
	Ready           bool   `json:"ready"`
	ConsecutiveGood int    `json:"consecutive_good"`
	GreenTest       bool   `json:"green_test"`
	Detail          string `json:"detail"`
}

type backupRowView struct {
	Server             string             `json:"server"`
	Name               string             `json:"name"`
	Kind               string             `json:"kind"`
	App                string             `json:"app,omitempty"`
	Accessory          string             `json:"accessory,omitempty"`
	Engine             string             `json:"engine,omitempty"`
	Container          string             `json:"container_state,omitempty"`
	State              string             `json:"state"`
	Adopted            bool               `json:"adopted"`
	Reason             string             `json:"reason,omitempty"`
	LastError          string             `json:"last_error,omitempty"`
	LastGood           *time.Time         `json:"last_good,omitempty"`
	LastGoodAgeSeconds *int64             `json:"last_good_age_seconds,omitempty"`
	SizeBytes          int64              `json:"size_bytes"`
	ProjectedBytes     int64              `json:"projected_bytes"`
	Encrypted          bool               `json:"encrypted"`
	LegacySchedule     bool               `json:"legacy_schedule,omitempty"`
	OwnSchedule        bool               `json:"own_schedule,omitempty"`
	Excludable         string             `json:"exclude_name,omitempty"`
	LastRestoreTest    *backupRowTest     `json:"last_restore_test,omitempty"`
	Migration          *backupMigrateView `json:"migration,omitempty"`
	UncoveredDatabases []string           `json:"uncovered_databases,omitempty"`
	EvidenceAgeSeconds int64              `json:"evidence_age_seconds"`
}

type backupServerView struct {
	Server             string           `json:"server"`
	State              string           `json:"state"`
	Error              string           `json:"error,omitempty"`
	AgeSeconds         *int64           `json:"age_seconds,omitempty"`
	Target             backupStatTarget `json:"target"`
	Policy             backupStatPolicy `json:"policy"`
	Encrypted          bool             `json:"encrypted"`
	AWSCLIProblem      string           `json:"aws_cli_problem,omitempty"`
	AWSCLI             bool             `json:"aws_cli"`
	AgeCLI             bool             `json:"age_cli"`
	LastRun            *backupStatRun   `json:"last_run,omitempty"`
	Warnings           []string         `json:"warnings,omitempty"`
	LeftoverUploads    int              `json:"leftover_uploads,omitempty"`
	LifecycleRule      string           `json:"lifecycle_rule,omitempty"`
	HeartbeatNote      string           `json:"heartbeat_note,omitempty"`
	HeartbeatMonitorID string           `json:"heartbeat_monitor_id,omitempty"`
}

type backupChecklistItem struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// State: done | todo | unknown.
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// backupHandwrittenJob is one of the fleet's known hand-written backup jobs
// (plan P2.9). Dash cannot read them off a server — systemd timers, /etc/cron.d
// and shell scripts are outside every CLI read it has (a CLI gap; see
// AUDIT_OPEN) — so the list is the owner's checklist and the page pairs it
// with the per-database readiness gate dash does know.
type backupHandwrittenJob struct {
	Name  string `json:"name"`
	Where string `json:"where"`
}

var backupHandwrittenJobs = []backupHandwrittenJob{
	{Name: "akiroo-backup (+ timer, + verify)", Where: "systemd unit and timer (`systemctl list-timers`)"},
	{Name: "teploy-backup.sh", Where: "cron lines and scripts in /etc/cron.d, /etc/cron.daily, /usr/local/bin"},
	{Name: "teploy-wp-dbbackup (with replicate)", Where: "systemd timer plus the replicate script it calls"},
	{Name: "omni-backup", Where: "cron lines and scripts"},
	{Name: "teploy-nucleus-backup", Where: "cron lines and scripts"},
	{Name: "dash-state-backup", Where: "cron lines and scripts on the Dash host"},
}

// HandwrittenRule is the gate, stated once for the page.
const HandwrittenRule = "Remove a hand-written job only when its database shows Ready below: two consecutive good policy backups watched by Dash and one passing restore test of the same engine kind. Keep the job's old dumps until they age out. Removing the job stays a human action on the server; Dash never edits cron or systemd."

type backupsPageView struct {
	CLISupported    bool                   `json:"cli_supported"`
	MinCLIVersion   string                 `json:"min_cli_version"`
	Note            string                 `json:"note,omitempty"`
	SweepError      string                 `json:"sweep_error,omitempty"`
	LastSweepAt     *time.Time             `json:"last_sweep_at,omitempty"`
	Checklist       []backupChecklistItem  `json:"checklist,omitempty"`
	ChecklistDone   bool                   `json:"checklist_done"`
	Storage         *backupStorageView     `json:"storage,omitempty"`
	Recovery        backupRecoveryView     `json:"recovery"`
	Policy          *backupPolicySpec      `json:"policy,omitempty"`
	Window          backupWindow           `json:"window"`
	Presets         []string               `json:"presets"`
	Intervals       []string               `json:"intervals"`
	PublicURLSet    bool                   `json:"public_url_set"`
	Servers         []backupServerView     `json:"servers"`
	Rows            []backupRowView        `json:"rows"`
	Tests           []backupAutoTestView   `json:"tests"`
	ProjectedBytes  int64                  `json:"projected_bytes"`
	SelfBackup      backupSelfView         `json:"self_backup"`
	Handwritten     []backupHandwrittenJob `json:"handwritten,omitempty"`
	HandwrittenRule string                 `json:"handwritten_rule,omitempty"`
}

func (s *Server) handleBackupsPage(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeData(w, s.backupsView(r.Context(), time.Now().UTC()))
}

func (s *Server) handleBackupsRefresh(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireBackupFleet(w, r) {
		return
	}
	s.kickBackupSweep()
	s.auditf(r, "backups.refresh", "", "fleet backup status read requested")
	w.WriteHeader(http.StatusAccepted)
	writeData(w, map[string]bool{"ok": true})
}

// handleBackupsAction serves POST /api/backups/{refresh,storage,policy,exclude,override}
// and /api/backups/recovery-key/{generate,confirm,drill}.
func (s *Server) handleBackupsAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/backups/"), "/")
	action, sub, _ := strings.Cut(rest, "/")
	switch action {
	case "refresh":
		s.handleBackupsRefresh(w, r)
	case "storage":
		s.handleBackupStorage(w, r)
	case "policy":
		s.handleBackupPolicy(w, r)
	case "exclude":
		s.handleBackupExclude(w, r)
	case "override":
		s.handleBackupOverride(w, r)
	case "recovery-key":
		switch sub {
		case "generate":
			s.handleBackupKeyGenerate(w, r)
		case "confirm":
			s.handleBackupKeyConfirm(w, r)
		case "drill":
			s.handleBackupDrill(w, r)
		default:
			jsonError(w, "not found", http.StatusNotFound)
		}
	default:
		jsonError(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) requireBackupFleet(w http.ResponseWriter, r *http.Request) bool {
	return s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.BackupFleet }, "Backups", backupFleetMinCLI)
}

func (s *Server) backupsView(ctx context.Context, now time.Time) backupsPageView {
	supported := s.cliInstalled() && s.capabilities(ctx).Features.BackupFleet
	channels := s.alertChannelsConfigured()
	publicURL := s.alertPublicURL()
	manual := s.manualRestoreTests()
	b := &s.backups
	b.mu.Lock()
	defer b.mu.Unlock()

	view := backupsPageView{
		CLISupported: supported, MinCLIVersion: backupFleetMinCLI, SweepError: b.rec.SweepError,
		Storage: b.rec.storageViewLocked(), Policy: b.rec.Policy, Window: b.rec.Window,
		Presets: backupPresetNames(), Intervals: backupIntervals, PublicURLSet: publicURL != "",
		Servers: []backupServerView{}, Rows: []backupRowView{}, Tests: []backupAutoTestView{},
		SelfBackup: backupSelfGap(), Handwritten: backupHandwrittenJobs, HandwrittenRule: HandwrittenRule,
	}
	if !supported {
		view.Note = "Backups needs teploy " + backupFleetMinCLI + " or later on the dashboard host."
	}
	if !b.rec.LastSweepAt.IsZero() {
		t := b.rec.LastSweepAt
		view.LastSweepAt = &t
	}

	names := make([]string, 0, len(b.rec.Servers))
	for name := range b.rec.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rec := b.rec.Servers[name]
		view.Servers = append(view.Servers, b.serverViewLocked(rec, now))
		rows := b.rowsLocked(rec, manual, now)
		for _, row := range rows {
			view.ProjectedBytes += row.ProjectedBytes
		}
		view.Rows = append(view.Rows, rows...)
	}
	view.Tests = b.testViewsLocked(now)
	view.Recovery = b.rec.recoveryViewLocked(now)
	view.Checklist = b.checklistLocked(view.Servers, view.Rows, channels)
	view.ChecklistDone = b.rec.ChecklistDoneAt != nil
	if !view.ChecklistDone {
		done := len(view.Checklist) > 0
		for _, item := range view.Checklist {
			done = done && item.State == "done"
		}
		if done {
			t := now
			b.rec.ChecklistDoneAt = &t
			b.saveLocked()
			view.ChecklistDone = true
		}
	}
	return view
}

func backupServerFresh(rec *backupServerRecord, now time.Time) bool {
	return rec.State == backupServerOK && rec.Status != nil && now.Sub(rec.LastSuccessAt) <= backupStaleAfter
}

func (b *backupState) serverViewLocked(rec *backupServerRecord, now time.Time) backupServerView {
	v := backupServerView{Server: rec.Server, State: backupStateUnknown, Error: rec.Error,
		HeartbeatNote: rec.HeartbeatNote, HeartbeatMonitorID: rec.HeartbeatMonitorID}
	switch {
	case backupServerFresh(rec, now):
		v.State = backupServerOK
	case rec.State == backupServerFailed:
		v.State = backupServerFailed
	case v.Error == "":
		v.Error = "the last status read is too old to trust"
	}
	if !rec.LastSuccessAt.IsZero() {
		age := int64(now.Sub(rec.LastSuccessAt).Seconds())
		v.AgeSeconds = &age
	}
	if st := rec.Status; st != nil {
		v.Target, v.Policy, v.Encrypted = st.Target, st.Policy, st.Encrypted
		v.AWSCLIProblem, v.AWSCLI, v.AgeCLI = st.AWSCLIProblem, st.AWSCLI, st.AgeCLI
		v.LastRun, v.Warnings, v.LeftoverUploads = st.LastRun, st.Warnings, len(st.LeftoverUploads)
		if st.Target.Configured && st.Target.IncomingLifecycle != "covered" {
			v.LifecycleRule = backupLifecycleRule(b.rec.Storage, st.Target)
		}
	}
	return v
}

// retainedCopies is how many objects this retention keeps of one series at
// steady state. Rules can overlap (the union keeps fewer), so it is an upper
// bound, and the page says so. No retention rule means nothing is pruned: a
// month of runs, since a month is what the projection is for.
func retainedCopies(p backupStatPolicy) int64 {
	perDay := backupRunsPerDay(p.Interval)
	r := p.Retention
	n := int64(r.KeepLast+r.KeepDaily+r.KeepWeekly+r.KeepMonthly) + int64(r.MaxAgeDays)*perDay
	if n == 0 {
		return 30 * perDay
	}
	return n
}

func (b *backupState) rowsLocked(rec *backupServerRecord, manual map[string]backupRowTest, now time.Time) []backupRowView {
	if rec.Status == nil {
		return nil
	}
	fresh := backupServerFresh(rec, now)
	evidenceAge := int64(now.Sub(rec.LastSuccessAt).Seconds())
	rows := make([]backupRowView, 0, len(rec.Status.Entries))
	for i := range rec.Status.Entries {
		e := &rec.Status.Entries[i]
		row := backupRowView{Server: rec.Server, Name: e.Name, Kind: e.Kind, App: e.App, Accessory: e.Accessory,
			Engine: e.Engine, Container: e.State, State: e.Coverage, Adopted: e.Adopted, Reason: e.Reason,
			LastError: e.LastError, Encrypted: e.Encrypted, LegacySchedule: e.ScheduleForm == "legacy",
			OwnSchedule: e.Schedule == "own", UncoveredDatabases: e.UncoveredDatabases, EvidenceAgeSeconds: evidenceAge,
			SizeBytes: entryStoredBytes(e), Excludable: backupExcludeName(e)}
		if !fresh {
			// An unreachable server reads as unknown, never as fine.
			row.State = backupStateUnknown
		}
		if e.LastSuccess != nil {
			t := *e.LastSuccess
			row.LastGood = &t
			age := int64(now.Sub(t).Seconds())
			row.LastGoodAgeSeconds = &age
		}
		if e.Coverage != "excluded" && e.Coverage != "uncovered" {
			row.ProjectedBytes = row.SizeBytes * retainedCopies(rec.Status.Policy)
		}
		row.LastRestoreTest = b.lastRestoreTestLocked(rec.Server, e, manual)
		if m := b.migrationViewLocked(rec, e); m != nil {
			row.Migration = m
		}
		rows = append(rows, row)
	}
	return rows
}

// migrationViewLocked is the P2.9 readiness gate for one entry, None for
// entries the gate does not apply to (volumes, excluded, uncovered).
func (b *backupState) migrationViewLocked(rec *backupServerRecord, e *backupEntry) *backupMigrateView {
	if e.Kind != "accessory" && e.Kind != "adopted" {
		return nil
	}
	if e.Coverage == "excluded" || e.Coverage == "uncovered" {
		return nil
	}
	m := rec.Migration[e.Name]
	v := &backupMigrateView{ConsecutiveGood: 0}
	if m != nil {
		v.ConsecutiveGood = m.ConsecutiveGood
	}
	for _, t := range b.rec.Tests {
		if !t.matches(rec.Server, e.Name, e) || t.LastRun == nil || !t.LastRun.OK || t.LastRun.Skipped != "" {
			continue
		}
		if t.Engine != "" && e.Engine != "" && t.Engine != e.Engine {
			continue // a green test of a DIFFERENT engine proves the wrong thing
		}
		v.GreenTest = true
		break
	}
	// The named hand-written jobs keep their own dumps until they age out;
	// readiness only says the policy engine has taken the job over safely.
	v.Ready = v.ConsecutiveGood >= 2 && v.GreenTest
	switch {
	case v.Ready:
		v.Detail = "Two consecutive good policy backups watched, and a restore test of this engine passed. The old job's dumps can stay until they age out."
	case v.ConsecutiveGood < 2 && !v.GreenTest:
		v.Detail = "Waiting for two consecutive good policy backups and a passing restore test of this engine."
	case v.ConsecutiveGood < 2:
		v.Detail = "A restore test passed; waiting for two consecutive good policy backups."
	default:
		v.Detail = "Two consecutive good policy backups watched; waiting for a passing restore test of this engine."
	}
	return v
}

// entryStoredBytes sums the newest object of each of an entry's series; with none
// it falls back to the measured tree (volumes), which is what a first copy costs.
func entryStoredBytes(e *backupEntry) int64 {
	var total int64
	seen := map[string]bool{}
	for _, db := range e.Databases {
		if db.Newest != nil && !seen[db.Newest.Key] {
			seen[db.Newest.Key] = true
			total += db.Newest.SizeBytes
		}
	}
	if e.Newest != nil && !seen[e.Newest.Key] {
		total += e.Newest.SizeBytes
	}
	if total == 0 {
		return e.SizeBytes
	}
	return total
}

// backupExcludeName is how the policy's --exclude names an entry, "" where it has
// no name the policy accepts.
func backupExcludeName(e *backupEntry) string {
	switch {
	case e.Kind == "volumes" && e.App != "":
		return e.App + "/volumes"
	case e.Kind == "accessory" && e.App != "" && e.Accessory != "":
		return e.App + "/" + e.Accessory
	case e.Name != "" && !strings.Contains(e.Name, "/"):
		return e.Name
	}
	return ""
}

func (b *backupState) checklistLocked(servers []backupServerView, rows []backupRowView, channels bool) []backupChecklistItem {
	if b.rec.ChecklistDoneAt != nil {
		return nil
	}
	storage := backupChecklistItem{ID: "storage", Label: "Connect storage"}
	switch {
	case len(servers) == 0:
		storage.State, storage.Detail = backupStateUnknown, "No server has been read yet."
	default:
		state, detail := "done", ""
		for _, sv := range servers {
			switch {
			case sv.State != backupServerOK:
				if state == "done" {
					state, detail = backupStateUnknown, sv.Server+" could not be read, so its storage is unknown."
				}
			case !sv.Target.Configured:
				state, detail = "todo", sv.Server+" has no backup target."
			case !sv.Target.Reachable:
				state, detail = "todo", sv.Server+" cannot reach its backup target."
			}
		}
		storage.State, storage.Detail = state, detail
	}
	alert := backupChecklistItem{ID: "alerts", Label: "Add an alert channel", State: "todo", Detail: "Failures have nowhere to go until a channel is set in Settings."}
	if channels {
		alert.State, alert.Detail = "done", ""
	}
	drill := backupChecklistItem{ID: "recovery", Label: "Generate a recovery key and do the decrypt drill", State: "todo"}
	switch {
	case b.rec.Recovery == nil:
		drill.Detail = "No recovery key yet: backups are stored unencrypted."
	case b.rec.Recovery.DrillPassedAt != nil:
		drill.State = "done"
	default:
		drill.Detail = "The key exists but has not been proven to decrypt a backup."
	}
	verified := backupChecklistItem{ID: "verified", Label: "First verified backup", State: "todo", Detail: "Waiting for a backup and a restore test that passes."}
	for _, row := range rows {
		if row.LastRestoreTest != nil && row.LastRestoreTest.OK {
			verified.State, verified.Detail = "done", ""
			break
		}
	}
	return []backupChecklistItem{storage, alert, drill, verified}
}

// backupSelfView says where Dash's own backup stands. See docs/rebuild-dash.md.
type backupSelfView struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	Runbook   string `json:"runbook"`
}

// backupSelfGap: plan P2.7 needs dash-state, teploy-config, ssh-key and the secrets
// file stored through the policy, encrypted to the owner key ALONE. CLI v0.1.52
// encrypts every object to the owner key and the server's identity, and backs up
// accessories, app volumes and adopted containers, with no way to name a recipient
// set per target. That is the CLI's to add; Dash does not build a second backup
// path around it (plan principle 1).
func backupSelfGap() backupSelfView {
	return backupSelfView{
		Reason: "Dash does not back itself up yet. The teploy CLI cannot yet encrypt a backup to the recovery key alone, " +
			"and Dash's SSH key must not be readable by a server's own key. Copy Dash's state by hand until it can.",
		Runbook: "docs/rebuild-dash.md",
	}
}
