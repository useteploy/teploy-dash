package operation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── useteploy__teploy-dash-04: append journal, bounded retention, replay
// gaps, FIFO admission ─────────────────────────────────────────────────────

func newJournalManager(t *testing.T, dir string, opts Options, executor Executor) *Manager {
	t.Helper()
	if opts.Resolver == nil {
		opts.Resolver = testResolver
	}
	opts.Executor = executor
	manager, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// The journal is append-only: every emitted event lands as one line, no
// per-event rewrite. The in-memory window is bounded independently, and
// EventsAfter serves full history via the disk fallback.
func TestJournalAppendOnlyFullReplayBoundedWindow(t *testing.T) {
	dir := t.TempDir()
	const emitted = 50
	manager := newJournalManager(t, dir, Options{MaxEvents: 5}, func(_ context.Context, _ Command, emit func(Stream, string)) (int, error) {
		for i := 0; i < emitted; i++ {
			emit(StreamStdout, fmt.Sprintf("line-%03d", i))
		}
		return 0, nil
	})
	op, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, op.ID, StatusSucceeded)

	// Every event is a journal line (queued + running + 50 + succeeded).
	data, err := os.ReadFile(filepath.Join(dir, "operations", "events", op.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != emitted+3 {
		t.Fatalf("journal holds %d lines, want %d (append-only, no rewrite)", lines, emitted+3)
	}

	// The in-memory window is bounded to maxEvents.
	manager.mu.Lock()
	window := len(manager.events[op.ID])
	manager.mu.Unlock()
	if window != 5 {
		t.Fatalf("window = %d, want 5", window)
	}

	// EventsAfter(0) still returns the complete history from disk.
	events, err := manager.EventsAfter(op.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != emitted+3 {
		t.Fatalf("EventsAfter(0) = %d events, want %d", len(events), emitted+3)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Sequence != events[i-1].Sequence+1 {
			t.Fatalf("non-monotonic replay: %+v", events[i-1:i+1])
		}
	}

	// After a restart the same full replay is available and the window is
	// re-bounded.
	restarted := newJournalManager(t, dir, Options{MaxEvents: 5}, func(context.Context, Command, func(Stream, string)) (int, error) {
		return 0, nil
	})
	events, err = restarted.EventsAfter(op.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != emitted+3 {
		t.Fatalf("post-restart replay = %d events, want %d", len(events), emitted+3)
	}
	restarted.mu.Lock()
	window = len(restarted.events[op.ID])
	restarted.mu.Unlock()
	if window != 5 {
		t.Fatalf("post-restart window = %d, want 5", window)
	}
}

// The per-operation journal byte cap triggers compaction: the file stays
// bounded, the retained history stays contiguous from its first sequence,
// and a replay from before the retained range surfaces a gap marker.
func TestJournalCompactionBoundsFileSize(t *testing.T) {
	dir := t.TempDir()
	const capBytes = int64(4 << 10)
	const emitted = 200
	manager := newJournalManager(t, dir, Options{MaxEvents: 1000, MaxJournalBytes: capBytes}, func(_ context.Context, _ Command, emit func(Stream, string)) (int, error) {
		for i := 0; i < emitted; i++ {
			emit(StreamStdout, fmt.Sprintf("payload-%06d-%s", i, strings.Repeat("x", 64)))
		}
		return 0, nil
	})
	op, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	finished := waitForStatus(t, manager, op.ID, StatusSucceeded)

	path := filepath.Join(dir, "operations", "events", op.ID+".jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Compaction runs before an append that would cross the cap, so the file
	// is bounded by the cap plus at most one (bounded) line.
	if info.Size() > capBytes+int64(maxEventDataBytes)+512 {
		t.Fatalf("journal size %d exceeds cap %d + one line", info.Size(), capBytes)
	}

	// Restart: the compacted history replays contiguously with a gap marker
	// covering whatever retention removed.
	restarted := newJournalManager(t, dir, Options{MaxEvents: 1000, MaxJournalBytes: capBytes}, func(context.Context, Command, func(Stream, string)) (int, error) {
		return 0, nil
	})
	events, err := restarted.EventsAfter(op.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Type != EventGap {
		t.Fatalf("compacted replay must open with a gap marker, got %+v", events[:min(2, len(events))])
	}
	for i := 1; i < len(events); i++ {
		if events[i].Sequence <= events[i-1].Sequence {
			t.Fatalf("non-monotonic compacted replay: %+v", events[i-1:i+1])
		}
	}
	// The tail is intact: the last events are the terminal status sequence.
	if len(events) < 2 || events[len(events)-1].Data != string(StatusSucceeded) {
		t.Fatalf("compaction lost the journal tail: %+v", events[len(events)-min(3, len(events)):])
	}
	_ = finished
}

// A torn journal tail (unclean shutdown mid-write) is isolated to the one
// operation, truncated on load, and surfaced as a gap event — never silently
// missing, never a service outage, and never grown behind.
func TestJournalTornTailTruncatedAndGapped(t *testing.T) {
	dir := t.TempDir()
	manager := newJournalManager(t, dir, Options{MaxEvents: 100}, func(_ context.Context, _ Command, emit func(Stream, string)) (int, error) {
		emit(StreamStdout, "one")
		emit(StreamStdout, "two")
		return 0, nil
	})
	op, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, op.ID, StatusSucceeded)
	// A second healthy operation proves isolation.
	healthy, _, err := manager.Enqueue(deployRequest("api", "img:2"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, healthy.ID, StatusSucceeded)

	// Simulate the torn write: a partial line with no newline.
	path := filepath.Join(dir, "operations", "events", op.ID+".jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"sequence":6,"operation_id":"` + op.ID + `","type":"stdo`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	restarted := newJournalManager(t, dir, Options{MaxEvents: 100}, func(context.Context, Command, func(Stream, string)) (int, error) {
		return 0, nil
	})
	events, err := restarted.EventsAfter(op.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Type != EventGap {
		t.Fatalf("torn tail must surface a trailing gap event, got %+v", events)
	}
	if !strings.Contains(events[len(events)-1].Data, "lost") {
		t.Fatalf("gap event data = %q", events[len(events)-1].Data)
	}
	// The torn line was truncated away: the file ends after the last clean
	// line, so a subsequent load reports no damage twice.
	events2, err := restarted.EventsAfter(op.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events2) != len(events) {
		t.Fatalf("repeat load changed history: %d then %d events", len(events), len(events2))
	}
	// The healthy operation's history is untouched.
	healthyEvents, err := restarted.EventsAfter(healthy.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range healthyEvents {
		if e.Type == EventGap {
			t.Fatalf("healthy operation acquired a gap: %+v", healthyEvents)
		}
	}
}

// A sequence hole in the journal (manual damage) is detected and marked,
// with the count of missing events.
func TestJournalMiddleGapCounted(t *testing.T) {
	dir := t.TempDir()
	store, err := openFileStore(dir, journalConfig{MaxEvents: 100})
	if err != nil {
		t.Fatal(err)
	}
	id := "abcdef0123456789abcdef0123456789"
	now := time.Now().UTC()
	if err := store.appendEvent(id, Event{Sequence: 1, OperationID: id, Type: EventStatus, Data: "queued", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.appendEvent(id, Event{Sequence: 2, OperationID: id, Type: EventStdout, Data: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.appendEvent(id, Event{Sequence: 5, OperationID: id, Type: EventStdout, Data: "b", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	events, err := store.loadEvents(id)
	if err != nil {
		t.Fatal(err)
	}
	foundGap := false
	for _, e := range events {
		if e.Type == EventGap {
			foundGap = true
			if e.Sequence != 3 || !strings.Contains(e.Data, "2 event(s)") {
				t.Fatalf("gap event = %+v, want sequence 3 covering 2 missing", e)
			}
		}
	}
	if !foundGap {
		t.Fatalf("no gap event in %+v", events)
	}
}

// Retention: terminal operations older than the age cap are deleted at
// startup (record + journal); the count cap removes the oldest terminal
// operations first; non-terminal operations are never removed.
func TestRetentionAgeAndCountBounds(t *testing.T) {
	dir := t.TempDir()
	store, err := openFileStore(dir, journalConfig{MaxEvents: 100})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	makeOp := func(n int, status Status, created time.Time) string {
		id := fmt.Sprintf("%032x", n)
		op := &Operation{
			ID: id, Request: deployRequest("web", "img:1"), Target: "server:prod/app:web",
			Status: status, Attempt: 1, CreatedAt: created,
		}
		if err := store.saveOperation(op); err != nil {
			t.Fatal(err)
		}
		if err := store.appendEvent(id, Event{Sequence: 1, OperationID: id, Type: EventStatus, Data: string(status), CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	agedID := makeOp(1, StatusSucceeded, old)
	freshID := makeOp(2, StatusSucceeded, time.Now().UTC())
	queuedID := makeOp(3, StatusQueued, old) // old but NOT terminal: survives

	removed := store.sweepRetention(time.Now().UTC(), 30*24*time.Hour, 0)
	if len(removed) != 1 || removed[0] != agedID {
		t.Fatalf("age sweep removed %v, want [%s]", removed, agedID)
	}
	if _, err := os.Stat(filepath.Join(dir, "operations", "records", agedID+".json")); !os.IsNotExist(err) {
		t.Fatal("aged record not deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "operations", "events", agedID+".jsonl")); !os.IsNotExist(err) {
		t.Fatal("aged journal not deleted")
	}
	for _, id := range []string{freshID, queuedID} {
		if _, err := os.Stat(filepath.Join(dir, "operations", "records", id+".json")); err != nil {
			t.Fatalf("survivor %s removed: %v", id, err)
		}
	}

	// Count bound: 3 terminal operations, cap 2 -> oldest terminal removed,
	// non-terminal untouched.
	third := makeOp(4, StatusFailed, time.Now().UTC().Add(time.Second))
	removed = store.sweepRetention(time.Now().UTC(), 30*24*time.Hour, 2)
	if len(removed) != 1 || removed[0] != freshID {
		t.Fatalf("count sweep removed %v, want [%s] (oldest terminal)", removed, freshID)
	}
	if _, err := os.Stat(filepath.Join(dir, "operations", "records", third+".json")); err != nil {
		t.Fatalf("newest terminal removed by count sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "operations", "records", queuedID+".json")); err != nil {
		t.Fatalf("non-terminal removed by count sweep: %v", err)
	}

	// Startup applies the same policy before loading (nothing stale parses).
	manager := newJournalManager(t, dir, Options{MaxEvents: 100}, func(context.Context, Command, func(Stream, string)) (int, error) {
		return 0, nil
	})
	if _, err := manager.Get(queuedID); err != nil {
		t.Fatalf("queued survivor missing after startup: %v", err)
	}
	if _, err := manager.Get(third); err != nil {
		t.Fatalf("terminal survivor missing after startup: %v", err)
	}
}

// Live count-bound retention: when operations exceed the configured cap, the
// post-completion sweep trims the oldest terminal ones from memory and disk.
func TestLiveRetentionWhenCountExceeded(t *testing.T) {
	dir := t.TempDir()
	manager := newJournalManager(t, dir, Options{MaxEvents: 100, MaxOperations: 2}, func(context.Context, Command, func(Stream, string)) (int, error) {
		return 0, nil
	})
	var ids []string
	for i := 0; i < 3; i++ {
		op, _, err := manager.Enqueue(deployRequest(fmt.Sprintf("app%d", i), "img:1"), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		waitForStatus(t, manager, op.ID, StatusSucceeded)
		ids = append(ids, op.ID)
	}
	manager.retire() // deterministic trigger (finish() fires it async)
	if _, err := manager.Get(ids[0]); err != ErrNotFound {
		t.Fatalf("oldest operation survived the count cap: %v", err)
	}
	for _, id := range ids[1:] {
		if _, err := manager.Get(id); err != nil {
			t.Fatalf("recent operation removed: %v", err)
		}
	}
}

// FIFO admission ordering (A12/A27 core): same-target operations execute in
// the order they were admitted, one at a time. Each operation carries a
// distinct app name so the executor can record which one started.
func TestFIFOAdmissionOrderPerTarget(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	imageOf := func(command Command) string {
		for i, arg := range command.Args {
			if arg == "--image" && i+1 < len(command.Args) {
				return command.Args[i+1]
			}
		}
		return ""
	}
	// Same server AND app for every operation: one target, one FIFO queue.
	// The image is the distinguisher.
	manager := newJournalManager(t, dir, Options{MaxEvents: 100}, func(_ context.Context, command Command, _ func(Stream, string)) (int, error) {
		mu.Lock()
		order = append(order, imageOf(command))
		mu.Unlock()
		<-release
		return 0, nil
	})
	const n = 5
	var ops []*Operation
	for i := 0; i < n; i++ {
		op, _, err := manager.Enqueue(deployRequest("web", fmt.Sprintf("img:%d", i)), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	// Let the runner start the first job, then confirm the rest are parked.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		started := len(order)
		mu.Unlock()
		if started == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if len(order) != 1 {
		mu.Unlock()
		t.Fatalf("%d operations started before release, want 1", len(order))
	}
	mu.Unlock()
	close(release)
	for _, op := range ops {
		waitForStatus(t, manager, op.ID, StatusSucceeded)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != n {
		t.Fatalf("executed %d operations, want %d", len(order), n)
	}
	for i, op := range ops {
		if order[i] != op.Request.Image {
			t.Fatalf("execution order %v is not admission order (step %d got %s, want %s)", order, i, order[i], op.Request.Image)
		}
	}
}

// Cross-target concurrency is preserved: FIFO is per target, different
// targets still run in parallel.
func TestFIFODoesNotSerializeTargets(t *testing.T) {
	bothStarted := make(chan struct{}, 2)
	release := make(chan struct{})
	var running atomic.Int32
	manager := newJournalManager(t, t.TempDir(), Options{MaxEvents: 100}, func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
		if running.Add(1) == 2 {
			close(bothStarted)
		}
		<-release
		running.Add(-1)
		return 0, nil
	})
	first, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := manager.Enqueue(Request{Kind: KindDeploy, Server: "staging", App: "web", Image: "img:2"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-bothStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("operations on different targets did not run concurrently")
	}
	close(release)
	// Join both workers before the test dir is cleaned up.
	waitForStatus(t, manager, first.ID, StatusSucceeded)
	waitForStatus(t, manager, second.ID, StatusSucceeded)
}

// Concurrent enqueue/emit/cancel/replay across several targets stays
// consistent under -race.
func TestConcurrentTargetsReplayAndCancel(t *testing.T) {
	dir := t.TempDir()
	var cancellable atomic.Int32
	manager := newJournalManager(t, dir, Options{MaxEvents: 32}, func(ctx context.Context, _ Command, emit func(Stream, string)) (int, error) {
		for i := 0; i < 200; i++ {
			emit(StreamStdout, fmt.Sprintf("event-%d", i))
			select {
			case <-ctx.Done():
				return -1, ctx.Err()
			default:
			}
		}
		return 0, nil
	})
	var wg sync.WaitGroup
	for target := 0; target < 4; target++ {
		wg.Add(1)
		go func(target int) {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				op, _, err := manager.Enqueue(Request{Kind: KindDeploy, Server: fmt.Sprintf("srv%d", target), App: "web", Image: "img:1"}, "", nil)
				if err != nil {
					t.Errorf("enqueue: %v", err)
					return
				}
				if i == 0 {
					cancellable.Add(1)
					if _, err := manager.Cancel(op.ID); err != nil && !errors.Is(err, ErrNotCancelable) {
						t.Errorf("cancel: %v", err)
					}
				}
			}
		}(target)
		// A concurrent reader replays one operation's history per target.
		wg.Add(1)
		go func(target int) {
			defer wg.Done()
			deadline := time.Now().Add(3 * time.Second)
			var id string
			for time.Now().Before(deadline) {
				ops := manager.List("", fmt.Sprintf("server:srv%d/app:web", target), 1)
				if len(ops) > 0 {
					id = ops[0].ID
					if events, err := manager.EventsAfter(id, 0); err == nil {
						for i := 1; i < len(events); i++ {
							if events[i].Sequence <= events[i-1].Sequence {
								t.Errorf("non-monotonic replay: %d after %d", events[i].Sequence, events[i-1].Sequence)
								return
							}
						}
					}
				}
				time.Sleep(2 * time.Millisecond)
			}
			_ = id
		}(target)
	}
	wg.Wait()
	// Drain: every operation reaches a terminal state.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ops := manager.List("", "", 0)
		done := true
		for _, op := range ops {
			if !op.Status.Terminal() {
				done = false
			}
		}
		if done && len(ops) == 12 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("operations did not all reach terminal states")
}
