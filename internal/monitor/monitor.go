package monitor

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/incident"
	"github.com/useteploy/teploy-dash/internal/store"
)

// Alerts v2 default policy (plan P3.1). Probe monitors debounce: three
// consecutive failed checks open an incident, two passing checks recover it.
// A push monitor's lateness already includes its grace window, so one late
// evaluation opens and one ping recovers.
const (
	DefaultFailAfter        = 3
	DefaultRecoverAfter     = 2
	DefaultPushFailAfter    = 1
	DefaultPushRecoverAfter = 1
)

// PolicyFor resolves a monitor's effective alerting policy.
func PolicyFor(m store.Monitor) incident.Policy {
	p := incident.Policy{FailAfter: m.FailAfter, RecoverAfter: m.RecoverAfter, RepeatEvery: m.RepeatEvery}
	if p.FailAfter <= 0 {
		p.FailAfter = DefaultFailAfter
		if m.Type == "push" {
			p.FailAfter = DefaultPushFailAfter
		}
	}
	if p.RecoverAfter <= 0 {
		p.RecoverAfter = DefaultRecoverAfter
		if m.Type == "push" {
			p.RecoverAfter = DefaultPushRecoverAfter
		}
	}
	return p
}

// RuleKey is the incident key of a monitor (stable across type edits).
func RuleKey(id string) string { return "monitor:" + id }

// RuleFor describes a monitor as an incident rule. A push monitor's target
// is its secret URL, so alerts name it generically instead.
func RuleFor(m store.Monitor) incident.Rule {
	r := incident.Rule{Key: RuleKey(m.ID), Source: alert.SourceMonitor, ID: m.ID, Name: m.Name, App: m.App,
		Server: m.Server, Target: m.Target, Severity: alert.SeverityCritical, Path: "/monitors/" + m.ID, Policy: PolicyFor(m)}
	if m.Type == "push" {
		r.Source = alert.SourceHeartbeat
		r.Target = "push heartbeat"
	}
	return r
}

// pushEntry is a running push monitor addressable by its token.
type pushEntry struct {
	m          store.Monitor
	generation uint64
}

// Alerter is what the runner notifies on state transitions. *alert.Dispatcher
// satisfies it directly (restore tests, un-durabled paths); the alert outbox
// satisfies it with durable delivery (D08).
type Alerter interface {
	Send(alert.Event)
}

// Runner manages uptime monitors and runs checks on their intervals.
type Runner struct {
	store    store.Store
	tracker  *incident.Tracker
	client   *http.Client
	stopChs  map[string]chan struct{}
	lastStat map[string]string // last known status per monitor (for transition detection)
	// generations invalidates in-flight checks: every teardown (remove or
	// reload) bumps the monitor's counter, and a check that started under an
	// older generation must not save its result, mutate lastStat, or alert —
	// it describes a configuration that no longer exists.
	generations map[string]uint64
	// push monitors: token -> running config, and the last accepted ping
	// per monitor (seeded from history).
	pushTokens map[string]pushEntry
	lastPing   map[string]time.Time
	// wg tracks in-flight checks so Stop can join them before the store
	// closes (A46).
	wg      sync.WaitGroup
	mu      sync.Mutex
	stopped bool
}

