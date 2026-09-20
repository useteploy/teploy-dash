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

// defaultMaxQueuedPerTarget bounds how many non-terminal operations may be
// admitted for one target before further enqueues are rejected (A12/A27
// remainder). Because same-target execution is strictly FIFO, non-terminal
// count IS queue depth (one running + the rest waiting).
const defaultMaxQueuedPerTarget = 50

// R17: the per-target budget alone does not bound TOTAL admitted work — a
// caller spraying distinct app/target names gets a worker and queue per
// target. These globals bound the whole manager instead: total live
// (non-terminal) operations, and how many CLI subprocesses may execute at
// once regardless of queue depth.
const (
	defaultMaxLiveOperations     = 500
	defaultMaxConcurrentExecutes = 8
)

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
	MaxOperations int
	// MaxQueuedPerTarget bounds the number of non-terminal operations
	// admitted per target before enqueues are rejected with
	// ErrAdmissionBudget (0 = package default, negative disables the bound).
	// Non-terminal operations are never deleted or dropped — they are
	// refused at admission (A12/A27 remainder).
	MaxQueuedPerTarget int
	// MaxLiveOperations bounds TOTAL non-terminal operations across every
	// target (R17; 0 = package default, negative disables).
	MaxLiveOperations int
	// MaxConcurrentExecutions bounds how many operations may execute their
	// CLI subprocess at the same time across all targets (R17; 0 = package
	// default, negative disables).
	MaxConcurrentExecutions int
	Resolver                Resolver
	ProjectResolver         ProjectResolver
	Executor                Executor
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
	// maxQueuedPerTarget is the per-target admission budget (A12/A27).
	maxQueuedPerTarget int
	// maxLive bounds total non-terminal operations (R17); executorSlots
	// bounds simultaneous executions (R17). A nil/nil pair disables the
	// respective bound.
	maxLive       int
	executorSlots chan struct{}
	admissionSeq  uint64
	retireMu      sync.Mutex
	lastSweep     time.Time
	// runners tracks live target-worker goroutines so Shutdown can join
	// them (A39/A47).
	runners sync.WaitGroup
	// shuttingDown marks the force-cancel phase of Shutdown so canceled
	// operations record WHY (bounded shutdown, not a user cancel). Guarded
	// by mu.
	shuttingDown bool
	// admissionClosed is set at the START of Shutdown, under mu, before any
	// wait begins (F022). Enqueue refuses new work once set; because
	// admission and the runners WaitGroup increment (admitToTarget) both
	// happen under m.mu, no Add can race a Wait that already observed zero.
	admissionClosed bool
	// recordErr/eventErr record the latest persistence failure per channel
	// (A39/A47): operation records and event journals are separate files, so
	// one failing must not be masked by the other succeeding. Events are
	// advisory and failures must not change a command's outcome, so they are
	// logged — and surfaced through Health() for readiness reporting.
	// Guarded by mu; each cleared by its own next success.
	recordErr   string
	recordErrAt time.Time
	eventErr    string
	eventErrAt  time.Time
	// droppedEvents counts events lost to append failures per operation
	// (R23), guarded by mu. The count is paid down by an explicit gap
	// marker committed before the next successful append, so a viewer sees
	// the hole instead of a silently shorter history.
	droppedEvents map[string]int
	// maintenanceCtx/maintenanceWG own the periodic retention loop (R25),
	// which applies AGE retention even while the operation count stays
	// below the count cap. Joined at the START of Shutdown so a sweep can
	// never race the closing store.
	maintenanceCtx    context.Context
	maintenanceCancel context.CancelFunc
	maintenanceWG     sync.WaitGroup
}

// noteRecordErrLocked records an operation-record persistence failure for
// Health(). Caller must hold m.mu.
func (m *Manager) noteRecordErrLocked(err error) {
	if err == nil {
		return
	}
	m.recordErr = err.Error()
	m.recordErrAt = time.Now().UTC()
}

// clearRecordErrLocked drops the record-channel degradation marker once a
// record write succeeds again. Caller must hold m.mu.
func (m *Manager) clearRecordErrLocked() {
	m.recordErr = ""
	m.recordErrAt = time.Time{}
}

