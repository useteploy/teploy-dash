package outbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
)

// funcSender lets tests script delivery outcomes and count attempts.
type funcSender struct {
	mu    sync.Mutex
	calls int
	fn    func(n int) error
}

func (s *funcSender) SendSync(ev alert.Event) error {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if s.fn != nil {
		return s.fn(n)
	}
	return nil
}

func (s *funcSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func testEvent(monitor, status string, at time.Time) alert.Event {
	return alert.Event{MonitorID: monitor, MonitorName: monitor, Status: status, Message: "m", OccurredAt: at}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

func testOptions(dir string, sender Sender) Options {
	return Options{
		ConfigFn:     func() (alert.Config, error) { return alert.Config{WebhookURL: "http:// configured.example/"}, nil },
		Sender:       sender,
		PollInterval: 5 * time.Millisecond,
		MaxAttempts:  5,
		BackoffBase:  time.Millisecond,
		BackoffMax:   10 * time.Millisecond,
		MaxRetained:  250,
	}
}

// D08: delivery attempts persist with retry/backoff; after MaxAttempts the
// record dead-letters and the last failure (error, timestamp, attempt count)
// is visible on the monitor.
func TestRetriesWithBackoffThenDeadLetter(t *testing.T) {
	dir := t.TempDir()
	sender := &funcSender{fn: func(int) error { return errors.New("boom") }}
	ob, err := New(dir, testOptions(dir, sender))
	if err != nil {
		t.Fatal(err)
	}
	// Override attempts for this test.
	ob.opts.MaxAttempts = 3

	at := time.Now()
	ob.Send(testEvent("m1", "down", at))
	ob.Start()
	defer ob.Stop(context.Background())

	waitFor(t, 5*time.Second, func() bool {
		d := ob.LatestForMonitor("m1")
		return d != nil && d.Status == StatusDeadLettered
	}, "record never dead-lettered")

	d := ob.LatestForMonitor("m1")
	if d.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", d.Attempts)
	}
	if d.LastError == "" {
		t.Fatal("last failure error must be visible")
	}
	if d.LastAttemptAt.IsZero() {
		t.Fatal("last failure timestamp must be visible")
	}
	if sender.count() != 3 {
		t.Fatalf("sender called %d times, want exactly MaxAttempts", sender.count())
	}
}

// D08: a delivered record is never re-sent after a restart (delivery-id
// dedupe via the persisted journal).
func TestDeliveredExactlyOnceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	sender := &funcSender{}
	ob, err := New(dir, testOptions(dir, sender))
	if err != nil {
		t.Fatal(err)
	}
	ob.Send(testEvent("m1", "down", time.Now()))
	ob.Start()
	waitFor(t, 5*time.Second, func() bool {
		d := ob.LatestForMonitor("m1")
		return d != nil && d.Status == StatusDelivered
	}, "record never delivered")
	ob.Stop(context.Background())

	// Restart over the same journal.
	sender2 := &funcSender{}
	ob2, err := New(dir, testOptions(dir, sender2))
	if err != nil {
		t.Fatal(err)
	}
	ob2.Start()
	defer ob2.Stop(context.Background())
	time.Sleep(50 * time.Millisecond) // several poll cycles

	if sender2.count() != 0 {
		t.Fatalf("delivered record re-sent after restart: %d attempts", sender2.count())
	}
	d := ob2.LatestForMonitor("m1")
	if d == nil || d.Status != StatusDelivered {
		t.Fatalf("delivered state must survive restart, got %+v", d)
	}
}

// D08: a pending (failed, backing-off) delivery survives a restart and is
// resumed by the next process — exactly once per attempt, with the attempt
// count carrying over.
func TestRestartResumesPendingDeliveryOnce(t *testing.T) {
	dir := t.TempDir()

	// Phase A: first attempt fails; the record stays pending in backoff.
	attemptsA := make(chan int, 16)
	senderA := &funcSender{fn: func(n int) error { attemptsA <- n; return errors.New("phase A failure") }}
	optsA := testOptions(dir, senderA)
	optsA.BackoffBase = 50 * time.Millisecond
	obA, err := New(dir, optsA)
	if err != nil {
		t.Fatal(err)
	}
	obA.Send(testEvent("m1", "down", time.Now()))
	obA.Start()
	<-attemptsA // exactly one attempt happened
	obA.Stop(context.Background())

	// Phase B: a fresh process over the same journal; delivery now succeeds.
	senderB := &funcSender{}
	obB, err := New(dir, testOptions(dir, senderB))
	if err != nil {
		t.Fatal(err)
	}
	obB.Start()
	defer obB.Stop(context.Background())
	waitFor(t, 5*time.Second, func() bool {
		d := obB.LatestForMonitor("m1")
		return d != nil && d.Status == StatusDelivered
	}, "pending record not resumed after restart")

	if senderB.count() != 1 {
		t.Fatalf("resumed delivery attempted %d times, want exactly 1", senderB.count())
	}
	d := obB.LatestForMonitor("m1")
	if d.Attempts < 2 {
		t.Fatalf("attempt count must carry across restart, got %d", d.Attempts)
	}
}

