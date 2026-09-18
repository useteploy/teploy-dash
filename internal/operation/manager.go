package operation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultMaxEvents = 1000

// maxEventDataBytes bounds one event's payload before it is persisted. JSON
// escaping of control characters can expand a raw line ~6x in its encoded
// form, and the event reader scans with a 1 MiB limit — an unbounded log
// line could therefore make an operation's whole history unreadable on
// restart (A26). 16 KiB of payload stays comfortably under the scanner limit
// even fully escaped.
const maxEventDataBytes = 16 << 10

// boundedEventData sanitizes and truncates event data at ingestion.
func boundedEventData(data string) string {
	data = strings.ToValidUTF8(data, "\uFFFD")
	if len(data) <= maxEventDataBytes {
		return data
	}
	return data[:maxEventDataBytes] + " [truncated]"
}

type Options struct {
	MaxEvents int
	// MaxJournalBytes caps one operation's on-disk event journal before it
	// is compacted to the retained suffix (0 = package default).
	MaxJournalBytes int64
	// MaxHistoryAge is the retention age for terminal operations: records
	// and journals older than this are deleted (0 = package default; a
	// negative value disables age retention).
	MaxHistoryAge time.Duration
	// MaxOperations bounds the total retained operation count by deleting
	// the oldest terminal operations first (0 = package default; negative
	// disables the count bound). Non-terminal operations are never deleted.
	MaxOperations   int
	Resolver        Resolver
	ProjectResolver ProjectResolver
	Executor        Executor
}

// job is one admitted operation waiting for or receiving execution on its
// target's FIFO runner.
type job struct {
	id      string
	command Command
	ctx     context.Context
}

// targetRunner serializes one target's jobs in admission order (A12/A27
// core): the queue is appended under the manager mutex when the operation is
// admitted, and a single worker goroutine pops it front-first, so same-target
// operations execute FIFO instead of racing for a semaphore.
type targetRunner struct {
	queue []job
	busy  bool
}

type Manager struct {
	mu              sync.Mutex
	store           *fileStore
	operations      map[string]*Operation
	events          map[string][]Event
	idempotency     map[string]string
	cancels         map[string]context.CancelFunc
	targets         map[string]*targetRunner
	subscribers     map[string]map[chan struct{}]struct{}
	resolver        Resolver
	projectResolver ProjectResolver
	executor        Executor
	maxEvents       int
	maxHistoryAge   time.Duration
	maxOperations   int
	admissionSeq    uint64
	retireMu        sync.Mutex
	lastSweep       time.Time
}

func New(dataDir string, options Options) (*Manager, error) {
	if options.MaxEvents <= 0 {
		options.MaxEvents = defaultMaxEvents
	}
	if options.MaxJournalBytes <= 0 {
		options.MaxJournalBytes = defaultMaxJournalBytes
	}
	if options.MaxHistoryAge == 0 {
		options.MaxHistoryAge = defaultMaxHistoryAge
	}
	if options.MaxOperations == 0 {
		options.MaxOperations = defaultMaxOperations
	}
	if options.Executor == nil {
		return nil, fmt.Errorf("operation executor is required")
	}
	store, err := openFileStore(dataDir, journalConfig{
		MaxEvents:       options.MaxEvents,
		MaxJournalBytes: options.MaxJournalBytes,
	})
	if err != nil {
		return nil, err
	}
	// Retention runs before the load so expired history is never parsed
	// into memory (useteploy__teploy-dash-04: bounded startup).
	if removed := store.sweepRetention(time.Now().UTC(), options.MaxHistoryAge, options.MaxOperations); len(removed) > 0 {
		log.Printf("[operation] retention removed %d terminal operation(s)", len(removed))
	}
	operations, err := store.loadOperations()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		store:           store,
		operations:      operations,
		events:          make(map[string][]Event),
		idempotency:     make(map[string]string),
		cancels:         make(map[string]context.CancelFunc),
		targets:         make(map[string]*targetRunner),
		subscribers:     make(map[string]map[chan struct{}]struct{}),
		resolver:        options.Resolver,
		projectResolver: options.ProjectResolver,
		executor:        options.Executor,
		maxEvents:       options.MaxEvents,
		maxHistoryAge:   options.MaxHistoryAge,
		maxOperations:   options.MaxOperations,
	}
	for id, op := range operations {
		if op.Metadata.Mode == "" {
			op.Metadata.Mode = op.Request.Mode
			if op.Metadata.Mode == "" && op.Request.Kind == KindDeploy {
				op.Metadata.Mode = "ad-hoc"
			}
		}
		if op.AdmissionSeq > m.admissionSeq {
			m.admissionSeq = op.AdmissionSeq
		}
		events, err := store.loadEvents(id)
		if err != nil {
			// A corrupt/oversized history for ONE operation must not disable
			// the whole operation service (A26). Drop that history loudly;
			// the operation record itself is intact and recovery decides its
			// fate below.
			log.Printf("[operation] dropping unreadable event history for %s: %v", id, err)
			events = nil
		}
		// Bounded in-memory window: only the retained tail is kept; older
		// history is served from the journal on demand (useteploy__teploy-dash-04).
		if len(events) > m.maxEvents {
			events = events[len(events)-m.maxEvents:]
		}
		m.events[id] = events
		if op.IdempotencyKey != "" {
			m.idempotency[op.IdempotencyKey] = id
		}
	}
	if err := m.recover(); err != nil {
		return nil, err
	}
	m.lastSweep = time.Now()
	return m, nil
}