// New creates a monitor runner.
func New(st store.Store) *Runner {
	return &Runner{
		store:       st,
		lastStat:    make(map[string]string),
		generations: make(map[string]uint64),
		pushTokens:  make(map[string]pushEntry),
		lastPing:    make(map[string]time.Time),
		client: &http.Client{
			// No client-level Timeout: each check applies the monitor's own
			// configured timeout via a per-request context (checkHTTP). A
			// fixed client timeout here would silently cap longer per-monitor
			// timeouts.
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		stopChs: make(map[string]chan struct{}),
	}
}

// Start loads all monitors from the store and begins checking.
func (r *Runner) Start() {
	monitors, err := r.store.ListMonitors()
	if err != nil {
		log.Printf("[monitor] Failed to load monitors: %v", err)
		return
	}

	started := 0
	for _, m := range monitors {
		if m.Enabled {
			r.startMonitor(m)
			started++
		}
	}
	log.Printf("[monitor] Started %d monitors", started)
}

// SetTracker routes check outcomes into the Alerts v2 incident tracker.
func (r *Runner) SetTracker(t *incident.Tracker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tracker = t
}

// SetAlerter is the convenience wiring for callers without a shared tracker
// (tests, embedded use): an in-memory tracker that sends through d.
func (r *Runner) SetAlerter(d Alerter) {
	t, _ := incident.New(incident.Options{Alerter: d})
	r.SetTracker(t)
}

// CheckNow runs a single check against the given monitor config without
// saving the result or firing alerts. Used by the "test monitor" button
// so users can verify configuration before enabling.
func (r *Runner) CheckNow(m store.Monitor) store.CheckResult {
	switch m.Type {
	case "http":
		return r.checkHTTP(m)
	case "tcp", "ping":
		return r.checkTCP(m)
	case "push":
		r.mu.Lock()
		last := r.lastPing[m.ID]
		r.mu.Unlock()
		return pushVerdict(m, last, time.Now())
	default:
		return store.CheckResult{
			MonitorID: m.ID,
			CheckedAt: time.Now(),
			Status:    "down",
			Message:   fmt.Sprintf("unknown monitor type: %s", m.Type),
		}
	}
}

// Stop stops all running monitors and waits for all scheduler goroutines
// and any in-flight check to finish, bounded by ctx's deadline (F023:
// callers pass one shared shutdown budget) with a hard default of 90s, so
// shutdown never closes the store underneath a check that is about to
// persist its result (A46). The whole goroutine lifetime is tracked —
// registering only each individual check left a window where Stop observed
// a zero WaitGroup while a scheduler goroutine was between its ticker and
// its next check, and that check then ran against a closed store.
// A straggler is logged rather than silently racing the closing store.
func (r *Runner) Stop(ctx context.Context) {
	r.mu.Lock()
	r.stopped = true
	for _, ch := range r.stopChs {
		close(ch)
	}
	r.stopChs = make(map[string]chan struct{})
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("[monitor] shutdown: worker join cut short by shutdown deadline; a check may race storage shutdown")
	case <-timer.C:
		log.Printf("[monitor] shutdown: an in-flight check did not finish in time; it may race storage shutdown")
	}
}

// Reload reloads a single monitor (stop + start with new config).
//
// A disabled monitor retires its open incident (incident_recovered reason
// "retired"): nobody is watching it any more, so a receiver's open card
// would otherwise stay open forever.
func (r *Runner) Reload(m store.Monitor) {
	if m.Enabled {
		r.stopMonitor(m.ID)
		r.startMonitor(m)
		return
	}
	r.mu.Lock()
	r.teardownLocked(m.ID)
	r.retireLocked(m.ID, "monitor disabled")
	r.mu.Unlock()
}

// retireLocked closes the monitor's incident; the caller holds r.mu (the
// same lock a check holds while it feeds the tracker).
func (r *Runner) retireLocked(id, why string) {
	if r.tracker != nil {
		r.tracker.Retire(RuleKey(id), why)
	}
}

