package operation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "persist-key")
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
	events, err := reopened.EventsAfter(op.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("retained %d events, want 5", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].Sequence != events[i-1].Sequence+1 {
			t.Fatalf("non-monotonic sequences: %+v", events)
		}
	}
	lines, err := os.ReadFile(filepath.Join(dir, "operations", "events", op.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(lines)), "\n") + 1; got != 5 {
		t.Fatalf("event file contains %d lines, want 5", got)
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
	first, replayed, err := manager.Enqueue(deployRequest("web", "example/web:1"), "same-key")
	if err != nil || replayed {
		t.Fatalf("first enqueue: replayed=%v err=%v", replayed, err)
	}
	second, replayed, err := manager.Enqueue(deployRequest("web", "example/web:1"), "same-key")
	if err != nil || !replayed || second.ID != first.ID {
		t.Fatalf("idempotent replay: first=%s second=%s replayed=%v err=%v", first.ID, second.ID, replayed, err)
	}
	_, _, err = manager.Enqueue(deployRequest("web", "example/web:2"), "same-key")
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
	first, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := manager.Enqueue(Request{Kind: KindRollback, Server: "prod", App: "web"}, "")
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
	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "")
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

func TestStartupRecoveryMarksOrphansInterrupted(t *testing.T) {
	dir := t.TempDir()
	store, err := openFileStore(dir, 100)
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

func TestStartupRecoveryResumesQueuedOperations(t *testing.T) {
	dir := t.TempDir()
	store, err := openFileStore(dir, 100)
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
	manager := newTestManager(t, dir, 100, func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil })
	waitForStatus(t, manager, queued.ID, StatusSucceeded)
}

func TestStartupRecoveryResolvesManifestProjectInternally(t *testing.T) {
	dir := t.TempDir()
	store, err := openFileStore(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 64)
	queued := &Operation{
		ID: "11111111111111111111111111111111",
		Request: Request{
			Kind: KindManifestApply, Server: "prod", App: "web", Mode: "dash-managed", ManifestRevision: revision,
		},
		Metadata: Metadata{Mode: "dash-managed"}, Target: "server:prod/app:web", Status: StatusQueued, Attempt: 1, CreatedAt: time.Now().UTC(),
	}
	if err := store.saveOperation(queued); err != nil {
		t.Fatal(err)
	}
	commands := make(chan Command, 1)
	manager, err := New(dir, Options{
		MaxEvents: 100,
		Resolver:  testResolver,
		ProjectResolver: func(server, app, gotRevision string) (string, error) {
			if server != "prod" || app != "web" || gotRevision != revision {
				t.Fatalf("unexpected manifest resolution: %s/%s@%s", server, app, gotRevision)
			}
			return "/internal/manifests/prod/web/revisions/" + revision, nil
		},
		Executor: func(_ context.Context, command Command, _ func(Stream, string)) (int, error) {
			commands <- command
			return 0, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, queued.ID, StatusSucceeded)
	command := <-commands
	if command.Args[0] != "--project-dir" || command.Args[2] != "deploy" {
		t.Fatalf("recovered command = %v", command.Args)
	}
}

func TestSecretRedactionInRecordsEventsAndErrors(t *testing.T) {
	const secret = "super-secret-token"
	dir := t.TempDir()
	manager := newTestManager(t, dir, 100, func(_ context.Context, _ Command, emit func(Stream, string)) (int, error) {
		emit(StreamStdout, "connecting with "+secret)
		emit(StreamStderr, "token="+secret)
		return 1, errors.New("rejected " + secret)
	})
	op, _, err := manager.Enqueue(Request{
		Kind: KindTemplateInstall, Server: "prod", Template: "postgres", Domain: "db.example.com",
		Vars: map[string]string{"PASSWORD": secret},
	}, "secret-key")
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
	if _, err := manager.Retry(op.ID); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("secret-bearing retry error = %v", err)
	}
}

func TestReplayAfterSequence(t *testing.T) {
	manager := newTestManager(t, t.TempDir(), 100, func(_ context.Context, _ Command, emit func(Stream, string)) (int, error) {
		emit(StreamStdout, "one")
		emit(StreamStdout, "two")
		return 0, nil
	})
	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "")
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