func (m *Manager) Enqueue(req Request, idempotencyKey string) (*Operation, bool, error) {
	return m.enqueue(req, idempotencyKey, "", 1)
}

func (m *Manager) enqueue(req Request, idempotencyKey, retryOf string, attempt int) (*Operation, bool, error) {
	if len(idempotencyKey) > 255 || strings.ContainsAny(idempotencyKey, "\r\n") {
		return nil, false, fmt.Errorf("invalid idempotency key")
	}
	if req.Kind == KindDeploy && req.Mode == "" {
		req.Mode = "ad-hoc"
	}
	command, admitted, target, err := Build(req, m.resolver, m.projectResolver)
	if err != nil {
		return nil, false, err
	}
	hash, err := requestHash(req)
	if err != nil {
		return nil, false, err
	}

	m.mu.Lock()
	if idempotencyKey != "" {
		if id, ok := m.idempotency[idempotencyKey]; ok {
			existing := m.operations[id]
			if existing.requestHash != hash {
				m.mu.Unlock()
				return nil, false, ErrIdempotencyConflict
			}
			copy := cloneOperation(existing)
			m.mu.Unlock()
			return copy, true, nil
		}
	}
	id, err := newID()
	if err != nil {
		m.mu.Unlock()
		return nil, false, err
	}
	now := time.Now().UTC()
	snapshot := admitted
	m.admissionSeq++
	op := &Operation{
		ID:             id,
		Request:        redactedRequest(req),
		Metadata:       Metadata{Mode: req.Mode},
		Target:         target,
		Status:         StatusQueued,
		IdempotencyKey: idempotencyKey,
		RetryOf:        retryOf,
		Attempt:        attempt,
		CreatedAt:      now,
		HasSecrets:     len(command.Secrets) > 0,
		AdmittedServer: &snapshot,
		AdmissionSeq:   m.admissionSeq,
		requestHash:    hash,
	}

	// Durable admission (A25): the queued RECORD is the commit point. The
	// initial event journal entry is written first; if the record write then
	// fails, the orphan journal is inert — recovery enumerates records, never
	// event files. The reverse order left a queued record on disk after a
	// rejected enqueue (event write failed, record already saved), which
	// recovery would EXECUTE even though the caller saw an error. Nothing is
	// published in memory (operations, idempotency, runner) until the record
	// commit has happened.
	initial := Event{Sequence: 1, OperationID: id, Type: EventStatus, Data: string(StatusQueued), CreatedAt: now}
	if err := m.store.appendEvent(id, initial); err != nil {
		m.mu.Unlock()
		return nil, false, fmt.Errorf("persist initial event: %w", err)
	}
	if err := m.store.saveOperation(op); err != nil {
		m.mu.Unlock()
		return nil, false, fmt.Errorf("persist operation record: %w", err)
	}
	m.operations[id] = op
	m.events[id] = []Event{initial}
	if idempotencyKey != "" {
		m.idempotency[idempotencyKey] = id
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[id] = cancel
	m.admitToTarget(target, job{id: id, command: command, ctx: ctx})
	result := cloneOperation(op)
	m.mu.Unlock()

	return result, false, nil
}

// admitToTarget appends the job to its target's FIFO queue and starts the
// target worker if idle. Caller must hold m.mu; execution happens on the
// worker goroutine.
func (m *Manager) admitToTarget(target string, j job) {
	runner := m.targets[target]
	if runner == nil {
		runner = &targetRunner{}
		m.targets[target] = runner
	}
	runner.queue = append(runner.queue, j)
	if !runner.busy {
		runner.busy = true
		go m.runTarget(target)
	}
}

// runTarget executes its target's jobs in admission order, one at a time,
// until the queue drains.
func (m *Manager) runTarget(target string) {
	for {
		m.mu.Lock()
		runner := m.targets[target]
		if runner == nil || len(runner.queue) == 0 {
			delete(m.targets, target)
			m.mu.Unlock()
			return
		}
		j := runner.queue[0]
		runner.queue = runner.queue[1:]
		m.mu.Unlock()
		m.execute(j)
	}
}

func (m *Manager) execute(j job) {
	// A cancellation that landed while the job was queued behind another
	// operation resolves here, before any execution side effect.
	if j.ctx.Err() != nil {
		m.finish(j.id, StatusCanceled, -1, "operation canceled")
		return
	}
	m.mu.Lock()
	op := m.operations[j.id]
	m.mu.Unlock()
	if op == nil {
		return
	}
	// Target identity check (A14): the server alias must still resolve to
	// the host/user admitted with the operation. Repointing an alias between
	// admission and execution must fail the operation for re-authorization,
	// not redirect queued work at whatever the name points to now. Records
	// from before the field existed (or with no resolver) skip the check.
	if mismatch := m.checkTarget(op); mismatch != "" {
		m.finish(j.id, StatusFailed, -1, mismatch)
		return
	}
	if err := m.setRunning(j.id); err != nil {
		if errors.Is(err, errCancelRequested) {
			m.finish(j.id, StatusCanceled, -1, "operation canceled")
		} else {
			m.finish(j.id, StatusFailed, -1, err.Error())
		}
		return
	}
	exitCode, err := m.executor(j.ctx, j.command, func(stream Stream, data string) {
		eventType := EventStdout
		if stream == StreamStderr {
			eventType = EventStderr
		}
		m.emit(j.id, eventType, Redact(data, j.command.Secrets))
	})
	if j.ctx.Err() != nil {
		m.finish(j.id, StatusCanceled, exitCode, "operation canceled")
		return
	}
	if err != nil || exitCode != 0 {
		message := "operation failed"
		if err != nil {
			message = Redact(err.Error(), j.command.Secrets)
		}
		m.finish(j.id, StatusFailed, exitCode, message)
		return
	}
	m.finish(j.id, StatusSucceeded, exitCode, "")
}

// checkTarget compares the operation's admitted server snapshot with the
// current resolution of its server name. Empty message means the target is
// unchanged (or the check does not apply).
func (m *Manager) checkTarget(op *Operation) string {
	if op.AdmittedServer == nil || m.resolver == nil {
		return ""
	}
	current, err := m.resolver(op.Request.Server)
	if err != nil {
		return fmt.Sprintf("server %q could not be resolved since admission (%v); verify the server configuration and retry", op.Request.Server, err)
	}
	if current.Host != op.AdmittedServer.Host || userOf(current) != userOf(*op.AdmittedServer) {
		return fmt.Sprintf("server %q was repointed since admission (admitted %s, now %s); explicit re-authorization required — retry the operation", op.Request.Server, op.AdmittedServer.Host, current.Host)
	}
	return ""
}

func userOf(srv Server) string {
	if srv.User == "" {
		return "root"
	}
	return srv.User
}

// errCancelRequested reports setRunning refusing to start an operation whose
// cancellation was durably requested while it waited for its target turn.
var errCancelRequested = errors.New("operation cancel requested")

func (m *Manager) setRunning(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.operations[id]
	if op == nil {
		return nil
	}
	if op.Status == StatusCancelRequested {
		return errCancelRequested
	}
	if op.Status != StatusQueued {
		return nil
	}
	now := time.Now().UTC()
	op.Status = StatusRunning
	op.StartedAt = &now
	if err := m.store.saveOperation(op); err != nil {
		return fmt.Errorf("persist running state: %w", err)
	}
	_ = m.appendEventLocked(id, EventStatus, string(StatusRunning))
	return nil
}

func (m *Manager) finish(id string, status Status, exitCode int, message string) {
	m.mu.Lock()
	op := m.operations[id]
	if op == nil || op.Status.Terminal() {
		m.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	op.Status = status
	op.FinishedAt = &now
	op.Error = message
	if exitCode >= 0 {
		op.ExitCode = &exitCode
	}
	delete(m.cancels, id)
	// The command's outcome is decided; these persistence results are not
	// allowed to change it, but they are also not allowed to be silent: a
	// failed terminal write means a restart recovers this operation as
	// interrupted and the operator loses the record of what happened.
	if err := m.store.saveOperation(op); err != nil {
		log.Printf("[operation] terminal state persist failed for %s: %v", id, err)
	}
	if err := m.appendEventLocked(id, EventStatus, string(status)); err != nil {
		log.Printf("[operation] terminal event persist failed for %s: %v", id, err)
	}
	m.closeSubscribersLocked(id)
	over := m.maxOperations > 0 && len(m.operations) > m.maxOperations
	m.mu.Unlock()
	// The journal handle is no longer needed; release it so long histories
	// do not pin file descriptors. Retention runs asynchronously so a sweep
	// never delays the next queued operation on this target.
	m.store.CloseJournal(id)
	if over {
		go m.retire()
	}
}

func (m *Manager) emit(id string, eventType EventType, data string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.appendEventLocked(id, eventType, data); err != nil {
		log.Printf("[operation] event persist failed for %s (%s): %v", id, eventType, err)
	}
}

// appendEventLocked appends one event to the operation's journal (a single
// append(2), no per-event rewrite or fsync — useteploy__teploy-dash-04), keeps
// the bounded in-memory window, and wakes subscribers. Caller must hold m.mu.
func (m *Manager) appendEventLocked(id string, eventType EventType, data string) error {
	data = boundedEventData(data)
	events := m.events[id]
	var sequence uint64 = 1
	if len(events) > 0 {
		sequence = events[len(events)-1].Sequence + 1
	}
	event := Event{Sequence: sequence, OperationID: id, Type: eventType, Data: data, CreatedAt: time.Now().UTC()}
	if err := m.store.appendEvent(id, event); err != nil {
		return err
	}
	events = append(events, event)
	if len(events) > m.maxEvents {
		events = events[len(events)-m.maxEvents:]
	}
	m.events[id] = events
	for subscriber := range m.subscribers[id] {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
	return nil
}

func (m *Manager) Get(id string) (*Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.operations[id]
	if op == nil {
		return nil, ErrNotFound
	}
	return cloneOperation(op), nil
}

func (m *Manager) List(status Status, target string, limit int) []*Operation {
	m.mu.Lock()
	defer m.mu.Unlock()
	operations := make([]*Operation, 0, len(m.operations))
	for _, op := range m.operations {
		if status != "" && op.Status != status {
			continue
		}
		if target != "" && op.Target != target {
			continue
		}
		operations = append(operations, cloneOperation(op))
	}
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].CreatedAt.Equal(operations[j].CreatedAt) {
			return operations[i].AdmissionSeq > operations[j].AdmissionSeq
		}
		return operations[i].CreatedAt.After(operations[j].CreatedAt)
	})
	if limit > 0 && len(operations) > limit {
		operations = operations[:limit]
	}
	return operations
}