// noteEventErrLocked records an event-journal persistence failure for
// Health(). Caller must hold m.mu.
func (m *Manager) noteEventErrLocked(err error) {
	if err == nil {
		return
	}
	m.eventErr = err.Error()
	m.eventErrAt = time.Now().UTC()
}

// clearEventErrLocked drops the event-channel degradation marker once an
// event append succeeds again. Caller must hold m.mu.
func (m *Manager) clearEventErrLocked() {
	m.eventErr = ""
	m.eventErrAt = time.Time{}
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
	if options.MaxQueuedPerTarget == 0 {
		options.MaxQueuedPerTarget = defaultMaxQueuedPerTarget
	}
	if options.MaxLiveOperations == 0 {
		options.MaxLiveOperations = defaultMaxLiveOperations
	}
	if options.MaxConcurrentExecutions == 0 {
		options.MaxConcurrentExecutions = defaultMaxConcurrentExecutes
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
		store:              store,
		operations:         operations,
		events:             make(map[string][]Event),
		idempotency:        make(map[string]string),
		cancels:            make(map[string]context.CancelFunc),
		targets:            make(map[string]*targetRunner),
		subscribers:        make(map[string]map[chan struct{}]struct{}),
		droppedEvents:      make(map[string]int),
		resolver:           options.Resolver,
		projectResolver:    options.ProjectResolver,
		executor:           options.Executor,
		maxEvents:          options.MaxEvents,
		maxHistoryAge:      options.MaxHistoryAge,
		maxOperations:      options.MaxOperations,
		maxQueuedPerTarget: options.MaxQueuedPerTarget,
		maxLive:            options.MaxLiveOperations,
	}
	if options.MaxConcurrentExecutions > 0 {
		m.executorSlots = make(chan struct{}, options.MaxConcurrentExecutions)
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
	// R25: age retention runs on a lifecycle-owned hourly loop, not only at
	// startup and on count-cap crossings — a low-volume process that stays
	// up retained expired history indefinitely while the hourly eligibility
	// check in retire() sat unreachable.
	m.maintenanceCtx, m.maintenanceCancel = context.WithCancel(context.Background())
	m.maintenanceWG.Add(1)
	go m.retentionLoop(m.maintenanceCtx)
	return m, nil
}

// retentionLoop applies retention periodically until the manager shuts
// down. retire() itself bounds sweeps (hourly + count-driven) and never
// removes non-terminal operations.
func (m *Manager) retentionLoop(ctx context.Context) {
	defer m.maintenanceWG.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.retire()
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) Enqueue(req Request, idempotencyKey string, actor *Actor) (*Operation, bool, error) {
	return m.enqueue(req, idempotencyKey, "", 1, actor)
}

func (m *Manager) enqueue(req Request, idempotencyKey, retryOf string, attempt int, actor *Actor) (*Operation, bool, error) {
	if len(idempotencyKey) > 255 || strings.ContainsAny(idempotencyKey, "\r\n") {
		return nil, false, fmt.Errorf("invalid idempotency key")
	}
	if req.Kind == KindDeploy && req.Mode == "" {
		req.Mode = "ad-hoc"
	}
	hash, err := requestHash(req)
	if err != nil {
		return nil, false, err
	}

	// R21: the idempotency lookup runs BEFORE Build. A replay of an already
	// admitted request must succeed even when the dependencies Build would
	// consult (server discovery, manifest resolution, capability probes)
	// are currently failing — recovery after a lost HTTP response was the
	// point of the key. Build still runs (unlocked, potentially slow) for
	// genuinely new work, and the replay check runs AGAIN inside the
	// admission transaction below because a concurrent caller may have
	// admitted the same key while Build ran.
	m.mu.Lock()
	if m.admissionClosed {
		m.mu.Unlock()
		return nil, false, ErrShuttingDown
	}
	if existing, replayed, err := m.lookupReplayLocked(idempotencyKey, hash); replayed || err != nil {
		m.mu.Unlock()
		return existing, replayed, err
	}
	m.mu.Unlock()

	command, admitted, target, err := Build(req, m.resolver, m.projectResolver)
	if err != nil {
		return nil, false, err
	}

	m.mu.Lock()
	// F022: admission is sealed under the same mutex that guards worker
	// accounting — Shutdown sets this before any wait, so work can never be
	// admitted after the process began joining (or touch stores during
	// closure).
	if m.admissionClosed {
		m.mu.Unlock()
		return nil, false, ErrShuttingDown
	}
	if existing, replayed, err := m.lookupReplayLocked(idempotencyKey, hash); replayed || err != nil {
		m.mu.Unlock()
		return existing, replayed, err
	}
	// Admission budget (A12/A27 remainder): same-target execution is FIFO,
	// so the count of non-terminal operations for the target is exactly its
	// queue depth. Reject (never drop silently) once it is at the cap —
	// an idempotent replay of an already-queued request still passes above.
	if m.maxQueuedPerTarget > 0 {
		depth := 0
		for _, o := range m.operations {
			if o.Target == target && !o.Status.Terminal() {
				depth++
			}
		}
		if depth >= m.maxQueuedPerTarget {
			m.mu.Unlock()
			return nil, false, ErrAdmissionBudget
		}
	}
	// R17: the global live bound caps total admitted (non-terminal) work
	// across every target, so spraying distinct targets cannot admit
	// unbounded queues and subprocess state.
	if m.maxLive > 0 {
		live := 0
		for _, o := range m.operations {
			if !o.Status.Terminal() {
				live++
			}
		}
		if live >= m.maxLive {
			m.mu.Unlock()
			return nil, false, ErrGlobalAdmissionBudget
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
		Actor:          cloneActor(actor),
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
		m.noteEventErrLocked(err)
		m.mu.Unlock()
		return nil, false, fmt.Errorf("persist initial event: %w", err)
	}
	if err := m.store.saveOperation(op); err != nil {
		m.noteRecordErrLocked(err)
		// F019: the record is the commit point and it did not commit — the
		// initial journal entry is a definite orphan (recovery enumerates
		// records, never event files). Drop the cached append handle and the
		// file so repeated storage failures leak neither descriptors nor
		// history files invisible to retention.
		m.store.discardJournal(id)
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
// lookupReplayLocked returns the stored operation for an idempotency key
// when it matches the submitted request hash (R21). A key held by a
// DIFFERENT request surfaces ErrIdempotencyConflict. Caller must hold m.mu.
func (m *Manager) lookupReplayLocked(idempotencyKey, hash string) (*Operation, bool, error) {
	if idempotencyKey == "" {
		return nil, false, nil
	}
	id, ok := m.idempotency[idempotencyKey]
	if !ok {
		return nil, false, nil
	}
	existing := m.operations[id]
	if existing == nil {
		// Stale index entry (the record was retired); drop it so the key
		// can be reused cleanly.
		delete(m.idempotency, idempotencyKey)
		return nil, false, nil
	}
	if existing.requestHash != hash {
		return nil, false, ErrIdempotencyConflict
	}
	return cloneOperation(existing), true, nil
}

// ErrGlobalAdmissionBudget reports that the manager-wide live-operation
// bound is exhausted (R17) — distinct from the per-target budget, but the
// same back-off contract for callers.
var ErrGlobalAdmissionBudget = errors.New("admission budget exceeded — too many operations already queued; retry after some complete")

func (m *Manager) admitToTarget(target string, j job) {
	runner := m.targets[target]
	if runner == nil {
		runner = &targetRunner{}
		m.targets[target] = runner
	}
	runner.queue = append(runner.queue, j)
	if !runner.busy {
		runner.busy = true
		m.runners.Add(1)
		go m.runTarget(target)
	}
}

// runTarget executes its target's jobs in admission order, one at a time,
// until the queue drains.
func (m *Manager) runTarget(target string) {
	defer m.runners.Done()
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
		m.finish(j.id, StatusCanceled, -1, m.cancelReason())
		return
	}
	// R17: execution capacity is a global bound. The slot is acquired
	// WITHOUT the manager mutex; a canceled or shutdown-bound operation
	// waiting for a slot resolves immediately instead of queueing forever.
	// Shutdown's force-cancel path cancels j.ctx, which unblocks the wait.
	if m.executorSlots != nil {
		select {
		case m.executorSlots <- struct{}{}:
			defer func() { <-m.executorSlots }()
		case <-j.ctx.Done():
			m.finish(j.id, StatusCanceled, -1, m.cancelReason())
			return
		}
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
		m.finish(j.id, StatusCanceled, exitCode, m.cancelReason())
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

// cancelReason distinguishes a user-requested cancellation from the
// force-cancel of a bounded shutdown, so the terminal record says which.
func (m *Manager) cancelReason() string {
	m.mu.Lock()
	shutdown := m.shuttingDown
	m.mu.Unlock()
	if shutdown {
		return "canceled: dashboard shutting down"
	}
	return "operation canceled"
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
		m.noteRecordErrLocked(err)
		return fmt.Errorf("persist running state: %w", err)
	}
	m.clearRecordErrLocked()
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
		m.noteRecordErrLocked(err)
		log.Printf("[operation] terminal state persist failed for %s: %v", id, err)
	} else {
		m.clearRecordErrLocked()
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
//
// R23: a failed append loses output WITHOUT consuming a sequence, so the
// next successful append used to produce an apparently contiguous history.
// The loss is now accounted per operation and paid down with an explicit
// gap marker committed before the next retained event; the dropped count
// survives until its marker persists. (A restart still forgets unpaid debt —
// the failed events never entered the journal, so there is no hole to find
// after recovery; recorded as a residual.)
func (m *Manager) appendEventLocked(id string, eventType EventType, data string) error {
	if lost := m.droppedEvents[id]; lost > 0 {
		delete(m.droppedEvents, id)
		if err := m.appendOneEventLocked(id, EventGap, fmt.Sprintf("%d event(s) were lost during a storage failure", lost)); err != nil {
			// The marker itself failed and this event is not attempted:
			// both the old debt and the current event are lost. Carry the
			// combined count so a later append retries the marker.
			if eventType != EventGap {
				lost++
			}
			m.droppedEvents[id] = lost
			return err
		}
	}
	return m.appendOneEventLocked(id, eventType, data)
}

func (m *Manager) appendOneEventLocked(id string, eventType EventType, data string) error {
	data = boundedEventData(data)
	events := m.events[id]
	var sequence uint64 = 1
	if len(events) > 0 {
		sequence = events[len(events)-1].Sequence + 1
	}
	event := Event{Sequence: sequence, OperationID: id, Type: eventType, Data: data, CreatedAt: time.Now().UTC()}
	if err := m.store.appendEvent(id, event); err != nil {
		m.noteEventErrLocked(err)
		if eventType != EventGap {
			m.droppedEvents[id]++
		}
		return err
	}
	m.clearEventErrLocked()
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

// Health is the readiness projection of the operation service (A39/A47):
// whether persistence has degraded (advisory events or terminal records are
// failing to write; commands still run), whether the journal directories are
// writable, and how many operations are live.
type Health struct {
	PersistDegraded bool      `json:"persist_degraded"`
	RecordError     string    `json:"record_error,omitempty"`
	RecordErrorAt   time.Time `json:"record_error_at,omitempty"`
	EventError      string    `json:"event_error,omitempty"`
	EventErrorAt    time.Time `json:"event_error_at,omitempty"`
	JournalError    string    `json:"journal_error,omitempty"`
	LiveOperations  int       `json:"live_operations"`
}

func (m *Manager) Health() Health {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := Health{
		PersistDegraded: m.recordErr != "" || m.eventErr != "",
		RecordError:     m.recordErr,
		RecordErrorAt:   m.recordErrAt,
		EventError:      m.eventErr,
		EventErrorAt:    m.eventErrAt,
	}
	for _, op := range m.operations {
		if !op.Status.Terminal() {
			h.LiveOperations++
		}
	}
	if err := m.store.Health(); err != nil {
		h.JournalError = err.Error()
	}
	return h
}

// Shutdown joins in-flight target work, bounded by ctx (A39/A47): a deploy
// can legitimately run for minutes, so shutdown first waits for the drain
// until ctx expires, then force-cancels everything still live (queued work
// resolves as canceled with an explicit shutdown message — strictly better
// than the hard exit it replaces, which killed children mid-write and left
// records for recovery to mark interrupted) and waits a fixed grace for the
// terminal states to persist. Callers stop the HTTP server first so no new
// work is admitted while draining.
//
// F022: admission is closed FIRST, synchronously and under the manager
// mutex, before any waiting begins. Enqueue checks the same flag under the
// same mutex, and admitToTarget's runners.Add also runs under it — so once
// Shutdown returns from setting the flag, no new worker can appear behind
// the WaitGroup wait below (the pre-fix race could Add after Wait observed
// zero, stranding admitted work against the closing store).
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.admissionClosed = true
	m.mu.Unlock()

	// R25: join the retention loop FIRST — an in-flight sweep must finish
	// (or never start) before anything below closes storage out from under
	// it. Cancel + Wait covers both: the loop exits on ctx.Done, and Wait
	// blocks until any in-flight retire() returns.
	if m.maintenanceCancel != nil {
		m.maintenanceCancel()
		m.maintenanceWG.Wait()
	}

	drained := make(chan struct{})
	go func() {
		m.runners.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return
	case <-ctx.Done():
	}
	m.mu.Lock()
	m.shuttingDown = true
	cancels := make([]context.CancelFunc, 0, len(m.cancels))
	for _, cancel := range m.cancels {
		cancels = append(cancels, cancel)
	}
	live := 0
	for _, op := range m.operations {
		if !op.Status.Terminal() {
			live++
		}
	}
	m.mu.Unlock()
	if live > 0 {
		log.Printf("[operation] shutdown: %d operation(s) still live after drain deadline; force-canceling", live)
	}
	for _, cancel := range cancels {
		cancel()
	}
	grace := time.NewTimer(5 * time.Second)
	defer grace.Stop()
	select {
	case <-drained:
	case <-grace.C:
		log.Printf("[operation] shutdown: workers did not exit after force-cancel; continuing shutdown anyway")
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
		m.noteRecordErrLocked(err)
		m.mu.Unlock()
		return nil, fmt.Errorf("persist cancellation intent: %w", err)
	}
	m.clearRecordErrLocked()
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

// Retry re-admits a failed/canceled/interrupted operation. The retry is
// attributed to the actor that re-authorized it (not the original enqueuer) —
// RetryOf preserves the lineage.
func (m *Manager) Retry(id string, actor *Actor) (*Operation, error) {
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
	retry, _, err := m.enqueue(req, "", id, attempt, actor)
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
			// R24: the event journal is ADVISORY. A failure to append the
			// interrupted marker must not disable the entire operation
			// service — the authoritative record above already committed.
			// The degradation is visible through Health() and the next
			// append retries with a gap marker (R23).
			if err := m.appendEventLocked(id, EventStatus, string(StatusInterrupted)); err != nil {
				m.noteEventErrLocked(err)
				log.Printf("[operation] recovery journal unavailable for %s: %v", id, err)
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
				m.noteEventErrLocked(err)
				log.Printf("[operation] recovery journal unavailable for %s: %v", id, err)
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
	// R27: every pointer field is deep-copied so a returned snapshot shares
	// nothing mutable with manager-owned state — an in-process caller
	// mutating a Get/List/Enqueue result (or the Actor pointer it passed to
	// Enqueue) can no longer reach the live operation.
	copy.Actor = cloneActor(op.Actor)
	if op.AdmittedServer != nil {
		snapshot := *op.AdmittedServer
		copy.AdmittedServer = &snapshot
	}
	if op.StartedAt != nil {
		t := *op.StartedAt
		copy.StartedAt = &t
	}
	if op.FinishedAt != nil {
		t := *op.FinishedAt
		copy.FinishedAt = &t
	}
	if op.ExitCode != nil {
		c := *op.ExitCode
		copy.ExitCode = &c
	}
	return &copy
}

func cloneActor(actor *Actor) *Actor {
	if actor == nil {
		return nil
	}
	snapshot := *actor
	return &snapshot
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
