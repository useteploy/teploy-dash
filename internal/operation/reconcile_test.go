package operation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seedOperationRecord persists an operation record directly, simulating what
// a previous process left on disk when it died mid-flight.
func seedOperationRecord(t *testing.T, dir, id string, status Status) *Operation {
	t.Helper()
	store, err := openFileStore(dir, journalConfig{MaxEvents: 100})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op := &Operation{
		ID: id, Request: deployRequest("web", "example/web:1"),
		Target: "server:prod/app:web", Status: status, Attempt: 1,
		CreatedAt: now, StartedAt: &now,
	}
	if err := store.saveOperation(op); err != nil {
		t.Fatal(err)
	}
	return op
}

// waitForReconciliation polls until the operation carries a reconciliation
// in the wanted state, then returns it.
func waitForReconciliation(t *testing.T, manager *Manager, id string, state ReconcileState) *Reconciliation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, err := manager.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if op.Reconciliation != nil && op.Reconciliation.State == state {
			return op.Reconciliation
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, _ := manager.Get(id)
	t.Fatalf("operation %s reconciliation = %+v, want state %s", id, op.Reconciliation, state)
	return nil
}

// ── Restart reconciliation (D02 piece a) ───────────────────────────────────

// D02 mutation 1 (was red against the pre-D02 blind-retry behavior): a
// restart marks mid-flight operations interrupted and immediately re-offered
// them as one-click retryable work without consulting the target. Retry must
// now refuse until the receipt reconciles the outcome, the restart path
// itself never executes anything, and an explicit retry afterwards keeps the
// lineage.
func TestRestartDoesNotReofferRetryBeforeReconciliation(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	seedOperationRecord(t, dir, id, StatusRunning)
	block := make(chan struct{})
	var runs atomic.Int32
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(context.Context, Command, func(Stream, string)) (int, error) {
			runs.Add(1)
			return 0, nil
		},
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			<-block // keep the outcome reconciling for as long as the test needs
			return Receipt{Applied: true, Evidence: "app web runs image example/web:1"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())

	recovered, err := manager.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != StatusInterrupted {
		t.Fatalf("recovered status = %s, want interrupted", recovered.Status)
	}
	if recovered.Reconciliation == nil || recovered.Reconciliation.State != ReconcileStateReconciling {
		t.Fatalf("recovered reconciliation = %+v, want reconciling", recovered.Reconciliation)
	}
	if _, err := manager.Retry(id, nil); !errors.Is(err, ErrReconciliationPending) {
		t.Fatalf("retry before reconciliation error = %v, want ErrReconciliationPending", err)
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("restart path executed the operation %d time(s); nothing may auto-retry", got)
	}

	close(block)
	waitForReconciliation(t, manager, id, ReconcileStateApplied)
	// Nothing was auto-executed by the restart itself.
	if got := runs.Load(); got != 0 {
		t.Fatalf("reconciliation executed the operation %d time(s); it only reads", got)
	}
	// After the answer lands, retry is an explicit re-authorization again.
	retry, err := manager.Retry(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retry.RetryOf != id || retry.Attempt != 2 {
		t.Fatalf("retry lineage = %s attempt %d, want %s attempt 2", retry.RetryOf, retry.Attempt, id)
	}
	waitForStatus(t, manager, retry.ID, StatusSucceeded)
}

// A receipt showing the effect landed resolves the interrupted record as
// reconciled-applied, with the evidence on the record and a stream event —
// and still without executing anything.
func TestRestartReconciliationApplied(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	seedOperationRecord(t, dir, id, StatusRunning)
	var runs atomic.Int32
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(context.Context, Command, func(Stream, string)) (int, error) {
			runs.Add(1)
			return 0, nil
		},
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			return Receipt{Applied: true, Evidence: `app "web" on prod.example runs image example/web:1`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())

	rec := waitForReconciliation(t, manager, id, ReconcileStateApplied)
	if rec.Evidence != `app "web" on prod.example runs image example/web:1` || rec.CheckedAt.IsZero() {
		t.Fatalf("reconciliation record = %+v", rec)
	}
	op, err := manager.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusInterrupted {
		t.Fatalf("status = %s, want interrupted (reconciled, not relabeled)", op.Status)
	}
	events, err := manager.EventsAfter(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !eventDataContains(events, string(ReconcileStateApplied)) {
		t.Fatalf("reconciliation not visible in the stream: %+v", events)
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("reconciliation executed the operation %d time(s)", got)
	}
	// The persisted record survives a restart (checked-at/evidence reload).
	reopened := newTestManager(t, dir, 100, func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil })
	defer reopened.Shutdown(context.Background())
	// The reopened manager has no receipt reader: its recovery must NOT
	// overwrite the already-answered reconciliation.
	stored, err := reopened.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Reconciliation == nil || stored.Reconciliation.State != ReconcileStateApplied {
		t.Fatalf("reloaded reconciliation = %+v, want reconciled-applied preserved", stored.Reconciliation)
	}
}

// A receipt showing the effect did NOT land resolves as reconciled-not-
// applied, and retry is offered again afterwards.
func TestRestartReconciliationNotApplied(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	seedOperationRecord(t, dir, id, StatusRunning)
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil },
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			return Receipt{Applied: false, Evidence: `app "web" absent from teploy app list on prod.example`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())

	rec := waitForReconciliation(t, manager, id, ReconcileStateNotApplied)
	if rec.Evidence == "" {
		t.Fatalf("reconciliation record = %+v", rec)
	}
	events, err := manager.EventsAfter(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !eventDataContains(events, string(ReconcileStateNotApplied)) {
		t.Fatalf("reconciliation not visible in the stream: %+v", events)
	}
	retry, err := manager.Retry(id, nil)
	if err != nil {
		t.Fatalf("retry after reconciled-not-applied: %v", err)
	}
	waitForStatus(t, manager, retry.ID, StatusSucceeded)
}

// A failing receipt read (CLI/transport error) resolves as needs-manual-check
// carrying the exact reason — never a silent "failed", never a guess.
func TestRestartReconciliationManualOnReadFailure(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	seedOperationRecord(t, dir, id, StatusRunning)
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil },
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			return Receipt{}, errors.New("running teploy app list for prod: dial tcp 10.0.0.9:22: i/o timeout")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())

	rec := waitForReconciliation(t, manager, id, ReconcileStateManual)
	if !strings.Contains(rec.Reason, "dial tcp 10.0.0.9:22: i/o timeout") {
		t.Fatalf("manual-check reason = %q, want the exact read failure", rec.Reason)
	}
	op, _ := manager.Get(id)
	if op.Status != StatusInterrupted {
		t.Fatalf("status = %s, want interrupted (manual check, not relabeled failed)", op.Status)
	}
	events, err := manager.EventsAfter(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !eventDataContains(events, string(ReconcileStateManual)) {
		t.Fatalf("manual-check not visible in the stream: %+v", events)
	}
}

