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

// receiptReadTimeout bounds one receipt read (D02): the same class of machine
// read the fleet uses (`teploy app list`), given headroom over the fleet's
// 10s per-server probe. Reconciliation must never hang a cancel resolution
// or the startup reconcile loop.
const receiptReadTimeout = 15 * time.Second

// defaultIdempotencyWindow bounds how long an admitted idempotency key is
// honored (D02): replays and conflicts resolve against the admitted
// operation while it is within this window of its ADMISSION time; after that
// the key is reusable for new work. The window bounds the in-memory index
// AND the restart-time restore — without it the key namespace grows for as
// long as the retained operation history (30d by default), which is the
// unbounded-growth half of the pre-namespacing defect.
const defaultIdempotencyWindow = 24 * time.Hour

// idemKey is the namespaced idempotency index key: one principal's key
// never aliases another's (D02). A struct (not string concatenation) so no
// separator character in a principal or key can forge a cross-namespace
// collision.
type idemKey struct {
	principal string
	key       string
}

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
	// IdempotencyWindow bounds how long an admitted idempotency key is
	// honored, measured from the admitted operation's creation time (D02
	// namespacing). 0 = package default (24h); negative disables expiry.
	// Replays past the window are new operations; the restart-time restore
	// skips keys whose operations are older than the window.
	IdempotencyWindow time.Duration
	Resolver          Resolver
	// ResolverByID resolves a server by stable id for the rename
	// reconciliation in checkTarget (X02 §1.3). Nil disables that path:
	// a vanished name stays a hard refusal, exactly as before.
	ResolverByID      ResolverByID
	ProjectResolver   ProjectResolver
	Executor          Executor
	// ReceiptReader answers whether an admitted operation's effect reached
	// its target (D02). Reconciliation after a restart, and the honest
	// resolution of a mid-flight cancellation, both consult it. Nil means
	// this deployment cannot reconcile: interrupted outcomes are labeled
	// needs-manual-check with that exact reason instead of guessing.
	ReceiptReader ReceiptReader
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
	idempotency     map[idemKey]string
	cancels         map[string]context.CancelFunc
	targets         map[string]*targetRunner
	subscribers     map[string]map[chan struct{}]struct{}
	resolver        Resolver
	resolverByID    ResolverByID
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
	// idempotencyWindow bounds how long a key is honored (D02); <= 0 rules:
	// 0 was defaulted in New, negative disables expiry.
	idempotencyWindow time.Duration
	retireMu          sync.Mutex
	lastSweep         time.Time
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
	// reconcileCtx/reconcileWG own the startup reconciliation loop (D02):
	// interrupted operations whose receipts still need reading. Joined at
	// the START of Shutdown, bounded per read by receiptReadTimeout.
	reconcileCtx    context.Context
	reconcileCancel context.CancelFunc
	reconcileWG     sync.WaitGroup
	// receiptReader is Options.ReceiptReader (nil = cannot reconcile).
	receiptReader ReceiptReader
	// readOnly is non-empty when stored records carry a schema version
	// newer than this build supports (X02 §5 row 6): reads work, every
	// mutation refuses with this reason. Set once at New; never cleared.
	readOnly string
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
	if options.IdempotencyWindow == 0 {
		options.IdempotencyWindow = defaultIdempotencyWindow
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
	operations, future, err := store.loadOperations()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		store:              store,
		operations:         operations,
		events:             make(map[string][]Event),
		idempotency:        make(map[idemKey]string),
		cancels:            make(map[string]context.CancelFunc),
		targets:            make(map[string]*targetRunner),
		subscribers:        make(map[string]map[chan struct{}]struct{}),
		droppedEvents:      make(map[string]int),
		resolver:           options.Resolver,
		resolverByID:       options.ResolverByID,
		projectResolver:    options.ProjectResolver,
		executor:           options.Executor,
		receiptReader:      options.ReceiptReader,
		maxEvents:          options.MaxEvents,
		maxHistoryAge:      options.MaxHistoryAge,
		maxOperations:      options.MaxOperations,
		maxQueuedPerTarget: options.MaxQueuedPerTarget,
		maxLive:            options.MaxLiveOperations,
		idempotencyWindow:  options.IdempotencyWindow,
	}
	if len(future) > 0 {
		// X02 §5 row 6 refuse-downgrade: records written by a newer
		// teploy-dash stay readable, but executing or rewriting them with
		// an older reader could corrupt them — mutations are refused with
		// the upgrade remedy, reads keep working.
		var maxV int
		for _, f := range future {
			if f.Version > maxV {
				maxV = f.Version
			}
		}
		m.readOnly = fmt.Sprintf("%d operation record(s) were written by a newer teploy-dash (highest schema version %d, e.g. %s; this build supports %d); mutations are refused until teploy-dash is upgraded — history remains readable", len(future), maxV, future[0].File, currentRecordVersion)
		log.Printf("[operation] READ-ONLY: %s", m.readOnly)
	}
	if options.MaxConcurrentExecutions > 0 {
		m.executorSlots = make(chan struct{}, options.MaxConcurrentExecutions)
	}
	// One clock reading for the whole restore so window comparisons are
	// stable across the load loop.
	loadTime := time.Now().UTC()
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
		// D02 namespacing: restore the idempotency index from the persisted
		// records (key + principal), bounded by the window — an expired key
		// is not resurrected by a restart.
		if op.IdempotencyKey != "" && m.idempotencyLive(op.CreatedAt, loadTime) {
			m.idempotency[idemKey{op.IdempotencyPrincipal, op.IdempotencyKey}] = id
		}
	}
	pending, err := m.recover()
	if err != nil {
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
	// D02: reconcile interrupted outcomes against the target's receipts in
	// the background — New must not block on CLI reads, and nothing is
	// retryable until each answer lands.
	if len(pending) > 0 {
		log.Printf("[operation] reconciling %d interrupted operation(s) against target receipts", len(pending))
		m.reconcileCtx, m.reconcileCancel = context.WithCancel(context.Background())
		m.reconcileWG.Add(1)
		go m.reconcileLoop(pending)
	}
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

// idempotencyLive reports whether an admitted operation is still within the
// idempotency window (D02). A non-positive window disables expiry.
func (m *Manager) idempotencyLive(createdAt, now time.Time) bool {
	if m.idempotencyWindow <= 0 {
		return true
	}
	return now.Sub(createdAt) <= m.idempotencyWindow
}

func (m *Manager) Enqueue(req Request, idempotencyKey string, actor *Actor) (*Operation, bool, error) {
	return m.enqueue(req, idempotencyKey, "", 1, actor)
}

func (m *Manager) enqueue(req Request, idempotencyKey, retryOf string, attempt int, actor *Actor) (*Operation, bool, error) {
	if m.readOnly != "" {
		return nil, false, ErrReadOnly
	}
	if len(idempotencyKey) > 255 || strings.ContainsAny(idempotencyKey, "\r\n") {
		return nil, false, fmt.Errorf("invalid idempotency key")
	}
	// D02 namespacing: the key resolves within the admitting principal's
	// namespace only — different principals using the same client key are
	// independent operations, never each other's replays.
	scope := IdempotencyScope(actor)
	namespaced := idemKey{principal: scope, key: idempotencyKey}
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
	if existing, replayed, err := m.lookupReplayLocked(namespaced, hash, time.Now().UTC()); replayed || err != nil {
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
	if existing, replayed, err := m.lookupReplayLocked(namespaced, hash, time.Now().UTC()); replayed || err != nil {
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
		// D02: the key's namespace is persisted with it so a restart
		// restores dedupe within the SAME principal's namespace.
		IdempotencyPrincipal: scope,
		RetryOf:              retryOf,
		Attempt:              attempt,
		CreatedAt:            now,
		HasSecrets:           len(command.Secrets) > 0,
		AdmittedServer:       &snapshot,
		AdmissionSeq:         m.admissionSeq,
		Actor:                cloneActor(actor),
		requestHash:          hash,
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
		m.idempotency[namespaced] = id
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
// lookupReplayLocked returns the stored operation for one principal's
// idempotency key when it matches the submitted request hash (R21). A key
// held by a DIFFERENT request surfaces ErrIdempotencyConflict. A key whose
// admitted operation is past the idempotency window is expired: the entry is
// dropped and the key is reusable for new work (D02). Caller must hold m.mu.
func (m *Manager) lookupReplayLocked(namespaced idemKey, hash string, now time.Time) (*Operation, bool, error) {
	if namespaced.key == "" {
		return nil, false, nil
	}
	id, ok := m.idempotency[namespaced]
	if !ok {
		return nil, false, nil
	}
	existing := m.operations[id]
	if existing == nil {
		// Stale index entry (the record was retired); drop it so the key
		// can be reused cleanly.
		delete(m.idempotency, namespaced)
		return nil, false, nil
	}
	if !m.idempotencyLive(existing.CreatedAt, now) {
		// Past the window: the principal may reuse the key for new work.
		delete(m.idempotency, namespaced)
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
	// Target identity check (A14, extended by X02 §1.3 stable ids): the
	// server must still be the one admitted with the operation. A repointed
	// alias, a same-name different server, or a vanished stable id fails the
	// operation for re-authorization; a VERIFIED rename follows the server's
	// identity and retargets the command to its current name. Records from
	// before AdmittedServer existed (or with no resolver) skip the check.
	if mismatch := m.checkTarget(op, &j); mismatch != "" {
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
		// D02: the command was canceled mid-flight — whether its effect
		// landed is a question, not an assumption. Persist the stopping
		// transition, read the receipt, and record the honest terminal
		// outcome (canceled | already_committed | canceled-unverified).
		m.resolveCancellation(j.id, exitCode)
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
// verified (or the check does not apply); on a verified rename the job's
// command is retargeted to the server's current name in place. Every other
// mismatch returns the refusal the operation fails with.
func (m *Manager) checkTarget(op *Operation, j *job) string {
	if op.AdmittedServer == nil || m.resolver == nil {
		return ""
	}
	admitted := *op.AdmittedServer
	current, err := m.resolver(op.Request.Server)
	if err != nil {
		// The admitted name no longer resolves. With a recorded stable id a
		// rename is safe and expected (X02 §1.3): when the id still names a
		// server with the admitted host/user, queued work follows the
		// identity and executes against the server's CURRENT name. No id
		// (legacy record) or no resolverByID: the name is all dash has, so
		// the historical refusal stands.
		if admitted.ID != "" && m.resolverByID != nil {
			renamed, ok := m.resolverByID(admitted.ID)
			if ok {
				if renamed.Host != admitted.Host || userOf(renamed) != userOf(admitted) {
					return fmt.Sprintf("server %q was renamed and repointed since admission (admitted %s, now %s); explicit re-authorization required — retry the operation", op.Request.Server, admitted.Host, renamed.Host)
				}
				j.command = retargetCommand(j.command, op.Request.Server, renamed.Name)
				return ""
			}
			return fmt.Sprintf("server %q could not be resolved since admission (%v) and its admitted id %s matches no configured server; verify the server configuration and retry", op.Request.Server, err, admitted.ID)
		}
		return fmt.Sprintf("server %q could not be resolved since admission (%v); verify the server configuration and retry", op.Request.Server, err)
	}
	if admitted.ID != "" {
		switch {
		case current.ID == "":
			// The downgrade hazard (X02 §5): an older CLI rewrote
			// servers.yml without ids. Ambiguous — never a new server.
			return fmt.Sprintf("server %q no longer carries its admitted stable id %s (an older teploy CLI may have rewritten servers.yml); binding is ambiguous — restore the id or re-try against an explicit server", op.Request.Server, admitted.ID)
		case current.ID != admitted.ID:
			// The same name now names a different server: the retarget
			// hazard A14 exists to prevent.
			return fmt.Sprintf("server %q now resolves to a different server (admitted id %s, now %s); explicit re-authorization required — retry the operation", op.Request.Server, admitted.ID, current.ID)
		}
	}
	if current.Host != admitted.Host || userOf(current) != userOf(admitted) {
		return fmt.Sprintf("server %q was repointed since admission (admitted %s, now %s); explicit re-authorization required — retry the operation", op.Request.Server, admitted.Host, current.Host)
	}
	return ""
}

// retargetCommand rewrites the admitted server NAME in a built command to
// the server's current name after a verified rename. Only the two
// name-addressed command shapes carry the name — deploy's positional
// server argument and the template --server flag; every other shape
// addresses the host, which a rename does not change. The structural
// guards (deploy at argv[0], --server immediately before) keep an app or
// template name that happens to equal the server name from being touched,
// and secret values never appear in Args (A11).
func retargetCommand(cmd Command, from, to string) Command {
	if from == "" || from == to {
		return cmd
	}
	replaced := append([]string(nil), cmd.Args...)
	for i, arg := range replaced {
		if arg != from {
			continue
		}
		if (i == 1 && len(replaced) > 0 && replaced[0] == "deploy") ||
			(i > 0 && replaced[i-1] == "--server") {
			replaced[i] = to
		}
	}
	cmd.Args = replaced
	return cmd
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

// beginStopping persists the stopping transition (D02: requested ->
// stopping/reconciling -> canceled | already_committed) and appends its
// event. Reports false when the operation is already terminal or already
// stopping (resolution owns it from here).
func (m *Manager) beginStopping(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.operations[id]
	if op == nil || op.Status.Terminal() || op.Status == StatusStopping {
		return false
	}
	next := cloneOperation(op)
	next.Status = StatusStopping
	if err := m.store.saveOperation(next); err != nil {
		m.noteRecordErrLocked(err)
		// The command's outcome is already decided; the persist failure is
		// surfaced (Health/log) but must not stop the resolution — the same
		// rule finish() applies to terminal writes.
		log.Printf("[operation] stopping state persist failed for %s: %v", id, err)
	} else {
		m.clearRecordErrLocked()
	}
	m.operations[id] = next
	if err := m.appendEventLocked(id, EventStatus, string(StatusStopping)); err != nil {
		log.Printf("[operation] stopping event persist failed for %s: %v", id, err)
	}
	return true
}

// resolveCancellation determines the honest outcome of a canceled mid-flight
// command (D02). "Interrupted" describes the coordinator, never the target:
// the receipt decides between canceled (effect did not land),
// already_committed (effect landed — reported as standing, never as rolled
// back), and canceled-with-unverified-outcome when no receipt can answer.
// During bounded shutdown the receipt read is skipped (shutdown must stay
// fast); the record then says the outcome is unknown so an operator checks.
func (m *Manager) resolveCancellation(id string, exitCode int) {
	if !m.beginStopping(id) {
		return
	}
	reason := m.cancelReason()
	m.mu.Lock()
	shutdown := m.shuttingDown
	reader := m.receiptReader
	op := cloneOperation(m.operations[id])
	m.mu.Unlock()

	var (
		receipt Receipt
		rerr    error
	)
	switch {
	case shutdown:
		rerr = errors.New("receipt check skipped: dashboard shutting down")
	default:
		receipt, rerr = m.readReceipt(nil, reader, op)
	}

	rec := &Reconciliation{CheckedAt: time.Now().UTC()}
	status := StatusCanceled
	message := reason
	switch {
	case rerr != nil:
		// No answer: the cancellation is real (dash stopped waiting), but
		// the effect's fate is unknown — say exactly that.
		rec.State = ReconcileStateManual
		rec.Reason = rerr.Error()
		message = reason + "; outcome could not be verified: " + rerr.Error()
	case receipt.Applied:
		status = StatusAlreadyCommitted
		rec.State = ReconcileStateApplied
		rec.Evidence = receipt.Evidence
		message = reason + " after the effect committed — the effect stood; no rollback was performed"
	default:
		rec.State = ReconcileStateNotApplied
		rec.Evidence = receipt.Evidence
		message = reason + "; receipt confirmed the effect did not land"
	}
	m.finishReconciled(id, status, exitCode, message, rec)
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
	m.finishReconciled(id, status, exitCode, message, nil)
}

// finishReconciled is finish with an optional receipt record (D02): the
// reconciliation is persisted ON the terminal record (evidence and reason
// ride the commit that decides the outcome), so the API serves one honest
// answer, not a status plus a separate guess.
func (m *Manager) finishReconciled(id string, status Status, exitCode int, message string, rec *Reconciliation) {
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
	if rec != nil {
		op.Reconciliation = rec
	}
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
	// ReadOnly is non-empty when stored records carry a schema version
	// newer than this build supports (X02 §5 row 6): reads work, mutations
	// refuse with this reason. Surfaced through /api/health as the banner.
	ReadOnly string `json:"read_only,omitempty"`
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
		ReadOnly:        m.readOnly,
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
	// blocks until any in-flight retire() returns. The D02 reconcile loop
	// joins the same way: its in-flight receipt read is bounded by
	// receiptReadTimeout, and unreconciled records stay reconciling on disk
	// for the next boot to pick back up.
	if m.maintenanceCancel != nil {
		m.maintenanceCancel()
		m.maintenanceWG.Wait()
	}
	if m.reconcileCancel != nil {
		m.reconcileCancel()
		m.reconcileWG.Wait()
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
	if m.readOnly != "" {
		return nil, ErrReadOnly
	}
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
	if op.Status == StatusStopping {
		// D02: the cancel intent is already recorded and the outcome is
		// being resolved — there is nothing left to request.
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
	if m.readOnly != "" {
		return nil, ErrReadOnly
	}
	m.mu.Lock()
	op := m.operations[id]
	if op == nil {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	if !op.Status.Terminal() || op.Status == StatusSucceeded || op.Status == StatusAlreadyCommitted || op.HasSecrets {
		m.mu.Unlock()
		return nil, ErrNotRetryable
	}
	// D02: an interrupted outcome stays un-offered until its receipt is
	// reconciled — retrying work whose effect may already stand is the
	// blind re-run this gate exists to prevent. Records from before D02
	// (nil Reconciliation) keep the old explicit-retry contract.
	if op.Status == StatusInterrupted && op.Reconciliation != nil && op.Reconciliation.State == ReconcileStateReconciling {
		m.mu.Unlock()
		return nil, ErrReconciliationPending
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
// nobody believes was admitted. Queued, running, and stopping records
// therefore surface as interrupted — outcome UNKNOWN (D02): "interrupted"
// describes the coordinator, not the target, so the record carries a
// Reconciliation the startup loop resolves from receipts before retry is
// offered again. cancel_requested records resolve as canceled (A09).
//
// The returned ids are the operations whose receipts still need reading this
// boot (D02): freshly interrupted ones plus leftovers from a previous boot
// that died mid-reconciliation.
func (m *Manager) recover() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var pending []string
	for id, op := range m.operations {
		switch op.Status {
		case StatusQueued, StatusRunning, StatusStopping:
			now := time.Now().UTC()
			op.Status = StatusInterrupted
			op.Error = "interrupted — outcome unknown: dashboard restarted before the operation completed"
			op.FinishedAt = &now
			if m.receiptReader == nil {
				// No reader means no honest answer is possible; say so
				// instead of leaving an eternal "reconciling" lie.
				op.Reconciliation = &Reconciliation{
					State:  ReconcileStateManual,
					Reason: "no receipt reader configured; verify the server state manually",
				}
			} else {
				op.Reconciliation = &Reconciliation{State: ReconcileStateReconciling}
				pending = append(pending, id)
			}
			if err := m.store.saveOperation(op); err != nil {
				return nil, err
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
				return nil, err
			}
			if err := m.appendEventLocked(id, EventStatus, string(StatusCanceled)); err != nil {
				m.noteEventErrLocked(err)
				log.Printf("[operation] recovery journal unavailable for %s: %v", id, err)
			}
			m.store.CloseJournal(id)
		case StatusInterrupted:
			// A previous boot marked this interrupted and died before its
			// receipt landed — pick the reconciliation back up.
			if m.receiptReader != nil && op.Reconciliation != nil && op.Reconciliation.State == ReconcileStateReconciling {
				pending = append(pending, id)
			}
		}
	}
	return pending, nil
}

// reconcileLoop resolves each pending interrupted operation against its
// target's receipts, sequentially and bounded (D02). It never enqueues
// anything: reconciliation answers a question, it does not re-run work —
// retry stays an explicit re-authorization.
func (m *Manager) reconcileLoop(ids []string) {
	defer m.reconcileWG.Done()
	for _, id := range ids {
		select {
		case <-m.reconcileCtx.Done():
			return
		default:
		}
		m.reconcileOne(id)
	}
}

// reconcileOne reads one operation's receipt and records the honest answer.
// The reader runs WITHOUT the manager mutex (it shells out); the answer is
// applied only if the operation is still interrupted-and-reconciling when it
// lands.
func (m *Manager) reconcileOne(id string) {
	m.mu.Lock()
	op := m.operations[id]
	if op == nil || op.Status != StatusInterrupted || op.Reconciliation == nil || op.Reconciliation.State != ReconcileStateReconciling {
		m.mu.Unlock()
		return
	}
	reader := m.receiptReader
	snapshot := cloneOperation(op)
	m.mu.Unlock()

	receipt, rerr := m.readReceipt(m.reconcileCtx, reader, snapshot)

	// A shutdown-canceled read is NOT an answer — leave the record
	// reconciling on disk so the next boot picks the question back up,
	// instead of recording "context canceled" as a manual-check reason.
	if m.reconcileCtx.Err() != nil {
		return
	}

	rec := &Reconciliation{CheckedAt: time.Now().UTC()}
	switch {
	case rerr != nil:
		rec.State = ReconcileStateManual
		rec.Reason = rerr.Error()
	case receipt.Applied:
		rec.State = ReconcileStateApplied
		rec.Evidence = receipt.Evidence
	default:
		rec.State = ReconcileStateNotApplied
		rec.Evidence = receipt.Evidence
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.operations[id]
	if current == nil || current.Status != StatusInterrupted || current.Reconciliation == nil || current.Reconciliation.State != ReconcileStateReconciling {
		return
	}
	next := cloneOperation(current)
	next.Reconciliation = rec
	// Fail-closed on the record save (R24's rule): if it cannot persist,
	// the record stays reconciling on disk and in memory — Retry keeps
	// refusing, the degradation shows in Health(), and a later boot retries
	// the read. Never silently downgrade to a guess.
	if err := m.store.saveOperation(next); err != nil {
		m.noteRecordErrLocked(err)
		log.Printf("[operation] reconciliation persist failed for %s: %v", id, err)
		return
	}
	m.clearRecordErrLocked()
	m.operations[id] = next
	if err := m.appendEventLocked(id, EventStatus, string(rec.State)); err != nil {
		log.Printf("[operation] reconciliation event persist failed for %s: %v", id, err)
	}
	m.store.CloseJournal(id)
}

// readReceipt runs one bounded receipt read. A nil reader is an explicit "no
// answer available", surfaced as an error so callers map it to
// needs-manual-check with the exact reason. The read is bounded by both the
// caller's parent context (shutdown, reconcile loop) and
// receiptReadTimeout.
func (m *Manager) readReceipt(parent context.Context, reader ReceiptReader, op *Operation) (Receipt, error) {
	if reader == nil {
		return Receipt{}, errors.New("no receipt reader configured")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, receiptReadTimeout)
	defer cancel()
	return reader(ctx, cloneOperation(op))
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
	now := time.Now().UTC()
	for _, id := range removed {
		op := m.operations[id]
		if op == nil || !op.Status.Terminal() {
			continue
		}
		delete(m.operations, id)
		delete(m.events, id)
		if op.IdempotencyKey != "" {
			delete(m.idempotency, idemKey{op.IdempotencyPrincipal, op.IdempotencyKey})
		}
	}
	// Expired keys are pruned on the same cadence (retire runs hourly and on
	// count-cap crossings): live lookups enforce the window anyway, so this
	// only reclaims index entries for records the window has outlived (D02).
	for key, id := range m.idempotency {
		if op := m.operations[id]; op == nil || !m.idempotencyLive(op.CreatedAt, now) {
			delete(m.idempotency, key)
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
	if op.Reconciliation != nil {
		rec := *op.Reconciliation
		copy.Reconciliation = &rec
	}
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
	if actor.Capabilities != nil {
		snapshot.Capabilities = append([]string(nil), actor.Capabilities...)
	}
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
