package outbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
)

// channelSender scripts PER-CHANNEL outcomes and records every call
// (channel + the event it carried).
type channelSender struct {
	mu      sync.Mutex
	outcome func(channel string, n int) error
	calls   []string
	events  []alert.Event
}

func (s *channelSender) SendChannel(ev alert.Event, channel string) error {
	s.mu.Lock()
	s.calls = append(s.calls, channel)
	s.events = append(s.events, ev)
	n := len(s.calls)
	s.mu.Unlock()
	if s.outcome != nil {
		return s.outcome(channel, n)
	}
	return nil
}

// Legacy whole-record sender (no per-channel capability).
func (s *channelSender) SendSync(ev alert.Event) error { return nil }

func (s *channelSender) channelCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *channelSender) channelEvents() []alert.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]alert.Event(nil), s.events...)
}

func twoChannelOptions(dir string, sender Sender) Options {
	return Options{
		ConfigFn: func() (alert.Config, error) {
			return alert.Config{WebhookURL: "http://configured.example/", SMTPHost: "smtp.example", EmailTo: "ops@example"}, nil
		},
		Sender:       sender,
		PollInterval: 5 * time.Millisecond,
		MaxAttempts:  5,
		BackoffBase:  time.Millisecond,
		BackoffMax:   10 * time.Millisecond,
	}
}

// D08 per-channel accounting: a webhook that acknowledged before an email
// failure is NEVER re-sent on retry — only the failed leg retries, and when
// the record dead-letters the delivered leg keeps its delivered state (the
// audit answer to "who was actually told?").
func TestPerChannelRetrySendsOnlyFailedLegs(t *testing.T) {
	dir := t.TempDir()
	cs := &channelSender{outcome: func(channel string, n int) error {
		if channel == alert.ChannelEmail {
			return errors.New("smtp down")
		}
		return nil
	}}
	opts := twoChannelOptions(dir, cs)
	opts.MaxAttempts = 2
	ob, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	ob.Send(testEvent("m1", "down", time.Now()))
	ob.Start()
	defer ob.Stop(context.Background())

	waitFor(t, 5*time.Second, func() bool {
		d := ob.LatestForMonitor("m1")
		return d != nil && d.Status == StatusDeadLettered
	}, "record never dead-lettered")

	calls := cs.channelCalls()
	webhook, email := 0, 0
	for _, c := range calls {
		switch c {
		case alert.ChannelWebhook:
			webhook++
		case alert.ChannelEmail:
			email++
		}
	}
	if webhook != 1 {
		t.Fatalf("webhook sent %d times, want exactly 1 (it acknowledged on the first attempt)", webhook)
	}
	if email != 2 {
		t.Fatalf("email sent %d times, want MaxAttempts=2", email)
	}

	d := ob.LatestForMonitor("m1")
	if d.Channels == nil {
		t.Fatal("delivery status must expose the per-channel ledger")
	}
	if st := d.Channels[alert.ChannelWebhook]; st.Status != StatusDelivered || st.Attempts != 1 {
		t.Fatalf("webhook leg = %+v, want delivered after 1 attempt", st)
	}
	if st := d.Channels[alert.ChannelEmail]; st.Status != StatusDeadLettered || st.LastError == "" {
		t.Fatalf("email leg = %+v, want dead-lettered with the exact failure", st)
	}
}

// D08: the retry math recovers — email heals on the second attempt and the
// record then DELIVERS without ever re-sending the webhook.
func TestPerChannelRecoversWithoutResendingDeliveredLeg(t *testing.T) {
	dir := t.TempDir()
	cs := &channelSender{outcome: func(channel string, n int) error {
		if channel == alert.ChannelEmail && n <= 2 { // first round: webhook(call 1), email(call 2)
			return errors.New("smtp flaky")
		}
		return nil
	}}
	ob, err := New(dir, twoChannelOptions(dir, cs))
	if err != nil {
		t.Fatal(err)
	}
	ob.Send(testEvent("m1", "down", time.Now()))
	ob.Start()
	defer ob.Stop(context.Background())

	waitFor(t, 5*time.Second, func() bool {
		d := ob.LatestForMonitor("m1")
		return d != nil && d.Status == StatusDelivered
	}, "record never delivered")

	calls := cs.channelCalls()
	webhook := 0
	for _, c := range calls {
		if c == alert.ChannelWebhook {
			webhook++
		}
	}
	if webhook != 1 {
		t.Fatalf("webhook sent %d times across the retry, want 1", webhook)
	}
	d := ob.LatestForMonitor("m1")
	if st := d.Channels[alert.ChannelEmail]; st.Status != StatusDelivered {
		t.Fatalf("email leg = %+v, want delivered", st)
	}
}