// Without a receipt reader the interrupted record is labeled needs-manual-
// check with the exact reason at recovery time — never an eternal
// "reconciling", never a silent guess.
func TestRestartReconciliationManualWithoutReader(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	seedOperationRecord(t, dir, id, StatusRunning)
	manager := newTestManager(t, dir, 100, func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil })
	defer manager.Shutdown(context.Background())

	op, err := manager.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if op.Reconciliation == nil || op.Reconciliation.State != ReconcileStateManual {
		t.Fatalf("reconciliation = %+v, want needs-manual-check", op.Reconciliation)
	}
	if !strings.Contains(op.Reconciliation.Reason, "no receipt reader") {
		t.Fatalf("manual-check reason = %q", op.Reconciliation.Reason)
	}
}

// A process that dies mid-reconciliation leaves reconciling records; the
// next boot picks the reads back up.
func TestRestartResumesUnfinishedReconciliation(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	seedOperationRecord(t, dir, id, StatusRunning)
	// Stage 1: a boot that begins reconciling but "dies" before the answer.
	block := make(chan struct{})
	dying, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil },
		ReceiptReader: func(ctx context.Context, _ *Operation) (Receipt, error) {
			select {
			case <-block:
				return Receipt{}, errors.New("unreachable in this boot")
			case <-ctx.Done():
				return Receipt{}, ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := dying.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Reconciliation == nil || recovered.Reconciliation.State != ReconcileStateReconciling {
		t.Fatalf("first boot reconciliation = %+v", recovered.Reconciliation)
	}
	dying.Shutdown(context.Background()) // cancels the blocked read; record stays reconciling
	close(block)

	// Stage 2: the next boot finishes what the first could not.
	second, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil },
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			return Receipt{Applied: false, Evidence: "app absent"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Shutdown(context.Background())
	waitForReconciliation(t, second, id, ReconcileStateNotApplied)
}

func eventDataContains(events []Event, data string) bool {
	for _, event := range events {
		if event.Data == data {
			return true
		}
	}
	return false
}

// ── Cancellation states (D02 piece b) ──────────────────────────────────────

// D02 mutation 2 (was red against the pre-D02 silent-loss behavior): a cancel
// landing while the CLI command is mid-flight used to record plain
// "canceled". When the receipt shows the effect landed, the terminal state is
// already_committed — the effect stood, no rollback is pretended — with the
// receipt evidence on the record and every transition in the stream.
func TestCancelDuringRunAlreadyCommitted(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(ctx context.Context, _ Command, _ func(Stream, string)) (int, error) {
			close(started)
			<-ctx.Done()
			return -1, ctx.Err()
		},
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			return Receipt{Applied: true, Evidence: `app "web" on prod.example runs image example/web:1`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())

	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := manager.Cancel(op.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitForStatus(t, manager, op.ID, StatusAlreadyCommitted)
	if !strings.Contains(finished.Error, "no rollback was performed") {
		t.Fatalf("already-committed message = %q", finished.Error)
	}
	if finished.Reconciliation == nil ||
		finished.Reconciliation.State != ReconcileStateApplied ||
		finished.Reconciliation.Evidence != `app "web" on prod.example runs image example/web:1` {
		t.Fatalf("already-committed receipt = %+v", finished.Reconciliation)
	}
	// The full transition chain is persisted and visible in the stream.
	events, err := manager.EventsAfter(op.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{string(StatusCancelRequested), string(StatusStopping), string(StatusAlreadyCommitted)}
	if !eventOrderContains(events, wantOrder) {
		t.Fatalf("stream transitions = %+v, want %v in order", events, wantOrder)
	}
	// The effect stood — re-running it is not offered.
	if _, err := manager.Retry(op.ID, nil); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("already-committed retry error = %v, want ErrNotRetryable", err)
	}
}

// A cancel whose receipt confirms the effect never landed records canceled
// with the evidence attached.
func TestCancelDuringRunVerifiedNotApplied(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	var startedOnce sync.Once
	var runs atomic.Int32
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(ctx context.Context, _ Command, _ func(Stream, string)) (int, error) {
			if runs.Add(1) > 1 {
				return 0, nil // the explicit retry completes normally
			}
			startedOnce.Do(func() { close(started) })
			<-ctx.Done()
			return -1, ctx.Err()
		},
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			return Receipt{Applied: false, Evidence: `app "web" absent from teploy app list on prod.example`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())

	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := manager.Cancel(op.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitForStatus(t, manager, op.ID, StatusCanceled)
	if !strings.Contains(finished.Error, "did not land") {
		t.Fatalf("canceled message = %q", finished.Error)
	}
	if finished.Reconciliation == nil || finished.Reconciliation.State != ReconcileStateNotApplied {
		t.Fatalf("canceled receipt = %+v", finished.Reconciliation)
	}
	// Verified stopped work stays retryable as an explicit action.
	retry, err := manager.Retry(op.ID, nil)
	if err != nil {
		t.Fatalf("canceled-verified retry: %v", err)
	}
	waitForStatus(t, manager, retry.ID, StatusSucceeded)
}

// A cancel before the operation starts never consults a receipt — the
// effect provably never began.
func TestCancelBeforeStartSkipsReceipt(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var reads atomic.Int32
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(_ context.Context, _ Command, _ func(Stream, string)) (int, error) {
			started <- struct{}{}
			<-release
			return 0, nil
		},
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			reads.Add(1)
			return Receipt{Applied: true, Evidence: "should not be consulted"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())

	first, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, _, err := manager.Enqueue(deployRequest("web", "example/web:2"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Cancel(second.ID); err != nil {
		t.Fatal(err)
	}
	// Let the target drain: the first finishes, the canceled second resolves
	// at its queue turn — before any execution side effect, so no receipt.
	close(release)
	canceled := waitForStatus(t, manager, second.ID, StatusCanceled)
	if canceled.Reconciliation != nil {
		t.Fatalf("pre-start cancel consulted a receipt: %+v", canceled.Reconciliation)
	}
	waitForStatus(t, manager, first.ID, StatusSucceeded)
	if got := reads.Load(); got != 0 {
		t.Fatalf("receipt read %d time(s) for a pre-start cancel", got)
	}
}

// When no receipt can answer (reader unavailable or read failure), the
// cancellation still records canceled — dash did stop waiting — but the
// record says the outcome could not be verified, with the exact reason.
func TestCancelDuringRunUnverifiableOutcome(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(ctx context.Context, _ Command, _ func(Stream, string)) (int, error) {
			close(started)
			<-ctx.Done()
			return -1, ctx.Err()
		},
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			return Receipt{}, errors.New("no receipt reader for operation kind \"rollback\"")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())

	op, _, err := manager.Enqueue(Request{Kind: KindRollback, Server: "prod", App: "web"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := manager.Cancel(op.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitForStatus(t, manager, op.ID, StatusCanceled)
	if !strings.Contains(finished.Error, "could not be verified") ||
		!strings.Contains(finished.Error, "no receipt reader for operation kind") {
		t.Fatalf("unverifiable cancel message = %q", finished.Error)
	}
	if finished.Reconciliation == nil || finished.Reconciliation.State != ReconcileStateManual {
		t.Fatalf("unverifiable cancel receipt = %+v", finished.Reconciliation)
	}
}

// A second cancel while the outcome is being resolved (stopping) is refused:
// the intent is already durable and the resolution owns the terminal state.
func TestCancelWhileStoppingRefused(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	block := make(chan struct{})
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(ctx context.Context, _ Command, _ func(Stream, string)) (int, error) {
			close(started)
			<-ctx.Done()
			return -1, ctx.Err()
		},
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			<-block
			return Receipt{Applied: false, Evidence: "app absent"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())

	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := manager.Cancel(op.ID); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, op.ID, StatusStopping)
	if _, err := manager.Cancel(op.ID); !errors.Is(err, ErrNotCancelable) {
		t.Fatalf("cancel while stopping error = %v, want ErrNotCancelable", err)
	}
	close(block)
	waitForStatus(t, manager, op.ID, StatusCanceled)
}

// A stopping record left by a process that died mid-resolution recovers as
// interrupted — outcome unknown — and is reconciled like any other.
func TestStoppingRecordRecoversAsInterruptedAndReconciles(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	seedOperationRecord(t, dir, id, StatusStopping)
	manager, err := New(dir, Options{
		Resolver: testResolver,
		Executor: func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil },
		ReceiptReader: func(context.Context, *Operation) (Receipt, error) {
			return Receipt{Applied: false, Evidence: "app absent"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())

	op, err := manager.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusInterrupted {
		t.Fatalf("recovered stopping record = %s, want interrupted", op.Status)
	}
	waitForReconciliation(t, manager, id, ReconcileStateNotApplied)
}

func eventOrderContains(events []Event, want []string) bool {
	index := 0
	for _, event := range events {
		if index < len(want) && event.Data == want[index] {
			index++
		}
	}
	return index == len(want)
}