// replayFor returns the events after `sequence` for a client. The in-memory
// window serves recent sequences; older ones fall back to the on-disk journal
// so a reconnecting viewer still gets complete history while memory stays
// bounded. When history older than the oldest retained event is requested, a
// gap event marks the truncation instead of silently returning a shorter
// stream. Caller must hold m.mu.
func (m *Manager) replayLocked(id string, sequence uint64) []Event {
	window := m.events[id]
	first := firstSequence(window)
	if len(window) == 0 || sequence >= first-1 {
		return copyEvents(eventsAfter(window, sequence))
	}
	// Older than the window: read the journal.
	loaded, err := m.store.loadEvents(id)
	if err != nil || len(loaded) == 0 {
		// Journal unavailable: serve the window with a truncation marker.
		out := []Event{{
			Sequence: first - 1, OperationID: id, Type: EventGap,
			Data: fmt.Sprintf("events 1-%d unavailable (journal unreadable)", first-1),
		}}
		return append(out, copyEvents(eventsAfter(window, sequence))...)
	}
	if loadedFirst := firstSequence(loaded); loadedFirst > sequence+1 {
		loaded = append([]Event{{
			Sequence: loadedFirst - 1, OperationID: id, Type: EventGap,
			Data: fmt.Sprintf("events %d-%d removed by retention", sequence+1, loadedFirst-1),
		}}, loaded...)
	}
	return copyEvents(eventsAfter(loaded, sequence))
}

