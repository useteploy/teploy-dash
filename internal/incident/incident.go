// Package incident is the Alerts v2 policy state machine (plan P3.1/P3.2):
// it turns a stream of pass/fail observations per rule (a monitor, a
// restore test, an operation kind on an app, an alert channel) into
// incidents — opened once after fail_after consecutive failures, recovered
// after recover_after consecutive passes, optionally repeated — and hands
// the resulting Event v2 alerts to the durable outbox.
//
// Rules:
//   - one incident per outage, with a stable incident_id from open to
//     recovery;
//   - a recovery is sent only when the open was sent;
//   - opening is HELD, not dropped, while a hold applies (maintenance, a
//     deploy lock, a running operation, an active silence): failures keep
//     counting, the hold is journaled visibly once per episode, and if the
//     rule is still failing once the hold ends the incident opens then;
//   - with no alert channel configured, incidents still open (unannounced);
//     configuring the first channel sends ONE summary of what is open
//     instead of an alert per rule (AnnounceUnannounced).
//
// Open incidents persist in <dir>/incidents.json (file store only, plan
// principle 9); consecutive pass/fail counts are restored from check
// history by the caller (Seed), not persisted.
package incident

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/durable"
)

const fileName = "incidents.json"

// Policy is one rule's alerting policy.
type Policy struct {
	FailAfter    int
	RecoverAfter int
	// RepeatEvery re-sends an open, announced incident at most this often
	// while it keeps failing (0 = off).
	RepeatEvery time.Duration
	// AutoResolveAfter closes a one-shot incident (an operation failure)
	// that saw no further attempt for this long (0 = never).
	AutoResolveAfter time.Duration
}

func (p Policy) normalized() Policy {
	if p.FailAfter < 1 {
		p.FailAfter = 1
	}
	if p.RecoverAfter < 1 {
		p.RecoverAfter = 1
	}
	return p
}

// Rule identifies what is being watched and describes it for alerts.
type Rule struct {
	// Key is the state key; default Source + ":" + ID. Monitors key on
	// "monitor:<id>" whatever their type, so a type edit keeps the incident.
	Key      string
	Source   string
	ID       string
	Name     string
	App      string
	Server   string
	Target   string
	Severity string
	// Path is the Dash UI path for links (joined onto the public URL).
	Path   string
	Policy Policy
	// NoHold exempts the rule from maintenance/lock/operation holds
	// (operation failures are the event itself; channel health is Dash's
	// own). Silences still apply.
	NoHold bool
	// ExcludeChannels keeps a rule's alerts off named channels (a failing
	// channel is never told about its own failure).
	ExcludeChannels []string
}

func (r Rule) key() string {
	if r.Key != "" {
		return r.Key
	}
	return r.Source + ":" + r.ID
}

