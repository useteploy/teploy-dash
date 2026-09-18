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
	"os/exec"
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

// runCLIFunc abstracts the CLI delegate call so tests can fake the
// subprocess. The contract (matching the real adapter below): exitErr is
// the CLI's NON-ZERO EXIT, which the documented verify-backup semantics say
// still carries a JSON verdict on stdout; err is a TRANSPORT failure
// (timeout, missing binary) that no stdout payload can override (A26).
type runCLIFunc func(server, user, app, accessory, bucket, region string) (stdout, stderr string, exitErr error, err error)

// Runner manages restore tests and runs them on their intervals.
type Runner struct {
	store   store.Store
	alerter *alert.Dispatcher
	// userFor resolves the SSH user for a server name (from servers.yml via
	// the dash server's cached list); nil/"" falls back to the CLI default.
	userFor func(server string) string
	// hostFor resolves a server ALIAS to its configured host/IP. The CLI's
	// --host flag in app-scoped mode is a raw address, not a servers.yml key,
	// so passing the alias fails with "no such host" whenever the two differ
	// (A16). Nil falls back to the alias itself.
	hostFor func(server string) string
	runCLI  runCLIFunc
	timers  map[string]*time.Ticker
	stopChs map[string]chan struct{}
	lastOK  map[string]bool // last known outcome per test (for transition alerts)
	// running is the per-test single-flight claim (A25): a manual run and a
	// scheduled tick (or a reload racing either) share one claim, so the
	// same test never runs two expensive verifications at once.
	running map[string]bool
	// wg tracks in-flight runs so Stop can join them before the store
	// closes (A46).
	wg sync.WaitGroup
	mu sync.Mutex
}

// New creates a restore-test runner.
func New(st store.Store) *Runner {
	return &Runner{
		store:   st,
		timers:  make(map[string]*time.Ticker),
		stopChs: make(map[string]chan struct{}),
		lastOK:  make(map[string]bool),
		running: make(map[string]bool),
		runCLI: func(server, user, app, accessory, bucket, region string) (string, string, error, error) {
			res, err := cli.AccessoryVerifyBackup(server, user, app, accessory, bucket, region)
			if res == nil {
				return "", "", nil, err
			}
			// The delegate runs verify-backup with plain Run: a NON-ZERO
			// EXIT is not an error for this command (the CLI prints its JSON
			// verdict on stdout and exits non-zero on failed verification),
			// so a real err here is transport-only. The exit-error parameter
			// stays available for adapters that surface it.
			var exitErr error
			if ee, ok := err.(*exec.ExitError); ok {
				exitErr, err = ee, nil
			}
			return res.Stdout, res.Stderr, exitErr, err
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

// SetHostResolver installs the server-alias -> host/IP lookup (A16).
func (r *Runner) SetHostResolver(f func(server string) string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hostFor = f
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

// Stop stops all scheduled tests and waits (bounded) for in-flight runs to
// finish, so shutdown never closes the store underneath a live verification
// (A46). A run that outlives the bound is logged loudly rather than
// silently racing the closing store.
func (r *Runner) Stop() {
	r.mu.Lock()
	for id, ch := range r.stopChs {
		close(ch)
		if t, ok := r.timers[id]; ok {
			t.Stop()
		}
	}
	r.timers = make(map[string]*time.Ticker)
	r.stopChs = make(map[string]chan struct{})
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		log.Printf("[restoretest] shutdown: a verification run did not finish in time; it may race storage shutdown")
	}
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
	// Bound the conversion: an absurd interval_hours (e.g. 1e9) overflows
	// time.Duration and produces a negative or tiny ticker period.
	if interval > 365*24*time.Hour {
		interval = 365 * 24 * time.Hour
	}

	ticker := time.NewTicker(interval)
	stopCh := make(chan struct{})
	r.timers[t.ID] = ticker
	r.stopChs[t.ID] = stopCh
	r.mu.Unlock()

	go func() {
		// Interval-boundary runs are expensive (they download the backup and
		// boot a scratch container), so scheduling follows the PERSISTED
		// last-run time rather than the process clock: a first-ever test
		// runs immediately, an overdue one runs promptly on restart, and a
		// fresh one waits exactly the remainder of its interval — restarting
		// more often than the interval can no longer postpone verification
		// indefinitely (A25).
		if !t.LastRunAt.IsZero() {
			if delay := initialDelay(t.LastRunAt, interval, time.Now()); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-stopCh:
					timer.Stop()
					return
				}
			}
		}
		if cur, err := r.store.GetRestoreTest(t.ID); err == nil && cur.Enabled {
			r.RunNow(*cur)
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

// initialDelay returns how long a restarted schedule waits before its first
// run: the remainder of lastRun+interval, or 0 when the test has never run
// or is already overdue (A25).
func initialDelay(lastRun time.Time, interval time.Duration, now time.Time) time.Duration {
	if lastRun.IsZero() || interval <= 0 {
		return 0
	}
	if due := lastRun.Add(interval); due.After(now) {
		return due.Sub(now)
	}
	return 0
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
// A manual request and a scheduled tick share the per-test claim, so a
// second concurrent invocation returns the unchanged record instead of
// stacking an expensive overlapping run (A25).
func (r *Runner) RunNow(t store.RestoreTest) store.RestoreTest {
	r.mu.Lock()
	if r.running[t.ID] {
		r.mu.Unlock()
		return t
	}
	r.running[t.ID] = true
	r.wg.Add(1)
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, t.ID)
		r.mu.Unlock()
		r.wg.Done()
	}()

	r.mu.Lock()
	userFor := r.userFor
	hostFor := r.hostFor
	runCLI := r.runCLI
	r.mu.Unlock()

	user := ""
	if userFor != nil {
		user = userFor(t.Server)
	}
	host := t.Server
	if hostFor != nil {
		if resolved := hostFor(t.Server); resolved != "" {
			host = resolved
		}
	}

	stdout, stderr, _, err := runCLI(host, user, t.App, t.Accessory, t.Bucket, t.Region)

	// A26: the verdict starts from a FRESH projection — failure branches
	// must not inherit the previous run's metric/date/duration, and a
	// transport error (timeout, missing binary) is a failed verification
	// even when the child managed to print a parseable {"ok":true} before
	// hanging.
	t.LastRunAt = time.Now()
	var res VerifyResult
	switch {
	case err != nil:
		t.LastOK = false
		t.LastMetric = ""
		t.LastDate = ""
		t.LastDurationMs = 0
		t.LastDetail = fmt.Sprintf("verify-backup did not complete: %v", err)
	case json.Unmarshal([]byte(strings.TrimSpace(stdout)), &res) != nil:
		t.LastOK = false
		t.LastMetric = ""
		t.LastDate = ""
		t.LastDurationMs = 0
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

	// Persist ONLY the result fields (A15): a config edit saved while this
	// long run was in flight must survive, and a deleted test must not be
	// resurrected by its own completion. A26: a failed save makes the run
	// visibly unpersisted — no alert is sent off a state nobody can see.
	if err := r.store.SaveRestoreTestResult(t.ID, t); err != nil {
		log.Printf("[restoretest] Failed to save result for %s: %v (result not persisted, no alert sent)", t.ID, err)
		t.LastDetail = fmt.Sprintf("%s [result could not be persisted: %v]", t.LastDetail, err)
		return t
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