// D08: Enqueue dedupes by delivery id — the same transition enqueued twice
// produces one record and one delivery.
func TestEnqueueDedupesByID(t *testing.T) {
	dir := t.TempDir()
	sender := &funcSender{}
	ob, err := New(dir, testOptions(dir, sender))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Truncate(0)
	ev := testEvent("m1", "up", at)
	ob.Send(ev)
	ob.Send(ev) // same id: second enqueue is a no-op
	ob.Start()
	defer ob.Stop(context.Background())

	waitFor(t, 5*time.Second, func() bool {
		d := ob.LatestForMonitor("m1")
		return d != nil && d.Status == StatusDelivered
	}, "record never delivered")

	if sender.count() != 1 {
		t.Fatalf("duplicate enqueue delivered %d times, want 1", sender.count())
	}
	ob.mu.Lock()
	n := len(ob.records)
	ob.mu.Unlock()
	if n != 1 {
		t.Fatalf("duplicate enqueue created %d records, want 1", n)
	}
}

// D08: with no channels configured there is nothing to make durable — Send
// stays the documented best-effort no-op (no phantom pending records that a
// later config would replay).
func TestNoChannelsConfiguredSkipsEnqueue(t *testing.T) {
	dir := t.TempDir()
	ob, err := New(dir, Options{ConfigFn: func() (alert.Config, error) { return alert.Config{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ob.Send(testEvent("m1", "down", time.Now()))
	if d := ob.LatestForMonitor("m1"); d != nil {
		t.Fatalf("unconfigured delivery left a record: %+v", d)
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !os.IsNotExist(err) {
		t.Fatal("unconfigured delivery wrote a journal")
	}
}

// D08: a config that cannot be READ is a delivery failure that retries and
// surfaces — not a silently dropped alert.
func TestUnreadableConfigRetriesAndSurfaces(t *testing.T) {
	dir := t.TempDir()
	sender := &funcSender{}
	opts := testOptions(dir, sender)
	opts.ConfigFn = func() (alert.Config, error) { return alert.Config{}, errors.New("unreadable notifications config") }
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
	}, "config failure never surfaced")
	d := ob.LatestForMonitor("m1")
	if d.LastError == "" {
		t.Fatal("config read failure must be the visible last error")
	}
}

// Bounded retention: compaction keeps the newest MaxRetained records.
// Pending records are never dropped (they ARE the undelivered work), so the
// bound applies once the worker delivers them to terminal state.
func TestCompactionBoundsRetainedRecords(t *testing.T) {
	dir := t.TempDir()
	sender := &funcSender{}
	opts := testOptions(dir, sender)
	opts.MaxRetained = 3
	ob, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	for i := 0; i < 10; i++ {
		ob.Send(testEvent("m", "down", base.Add(time.Duration(i)*time.Second)))
	}
	ob.Start()
	defer ob.Stop(context.Background())
	waitFor(t, 5*time.Second, func() bool {
		ob.mu.Lock()
		n := len(ob.records)
		ob.mu.Unlock()
		return n <= 3
	}, "compaction never bounded the retained set")

	ob.mu.Lock()
	n := len(ob.records)
	ob.mu.Unlock()
	if n > 3 {
		t.Fatalf("compaction kept %d records, want <= MaxRetained", n)
	}
	d := ob.LatestForMonitor("m")
	if d == nil || !d.OccurredAt.Equal(base.Add(9*time.Second)) {
		t.Fatalf("compaction dropped the newest record: %+v", d)
	}
}

// A torn journal tail (crash mid-append) is dropped, not fatal.
func TestTornJournalTailDropped(t *testing.T) {
	dir := t.TempDir()
	sender := &funcSender{}
	ob, err := New(dir, testOptions(dir, sender))
	if err != nil {
		t.Fatal(err)
	}
	ob.Send(testEvent("m1", "down", time.Now()))
	ob.mu.Lock()
	path := filepath.Join(dir, journalName)
	ob.mu.Unlock()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"torn","status":"pen`); err != nil { // no trailing newline
		t.Fatal(err)
	}
	f.Close()

	ob2, err := New(dir, testOptions(dir, sender))
	if err != nil {
		t.Fatalf("torn tail must not kill the journal: %v", err)
	}
	if d := ob2.LatestForMonitor("m1"); d == nil || d.ID == "torn" {
		t.Fatalf("valid records must survive a torn tail, got %+v", d)
	}
}

// A corrupt journal MIDDLE line is a loud error, not a silently truncated
// history.
func TestCorruptJournalMiddleIsLoud(t *testing.T) {
	dir := t.TempDir()
	ob, err := New(dir, Options{ConfigFn: func() (alert.Config, error) { return alert.Config{WebhookURL: "http://x/"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	ob.Send(testEvent("m1", "down", base))
	ob.Send(testEvent("m2", "down", base.Add(time.Second)))

	path := filepath.Join(dir, journalName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(string(data))
	if len(lines) != 2 {
		t.Fatalf("expected 2 journal lines, got %d", len(lines))
	}
	// Corrupt the FIRST line (a middle line once more follow), keep valid tail.
	corrupt := "not json at all\n" + lines[1]
	if err := os.WriteFile(path, []byte(corrupt), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir, Options{ConfigFn: func() (alert.Config, error) { return alert.Config{}, nil }}); err == nil {
		t.Fatal("corrupt journal middle must fail loudly")
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