func (r *Runner) startMonitor(m store.Monitor) {
	// Register the configuration read before Stop starts joining workers.
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.wg.Add(1)
	r.mu.Unlock()
	defer r.wg.Done()
	// Read the last persisted status and check time outside the lock (the
	// store call can be slow). Seeding the status lets the first check after
	// a restart fire a transition alert instead of silently adopting the new
	// status with no baseline; the check time is the durable schedule
	// anchor (D08, pass-7 A25 residual).
	var seed string
	var anchor time.Time
	var history []incident.Sample
	var lastPing time.Time
	var newestMessage string
	if r.store != nil {
		// No time floor: the seed only establishes a baseline status, so an old
		// timestamp is fine. A 24h window meant a monitor down longer than a day
		// had no baseline after a restart and its recovery alert was suppressed.
		// Alerts v2: enough history to restore the consecutive pass/fail
		// streak the policy counts (plan P3.1).
		p := PolicyFor(m)
		n := p.FailAfter
		if p.RecoverAfter > n {
			n = p.RecoverAfter
		}
		if n++; n > 50 {
			n = 50
		}
		if checks, err := r.store.GetChecks(m.ID, time.Time{}, n); err == nil && len(checks) > 0 {
			seed = checks[0].Status
			anchor = checks[0].CheckedAt
			newestMessage = checks[0].Message
			for _, c := range checks {
				history = append(history, incident.Sample{OK: c.Status == "up", At: c.CheckedAt})
				if lastPing.IsZero() && c.Status == "up" {
					lastPing = c.CheckedAt
				}
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		return
	}

	// Idempotent: tear down any existing checker for this ID before registering
	// a new one, so a re-add/reload can't leak the previous ticker + goroutine.
	r.teardownLocked(m.ID)
	if seed != "" {
		r.lastStat[m.ID] = seed
	}
	if r.tracker != nil {
		rule := RuleFor(m)
		r.tracker.Seed(rule, history)
		// First run after the upgrade: an outage already visible in the
		// history is adopted unannounced and joins the one summary
		// (incident.Tracker.EndAdoption) instead of alerting on its own.
		if r.tracker.Adopting() {
			if fails, _ := r.tracker.Counts(rule.Key); fails > 0 {
				r.tracker.AdoptOpen(rule, history[fails-1].At, fails, newestMessage)
			}
		}
	}

	interval := m.Interval
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}

	stopCh := make(chan struct{})
	r.stopChs[m.ID] = stopCh

	// R33: capture the generation THIS scheduler runs under while the
	// lifecycle lock is held (teardownLocked just bumped it). The goroutine
	// must never read the live counter later: a scheduler delayed past a
	// reload/remove would otherwise adopt the NEW generation with its OLD
	// captured configuration and pass every freshness check.
	generation := r.generations[m.ID]

	if m.Type == "push" {
		// A push monitor has no probe: its schedule evaluates lateness. The
		// first deadline counts from the newest accepted ping, or from now
		// when there is none (a new monitor gets a full interval + grace).
		prev, known := r.lastPing[m.ID]
		switch {
		case !lastPing.IsZero() && (!known || prev.Before(lastPing)):
			r.lastPing[m.ID] = lastPing
		case !known:
			r.lastPing[m.ID] = time.Now()
		}
		if m.PushToken != "" {
			r.pushTokens[m.PushToken] = pushEntry{m: m, generation: generation}
		}
		r.wg.Add(1)
		go r.runPush(m, generation, stopCh)
		return
	}

	// F023: count the goroutine's WHOLE lifetime before launching it, so
	// Stop's Wait can never observe zero while this scheduler is still
	// between its ticker and its next check (registering per check inside
	// the goroutine left exactly that window).
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// D08 (durable next_due_at, pass-7 A25 residual): the schedule
		// survives restarts off the PERSISTED check history, the same
		// contract the restore runner has with LastRunAt. The first fire
		// waits the true remainder of anchor+interval; an overdue monitor
		// (or one with no history — newly created, or history pruned by
		// retention) fires immediately. Restarting dash more often than the
		// interval can no longer multiply checks. A RELOAD also derives
		// from the anchor, so an edit keeps the cadence — instant
		// verification stays available through the test-monitor button
		// (CheckNow), which is unaffected.
		if delay := initialDelay(anchor, interval, time.Now()); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-stopCh:
				timer.Stop()
				return
			}
		}
		// F035 discipline (the restore runner's fix, applied here): a
		// resettable ONE-SHOT timer, not a Ticker. A Ticker created before
		// the initial-delay wait is phased to scheduler start, and its
		// buffered tick could fire a second check immediately after the
		// first; one-shot timers recomputed after each completion also stop
		// a slow check (timeout up to 60s) from pile-up drift.
		for {
			r.runCheck(m, generation)
			timer := time.NewTimer(interval)
			select {
			case <-timer.C:
			case <-stopCh:
				timer.Stop()
				return
			}
		}
	}()
}

