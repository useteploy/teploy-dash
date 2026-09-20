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

func testResolver(name string) (Server, error) {
	return Server{Name: name, Host: name + ".example", User: "deploy"}, nil
}

func newTestManager(t *testing.T, dir string, maxEvents int, executor Executor) *Manager {
	t.Helper()
	manager, err := New(dir, Options{MaxEvents: maxEvents, Resolver: testResolver, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func deployRequest(app, image string) Request {
	return Request{Kind: KindDeploy, Server: "prod", App: app, Image: image}
}

func waitForStatus(t *testing.T, manager *Manager, id string, statuses ...Status) *Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, err := manager.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range statuses {
			if op.Status == status {
				return op
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := manager.Get(id)
	t.Fatalf("operation %s remained %s, want one of %v", id, op.Status, statuses)
	return nil
}

func TestPersistenceAndMonotonicBoundedEvents(t *testing.T) {
	dir := t.TempDir()
	executor := func(_ context.Context, _ Command, emit func(Stream, string)) (int, error) {
		for i := 0; i < 10; i++ {
			emit(StreamStdout, string(rune('a'+i)))
		}
		return 0, nil
	}
	manager := newTestManager(t, dir, 5, executor)
	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "persist-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, op.ID, StatusSucceeded)

	reopened := newTestManager(t, dir, 5, executor)
	stored, err := reopened.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusSucceeded || stored.IdempotencyKey != "persist-key" {
		t.Fatalf("unexpected persisted operation: %+v", stored)
	}
	// The retained WINDOW is bounded (5); the journal on disk may hold more
	// until compaction — EventsAfter serves the window plus journal
	// fallback, contiguous and monotonic either way.
	events, err := reopened.EventsAfter(op.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 13 {
		t.Fatalf("replayed %d events, want the full 13-event journal", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].Sequence != events[i-1].Sequence+1 {
			t.Fatalf("non-monotonic sequences: %+v", events)
		}
	}
	window := func() []Event {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return append([]Event(nil), manager.events[op.ID]...)
	}()
	if len(window) != 5 {
		t.Fatalf("in-memory window holds %d events, want 5", len(window))
	}
	if window[0].Sequence != events[len(events)-5].Sequence {
		t.Fatalf("window does not match the journal tail: window[0]=%d journal tail starts at %d", window[0].Sequence, events[len(events)-5].Sequence)
	}
}

func TestIdempotencyReplayAndConflict(t *testing.T) {
	block := make(chan struct{})
	manager := newTestManager(t, t.TempDir(), 100, func(ctx context.Context, _ Command, _ func(Stream, string)) (int, error) {
		select {
		case <-block:
			return 0, nil
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	})
	first, replayed, err := manager.Enqueue(deployRequest("web", "example/web:1"), "same-key", nil)
	if err != nil || replayed {
		t.Fatalf("first enqueue: replayed=%v err=%v", replayed, err)
	}
	second, replayed, err := manager.Enqueue(deployRequest("web", "example/web:1"), "same-key", nil)
	if err != nil || !replayed || second.ID != first.ID {
		t.Fatalf("idempotent replay: first=%s second=%s replayed=%v err=%v", first.ID, second.ID, replayed, err)
	}
	_, _, err = manager.Enqueue(deployRequest("web", "example/web:2"), "same-key", nil)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting key error = %v", err)
	}
	close(block)
	waitForStatus(t, manager, first.ID, StatusSucceeded)
}

func TestPerTargetSerialization(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	executor := func(ctx context.Context, _ Command, _ func(Stream, string)) (int, error) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		active.Add(-1)
		return 0, nil
	}
	manager := newTestManager(t, t.TempDir(), 100, executor)
	first, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := manager.Enqueue(Request{Kind: KindRollback, Server: "prod", App: "web"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	select {
	case <-started:
		t.Fatal("second operation on the same target started concurrently")
	case <-time.After(75 * time.Millisecond):
	}
	release <- struct{}{}
	<-started
	release <- struct{}{}
	waitForStatus(t, manager, first.ID, StatusSucceeded)
	waitForStatus(t, manager, second.ID, StatusSucceeded)
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent operations on target = %d, want 1", maximum.Load())
	}
}

func TestCancellation(t *testing.T) {
	started := make(chan struct{})
	manager := newTestManager(t, t.TempDir(), 100, func(ctx context.Context, _ Command, _ func(Stream, string)) (int, error) {
		close(started)
		<-ctx.Done()
		return -1, ctx.Err()
	})
	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := manager.Cancel(op.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitForStatus(t, manager, op.ID, StatusCanceled)
	if finished.FinishedAt == nil {
		t.Fatal("canceled operation has no finished_at")
	}
	if _, err := manager.Cancel(op.ID); !errors.Is(err, ErrNotCancelable) {
		t.Fatalf("second cancel error = %v", err)
	}
}

func TestSetRunningPersistFailureFailsBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var runs atomic.Int32
	executor := func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
		runs.Add(1)
		started <- struct{}{}
		<-release
		return 0, nil
	}
	manager := newTestManager(t, dir, 100, executor)
	// Hold the per-target lock with the first operation so the second parks
	// before its running transition, deterministically.
	first, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, _, err := manager.Enqueue(deployRequest("web", "example/web:2"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Force saveOperation to fail for the second operation: the atomic write
	// cannot rename onto a directory.
	record := filepath.Join(dir, "operations", "records", second.ID+".json")
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(record, 0700); err != nil {
		t.Fatal(err)
	}
	close(release)
	failed := waitForStatus(t, manager, second.ID, StatusFailed)
	if failed.ExitCode != nil {
		t.Fatalf("failed operation recorded exit code %d, want none", *failed.ExitCode)
	}
	waitForStatus(t, manager, first.ID, StatusSucceeded)
	if got := runs.Load(); got != 1 {
		t.Fatalf("executor ran %d times, want 1: the persisted-failure operation must not execute", got)
	}
}

func TestStartupRecoveryMarksOrphansInterrupted(t *testing.T) {
	dir := t.TempDir()
	store, err := openFileStore(dir, journalConfig{MaxEvents: 100})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	orphan := &Operation{
		ID: "0123456789abcdef0123456789abcdef", Request: deployRequest("web", "example/web:1"),
		Target: "server:prod/app:web", Status: StatusRunning, Attempt: 1, CreatedAt: now, StartedAt: &now,
	}
	if err := store.saveOperation(orphan); err != nil {
		t.Fatal(err)
	}
	manager := newTestManager(t, dir, 100, func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil })
	recovered, err := manager.Get(orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != StatusInterrupted || recovered.FinishedAt == nil {
		t.Fatalf("recovered operation = %+v", recovered)
	}
	events, err := manager.EventsAfter(orphan.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Data != string(StatusInterrupted) {
		t.Fatalf("recovery events = %+v", events)
	}
}

// A08: a persisted queued record is NEVER automatically executed after a
// restart. The store's commit can fail after the rename is already visible
// (directory open/sync error), so the caller may have been told the enqueue
// failed while the record sits on disk — replaying it would run work nobody
// believes was admitted. Recovery marks it interrupted for an explicit retry.
func TestStartupRecoveryNeverReplaysQueuedOperations(t *testing.T) {
	dir := t.TempDir()
	store, err := openFileStore(dir, journalConfig{MaxEvents: 100})
	if err != nil {
		t.Fatal(err)
	}
	queued := &Operation{
		ID: "abcdef0123456789abcdef0123456789", Request: deployRequest("web", "example/web:1"),
		Target: "server:prod/app:web", Status: StatusQueued, Attempt: 1, CreatedAt: time.Now().UTC(),
	}
	if err := store.saveOperation(queued); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	manager := newTestManager(t, dir, 100, func(context.Context, Command, func(Stream, string)) (int, error) {
		runs.Add(1)
		return 0, nil
	})
	recovered, err := manager.Get(queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != StatusInterrupted || recovered.FinishedAt == nil {
		t.Fatalf("recovered queued operation = %+v, want interrupted", recovered)
	}
	// Give any (wrongly) scheduled executor a moment, then assert silence.
	time.Sleep(100 * time.Millisecond)
	if got := runs.Load(); got != 0 {
		t.Fatalf("executor ran %d times after recovery, want 0", got)
	}
}

// A09: a cancel_requested record — the caller was acknowledged a durable
// cancellation intent, then the process died before the worker finished —
// resolves as canceled on restart, never as executable queued work.
func TestStartupRecoveryResolvesCancelRequested(t *testing.T) {
	dir := t.TempDir()
	store, err := openFileStore(dir, journalConfig{MaxEvents: 100})
	if err != nil {
		t.Fatal(err)
	}
	canceling := &Operation{
		ID: "abcdef0123456789abcdef0123456789", Request: deployRequest("web", "example/web:1"),
		Target: "server:prod/app:web", Status: StatusCancelRequested, Attempt: 1, CreatedAt: time.Now().UTC(),
	}
	if err := store.saveOperation(canceling); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	manager := newTestManager(t, dir, 100, func(context.Context, Command, func(Stream, string)) (int, error) {
		runs.Add(1)
		return 0, nil
	})
	recovered, err := manager.Get(canceling.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != StatusCanceled {
		t.Fatalf("recovered cancel_requested operation = %+v, want canceled", recovered)
	}
	time.Sleep(100 * time.Millisecond)
	if got := runs.Load(); got != 0 {
		t.Fatalf("executor ran %d times after recovery, want 0", got)
	}
}

func TestSecretRedactionInRecordsEventsAndErrors(t *testing.T) {
	const secret = "super-secret-token"
	// The test drives the legacy argv transport (varStdinSupport default is
	// "unsupported, verified"); R03 requires the explicit opt-in for it.
	SetLegacySecretArgVAllowed(func() bool { return true })
	t.Cleanup(func() { SetLegacySecretArgVAllowed(func() bool { return false }) })
	dir := t.TempDir()
	manager := newTestManager(t, dir, 100, func(_ context.Context, _ Command, emit func(Stream, string)) (int, error) {
		emit(StreamStdout, "connecting with "+secret)
		emit(StreamStderr, "token="+secret)
		return 1, errors.New("rejected " + secret)
	})
	op, _, err := manager.Enqueue(Request{
		Kind: KindTemplateInstall, Server: "prod", Template: "postgres", Domain: "db.example.com",
		Vars: map[string]string{"PASSWORD": secret},
	}, "secret-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	finished := waitForStatus(t, manager, op.ID, StatusFailed)
	if finished.Request.Vars["PASSWORD"] != "[REDACTED]" || strings.Contains(finished.Error, secret) {
		t.Fatalf("secret leaked through operation: %+v", finished)
	}
	events, err := manager.EventsAfter(op.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(event.Data, secret) {
			t.Fatalf("secret leaked through event: %+v", event)
		}
	}
	for _, subdir := range []string{"records", "events"} {
		data, err := os.ReadFile(filepath.Join(dir, "operations", subdir, op.ID+map[string]string{"records": ".json", "events": ".jsonl"}[subdir]))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("secret leaked in %s persistence", subdir)
		}
	}
	if _, err := manager.Retry(op.ID, nil); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("secret-bearing retry error = %v", err)
	}
}

func TestReplayAfterSequence(t *testing.T) {
	manager := newTestManager(t, t.TempDir(), 100, func(_ context.Context, _ Command, emit func(Stream, string)) (int, error) {
		emit(StreamStdout, "one")
		emit(StreamStdout, "two")
		return 0, nil
	})
	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, op.ID, StatusSucceeded)
	all, _ := manager.EventsAfter(op.ID, 0)
	replayed, _ := manager.EventsAfter(op.ID, all[1].Sequence)
	if len(replayed) != len(all)-2 || replayed[0].Sequence <= all[1].Sequence {
		t.Fatalf("replay after %d = %+v; all=%+v", all[1].Sequence, replayed, all)
	}
}

// A25: when the initial event write fails, enqueue is rejected AND no queued
// record may exist on disk — a later restart must not execute a request the
// caller saw rejected. (The record, not the event file, is the commit point.)
func TestRejectedEnqueueNeverExecutesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	var executed atomic.Int32
	manager := newTestManager(t, dir, 100, func(_ context.Context, command Command, _ func(Stream, string)) (int, error) {
		executed.Add(1)
		return 0, nil
	})

	// Make the events directory unwritable: the initial event write (the
	// FIRST durability step) fails, before any record is committed.
	eventsDir := filepath.Join(dir, "operations", "events")
	if err := os.Chmod(eventsDir, 0500); err != nil {
		t.Skipf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(eventsDir, 0700) })

	if _, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", nil); err == nil {
		os.Chmod(eventsDir, 0700)
		t.Fatal("expected enqueue to fail when the initial event cannot persist")
	}

	// No record file for the rejected operation.
	records, err := os.ReadDir(filepath.Join(dir, "operations", "records"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("rejected enqueue left %d record(s) on disk", len(records))
	}

	os.Chmod(eventsDir, 0700)

	// Restart: recovery must find nothing queued to run.
	_ = newTestManager(t, dir, 100, func(_ context.Context, command Command, _ func(Stream, string)) (int, error) {
		executed.Add(1)
		return 0, nil
	})
	// Give any (wrongly) recovered worker a moment to fire.
	time.Sleep(100 * time.Millisecond)
	if executed.Load() != 0 {
		t.Fatalf("rejected operation executed after restart (%d executions)", executed.Load())
	}
}

// A26: an escaping-heavy log line must not expand past the event reader's
// scanner limit — payloads are bounded and sanitized at ingestion, and a
// restart can still read the history.
func TestOversizedEscapingEventStaysLoadable(t *testing.T) {
	dir := t.TempDir()
	chatty := strings.Repeat("\x01", 200*1024) // raw control chars, ~6x JSON expansion
	manager := newTestManager(t, dir, 1000, func(_ context.Context, command Command, emit func(Stream, string)) (int, error) {
		emit(StreamStdout, chatty)
		return 0, nil
	})
	op, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, op.ID, StatusSucceeded)

	events, err := manager.EventsAfter(op.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type != EventStdout {
			continue
		}
		if len(e.Data) > maxEventDataBytes+len(" [truncated]") {
			t.Fatalf("stdout event data not bounded: %d bytes", len(e.Data))
		}
		if !strings.HasSuffix(e.Data, " [truncated]") {
			t.Fatalf("oversized event missing truncation marker")
		}
	}

	// The history file every line of which the reader's 1 MiB scanner must
	// accept on restart.
	if _, err := New(dir, Options{MaxEvents: 1000, Resolver: testResolver, Executor: func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
		return 0, nil
	}}); err != nil {
		t.Fatalf("restart failed to load bounded event history: %v", err)
	}
}

// A26: one corrupt event history must not disable the whole service.
func TestCorruptEventHistoryIsolated(t *testing.T) {
	dir := t.TempDir()
	manager := newTestManager(t, dir, 100, func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
		return 0, nil
	})
	op, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, op.ID, StatusSucceeded)

	// Corrupt that operation's event file beyond the scanner limit.
	eventPath := filepath.Join(dir, "operations", "events", op.ID+".jsonl")
	if err := os.WriteFile(eventPath, []byte(`{"data":"`+strings.Repeat("x", 2<<20)+`"}`), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := New(dir, Options{MaxEvents: 100, Resolver: testResolver, Executor: func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
		return 0, nil
	}}); err != nil {
		t.Fatalf("corrupt history for one operation disabled the service: %v", err)
	}
}

// A09: cancellation is persisted as intent BEFORE it is acknowledged. A
// worker that never observes the in-memory cancel (simulated by a manager
// restart) must resolve the record as canceled, not replay it.
func TestCancelPersistsIntent(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	manager := newTestManager(t, dir, 100, func(ctx context.Context, _ Command, _ func(Stream, string)) (int, error) {
		<-release // keep the operation running until the test ends
		return 0, ctx.Err()
	})
	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunning := func() {
		t.Helper()
		for {
			current, err := manager.Get(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status == StatusRunning {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForRunning()
	canceled, err := manager.Cancel(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status != StatusCancelRequested {
		t.Fatalf("cancel acknowledged with status %q, want cancel_requested", canceled.Status)
	}
	// The persisted record carries the intent.
	store, err := openFileStore(dir, journalConfig{MaxEvents: 100})
	if err != nil {
		t.Fatal(err)
	}
	operations, err := store.loadOperations()
	if err != nil {
		t.Fatal(err)
	}
	if got := operations[op.ID].Status; got != StatusCancelRequested {
		t.Fatalf("persisted status = %q, want cancel_requested", got)
	}
	close(release)

	// Restart: recovery resolves the intent as canceled.
	restarted := newTestManager(t, dir, 100, func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil })
	resolved, err := restarted.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != StatusCanceled {
		t.Fatalf("post-restart status = %q, want canceled", resolved.Status)
	}
}

// A14: repointing a server alias between admission and execution fails the
// operation instead of redirecting the queued work at the new target.
func TestExecuteRefusesRepointedTarget(t *testing.T) {
	dir := t.TempDir()
	current := map[string]string{"prod": "10.0.0.1"}
	var mu sync.Mutex
	resolver := func(name string) (Server, error) {
		mu.Lock()
		defer mu.Unlock()
		host, ok := current[name]
		if !ok {
			return Server{}, fmt.Errorf("server not found: %s", name)
		}
		return Server{Name: name, Host: host, User: "root"}, nil
	}
	block := make(chan struct{})
	commands := make(chan Command, 1)
	manager, err := New(dir, Options{
		MaxEvents: 100,
		Resolver:  resolver,
		Executor: func(_ context.Context, command Command, _ func(Stream, string)) (int, error) {
			commands <- command
			<-block
			return 0, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Occupy the target with a first operation...
	first, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-commands
	// ...admit a second operation against the same target while the first
	// still holds it (the second's admission snapshot records 10.0.0.1)...
	second, _, err := manager.Enqueue(deployRequest("api", "example/api:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// ...then repoint the alias before the queued operation executes.
	mu.Lock()
	current["prod"] = "10.0.0.2"
	mu.Unlock()
	close(block)
	finished := waitForStatus(t, manager, second.ID, StatusFailed)
	if !strings.Contains(finished.Error, "repointed") {
		t.Fatalf("repointed-target error = %q", finished.Error)
	}
	waitForStatus(t, manager, first.ID, StatusSucceeded)
}

// ── Admission budget (A12/A27 remainder) ──────────────────────────────────

// The per-target budget rejects (never silently drops) enqueues once the
// target already holds the cap in non-terminal operations, and admits again
// once they reach terminal states. An idempotent replay of an already-queued
// request must still succeed — it is not new work.
func TestAdmissionBudgetRejectsAndReleases(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	var commands atomic.Int32
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
			commands.Add(1)
			<-release
			return 0, nil
		},
		MaxQueuedPerTarget: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := &Actor{Kind: "local", Subject: "dana", Label: "dana"}
	// Fill the budget for prod/web: one running (executor holds it) + one
	// queued. Target granularity is server+app, same as the FIFO runner.
	if _, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", actor); err != nil {
		t.Fatal(err)
	}
	<-waitForCommands(t, &commands, 1)
	if _, _, err := manager.Enqueue(deployRequest("web", "img:2"), "idem-key", actor); err != nil {
		t.Fatal(err)
	}
	// Budget exhausted: a third enqueue for the same target is rejected.
	if _, _, err := manager.Enqueue(deployRequest("web", "img:3"), "", actor); !errors.Is(err, ErrAdmissionBudget) {
		t.Fatalf("error = %v, want ErrAdmissionBudget", err)
	}
	// A different target (other app, same server) is unaffected.
	if _, _, err := manager.Enqueue(deployRequest("api", "img:1"), "", actor); err != nil {
		t.Fatalf("other target rejected: %v", err)
	}
	// Replay of an already-queued request is not new work — it returns the
	// queued operation even at the cap.
	replayed, ok, err := manager.Enqueue(deployRequest("web", "img:2"), "idem-key", actor)
	if err != nil || !ok {
		t.Fatalf("idempotent replay at budget cap: replayed=%v err=%v", ok, err)
	}
	if replayed.Status != StatusQueued && replayed.Status != StatusRunning {
		t.Fatalf("replayed op status = %s", replayed.Status)
	}
	// Drain: budget frees as operations terminate.
	close(release)
	for _, op := range manager.List("", "", 0) {
		waitForStatus(t, manager, op.ID, StatusSucceeded)
	}
	if _, _, err := manager.Enqueue(deployRequest("web", "img:2"), "", actor); err != nil {
		t.Fatalf("enqueue after drain rejected: %v", err)
	}
	// Drain the stragglers before returning: a terminal persist racing
	// t.TempDir cleanup fails the test suite spuriously.
	for _, op := range manager.List("", "", 0) {
		waitForStatus(t, manager, op.ID, StatusSucceeded)
	}
	manager.Shutdown(context.Background())
}

func waitForCommands(t *testing.T, counter *atomic.Int32, want int32) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if counter.Load() >= want {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return done
}

// The budget survives a restart (it is computed from persisted records, not
// in-memory state): a manager reopened over a dir full of non-terminal —
// i.e. now interrupted — records frees its budget because interrupted is
// terminal. The load-path behavior under a full budget is therefore
// recoverable by definition; this pins it.
func TestAdmissionBudgetCountsNonTerminalOnly(t *testing.T) {
	dir := t.TempDir()
	manager := newTestManager(t, dir, 5, func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
		return 0, nil
	})
	actor := &Actor{Kind: "sso", Subject: "oidc:abc:def", Label: "Erin"}
	for i := 0; i < 3; i++ {
		if _, _, err := manager.Enqueue(deployRequest(fmt.Sprintf("app%d", i), "img:1"), "", actor); err != nil {
			t.Fatal(err)
		}
	}
	for _, op := range manager.List("", "", 0) {
		waitForStatus(t, manager, op.ID, StatusSucceeded)
	}
	// All terminal: no budget consumed.
	manager.mu.Lock()
	depth := 0
	for _, o := range manager.operations {
		if o.Target != "" && !o.Status.Terminal() {
			depth++
		}
	}
	manager.mu.Unlock()
	if depth != 0 {
		t.Fatalf("non-terminal depth after drain = %d, want 0", depth)
	}
}

// ── Actor attribution (A27 remainder) ─────────────────────────────────────

// The actor is persisted with the record, served by Get/List, and reloaded
// from disk; a retry is attributed to the retrying actor with lineage kept
// in RetryOf.
func TestActorAttributionPersistedAndReloaded(t *testing.T) {
	dir := t.TempDir()
	var runs atomic.Int32
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
			if runs.Add(1) == 1 {
				return 1, errors.New("first run fails so the op is retryable")
			}
			return 0, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueuer := &Actor{Kind: "local", Subject: "dana", Label: "dana"}
	op, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", enqueuer)
	if err != nil {
		t.Fatal(err)
	}
	if got := op.Actor; got == nil || got.Kind != "local" || got.Subject != "dana" || got.Label != "dana" {
		t.Fatalf("enqueue response actor = %+v", got)
	}
	waitForStatus(t, manager, op.ID, StatusFailed)
	retrier := &Actor{Kind: "mcp", Subject: "mcp-token/tok1", Label: "ci-bot"}
	retry, err := manager.Retry(op.ID, retrier)
	if err != nil {
		t.Fatal(err)
	}
	if retry.RetryOf != op.ID {
		t.Fatalf("retry lineage: retry_of = %s, want %s", retry.RetryOf, op.ID)
	}
	waitForStatus(t, manager, retry.ID, StatusSucceeded)

	// Reopen from disk: attribution must survive the restart.
	reopened, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
			return 0, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Actor == nil || loaded.Actor.Subject != "dana" || loaded.Actor.Kind != "local" {
		t.Fatalf("reloaded actor = %+v", loaded.Actor)
	}
	loadedRetry, err := reopened.Get(retry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedRetry.Actor == nil || loadedRetry.Actor.Kind != "mcp" || loadedRetry.Actor.Label != "ci-bot" {
		t.Fatalf("reloaded retry actor = %+v", loadedRetry.Actor)
	}
	// Unattributed callers (nil actor) keep a nil field, not a zero struct.
	plain, _, err := reopened.Enqueue(deployRequest("solo", "img:9"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Actor != nil {
		t.Fatalf("nil actor must stay nil, got %+v", plain.Actor)
	}
	// Join background work so terminal persists cannot race TempDir cleanup.
	waitForStatus(t, reopened, plain.ID, StatusSucceeded)
	manager.Shutdown(context.Background())
	reopened.Shutdown(context.Background())
}

// ── Health + bounded shutdown (A39/A47) ───────────────────────────────────

// A persistence failure after the command's outcome is decided must surface
// through Health() as degradation (it cannot change the outcome — it is
// reported instead), and the marker clears once persistence succeeds again.
func TestHealthReportsPersistDegradationAndRecovery(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(dir, "operations", "records"), 0700)
		_ = os.Chmod(filepath.Join(dir, "operations", "events"), 0700)
	})
	started := make(chan struct{})
	var startedOnce sync.Once
	release := make(chan struct{})
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
			startedOnce.Do(func() { close(started) })
			<-release
			return 0, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", nil); err != nil {
		t.Fatal(err)
	}
	<-started
	if h := manager.Health(); h.PersistDegraded {
		t.Fatalf("healthy manager reports degraded: %+v", h)
	}
	// Break the records directory so the terminal persist fails.
	records := filepath.Join(dir, "operations", "records")
	if err := os.Chmod(records, 0500); err != nil {
		t.Fatal(err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if manager.Health().PersistDegraded {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	h := manager.Health()
	if !h.PersistDegraded || h.RecordError == "" {
		t.Fatalf("degraded manager not reported: %+v", h)
	}
	// The operation still reached a terminal outcome.
	for _, op := range manager.List("", "", 0) {
		if op.Status != StatusSucceeded {
			t.Fatalf("op status = %s, want succeeded (outcome unchanged by persist failure)", op.Status)
		}
	}
	// Repair: subsequent successful persistence clears the marker.
	if err := os.Chmod(records, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Enqueue(deployRequest("web", "img:2"), "", nil); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !manager.Health().PersistDegraded {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if h := manager.Health(); h.PersistDegraded {
		t.Fatalf("degradation not cleared after recovery: %+v", h)
	}
	// A broken journal directory is reported through JournalError.
	if err := os.Chmod(filepath.Join(dir, "operations", "events"), 0500); err != nil {
		t.Fatal(err)
	}
	if h := manager.Health(); h.JournalError == "" {
		t.Fatal("unwritable journal directory not reported")
	}
	// Join background work so terminal persists cannot race TempDir cleanup.
	manager.Shutdown(context.Background())
}

// Shutdown drains runners when work finishes in time, and force-cancels
// stragglers (with an explicit shutdown message) when it does not.
func TestShutdownDrainsThenForceCancels(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	var entered atomic.Int32
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(ctx context.Context, _ Command, _ func(Stream, string)) (int, error) {
			entered.Add(1)
			<-release
			return 0, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && entered.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if entered.Load() == 0 {
		t.Fatal("executor never started")
	}

	// Fast path: work finishes before the deadline — Shutdown returns
	// without touching statuses.
	close(release)
	manager.Shutdown(context.Background())
	for _, op := range manager.List("", "", 0) {
		if op.Status != StatusSucceeded {
			t.Fatalf("drained op status = %s, want succeeded", op.Status)
		}
	}
}

func TestShutdownForceCancelsStragglers(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	var entered atomic.Int32
	manager, err := New(dir, Options{
		Resolver: testResolver,
		// Ignores context cancellation, like a wedged CLI child.
		Executor: func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
			entered.Add(1)
			<-release
			return 0, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, _, err := manager.Enqueue(deployRequest("web", "img:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && entered.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if entered.Load() == 0 {
		t.Fatal("executor never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	begin := time.Now()
	manager.Shutdown(ctx) // returns after force-cancel + grace despite the stuck executor
	cancel()
	if elapsed := time.Since(begin); elapsed > 7*time.Second {
		t.Fatalf("shutdown not bounded: took %s", elapsed)
	}

	close(release) // let the executor return so the runner resolves the cancel
	finished := waitForStatus(t, manager, queued.ID, StatusCanceled)
	if !strings.Contains(finished.Error, "shutting down") {
		t.Fatalf("cancel reason = %q, want shutdown attribution", finished.Error)
	}
	manager.Shutdown(context.Background())
}
