// Package outbox provides durable alert delivery for monitor state
// transitions and restore-test verdicts (D08): attempts persist in an
// append-only journal, failures retry with exponential backoff up to a
// bounded attempt count, exhausted records dead-letter, and the last
// failure is visible per channel. A restart resumes PENDING deliveries and
// never re-sends delivered ones (delivery-id dedupe against the journal).
//
// Delivery is accounted PER CHANNEL: a webhook that acknowledged before an
// email failure is not re-sent on retry. Payloads carry the delivery id
// (webhook JSON field, X-Teploy-Delivery-Id email header) so receivers can
// dedupe the at-least-once redelivery a crash mid-attempt still permits.
package outbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/durable"
)

const journalName = "alert-outbox.jsonl"

// Status of a delivery record (and of one channel leg inside it).
const (
	StatusPending      = "pending"
	StatusDelivered    = "delivered"
	StatusDeadLettered = "dead_lettered"
	// StatusSuppressed is a terminal record that was never sent: the alert
	// was held (maintenance, deploy lock, a running operation, a silence)
	// and is journaled so the hold is visible in delivery history.
	StatusSuppressed = "suppressed"
)

// Event sources sharing one outbox (D08): the label scopes delivery-status
// lookups so a restore test's record never answers for a monitor (and vice
// versa). Empty on records/events from before the label existed — read as
// monitor. Canonical values live in the alert package (they are part of the
// Event contract); these aliases keep call sites local.
const (
	SourceMonitor     = alert.SourceMonitor
	SourceRestoreTest = alert.SourceRestoreTest
	SourceHeartbeat   = alert.SourceHeartbeat
)

// Sender delivers one event synchronously; a nil error means every
// configured channel acknowledged it. *alert.Dispatcher satisfies it.
type Sender interface {
	SendSync(alert.Event) error
}

// ChannelSender delivers one event through exactly one named channel
// (D08 per-channel accounting): the outbox retries only the legs that
// failed. *alert.Dispatcher satisfies it; a plain Sender falls back to
// whole-record attempts (legacy semantics — every configured channel
// re-sent per retry).
type ChannelSender interface {
	SendChannel(event alert.Event, channel string) error
}

// ChannelRecord is one channel leg's delivery ledger entry.
type ChannelRecord struct {
	Status        string    `json:"status"` // pending | delivered | dead_lettered
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error,omitempty"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
}