// D08: the delivery id travels on the event the sender sees — webhook
// payloads carry it as a JSON field and emails as X-Teploy-Delivery-Id, so
// receivers can dedupe at-least-once redelivery.
func TestDeliveryIDTravelsOnTheEvent(t *testing.T) {
	dir := t.TempDir()
	cs := &channelSender{}
	ob, err := New(dir, twoChannelOptions(dir, cs))
	if err != nil {
		t.Fatal(err)
	}
	ob.Send(testEvent("m1", "down", time.Now()))
	ob.Start()
	defer ob.Stop(context.Background())

	waitFor(t, 5*time.Second, func() bool {
		d := ob.LatestForMonitor("m1")
		return d != nil && d.Status == StatusDelivered
	}, "record never delivered")

	d := ob.LatestForMonitor("m1")
	for _, ev := range cs.channelEvents() {
		if ev.DeliveryID != d.ID {
			t.Fatalf("event carried delivery id %q, want the record id %q", ev.DeliveryID, d.ID)
		}
	}
}

// D08: restore-test and monitor records share the journal; the source label
// scopes the lookups — one must never answer for the other (same entity id,
// different kinds).
func TestLatestForSourceScopesRestoreTestsFromMonitors(t *testing.T) {
	dir := t.TempDir()
	cs := &channelSender{}
	ob, err := New(dir, twoChannelOptions(dir, cs))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Truncate(time.Second)
	mon := testEvent("shared-id", "down", base)
	rt := testEvent("shared-id", "down", base.Add(time.Second))
	rt.Source = alert.SourceRestoreTest
	ob.Send(mon)
	ob.Send(rt)
	ob.Start()
	defer ob.Stop(context.Background())

	waitFor(t, 5*time.Second, func() bool {
		return ob.LatestForMonitor("shared-id") != nil && ob.LatestForRestoreTest("shared-id") != nil
	}, "both records never delivered")

	m := ob.LatestForMonitor("shared-id")
	rt2 := ob.LatestForRestoreTest("shared-id")
	if !m.OccurredAt.Equal(base) {
		t.Fatalf("monitor lookup returned the restore record: %+v", m)
	}
	if !rt2.OccurredAt.Equal(base.Add(time.Second)) {
		t.Fatalf("restore lookup returned the monitor record: %+v", rt2)
	}
}

// A legacy journal line (pre-accounting, no channels map) still loads, and a
// still-pending one replays through per-channel accounting — its first
// replay re-sends every configured leg (what a legacy retry did), then only
// the failing one.
func TestLegacyPendingRecordGainsPerChannelLedgerOnReplay(t *testing.T) {
	dir := t.TempDir()
	cs := &channelSender{outcome: func(channel string, n int) error {
		if channel == alert.ChannelEmail && n <= 2 {
			return errors.New("smtp down")
		}
		return nil
	}}
	ob, err := New(dir, twoChannelOptions(dir, cs))
	if err != nil {
		t.Fatal(err)
	}
	// Hand-write a legacy record: no Channels map, status pending.
	at := time.Now()
	ev := testEvent("legacy", "down", at)
	id := deliveryID(ev)
	rec := &Record{ID: id, Event: ev, Status: StatusPending, NextAttemptAt: time.Now().UTC(), CreatedAt: time.Now().UTC()}
	if err := ob.persist(rec); err != nil {
		t.Fatal(err)
	}
	ob.Stop(context.Background()) // never started; safe no-op join

	ob2, err := New(dir, twoChannelOptions(dir, cs))
	if err != nil {
		t.Fatal(err)
	}
	ob2.Start()
	defer ob2.Stop(context.Background())
	waitFor(t, 5*time.Second, func() bool {
		d := ob2.LatestForMonitor("legacy")
		return d != nil && d.Status == StatusDelivered
	}, "legacy pending record never delivered")

	d := ob2.LatestForMonitor("legacy")
	if d.Channels == nil {
		t.Fatal("replayed legacy record must gain the per-channel ledger")
	}
	if st := d.Channels[alert.ChannelWebhook]; st.Attempts != 1 {
		t.Fatalf("webhook leg attempts = %d, want 1", st.Attempts)
	}
	if st := d.Channels[alert.ChannelEmail]; st.Attempts != 2 {
		t.Fatalf("email leg attempts = %d, want 2", st.Attempts)
	}
}