func (m *Manager) EventsAfter(id string, sequence uint64) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.operations[id] == nil {
		return nil, ErrNotFound
	}
	return m.replayLocked(id, sequence), nil
}

func (m *Manager) Subscribe(id string, sequence uint64) ([]Event, <-chan struct{}, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.operations[id]
	if op == nil {
		return nil, nil, false, ErrNotFound
	}
	replay := m.replayLocked(id, sequence)
	if op.Status.Terminal() {
		closed := make(chan struct{})
		close(closed)
		return replay, closed, true, nil
	}
	channel := make(chan struct{}, 1)
	if m.subscribers[id] == nil {
		m.subscribers[id] = make(map[chan struct{}]struct{})
	}
	m.subscribers[id][channel] = struct{}{}
	return replay, channel, false, nil
}

func (m *Manager) Unsubscribe(id string, channel <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for subscriber := range m.subscribers[id] {
		if subscriber == channel {
			delete(m.subscribers[id], subscriber)
			return
		}
	}
}

// Cancel requests cancellation of an operation. The intent is PERSISTED
// before the cancellation is acknowledged (A09): a crash before the worker
// reaches a terminal state leaves a cancel_requested record that recovery
// resolves as canceled, never as queued work to replay. The in-memory cancel
// still fires so a live worker stops promptly; "canceled" as a final status
// is the worker's observation, and already-applied remote changes may remain.
func (m *Manager) Cancel(id string) (*Operation, error) {
	m.mu.Lock()
	op := m.operations[id]
	if op == nil {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	if op.Status.Terminal() {
		m.mu.Unlock()
		return nil, ErrNotCancelable
	}
	next := cloneOperation(op)
	next.Status = StatusCancelRequested
	if err := m.store.saveOperation(next); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("persist cancellation intent: %w", err)
	}
	m.operations[id] = next
	if err := m.appendEventLocked(id, EventStatus, string(StatusCancelRequested)); err != nil {
		log.Printf("[operation] cancel-intent event persist failed for %s: %v", id, err)
	}
	cancel := m.cancels[id]
	copy := cloneOperation(next)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return copy, nil
}

