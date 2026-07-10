// Package restoretest schedules backup verification runs: on each test's
// interval it shells out to `teploy accessory verify-backup`, which restores
// the accessory's latest S3 backup into a scratch container on the server and
// proves the restored copy is usable. Modeled on internal/monitor's runner
// (per-item ticker + goroutine), but hourly-scale instead of seconds-scale,
// so only the last result is persisted — on the RestoreTest entity itself.
package restoretest

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/store"
)

// VerifyResult mirrors the CLI's --json output for `accessory verify-backup`.
type VerifyResult struct {
	App        string `json:"app"`
	Accessory  string `json:"accessory"`
	Image      string `json:"image"`
	Kind       string `json:"kind"`
	Date       string `json:"date"`
	S3Key      string `json:"s3_key"`
	SizeBytes  int64  `json:"size_bytes"`
	Metric     string `json:"metric"`
	DurationMs int64  `json:"duration_ms"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail,omitempty"`
}

// runCLIFunc abstracts the CLI delegate call so tests can fake the subprocess.
type runCLIFunc func(server, user, app, accessory, bucket, region string) (stdout, stderr string, err error)

// Runner manages restore tests and runs them on their intervals.
type Runner struct {
	store   store.Store
	alerter *alert.Dispatcher
	// userFor resolves the SSH user for a server name (from servers.yml via
	// the dash server's cached list); nil/"" falls back to the CLI default.
	userFor func(server string) string
	runCLI  runCLIFunc
	timers  map[string]*time.Ticker
	stopChs map[string]chan struct{}
	lastOK  map[string]bool // last known outcome per test (for transition alerts)
	mu      sync.Mutex
}

// New creates a restore-test runner.
func New(st store.Store) *Runner {
	return &Runner{
		store:   st,
		timers:  make(map[string]*time.Ticker),
		stopChs: make(map[string]chan struct{}),
		lastOK:  make(map[string]bool),
		runCLI: func(server, user, app, accessory, bucket, region string) (string, string, error) {
			res, err := cli.AccessoryVerifyBackup(server, user, app, accessory, bucket, region)
			if res == nil {
				return "", "", err
			}
			return res.Stdout, res.Stderr, err
		},
	}
}

// SetAlerter configures the alert dispatcher for fail/recover notifications.
func (r *Runner) SetAlerter(d *alert.Dispatcher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerter = d
}

// SetUserResolver installs the server-name -> SSH-user lookup.
func (r *Runner) SetUserResolver(f func(server string) string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userFor = f
}

// Start loads all restore tests from the store and begins scheduling.
func (r *Runner) Start() {
	tests, err := r.store.ListRestoreTests()
	if err != nil {
		log.Printf("[restoretest] Failed to load restore tests: %v", err)
		return
	}
	for _, t := range tests {
		if t.Enabled {
			r.startTest(t)
		}
	}
	log.Printf("[restoretest] Started %d restore tests", len(tests))
}

// Stop stops all scheduled tests.
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, ch := range r.stopChs {
		close(ch)
		if t, ok := r.timers[id]; ok {
			t.Stop()
		}
	}
	r.timers = make(map[string]*time.Ticker)
	r.stopChs = make(map[string]chan struct{})
}

// Reload reloads a single test (stop + start with new config).
func (r *Runner) Reload(t store.RestoreTest) {
	r.stopTest(t.ID)
	if t.Enabled {
		r.startTest(t)
	}
}

// Remove stops a test's schedule and clears its last-known outcome.
func (r *Runner) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.teardownLocked(id)
	delete(r.lastOK, id)
}