func (r *Runner) stopMonitor(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.teardownLocked(id)
}

// teardownLocked stops and removes a monitor's scheduler goroutine. The caller
// must hold r.mu (so startMonitor can reuse it without a non-reentrant relock).
// The goroutine's one-shot timers are owned by the goroutine itself — closing
// stopCh is the whole cancellation story.
func (r *Runner) teardownLocked(id string) {
	r.generations[id]++
	for token, e := range r.pushTokens {
		if e.m.ID == id {
			delete(r.pushTokens, token)
		}
	}
	if ch, ok := r.stopChs[id]; ok {
		close(ch)
		delete(r.stopChs, id)
	}
}

// initialDelay returns how long a (re)started schedule waits before its first
// check: the remainder of anchor+interval, or 0 when there is no anchor (never
// checked, or history pruned) or the schedule is already overdue (D08, pass-7
// A25 residual — same semantics as the restore runner's LastRunAt derivation).
func initialDelay(anchor time.Time, interval time.Duration, now time.Time) time.Duration {
	if anchor.IsZero() || interval <= 0 {
		return 0
	}
	if due := anchor.Add(interval); due.After(now) {
		return due.Sub(now)
	}
	return 0
}

// Remove stops a monitor's checker and clears its last-known status. Call this
// when a monitor is deleted — otherwise its ticker + goroutine leak and keep
// recording checks for a monitor that no longer exists.
//
// An open incident is retired (incident_recovered reason "retired"): a
// deleted monitor — by a person or by the fleet reconciler — must not leave
// a receiver holding an incident nothing will ever close.
func (r *Runner) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.teardownLocked(id)
	delete(r.lastStat, id)
	delete(r.lastPing, id)
	r.retireLocked(id, "monitor removed")
}

// runCheck executes one check under the scheduler-owned configuration and
// generation. expected is the generation captured when THIS scheduler was
// constructed (R33): a check is stale the moment the live generation moves,
// and staleness is re-checked before any network work, before persistence,
// and before the transition baseline is touched.
func (r *Runner) runCheck(m store.Monitor, expected uint64) {
	r.mu.Lock()
	stale := r.generations[m.ID] != expected
	r.mu.Unlock()
	if stale {
		return
	}

	var result store.CheckResult
	result.MonitorID = m.ID
	result.CheckedAt = time.Now()

	switch m.Type {
	case "http":
		result = r.checkHTTP(m)
	case "tcp":
		result = r.checkTCP(m)
	case "ping":
		result = r.checkTCP(m) // simplified: use TCP as ping substitute
	default:
		result.Status = "down"
		result.Message = fmt.Sprintf("unknown monitor type: %s", m.Type)
	}
	// D08: stamp AFTER the probe helpers — they return fresh CheckResult
	// values that would overwrite a pre-switch stamp. The check runs under
	// the scheduler-captured configuration, so its incarnation is that
	// configuration's; the store CAS commits only if the monitor is still at
	// this incarnation (edit/delete/recreate mid-check drops the row).
	result.Incarnation = m.Incarnation

	// The check ran under the configuration captured above; if the monitor
	// was removed or reloaded while it was in flight, the result is stale:
	// saving it would resurrect lastStat for a deleted monitor or overwrite
	// the new configuration's status, and a transition alert off it would
	// be spurious.
	r.mu.Lock()
	stale = r.generations[m.ID] != expected
	r.mu.Unlock()
	if stale {
		return
	}

	applied, err := r.store.SaveCheck(result)
	if err != nil {
		log.Printf("[monitor] Failed to save check for %s: %v", m.ID, err)
	}
	if !applied {
		// The store's incarnation CAS dropped the result (monitor deleted,
		// edited, or recreated while this check was in flight). The store is
		// the commit authority: no baseline mutation, no alert (D08).
		log.Printf("[monitor] Dropped check for %s: monitor changed while the check was in flight", m.ID)
		return
	}

	// F033 (ours half): revalidate the generation BEFORE publishing the
	// transition and mutating the alert baseline — a reload/remove that
	// landed while the result was being saved must not have its baseline
	// overwritten or its alert fired off stale work. (The save itself is
	// store-side and stays with the deferred revision-CAS work, A19.)
	r.mu.Lock()
	if r.generations[m.ID] != expected {
		r.mu.Unlock()
		return
	}
	r.lastStat[m.ID] = result.Status
	// Alerts v2: every committed check feeds the incident policy (debounce,
	// one incident per outage, holds). A monitor with no history that comes
	// up down still opens — after fail_after checks — so fleet monitors for
	// already-broken domains are not silent forever. Observed UNDER r.mu,
	// after the generation check: a Remove (which retires the incident under
	// the same lock) can never be followed by a stale check recreating the
	// deleted monitor's state.
	if r.tracker != nil {
		r.tracker.Observe(RuleFor(m), result.Status == "up", result.CheckedAt, result.Message)
	}
	r.mu.Unlock()
}