// Record is one durable delivery attempt chain, journal-snake-shot on every
// transition (enqueue, each attempt outcome); replay folds by ID keeping
// the last line.
type Record struct {
	ID            string      `json:"id"`
	Event         alert.Event `json:"event"`
	Status        string      `json:"status"`
	Attempts      int         `json:"attempts"`
	NextAttemptAt time.Time   `json:"next_attempt_at,omitempty"`
	LastError     string      `json:"last_error,omitempty"`
	LastAttemptAt time.Time   `json:"last_attempt_at,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	// Channels is the per-channel ledger (D08). Nil on records written
	// before per-channel accounting landed: those replay with record-level
	// semantics (each retry re-sends every configured channel), and once
	// such a record delivers it stays delivered exactly as before.
	Channels map[string]ChannelRecord `json:"channels,omitempty"`
	// Note explains a suppressed record (what held it).
	Note string `json:"note,omitempty"`
}

// ChannelStatus is the read-only view of one channel leg.
type ChannelStatus struct {
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error,omitempty"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
}

// DeliveryStatus is the read-only view exposed on the monitor API.
type DeliveryStatus struct {
	ID            string                   `json:"id"`
	Status        string                   `json:"status"`
	Attempts      int                      `json:"attempts"`
	LastError     string                   `json:"last_error,omitempty"`
	LastAttemptAt time.Time                `json:"last_attempt_at,omitempty"`
	NextAttemptAt time.Time                `json:"next_attempt_at,omitempty"`
	EventStatus   string                   `json:"event_status"`
	OccurredAt    time.Time                `json:"occurred_at"`
	Channels      map[string]ChannelStatus `json:"channels,omitempty"`
	// History fields (delivery-history listing).
	Kind       string    `json:"kind,omitempty"`
	Source     string    `json:"source,omitempty"`
	RuleID     string    `json:"rule_id,omitempty"`
	RuleName   string    `json:"rule_name,omitempty"`
	IncidentID string    `json:"incident_id,omitempty"`
	Note       string    `json:"note,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// ChannelHealth summarizes one channel's retained delivery history (plan
// P3.6): how many legs delivered, are pending, and dead-lettered, and the
// most recent of each outcome.
type ChannelHealth struct {
	Delivered        int       `json:"delivered"`
	Pending          int       `json:"pending"`
	DeadLettered     int       `json:"dead_lettered"`
	LastDeliveredAt  time.Time `json:"last_delivered_at,omitempty"`
	LastDeadLetterAt time.Time `json:"last_dead_letter_at,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
}

// Options configures an Outbox. Zero fields take the documented defaults.
type Options struct {
	// ConfigFn is the live alert configuration source, read at every attempt
	// so a Settings PATCH applies to retries without rewiring.
	ConfigFn func() (alert.Config, error)
	// Sender overrides the delivery mechanism (tests). Default: a Dispatcher
	// built from the CURRENT config per attempt.
	Sender Sender
	// PollInterval bounds how soon a due (or resumed) delivery is attempted.
	PollInterval time.Duration
	// MaxAttempts is the attempt count before a record dead-letters.
	MaxAttempts int
	// BackoffBase/BackoffMax bound the exponential retry backoff.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// MaxRetained bounds the record count kept after compaction (both
	// delivered and dead-lettered history for visibility).
	MaxRetained int
	// OnDeadLetter is called (outside the outbox lock) once per channel leg
	// that dead-letters, with the record's event. OnDelivered is called per
	// leg that acknowledges. Both drive channel-health alerts (plan P3.6).
	OnDeadLetter func(channel string, ev alert.Event, lastError string)
	OnDelivered  func(channel string, ev alert.Event)
}

func (o Options) withDefaults() Options {
	if o.PollInterval <= 0 {
		o.PollInterval = 5 * time.Second
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 5
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = 30 * time.Second
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 30 * time.Minute
	}
	if o.MaxRetained <= 0 {
		o.MaxRetained = 250
	}
	return o
}

// Outbox is the durable alert delivery queue.
type Outbox struct {
	dir  string
	opts Options

	mu      sync.Mutex
	records map[string]*Record
	order   []string // insertion order (creation), for compaction
	lines   int      // journal lines since load (compaction pressure)

	wake   chan struct{}
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// New loads (or initializes) the outbox journal in dir. A torn final line
// (crash mid-append, no trailing newline) is dropped; a corrupt line
// anywhere else fails loudly — a silently truncated alert history is worse
// than a refused start.
func New(dir string, opts Options) (*Outbox, error) {
	opts = opts.withDefaults()
	o := &Outbox{
		dir:     dir,
		opts:    opts,
		records: make(map[string]*Record),
		wake:    make(chan struct{}, 1),
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("alert outbox dir: %w", err)
	}
	if err := o.load(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *Outbox) journalPath() string { return filepath.Join(o.dir, journalName) }

func (o *Outbox) load() error {
	data, err := durable.ReadJSONL(o.journalPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read alert outbox journal: %w", err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("alert outbox journal line %d: %w", line, err)
		}
		if prev, ok := o.records[rec.ID]; ok {
			*prev = rec // fold: the last line for an ID is its state
		} else {
			cp := rec
			o.records[rec.ID] = &cp
			o.order = append(o.order, rec.ID)
		}
		o.lines++
	}
	return scanner.Err()
}

func endsWithNewline(s string) bool { return len(s) > 0 && s[len(s)-1] == '\n' }

func lastLineIndex(s string) int {
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			n++
		}
	}
	return n
}

// deliveryID derives the deterministic delivery id from the transition:
// monitor + status + occurrence time. The runner enqueues once per
// transition, so a repeated Send of the same transition (or a journal
// replay after restart) maps to one delivery.
//
// v2 events add the kind, incident and routing into the derivation: a
// repeat and a recovery of one incident, or the per-channel copies of one
// channel-health alert, are distinct deliveries. Pre-v2 events keep the
// original derivation so journaled ids stay stable.
func deliveryID(ev alert.Event) string {
	key := fmt.Sprintf("%s|%s|%d", ev.MonitorID, ev.Status, ev.OccurredAt.UnixNano())
	if ev.Version >= 2 {
		key = fmt.Sprintf("v2|%s|%s|%s|%s|%s|%d|%v|%v|%s", ev.Source, ev.MonitorID, ev.Kind, ev.Reason, ev.IncidentID,
			ev.OccurredAt.UnixNano(), ev.OnlyChannels, ev.ExcludeChannels, ev.Audience)
	}
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:16])
}

// DeliveryID exposes the deterministic id an event would be journaled under.
func DeliveryID(ev alert.Event) string { return deliveryID(ev) }

// Send enqueues an event for durable delivery (deduped by delivery id). It
// is the monitor.Alerter contract. With no channels configured AND the
// config readable, there is nothing to deliver and nothing to make durable:
// the call stays the documented best-effort no-op. An unreadable config
// still enqueues — the attempt will fail loudly and the failure surfaces.
func (o *Outbox) Send(ev alert.Event) {
	cfg, cfgErr := o.currentConfig()
	if cfgErr == nil && len(alert.ChannelsFor(cfg, ev)) == 0 {
		return
	}
	id := deliveryID(ev)

	o.mu.Lock()
	if _, exists := o.records[id]; exists {
		o.mu.Unlock()
		return // delivery-id dedupe: pending resumes, terminal stays done
	}
	now := time.Now().UTC()
	rec := &Record{ID: id, Event: ev, Status: StatusPending, NextAttemptAt: now, CreatedAt: now}
	o.records[id] = rec
	o.order = append(o.order, id)
	o.mu.Unlock()

	if err := o.persist(rec); err != nil {
		// Loud, visible, delivery still proceeds: degraded durability (a
		// restart may re-send), never a silently lost alert.
		log.Printf("[outbox] ALERT JOURNAL APPEND FAILED (delivery proceeds, restart may re-send): %v", err)
	}
	o.kick()
}

// RecordSuppressed journals an alert that was HELD rather than sent (plan
// P3.2: suppressed events are recorded visibly). The record is terminal;
// it is never delivered. Deduped by delivery id like Send.
func (o *Outbox) RecordSuppressed(ev alert.Event, note string) {
	id := deliveryID(ev)
	o.mu.Lock()
	if _, exists := o.records[id]; exists {
		o.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	rec := &Record{ID: id, Event: ev, Status: StatusSuppressed, CreatedAt: now, Note: note}
	o.records[id] = rec
	o.order = append(o.order, id)
	o.mu.Unlock()
	o.persistQuiet(rec)
}

// Start launches the delivery worker (single-threaded: alert volume is
// human-scale and serialized retries keep the journal strictly ordered).
func (o *Outbox) Start() {
	o.stopCh = make(chan struct{})
	o.wg.Add(1)
	go o.run()
}

// Stop joins the worker bounded by ctx's deadline (F022/F023 discipline).
func (o *Outbox) Stop(ctx context.Context) {
	if o.stopCh == nil {
		return
	}
	close(o.stopCh)
	done := make(chan struct{})
	go func() {
		o.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("[outbox] shutdown: worker join cut short; an in-flight attempt may be re-sent on next start (at-least-once)")
	}
}

func (o *Outbox) kick() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *Outbox) run() {
	defer o.wg.Done()
	ticker := time.NewTicker(o.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-o.stopCh:
			return
		case <-ticker.C:
		case <-o.wake:
		}
		o.deliverDue()
	}
}

func (o *Outbox) deliverDue() {
	now := time.Now().UTC()
	var due []*Record
	o.mu.Lock()
	for _, rec := range o.records {
		if rec.Status == StatusPending && !rec.NextAttemptAt.After(now) {
			cp := *rec
			due = append(due, &cp)
		}
	}
	o.mu.Unlock()

	sort.Slice(due, func(i, j int) bool { return due[i].CreatedAt.Before(due[j].CreatedAt) })
	for _, snap := range due {
		o.attempt(snap)
	}
}

// attempt delivers one due record and persists the outcome. The snapshot is
// re-checked under the lock: a terminal transition that landed between the
// due-scan and here wins.
//
// Per-channel accounting (D08): when the sender can deliver one channel at
// a time, each configured leg gets its own verdict and a retry re-sends
// only legs that have not acknowledged. A channel removed from the config
// stops counting toward delivery (it can never acknowledge); its ledger
// entry stays for audit. A channel ADDED after enqueue joins the pending
// set — the config is re-read every attempt by design.
func (o *Outbox) attempt(snap *Record) {
	type channelOutcome struct {
		channel string
		err     error
	}
	var outcomes []channelOutcome
	var deliveryErr error

	cfg, cfgErr := o.currentConfig()
	if cfgErr != nil {
		deliveryErr = fmt.Errorf("config: %w", cfgErr)
	} else {
		sender := o.opts.Sender
		if sender == nil {
			sender = alert.New(cfg)
		}
		if cs, ok := sender.(ChannelSender); ok {
			// snap.Channels nil (a pre-accounting record still pending after
			// an upgrade): every configured leg is undelivered, so this
			// round re-sends them all — exactly what a legacy retry did —
			// and the ledger built here makes FUTURE retries per-channel.
			for _, channel := range alert.ChannelsFor(cfg, snap.Event) {
				if snap.Channels[channel].Status == StatusDelivered {
					continue // this leg already acknowledged
				}
				ev := snap.Event
				ev.DeliveryID = snap.ID
				outcomes = append(outcomes, channelOutcome{channel, cs.SendChannel(ev, channel)})
			}
		} else {
			ev := snap.Event
			ev.DeliveryID = snap.ID
			deliveryErr = sender.SendSync(ev)
		}
	}

	now := time.Now().UTC()
	o.mu.Lock()
	rec, ok := o.records[snap.ID]
	if !ok || rec.Status != StatusPending {
		o.mu.Unlock()
		return
	}
	rec.Attempts++
	rec.LastAttemptAt = now
	var retryAfter time.Duration
	var delivered []string
	if d := alert.RetryAfter(deliveryErr); d > retryAfter {
		retryAfter = d
	}
	if len(outcomes) > 0 && rec.Channels == nil {
		rec.Channels = make(map[string]ChannelRecord)
	}
	for _, out := range outcomes {
		st := rec.Channels[out.channel] // zero value = pending
		st.Attempts++
		st.LastAttemptAt = now
		if out.err == nil {
			st.Status = StatusDelivered
			st.LastError = ""
			delivered = append(delivered, out.channel)
		} else {
			st.Status = StatusPending
			st.LastError = out.err.Error()
			if d := alert.RetryAfter(out.err); d > retryAfter {
				retryAfter = d
			}
		}
		rec.Channels[out.channel] = st
	}

	switch {
	case cfgErr != nil:
		rec.LastError = deliveryErr.Error()
	case rec.Channels != nil:
		// Record-level error = the first failing leg this round (stable order).
		pending := 0
		var firstErr string
		for _, channel := range alert.ChannelsFor(cfg, snap.Event) {
			st := rec.Channels[channel]
			if st.Status != StatusDelivered {
				pending++
				if firstErr == "" {
					firstErr = channel + ": " + st.LastError
				}
			}
		}
		if pending == 0 {
			rec.Status = StatusDelivered
			rec.LastError = ""
			rec.NextAttemptAt = time.Time{}
			snapshot := *rec
			o.mu.Unlock()
			o.persistQuiet(&snapshot)
			o.notify(delivered, nil, snapshot.Event)
			return
		}
		rec.LastError = firstErr
	default:
		if deliveryErr == nil {
			rec.Status = StatusDelivered
			rec.LastError = ""
			rec.NextAttemptAt = time.Time{}
			snapshot := *rec
			o.mu.Unlock()
			o.persistQuiet(&snapshot)
			return
		}
		rec.LastError = deliveryErr.Error()
	}

	deadLegs := map[string]string{}
	if rec.Attempts >= o.opts.MaxAttempts {
		// Exhausted: dead-letter the record and every still-pending leg
		// (delivered legs keep their delivered state — that history is the
		// audit answer to "who was actually told?").
		rec.Status = StatusDeadLettered
		rec.NextAttemptAt = time.Time{}
		for channel, st := range rec.Channels {
			if st.Status == StatusPending {
				st.Status = StatusDeadLettered
				rec.Channels[channel] = st
				deadLegs[channel] = st.LastError
			}
		}
	} else {
		// Honor the receiver's Retry-After when it asks for longer than our
		// own backoff.
		wait := o.backoff(rec.Attempts)
		if retryAfter > wait {
			wait = retryAfter
		}
		rec.NextAttemptAt = now.Add(wait)
	}
	snapshot := *rec
	o.mu.Unlock()

	if err := o.persist(&snapshot); err != nil {
		log.Printf("[outbox] ALERT JOURNAL APPEND FAILED (state in memory only until next write): %v", err)
	}
	o.notify(delivered, deadLegs, snapshot.Event)
}

// notify runs the channel-health hooks outside the lock.
func (o *Outbox) notify(delivered []string, dead map[string]string, ev alert.Event) {
	if o.opts.OnDelivered != nil {
		for _, ch := range delivered {
			o.opts.OnDelivered(ch, ev)
		}
	}
	if o.opts.OnDeadLetter != nil {
		channels := make([]string, 0, len(dead))
		for ch := range dead {
			channels = append(channels, ch)
		}
		sort.Strings(channels)
		for _, ch := range channels {
			o.opts.OnDeadLetter(ch, ev, dead[ch])
		}
	}
}

func (o *Outbox) persistQuiet(rec *Record) {
	if err := o.persist(rec); err != nil {
		log.Printf("[outbox] ALERT JOURNAL APPEND FAILED (state in memory only until next write): %v", err)
	}
}

func (o *Outbox) backoff(attempts int) time.Duration {
	// attempts is the count INCLUDING the just-failed one; the first retry
	// waits one base interval.
	d := o.opts.BackoffBase
	for i := 1; i < attempts && d < o.opts.BackoffMax; i++ {
		d *= 2
	}
	if d > o.opts.BackoffMax {
		d = o.opts.BackoffMax
	}
	return d
}

func (o *Outbox) currentConfig() (alert.Config, error) {
	if o.opts.ConfigFn == nil {
		return alert.Config{}, errors.New("no alert configuration source configured")
	}
	return o.opts.ConfigFn()
}

// persist appends the record's current state as one journal line, compacting
// when the retained set outgrows MaxRetained.
func (o *Outbox) persist(rec *Record) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.persistLocked(rec)
}

func (o *Outbox) persistLocked(rec *Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(o.journalPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	o.lines++

	if len(o.records) > o.opts.MaxRetained {
		return o.compactLocked()
	}
	return nil
}

// compactLocked rewrites the journal keeping the newest MaxRetained records
// (durable.Replace: unique temp, sync, rename, dir sync). Retention drops
// the OLDEST records first regardless of status; pending records are never
// dropped (they are the undelivered work).
func (o *Outbox) compactLocked() error {
	drop := len(o.records) - o.opts.MaxRetained
	kept := make([]string, 0, o.opts.MaxRetained)
	for _, id := range o.order {
		rec := o.records[id]
		if drop > 0 && rec.Status != StatusPending {
			delete(o.records, id)
			drop--
			continue
		}
		kept = append(kept, id)
	}
	o.order = kept

	var buf []byte
	for _, id := range o.order {
		line, err := json.Marshal(o.records[id])
		if err != nil {
			return err
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if err := durable.Replace(o.journalPath(), buf, 0600); err != nil {
		return err
	}
	o.lines = len(o.order)
	return nil
}

// LatestForMonitor returns the newest MONITOR delivery record for the
// monitor id, or nil when it has none. Restore-test records share the
// journal; the event source label keeps them from answering for a monitor.
func (o *Outbox) LatestForMonitor(monitorID string) *DeliveryStatus {
	return o.latest(monitorID, func(source string) bool { return source == SourceMonitor || source == SourceHeartbeat })
}

// LatestForRestoreTest returns the newest restore-test delivery record for
// the test id, or nil when it has none (D08: restore verdicts ride the same
// durable outbox as monitor transitions).
func (o *Outbox) LatestForRestoreTest(testID string) *DeliveryStatus {
	return o.LatestForSource(SourceRestoreTest, testID)
}

// LatestForSource is the source-scoped lookup. An empty source matches the
// legacy default (monitor) so pre-label journals keep answering.
func (o *Outbox) LatestForSource(source, id string) *DeliveryStatus {
	return o.latest(id, func(s string) bool { return s == source })
}

func recordSource(rec *Record) string {
	if rec.Event.Source == "" {
		return SourceMonitor
	}
	return rec.Event.Source
}

func (o *Outbox) latest(id string, sourceOK func(string) bool) *DeliveryStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	var best *Record
	for _, rec := range o.records {
		if !sourceOK(recordSource(rec)) || rec.Event.MonitorID != id {
			continue
		}
		if best == nil || rec.CreatedAt.After(best.CreatedAt) {
			best = rec
		}
	}
	if best == nil {
		return nil
	}
	return statusOf(best)
}

// History lists the newest retained records first (delivered, pending,
// dead-lettered and suppressed), at most limit (0 = all).
func (o *Outbox) History(limit int) []DeliveryStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]DeliveryStatus, 0, len(o.order))
	for i := len(o.order) - 1; i >= 0; i-- {
		if rec := o.records[o.order[i]]; rec != nil {
			out = append(out, *statusOf(rec))
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// ChannelHealth folds the retained records into per-channel health.
func (o *Outbox) ChannelHealth() map[string]ChannelHealth {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[string]ChannelHealth{}
	for _, id := range o.order {
		rec := o.records[id]
		if rec == nil {
			continue
		}
		for channel, leg := range rec.Channels {
			h := out[channel]
			switch leg.Status {
			case StatusDelivered:
				h.Delivered++
				if leg.LastAttemptAt.After(h.LastDeliveredAt) {
					h.LastDeliveredAt = leg.LastAttemptAt
				}
			case StatusDeadLettered:
				h.DeadLettered++
				if !leg.LastAttemptAt.Before(h.LastDeadLetterAt) {
					h.LastDeadLetterAt = leg.LastAttemptAt
					h.LastError = leg.LastError
				}
			default:
				h.Pending++
			}
			out[channel] = h
		}
	}
	return out
}

func statusOf(best *Record) *DeliveryStatus {
	status := &DeliveryStatus{
		ID:            best.ID,
		Status:        best.Status,
		Attempts:      best.Attempts,
		LastError:     best.LastError,
		LastAttemptAt: best.LastAttemptAt,
		NextAttemptAt: best.NextAttemptAt,
		EventStatus:   best.Event.Status,
		OccurredAt:    best.Event.OccurredAt,
		Kind:          best.Event.Wire().Kind,
		Source:        recordSource(best),
		RuleID:        best.Event.MonitorID,
		RuleName:      best.Event.MonitorName,
		IncidentID:    best.Event.IncidentID,
		Note:          best.Note,
		CreatedAt:     best.CreatedAt,
	}
	if len(best.Channels) > 0 {
		status.Channels = make(map[string]ChannelStatus, len(best.Channels))
		for channel, st := range best.Channels {
			status.Channels[channel] = ChannelStatus{
				Status:        st.Status,
				Attempts:      st.Attempts,
				LastError:     st.LastError,
				LastAttemptAt: st.LastAttemptAt,
			}
		}
	}
	return status
}