func (m *Manager) Retry(id string) (*Operation, error) {
	m.mu.Lock()
	op := m.operations[id]
	if op == nil {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	if !op.Status.Terminal() || op.Status == StatusSucceeded || op.HasSecrets {
		m.mu.Unlock()
		return nil, ErrNotRetryable
	}
	req := cloneRequest(op.Request)
	attempt := op.Attempt + 1
	m.mu.Unlock()
	retry, _, err := m.enqueue(req, "", id, attempt)
	return retry, err
}

// recover reconciles persisted records with a fresh process. NOTHING is
// automatically replayed (A08): the store's commit is temp+sync+rename with
// a directory sync after the rename, so an enqueue can FAIL after the queued
// record is already visible (dir-open/sync error). The caller was told the
// work was not queued; executing it after a restart would deploy something
// nobody believes was admitted. Queued and running records therefore surface
// as interrupted for an explicit, re-authorized retry; cancel_requested
// records resolve as canceled (A09).
func (m *Manager) recover() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, op := range m.operations {
		switch op.Status {
		case StatusQueued, StatusRunning:
			now := time.Now().UTC()
			op.Status = StatusInterrupted
			op.Error = "dashboard restarted before operation completed; verify remote state and retry"
			op.FinishedAt = &now
			if err := m.store.saveOperation(op); err != nil {
				return err
			}
			if err := m.appendEventLocked(id, EventStatus, string(StatusInterrupted)); err != nil {
				return err
			}
			m.store.CloseJournal(id)
		case StatusCancelRequested:
			now := time.Now().UTC()
			op.Status = StatusCanceled
			op.Error = "canceled before the dashboard restarted"
			op.FinishedAt = &now
			if err := m.store.saveOperation(op); err != nil {
				return err
			}
			if err := m.appendEventLocked(id, EventStatus, string(StatusCanceled)); err != nil {
				return err
			}
			m.store.CloseJournal(id)
		}
	}
	return nil
}