func (r *Runner) checkHTTP(m store.Monitor) store.CheckResult {
	result := store.CheckResult{
		MonitorID: m.ID,
		CheckedAt: time.Now(),
	}

	method := m.Method
	if method == "" {
		method = "GET"
	}

	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, m.Target, nil)
	if err != nil {
		result.Status = "down"
		result.Message = err.Error()
		return result
	}
	req.Header.Set("User-Agent", "teploy-dash/1.0")

	client := httpClientFor(r.client, m.AllowInternal)
	start := time.Now()
	resp, err := client.Do(req)
	result.ResponseTime = time.Since(start)

	if err != nil {
		result.Status = "down"
		result.Message = err.Error()
		return result
	}
	defer resp.Body.Close()

	result.StatusCode = resp.StatusCode

	// If the user configured an exact expected status, require it. Otherwise
	// treat any 2xx/3xx as up. Previously the configured ExpectedStatus was
	// computed but never compared — it only appeared in the error message, so
	// a monitor set to expect e.g. 401 or 201 was judged purely on 2xx/3xx.
	switch {
	case m.ExpectedStatus > 0:
		if resp.StatusCode == m.ExpectedStatus {
			result.Status = "up"
		} else {
			result.Status = "down"
			result.Message = fmt.Sprintf("expected %d, got %d", m.ExpectedStatus, resp.StatusCode)
		}
	case m.Reachable && resp.StatusCode < 500:
		result.Status = "up"
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		result.Status = "up"
	default:
		result.Status = "down"
		result.Message = fmt.Sprintf("unexpected status %d", resp.StatusCode)
	}

	return result
}

func (r *Runner) checkTCP(m store.Monitor) store.CheckResult {
	result := store.CheckResult{
		MonitorID: m.ID,
		CheckedAt: time.Now(),
	}

	timeout := m.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	dial := policyDialContext(m.AllowInternal)

	start := time.Now()
	conn, err := dial(ctx, "tcp", m.Target)
	result.ResponseTime = time.Since(start)

	if err != nil {
		result.Status = "down"
		result.Message = err.Error()
		return result
	}
	conn.Close()

	result.Status = "up"
	return result
}

