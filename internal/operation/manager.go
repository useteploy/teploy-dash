package operation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultMaxEvents = 1000

type Options struct {
	MaxEvents       int
	Resolver        Resolver
	ProjectResolver ProjectResolver
	Executor        Executor
}

type Manager struct {
	mu              sync.Mutex
	store           *fileStore
	operations      map[string]*Operation
	events          map[string][]Event
	idempotency     map[string]string
	cancels         map[string]context.CancelFunc
	targets         map[string]chan struct{}
	subscribers     map[string]map[chan struct{}]struct{}
	resolver        Resolver
	projectResolver ProjectResolver
	executor        Executor
	maxEvents       int
}

func New(dataDir string, options Options) (*Manager, error) {
	if options.MaxEvents <= 0 {
		options.MaxEvents = defaultMaxEvents
	}
	if options.Executor == nil {
		return nil, fmt.Errorf("operation executor is required")
	}
	store, err := openFileStore(dataDir, options.MaxEvents)
	if err != nil {
		return nil, err
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
		targets:         make(map[string]chan struct{}),
		subscribers:     make(map[string]map[chan struct{}]struct{}),
		resolver:        options.Resolver,
		projectResolver: options.ProjectResolver,
		executor:        options.Executor,
		maxEvents:       options.MaxEvents,
	}
	for id, op := range operations {
		if op.Metadata.Mode == "" {
			op.Metadata.Mode = op.Request.Mode
			if op.Metadata.Mode == "" && op.Request.Kind == KindDeploy {
				op.Metadata.Mode = "ad-hoc"
			}
		}
		events, err := store.loadEvents(id)
		if err != nil {
			return nil, fmt.Errorf("load events for %s: %w", id, err)
		}
		m.events[id] = events
		if op.IdempotencyKey != "" {
			m.idempotency[op.IdempotencyKey] = id
		}
	}
	if err := m.recover(); err != nil {
		return nil, err
	}
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
	command, target, err := Build(req, m.resolver, m.projectResolver)
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
		requestHash:    hash,
	}
	m.operations[id] = op
	if idempotencyKey != "" {
		m.idempotency[idempotencyKey] = id
	}
	if err := m.store.saveOperation(op); err != nil {
		delete(m.operations, id)
		delete(m.idempotency, idempotencyKey)
		m.mu.Unlock()
		return nil, false, err
	}
	if err := m.appendEventLocked(id, EventStatus, string(StatusQueued)); err != nil {
		m.mu.Unlock()
		return nil, false, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[id] = cancel
	result := cloneOperation(op)
	m.mu.Unlock()

	go m.execute(ctx, id, command)
	return result, false, nil
}

func (m *Manager) execute(ctx context.Context, id string, command Command) {
	m.mu.Lock()
	op := m.operations[id]
	if op == nil {
		m.mu.Unlock()
		return
	}
	targetLock := m.targets[op.Target]
	if targetLock == nil {
		targetLock = make(chan struct{}, 1)
		m.targets[op.Target] = targetLock
	}
	m.mu.Unlock()

	select {
	case targetLock <- struct{}{}:
		defer func() { <-targetLock }()
	case <-ctx.Done():
		m.finish(id, StatusCanceled, -1, "operation canceled")
		return
	}

	if ctx.Err() != nil {
		m.finish(id, StatusCanceled, -1, "operation canceled")
		return
	}
	m.setRunning(id)
	exitCode, err := m.executor(ctx, command, func(stream Stream, data string) {
		eventType := EventStdout
		if stream == StreamStderr {
			eventType = EventStderr
		}
		m.emit(id, eventType, Redact(data, command.Secrets))
	})
	if ctx.Err() != nil {
		m.finish(id, StatusCanceled, exitCode, "operation canceled")
		return
	}
	if err != nil || exitCode != 0 {
		message := "operation failed"
		if err != nil {
			message = Redact(err.Error(), command.Secrets)
		}
		m.finish(id, StatusFailed, exitCode, message)
		return
	}
	m.finish(id, StatusSucceeded, exitCode, "")
}

func (m *Manager) setRunning(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.operations[id]
	if op == nil || op.Status != StatusQueued {
		return
	}
	now := time.Now().UTC()
	op.Status = StatusRunning
	op.StartedAt = &now
	if err := m.store.saveOperation(op); err != nil {
		op.Error = err.Error()
	}
	_ = m.appendEventLocked(id, EventStatus, string(StatusRunning))
}

func (m *Manager) finish(id string, status Status, exitCode int, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.operations[id]
	if op == nil || op.Status.Terminal() {
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
	_ = m.store.saveOperation(op)
	_ = m.appendEventLocked(id, EventStatus, string(status))
	m.closeSubscribersLocked(id)
}

func (m *Manager) emit(id string, eventType EventType, data string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.appendEventLocked(id, eventType, data)
}

func (m *Manager) appendEventLocked(id string, eventType EventType, data string) error {
	events := m.events[id]
	var sequence uint64 = 1
	if len(events) > 0 {
		sequence = events[len(events)-1].Sequence + 1
	}
	events = append(events, Event{Sequence: sequence, OperationID: id, Type: eventType, Data: data, CreatedAt: time.Now().UTC()})
	if len(events) > m.maxEvents {
		events = events[len(events)-m.maxEvents:]
	}
	if err := m.store.saveEvents(id, events); err != nil {
		return err
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
	sort.Slice(operations, func(i, j int) bool { return operations[i].CreatedAt.After(operations[j].CreatedAt) })
	if limit > 0 && len(operations) > limit {
		operations = operations[:limit]
	}
	return operations
}

func (m *Manager) EventsAfter(id string, sequence uint64) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.operations[id] == nil {
		return nil, ErrNotFound
	}
	return eventsAfter(m.events[id], sequence), nil
}

func (m *Manager) Subscribe(id string, sequence uint64) ([]Event, <-chan struct{}, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.operations[id]
	if op == nil {
		return nil, nil, false, ErrNotFound
	}
	replay := eventsAfter(m.events[id], sequence)
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
	cancel := m.cancels[id]
	copy := cloneOperation(op)
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

func (m *Manager) recover() error {
	m.mu.Lock()
	type queuedTask struct {
		id      string
		ctx     context.Context
		command Command
	}
	var queued []queuedTask
	for id, op := range m.operations {
		if op.Status == StatusQueued && !op.HasSecrets {
			command, target, err := Build(op.Request, m.resolver, m.projectResolver)
			if err == nil && target == op.Target {
				ctx, cancel := context.WithCancel(context.Background())
				m.cancels[id] = cancel
				queued = append(queued, queuedTask{id: id, ctx: ctx, command: command})
				continue
			}
		}
		if op.Status != StatusRunning && op.Status != StatusQueued {
			continue
		}
		now := time.Now().UTC()
		op.Status = StatusInterrupted
		op.Error = "dashboard restarted before operation completed"
		op.FinishedAt = &now
		if err := m.store.saveOperation(op); err != nil {
			m.mu.Unlock()
			return err
		}
		if err := m.appendEventLocked(id, EventStatus, string(StatusInterrupted)); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	m.mu.Unlock()
	for _, task := range queued {
		go m.execute(task.ctx, task.id, task.command)
	}
	return nil
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