// retire applies retention caps (age + count) to in-memory and on-disk state.
// Non-terminal operations are never removed. Live sweeps only fire when the
// count bound is crossed or an hour has passed since the last one — the sweep
// reads every record file, so it must not run per completion. Age retention
// always applies at startup.
func (m *Manager) retire() {
	if !m.retireMu.TryLock() {
		return
	}
	defer m.retireMu.Unlock()
	m.mu.Lock()
	over := m.maxOperations > 0 && len(m.operations) > m.maxOperations
	m.mu.Unlock()
	if !over && !time.Now().After(m.lastSweep.Add(time.Hour)) {
		return
	}
	m.lastSweep = time.Now()
	removed := m.store.sweepRetention(time.Now().UTC(), m.maxHistoryAge, m.maxOperations)
	if len(removed) == 0 {
		return
	}
	m.mu.Lock()
	for _, id := range removed {
		op := m.operations[id]
		if op == nil || !op.Status.Terminal() {
			continue
		}
		delete(m.operations, id)
		delete(m.events, id)
		if op.IdempotencyKey != "" {
			delete(m.idempotency, op.IdempotencyKey)
		}
	}
	m.mu.Unlock()
	log.Printf("[operation] retention removed %d terminal operation(s)", len(removed))
}

func (m *Manager) closeSubscribersLocked(id string) {
	for subscriber := range m.subscribers[id] {
		close(subscriber)
	}
	delete(m.subscribers, id)
}

func eventsAfter(events []Event, sequence uint64) []Event {
	result := make([]Event, 0, len(events))
	for _, event := range events {
		if event.Sequence > sequence {
			result = append(result, event)
		}
	}
	return result
}

func firstSequence(events []Event) uint64 {
	if len(events) == 0 {
		return 1
	}
	return events[0].Sequence
}

func copyEvents(events []Event) []Event {
	out := make([]Event, len(events))
	copy(out, events)
	return out
}

func requestHash(req Request) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func newID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func cloneOperation(op *Operation) *Operation {
	if op == nil {
		return nil
	}
	copy := *op
	copy.Request = cloneRequest(op.Request)
	return &copy
}

func cloneRequest(req Request) Request {
	copy := req
	if req.Vars != nil {
		copy.Vars = make(map[string]string, len(req.Vars))
		for key, value := range req.Vars {
			copy.Vars[key] = value
		}
	}
	return copy
}