func (r *Runner) startTest(t store.RestoreTest) {
	r.mu.Lock()

	// Idempotent: tear down any existing schedule for this ID first.
	r.teardownLocked(t.ID)
	// Seed the transition baseline from the persisted outcome so a failure
	// that predates a dash restart still alerts on recovery (and vice versa).
	if !t.LastRunAt.IsZero() {
		r.lastOK[t.ID] = t.LastOK
	}

	interval := time.Duration(t.IntervalHours) * time.Hour
	if interval < time.Hour {
		interval = time.Hour
	}

	ticker := time.NewTicker(interval)
	stopCh := make(chan struct{})
	r.timers[t.ID] = ticker
	r.stopChs[t.ID] = stopCh
	r.mu.Unlock()

	go func() {
		// Run immediately only on first-ever schedule; an interval-boundary
		// run is expensive (downloads the backup, boots a container), so a
		// dash restart must not re-trigger every configured test at once.
		if t.LastRunAt.IsZero() {
			r.RunNow(t)
		}
		for {
			select {
			case <-ticker.C:
				// Re-read config each tick so edits between ticks apply and a
				// deleted test doesn't get re-persisted by a stale copy.
				cur, err := r.store.GetRestoreTest(t.ID)
				if err != nil || !cur.Enabled {
					continue
				}
				r.RunNow(*cur)
			case <-stopCh:
				return
			}
		}
	}()
}

func (r *Runner) stopTest(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.teardownLocked(id)
}

// teardownLocked stops and removes a test's ticker + goroutine. Caller must
// hold r.mu.
func (r *Runner) teardownLocked(id string) {
	if ch, ok := r.stopChs[id]; ok {
		close(ch)
		delete(r.stopChs, id)
	}
	if t, ok := r.timers[id]; ok {
		t.Stop()
		delete(r.timers, id)
	}
}

// RunNow executes one verification run synchronously, persists the outcome
// onto the test, and fires fail/recover alerts. Returns the updated test.
func (r *Runner) RunNow(t store.RestoreTest) store.RestoreTest {
	r.mu.Lock()
	userFor := r.userFor
	runCLI := r.runCLI
	r.mu.Unlock()

	user := ""
	if userFor != nil {
		user = userFor(t.Server)
	}

	stdout, stderr, err := runCLI(t.Server, user, t.App, t.Accessory, t.Bucket, t.Region)

	t.LastRunAt = time.Now()
	var res VerifyResult
	switch {
	case err != nil && strings.TrimSpace(stdout) == "":
		// The subprocess itself failed (timeout, missing binary) — no result.
		t.LastOK = false
		t.LastDetail = fmt.Sprintf("verify-backup did not run: %v", err)
	case json.Unmarshal([]byte(strings.TrimSpace(stdout)), &res) != nil:
		// Non-zero exit before a result was produced (bad flags, SSH failure).
		t.LastOK = false
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = strings.TrimSpace(stdout)
		}
		t.LastDetail = fmt.Sprintf("no verification result: %s", detail)
	default:
		t.LastOK = res.OK
		t.LastDetail = res.Detail
		t.LastMetric = res.Metric
		t.LastDate = res.Date
		t.LastDurationMs = res.DurationMs
	}

	if err := r.store.SaveRestoreTest(t); err != nil {
		log.Printf("[restoretest] Failed to save result for %s: %v", t.ID, err)
	}

	// Alert on failure, and on recovery after a known failure.
	r.mu.Lock()
	prev, hadPrev := r.lastOK[t.ID]
	r.lastOK[t.ID] = t.LastOK
	alerter := r.alerter
	r.mu.Unlock()

	if alerter != nil {
		name := fmt.Sprintf("restore-test %s/%s on %s", t.App, t.Accessory, t.Server)
		if !t.LastOK {
			alerter.Send(alert.Event{
				MonitorID:   t.ID,
				MonitorName: name,
				Status:      "down",
				Message:     t.LastDetail,
				OccurredAt:  t.LastRunAt,
			})
		} else if hadPrev && !prev {
			alerter.Send(alert.Event{
				MonitorID:   t.ID,
				MonitorName: name,
				Status:      "up",
				Message:     fmt.Sprintf("backup %s verified (%s)", t.LastDate, t.LastMetric),
				OccurredAt:  t.LastRunAt,
			})
		}
	}

	return t
}