// pushVerdict judges a push monitor: down once no ping arrived within
// interval + grace of the last accepted one.
func pushVerdict(m store.Monitor, lastPing, now time.Time) store.CheckResult {
	res := store.CheckResult{MonitorID: m.ID, CheckedAt: now, Status: "up"}
	window := m.Interval + m.Grace
	if !lastPing.IsZero() && now.Sub(lastPing) > window {
		res.Status = "down"
		res.Message = fmt.Sprintf("no ping for %s (expected every %s, grace %s)",
			now.Sub(lastPing).Round(time.Second), m.Interval, m.Grace)
	}
	return res
}

// runPush is a push monitor's scheduler: it sleeps until the current
// deadline (last ping + interval + grace), records a down check when the
// deadline passed without a ping, and then waits one more interval before
// recording the next miss.
func (r *Runner) runPush(m store.Monitor, generation uint64, stopCh chan struct{}) {
	defer r.wg.Done()
	for {
		r.mu.Lock()
		last := r.lastPing[m.ID]
		r.mu.Unlock()
		wait := time.Until(last.Add(m.Interval + m.Grace))
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-stopCh:
			timer.Stop()
			return
		}
		r.mu.Lock()
		last = r.lastPing[m.ID]
		r.mu.Unlock()
		res := pushVerdict(m, last, time.Now())
		if res.Status != "down" {
			continue // a ping moved the deadline
		}
		r.commit(m, generation, res)
		// Next miss is recorded one interval later, not in a tight loop.
		timer = time.NewTimer(m.Interval)
		select {
		case <-timer.C:
		case <-stopCh:
			timer.Stop()
			return
		}
	}
}

// commit saves a check under the scheduler's generation and feeds the
// tracker (shared by push evaluations and pings).
func (r *Runner) commit(m store.Monitor, generation uint64, result store.CheckResult) bool {
	r.mu.Lock()
	stale := r.generations[m.ID] != generation
	r.mu.Unlock()
	if stale {
		return false
	}
	result.Incarnation = m.Incarnation
	applied, err := r.store.SaveCheck(result)
	if err != nil {
		log.Printf("[monitor] Failed to save check for %s: %v", m.ID, err)
	}
	if !applied {
		return false
	}
	r.mu.Lock()
	if r.generations[m.ID] != generation {
		r.mu.Unlock()
		return false
	}
	r.lastStat[m.ID] = result.Status
	if r.tracker != nil {
		r.tracker.Observe(RuleFor(m), result.Status == "up", result.CheckedAt, result.Message)
	}
	r.mu.Unlock()
	return true
}

// HasPushToken reports whether a running push monitor owns token (the push
// endpoint checks before spending a rate-limit slot on it).
func (r *Runner) HasPushToken(token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := false
	for t := range r.pushTokens {
		if tokenEqual(t, token) {
			found = true
		}
	}
	return found
}

// ErrUnknownPushToken answers a ping for no running push monitor.
var ErrUnknownPushToken = fmt.Errorf("unknown push token")

// Ping records a push monitor's heartbeat (GET/POST /api/push/<token>). ok
// false records an explicit failure the pinging job reported. The token is
// matched in constant time against the running push monitors.
func (r *Runner) Ping(token string, ok bool, message string) (store.Monitor, error) {
	r.mu.Lock()
	var entry pushEntry
	found := false
	for t, e := range r.pushTokens {
		if tokenEqual(t, token) {
			entry, found = e, true
		}
	}
	r.mu.Unlock()
	if !found {
		return store.Monitor{}, ErrUnknownPushToken
	}
	now := time.Now()
	res := store.CheckResult{MonitorID: entry.m.ID, CheckedAt: now, Status: "up", Message: message}
	if !ok {
		res.Status = "down"
		if res.Message == "" {
			res.Message = "job reported failure"
		}
	}
	if !r.commit(entry.m, entry.generation, res) {
		return entry.m, fmt.Errorf("monitor changed while the ping was recorded; retry")
	}
	if ok {
		r.mu.Lock()
		if r.generations[entry.m.ID] == entry.generation {
			r.lastPing[entry.m.ID] = now
		}
		r.mu.Unlock()
	}
	return entry.m, nil
}

func tokenEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
