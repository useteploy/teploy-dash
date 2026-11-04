package outbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
)

// Ledger re-key: a record journaled under the legacy config (ledger keys
// "webhook"/"email") with the webhook leg delivered and the email leg
// pending replays after the config migrated to a channel list: the migrated
// channels own the same ids, so ONLY the email leg is re-sent.
func TestLedgerReplaysPendingLegsAfterChannelMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := alert.Config{WebhookURL: "https://hook.example", SMTPHost: "smtp.example", EmailTo: "ops@example"}
	cs := &channelSender{}
	opts := twoChannelOptions(dir, cs)
	opts.ConfigFn = func() (alert.Config, error) { return legacy, nil }
	ob, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	ev := testEvent("m1", "down", time.Now())
	rec := &Record{ID: deliveryID(ev), Event: ev, Status: StatusPending, NextAttemptAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
		Channels: map[string]ChannelRecord{
			alert.ChannelWebhook: {Status: StatusDelivered, Attempts: 1},
			alert.ChannelEmail:   {Status: StatusPending, Attempts: 1, LastError: "smtp down"},
		}}
	if err := ob.persist(rec); err != nil {
		t.Fatal(err)
	}

	migrated := legacy.Migrate()
	migrated.Channels = append(migrated.Channels, alert.Channel{ID: "phone", Type: alert.TypeNtfy, URL: "https://ntfy.example/t", Kinds: []string{alert.KindRecovered}})
	opts.ConfigFn = func() (alert.Config, error) { return migrated, nil }
	ob2, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	ob2.Start()
	defer ob2.Stop(context.Background())
	waitFor(t, 5*time.Second, func() bool {
		d := ob2.LatestForMonitor("m1")
		return d != nil && d.Status == StatusDelivered
	}, "migrated pending record never delivered")
	calls := cs.channelCalls()
	if len(calls) != 1 || calls[0] != alert.ChannelEmail {
		t.Fatalf("replay sent %v, want only the pending email leg (phone filters out opens)", calls)
	}
}

type retryAfterSender struct {
	mu    sync.Mutex
	calls int
}

func (s *retryAfterSender) SendChannel(ev alert.Event, channel string) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return &alert.RetryAfterError{After: time.Hour, Err: errors.New("HTTP 429")}
}
func (s *retryAfterSender) SendSync(alert.Event) error { return nil }

// A receiver's Retry-After beats our (much shorter) backoff.
func TestRetryAfterDelaysNextAttempt(t *testing.T) {
	dir := t.TempDir()
	s := &retryAfterSender{}
	ob, err := New(dir, testOptions(dir, s))
	if err != nil {
		t.Fatal(err)
	}
	ob.Start()
	defer ob.Stop(context.Background())
	ob.Send(testEvent("m1", "down", time.Now()))
	waitFor(t, 5*time.Second, func() bool {
		d := ob.LatestForMonitor("m1")
		return d != nil && d.Attempts == 1
	}, "first attempt never ran")
	time.Sleep(50 * time.Millisecond)
	d := ob.LatestForMonitor("m1")
	if d.Attempts != 1 || d.NextAttemptAt.Before(time.Now().Add(59*time.Minute)) {
		t.Fatalf("attempts=%d next=%s: Retry-After of 1h not honored", d.Attempts, d.NextAttemptAt)
	}
}

// Suppressed alerts are journaled, terminal, never sent, and visible.
func TestRecordSuppressedIsVisibleAndNeverSent(t *testing.T) {
	dir := t.TempDir()
	cs := &channelSender{}
	ob, err := New(dir, twoChannelOptions(dir, cs))
	if err != nil {
		t.Fatal(err)
	}
	ob.Start()
	ev := alert.Event{MonitorID: "m1", MonitorName: "api", Status: "down", OccurredAt: time.Now(), Version: 2, Kind: alert.KindOpened, IncidentID: "held"}
	ob.RecordSuppressed(ev, "held: app in maintenance")
	ob.RecordSuppressed(ev, "held: app in maintenance") // deduped
	time.Sleep(30 * time.Millisecond)
	ob.Stop(context.Background())
	if n := len(cs.channelCalls()); n != 0 {
		t.Fatalf("suppressed record was sent %d times", n)
	}
	ob2, err := New(dir, twoChannelOptions(dir, cs))
	if err != nil {
		t.Fatal(err)
	}
	h := ob2.History(10)
	if len(h) != 1 || h[0].Status != StatusSuppressed || h[0].Note != "held: app in maintenance" || h[0].Kind != alert.KindOpened {
		t.Fatalf("history = %+v", h)
	}
}

// A dead-lettered leg fires the channel-health hook and is counted per
// channel; the delivered leg is counted as delivered.
func TestDeadLetterHookAndChannelHealth(t *testing.T) {
	dir := t.TempDir()
	cs := &channelSender{outcome: func(channel string, n int) error {
		if channel == alert.ChannelEmail {
			return errors.New("smtp down")
		}
		return nil
	}}
	opts := twoChannelOptions(dir, cs)
	opts.MaxAttempts = 2
	var mu sync.Mutex
	var dead, delivered []string
	opts.OnDeadLetter = func(ch string, ev alert.Event, lastErr string) {
		mu.Lock()
		dead = append(dead, ch+":"+lastErr)
		mu.Unlock()
	}
	opts.OnDelivered = func(ch string, ev alert.Event) {
		mu.Lock()
		delivered = append(delivered, ch)
		mu.Unlock()
	}
	ob, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	ob.Start()
	defer ob.Stop(context.Background())
	ob.Send(testEvent("m1", "down", time.Now()))
	waitFor(t, 5*time.Second, func() bool {
		d := ob.LatestForMonitor("m1")
		return d != nil && d.Status == StatusDeadLettered
	}, "never dead-lettered")
	waitFor(t, 5*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(dead) > 0 }, "dead-letter hook never fired")
	mu.Lock()
	defer mu.Unlock()
	if len(dead) != 1 || dead[0] != "email:smtp down" {
		t.Fatalf("dead-letter hook = %v", dead)
	}
	if len(delivered) != 1 || delivered[0] != alert.ChannelWebhook {
		t.Fatalf("delivered hook = %v", delivered)
	}
	h := ob.ChannelHealth()
	if h[alert.ChannelEmail].DeadLettered != 1 || h[alert.ChannelEmail].LastError != "smtp down" || h[alert.ChannelWebhook].Delivered != 1 {
		t.Fatalf("health = %+v", h)
	}
}

// v2 delivery ids distinguish kinds of one incident; v1 ids are unchanged.
func TestDeliveryIDSeparatesKinds(t *testing.T) {
	at := time.Unix(1700000000, 0)
	open := alert.Event{MonitorID: "m", Status: "down", OccurredAt: at, Version: 2, Kind: alert.KindOpened, IncidentID: "i"}
	rep := open
	rep.Kind = alert.KindRepeat
	if deliveryID(open) == deliveryID(rep) {
		t.Fatal("open and repeat share a delivery id")
	}
	legacy := alert.Event{MonitorID: "m", Status: "down", OccurredAt: at}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", "m", "down", at.UnixNano())))
	if deliveryID(legacy) != hex.EncodeToString(h[:16]) {
		t.Fatal("pre-v2 delivery id derivation changed (journaled ids would no longer dedupe)")
	}
}