// Incident is one open outage.
type Incident struct {
	Key                 string    `json:"key"`
	ID                  string    `json:"id"`
	Source              string    `json:"source"`
	RuleID              string    `json:"rule_id"`
	Name                string    `json:"name"`
	App                 string    `json:"app,omitempty"`
	Server              string    `json:"server,omitempty"`
	Target              string    `json:"target,omitempty"`
	Severity            string    `json:"severity"`
	Path                string    `json:"path,omitempty"`
	Message             string    `json:"message,omitempty"`
	StartedAt           time.Time `json:"started_at"`
	OpenedAt            time.Time `json:"opened_at"`
	Notified            bool      `json:"notified"`
	LastNotifiedAt      time.Time `json:"last_notified_at,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	AutoResolveAt       time.Time `json:"auto_resolve_at,omitempty"`
	ExcludeChannels     []string  `json:"exclude_channels,omitempty"`
}

// Held is a rule whose open is currently being held.
type Held struct {
	Key                 string    `json:"key"`
	Source              string    `json:"source"`
	RuleID              string    `json:"rule_id"`
	Name                string    `json:"name"`
	App                 string    `json:"app,omitempty"`
	Reason              string    `json:"reason"`
	Since               time.Time `json:"since"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

// Sample is one historical observation (for Seed).
type Sample struct {
	OK bool
	At time.Time
}

// Alerter receives the tracker's alerts (the durable outbox).
type Alerter interface {
	Send(alert.Event)
}

// SuppressedRecorder is implemented by alerters that journal held alerts.
type SuppressedRecorder interface {
	RecordSuppressed(ev alert.Event, note string)
}

// Options configures a Tracker. Nil funcs take permissive defaults.
type Options struct {
	Dir     string // "" = in-memory (tests)
	Alerter Alerter
	// HasChannels reports whether any alert channel is configured. Nil =
	// always true.
	HasChannels func() bool
	// Hold reports a maintenance/lock/operation hold for a rule.
	Hold func(Rule, time.Time) (bool, string)
	// Silenced reports an active silence covering a rule.
	Silenced  func(Rule, time.Time) (bool, string)
	PublicURL func() string
	Now       func() time.Time
}

type ruleState struct {
	fails, oks  int
	firstFailAt time.Time
	held        *Held
	// rule and lastMessage let the hold pass open a held rule whose hold
	// ended without waiting for its next observation.
	rule        Rule
	lastMessage string
}

// Tracker is the incident state machine.
type Tracker struct {
	opts Options

	mu        sync.Mutex
	states    map[string]*ruleState
	incidents map[string]*Incident
	// adopting is true from a start with NO incidents.json (first run
	// after the upgrade to incident tracking) until EndAdoption: opens in
	// that window are unannounced, so the outages that already existed are
	// announced as ONE summary instead of an alert per monitor.
	adopting bool
}

// New loads open incidents from dir (when set).
func New(opts Options) (*Tracker, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	t := &Tracker{opts: opts, states: map[string]*ruleState{}, incidents: map[string]*Incident{}}
	if opts.Dir == "" {
		return t, nil
	}
	path := filepath.Join(opts.Dir, fileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.adopting = true
		return t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read incidents: %w", err)
	}
	var doc struct {
		Incidents []*Incident `json:"incidents"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		// A corrupt file must not stop Dash from watching anything: set it
		// aside for inspection and start empty (adopting what is failing,
		// so the outages it held come back as one summary).
		aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if rerr := os.Rename(path, aside); rerr != nil {
			return nil, fmt.Errorf("decode incidents: %w (and could not set it aside: %v)", err, rerr)
		}
		log.Printf("[incident] %s was unreadable (%v); moved to %s and starting with no open incidents", path, err, aside)
		t.adopting = true
		return t, nil
	}
	for _, inc := range doc.Incidents {
		if inc != nil && inc.Key != "" {
			t.incidents[inc.Key] = inc
		}
	}
	return t, nil
}

// SetAlerter swaps the alert sink (wiring order convenience).
func (t *Tracker) SetAlerter(a Alerter) {
	t.mu.Lock()
	t.opts.Alerter = a
	t.mu.Unlock()
}

func newIncidentID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("inc_%x", time.Now().UnixNano())
	}
	return "inc_" + hex.EncodeToString(b[:])
}

func (t *Tracker) state(key string) *ruleState {
	st := t.states[key]
	if st == nil {
		st = &ruleState{}
		t.states[key] = st
	}
	return st
}

type outgoing struct {
	ev   alert.Event
	note string // non-empty = record as suppressed, do not send
}

// flushLocked hands alerts to the outbox WHILE the tracker lock is held, so
// the outbox journals them in the order the state machine produced them: a
// recovery can never be enqueued ahead of its own open by a concurrent
// observation. (The outbox never calls back into the tracker from Send.)
func (t *Tracker) flushLocked(out []outgoing) {
	a := t.opts.Alerter
	if a == nil {
		return
	}
	for _, o := range out {
		if o.note != "" {
			if rec, ok := a.(SuppressedRecorder); ok {
				rec.RecordSuppressed(o.ev, o.note)
			}
			continue
		}
		a.Send(o.ev)
	}
}

func (t *Tracker) hasChannels() bool {
	return t.opts.HasChannels == nil || t.opts.HasChannels()
}

func (t *Tracker) event(inc *Incident, kind, reason, message string, at time.Time) alert.Event {
	status := "down"
	if kind == alert.KindRecovered {
		status = "up"
	}
	var dur int64
	if kind != alert.KindOpened && !inc.StartedAt.IsZero() && at.After(inc.StartedAt) {
		dur = int64(at.Sub(inc.StartedAt) / time.Second)
	}
	service := inc.App
	if service == "" {
		service = inc.Name
	}
	link := ""
	if t.opts.PublicURL != nil && inc.Path != "" {
		if base := strings.TrimRight(t.opts.PublicURL(), "/"); base != "" {
			link = base + inc.Path
		}
	}
	return alert.Event{
		MonitorID: inc.RuleID, MonitorName: inc.Name, Status: status, Message: message,
		OccurredAt: at.UTC(), Source: inc.Source, Version: 2, Kind: kind, Reason: reason,
		IncidentID: inc.ID, RuleID: inc.RuleID, Service: service, App: inc.App, Server: inc.Server,
		Target: inc.Target, Severity: inc.Severity, StartedAt: inc.StartedAt.UTC(), DurationS: dur,
		ConsecutiveFailures: inc.ConsecutiveFailures, URL: link,
		ExcludeChannels: append([]string(nil), inc.ExcludeChannels...),
	}
}

func describe(inc *Incident, rule Rule) {
	inc.Source, inc.RuleID, inc.Name = rule.Source, rule.ID, rule.Name
	inc.App, inc.Server, inc.Target, inc.Path = rule.App, rule.Server, rule.Target, rule.Path
	inc.Severity = rule.Severity
	if inc.Severity == "" {
		inc.Severity = alert.SeverityCritical
	}
	inc.ExcludeChannels = append([]string(nil), rule.ExcludeChannels...)
}

// holdFor returns the reason a rule's open is held now ("" = not held).
func (t *Tracker) holdFor(rule Rule, at time.Time) string {
	if t.opts.Silenced != nil {
		if ok, why := t.opts.Silenced(rule, at); ok {
			return "silenced: " + why
		}
	}
	if !rule.NoHold && t.opts.Hold != nil {
		if ok, why := t.opts.Hold(rule, at); ok {
			return "held: " + why
		}
	}
	return ""
}

// Adopting reports whether the tracker is in its first-run adoption window.
func (t *Tracker) Adopting() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.adopting
}

// AdoptOpen records an outage that already existed when tracking started
// (a monitor whose history is already failing, a restore test whose last
// run failed) as an unannounced open incident. A no-op outside the
// adoption window or when the rule already has an incident.
func (t *Tracker) AdoptOpen(rule Rule, startedAt time.Time, fails int, message string) {
	rule.Policy = rule.Policy.normalized()
	t.mu.Lock()
	defer t.mu.Unlock()
	key := rule.key()
	if !t.adopting || t.incidents[key] != nil || fails < rule.Policy.FailAfter {
		return
	}
	inc := &Incident{Key: key, ID: newIncidentID(), StartedAt: startedAt, OpenedAt: t.opts.Now(),
		ConsecutiveFailures: fails, Message: message}
	describe(inc, rule)
	t.incidents[key] = inc
	t.persistLocked()
}

// AdoptFailing records a failure that existed before anything watched it (a
// domain's first check) as an unannounced open incident, whatever the tracker's
// own adoption window says. The caller follows a batch with AnnounceUnannounced,
// so what was already broken arrives as one summary rather than an alert each.
// A held or silenced rule is left to Observe, which applies the hold; reports
// whether the rule was adopted.
func (t *Tracker) AdoptFailing(rule Rule, startedAt time.Time, message string) bool {
	rule.Policy = rule.Policy.normalized()
	t.mu.Lock()
	defer t.mu.Unlock()
	key := rule.key()
	if t.incidents[key] != nil {
		return false
	}
	now := t.opts.Now()
	if why := t.holdFor(rule, now); why != "" {
		return false
	}
	st := t.state(key)
	st.rule, st.lastMessage = rule, message
	st.oks, st.fails, st.firstFailAt = 0, rule.Policy.FailAfter, startedAt
	inc := &Incident{Key: key, ID: newIncidentID(), StartedAt: startedAt, OpenedAt: now,
		ConsecutiveFailures: st.fails, Message: message}
	describe(inc, rule)
	t.incidents[key] = inc
	t.persistLocked()
	return true
}

// EndAdoption closes the adoption window and announces what it collected
// (one summary for human channels; per-incident opens for machine ones).
// It always writes incidents.json, so the next start is not a first run.
func (t *Tracker) EndAdoption() int {
	t.mu.Lock()
	t.adopting = false
	t.persistLocked()
	t.mu.Unlock()
	return t.AnnounceUnannounced()
}

// ReleaseHolds opens held rules whose hold has ended while they are still
// failing, without waiting for their next observation (a restore test held
// during a deploy would otherwise wait a whole interval, and never alert if
// that next run passed).
func (t *Tracker) ReleaseHolds() int {
	now := t.opts.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []outgoing
	opened := 0
	for key, st := range t.states {
		if st.held == nil || t.incidents[key] != nil || st.fails < st.rule.Policy.normalized().FailAfter {
			continue
		}
		if why := t.holdFor(st.rule, now); why != "" {
			continue
		}
		st.held = nil
		inc := &Incident{Key: key, ID: newIncidentID(), StartedAt: st.firstFailAt, OpenedAt: now,
			ConsecutiveFailures: st.fails, Message: st.lastMessage}
		describe(inc, st.rule)
		if st.rule.Policy.AutoResolveAfter > 0 {
			inc.AutoResolveAt = now.Add(st.rule.Policy.AutoResolveAfter)
		}
		if !t.adopting && t.hasChannels() {
			inc.Notified = true
			inc.LastNotifiedAt = now
			out = append(out, outgoing{ev: t.event(inc, alert.KindOpened, "", st.lastMessage, now)})
		}
		t.incidents[key] = inc
		opened++
	}
	if opened > 0 {
		t.persistLocked()
	}
	t.flushLocked(out)
	return opened
}

// Seed restores a rule's consecutive pass/fail streak from its history
// (newest first) after a restart or reload. It sends nothing.
func (t *Tracker) Seed(rule Rule, history []Sample) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state(rule.key())
	st.fails, st.oks, st.firstFailAt = 0, 0, time.Time{}
	if len(history) == 0 {
		return
	}
	first := history[0].OK
	n := 0
	var oldest time.Time
	for _, s := range history {
		if s.OK != first {
			break
		}
		n++
		oldest = s.At
	}
	if first {
		st.oks = n
	} else {
		st.fails = n
		st.firstFailAt = oldest
	}
}

// Observe feeds one observation for a rule.
func (t *Tracker) Observe(rule Rule, ok bool, at time.Time, message string) {
	rule.Policy = rule.Policy.normalized()
	key := rule.key()
	var out []outgoing

	t.mu.Lock()
	st := t.state(key)
	st.rule, st.lastMessage = rule, message
	inc := t.incidents[key]
	changed := false
	if !ok {
		st.oks = 0
		if st.fails == 0 {
			st.firstFailAt = at
		}
		st.fails++
		switch {
		case inc != nil:
			describe(inc, rule)
			inc.ConsecutiveFailures = st.fails
			inc.Message = message
			if rule.Policy.AutoResolveAfter > 0 {
				inc.AutoResolveAt = at.Add(rule.Policy.AutoResolveAfter)
				changed = true
			}
			if rule.Policy.RepeatEvery > 0 && inc.Notified && at.Sub(inc.LastNotifiedAt) >= rule.Policy.RepeatEvery &&
				t.holdFor(rule, at) == "" && t.hasChannels() {
				inc.LastNotifiedAt = at
				out = append(out, outgoing{ev: t.event(inc, alert.KindRepeat, "", message, at)})
				changed = true
			}
		case st.fails >= rule.Policy.FailAfter:
			if why := t.holdFor(rule, at); why != "" {
				// Journaled once per hold episode (a deploy moving from queued
				// to running is the same hold); the reason shown stays current.
				if st.held != nil {
					st.held.Reason = why
				} else {
					st.held = &Held{Key: key, Source: rule.Source, RuleID: rule.ID, Name: rule.Name, App: rule.App, Reason: why, Since: at}
					probe := &Incident{ID: "held_" + key, StartedAt: st.firstFailAt, ConsecutiveFailures: st.fails}
					describe(probe, rule)
					ev := t.event(probe, alert.KindOpened, "", message, at)
					ev.IncidentID = ""
					out = append(out, outgoing{ev: ev, note: why})
				}
				st.held.ConsecutiveFailures = st.fails
				break
			}
			st.held = nil
			inc = &Incident{Key: key, ID: newIncidentID(), StartedAt: st.firstFailAt, OpenedAt: at,
				ConsecutiveFailures: st.fails, Message: message}
			describe(inc, rule)
			if rule.Policy.AutoResolveAfter > 0 {
				inc.AutoResolveAt = at.Add(rule.Policy.AutoResolveAfter)
			}
			if !t.adopting && t.hasChannels() {
				inc.Notified = true
				inc.LastNotifiedAt = at
				out = append(out, outgoing{ev: t.event(inc, alert.KindOpened, "", message, at)})
			}
			t.incidents[key] = inc
			changed = true
		}
	} else {
		st.fails, st.firstFailAt, st.held = 0, time.Time{}, nil
		st.oks++
		if inc != nil && st.oks >= rule.Policy.RecoverAfter {
			describe(inc, rule)
			if inc.Notified {
				if message == "" {
					message = "recovered"
				}
				out = append(out, outgoing{ev: t.event(inc, alert.KindRecovered, alert.ReasonHealthy, message, at)})
			}
			delete(t.incidents, key)
			changed = true
		}
	}
	if changed {
		t.persistLocked()
	}
	t.flushLocked(out)
	t.mu.Unlock()
}

// Retire forgets a rule (monitor deleted or disabled, restore test removed,
// fleet monitor retired). An announced open incident is closed with
// incident_recovered reason "retired" so receivers do not keep stale cards.
func (t *Tracker) Retire(key, message string) {
	t.mu.Lock()
	inc := t.incidents[key]
	delete(t.states, key)
	var out []outgoing
	if inc != nil {
		delete(t.incidents, key)
		if inc.Notified {
			if message == "" {
				message = "no longer monitored"
			}
			out = append(out, outgoing{ev: t.event(inc, alert.KindRecovered, alert.ReasonRetired, message, t.opts.Now())})
		}
		t.persistLocked()
	}
	t.flushLocked(out)
	t.mu.Unlock()
}

// CloseMatching closes every open incident pred selects, with the given
// recovery reason (a new silence closes what it covers with
// "suppressed_end"; if the rule is still failing after the silence ends a
// fresh incident opens). The rule's failure count is kept, so the hold
// applies to the next failing observation.
func (t *Tracker) CloseMatching(pred func(Incident) bool, reason, message string) int {
	now := t.opts.Now()
	t.mu.Lock()
	var out []outgoing
	n := 0
	for key, inc := range t.incidents {
		if !pred(*inc) {
			continue
		}
		delete(t.incidents, key)
		n++
		if inc.Notified {
			out = append(out, outgoing{ev: t.event(inc, alert.KindRecovered, reason, message, now)})
		}
	}
	if n > 0 {
		t.persistLocked()
	}
	t.flushLocked(out)
	t.mu.Unlock()
	return n
}

// Sweep closes one-shot incidents whose auto-resolve time passed (reason
// "retired": the failing attempt is no longer a live signal).
func (t *Tracker) Sweep() {
	now := t.opts.Now()
	t.mu.Lock()
	var out []outgoing
	changed := false
	for key, inc := range t.incidents {
		if inc.AutoResolveAt.IsZero() || now.Before(inc.AutoResolveAt) {
			continue
		}
		delete(t.incidents, key)
		delete(t.states, key)
		changed = true
		if inc.Notified {
			out = append(out, outgoing{ev: t.event(inc, alert.KindRecovered, alert.ReasonRetired,
				"closed automatically: no further attempt since the failure", now)})
		}
	}
	if changed {
		t.persistLocked()
	}
	t.flushLocked(out)
	t.mu.Unlock()
}

// AnnounceUnannounced sends what opened while no channel was configured:
// one incident is sent as a normal open; several become ONE summary
// message for human channels, while machine channels (webhook, ship) get
// the individual opens they need to group and later resolve incidents.
// Every covered incident is then announced, so its recovery is sent.
func (t *Tracker) AnnounceUnannounced() int {
	if !t.hasChannels() {
		return 0
	}
	now := t.opts.Now()
	t.mu.Lock()
	var pending []*Incident
	for _, inc := range t.incidents {
		if !inc.Notified {
			pending = append(pending, inc)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].StartedAt.Before(pending[j].StartedAt) })
	var out []outgoing
	switch len(pending) {
	case 0:
	case 1:
		out = append(out, outgoing{ev: t.event(pending[0], alert.KindOpened, "", pending[0].Message, now)})
	default:
		var lines []string
		for _, inc := range pending {
			line := fmt.Sprintf("- %s (%s) since %s", inc.Name, inc.Source, inc.StartedAt.UTC().Format(time.RFC3339))
			if inc.Message != "" {
				line += ": " + inc.Message
			}
			lines = append(lines, line)
			ev := t.event(inc, alert.KindOpened, "", inc.Message, now)
			ev.Audience = alert.AudienceMachine
			out = append(out, outgoing{ev: ev})
		}
		link := ""
		if t.opts.PublicURL != nil {
			if base := strings.TrimRight(t.opts.PublicURL(), "/"); base != "" {
				link = base + "/monitors"
			}
		}
		summary := alert.Event{
			MonitorID: "teploy-dash-summary", MonitorName: fmt.Sprintf("%d problems already open", len(pending)),
			Status: "down", Message: "Alerts are now configured. Currently open:\n" + strings.Join(lines, "\n"),
			OccurredAt: now.UTC(), Source: alert.SourceMonitor, Version: 2, Kind: alert.KindSummary,
			Severity: alert.SeverityCritical, StartedAt: pending[0].StartedAt.UTC(), URL: link, Audience: alert.AudienceHuman,
		}
		out = append([]outgoing{{ev: summary}}, out...)
	}
	for _, inc := range pending {
		inc.Notified = true
		inc.LastNotifiedAt = now
	}
	if len(pending) > 0 {
		t.persistLocked()
	}
	t.flushLocked(out)
	t.mu.Unlock()
	return len(pending)
}

// Open lists open incidents, oldest first.
func (t *Tracker) Open() []Incident {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Incident, 0, len(t.incidents))
	for _, inc := range t.incidents {
		out = append(out, *inc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// HeldRules lists rules whose open is currently held.
func (t *Tracker) HeldRules() []Held {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Held
	for _, st := range t.states {
		if st.held != nil {
			out = append(out, *st.held)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// Counts reports a rule's current consecutive failures and passes.
func (t *Tracker) Counts(key string) (fails, oks int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.states[key]; st != nil {
		return st.fails, st.oks
	}
	return 0, 0
}

func (t *Tracker) persistLocked() {
	if t.opts.Dir == "" {
		return
	}
	list := make([]*Incident, 0, len(t.incidents))
	for _, inc := range t.incidents {
		list = append(list, inc)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	raw, err := json.MarshalIndent(map[string]any{"incidents": list}, "", "  ")
	if err == nil {
		err = durable.Replace(filepath.Join(t.opts.Dir, fileName), raw, 0600)
	}
	if err != nil {
		log.Printf("[incident] persist failed (state in memory only until next write): %v", err)
	}
}
