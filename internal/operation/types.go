package operation

import (
	"context"
	"errors"
	"regexp"
	"time"
)

var operationIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Kind string

const (
	KindDeploy           Kind = "deploy"
	KindRollback         Kind = "rollback"
	KindRemove           Kind = "remove"
	KindTemplateInstall  Kind = "template_install"
	KindAppLifecycle     Kind = "app_lifecycle"
	KindMaintenance      Kind = "maintenance"
	KindManifestApply    Kind = "manifest_apply"
	KindManifestPlan     Kind = "manifest_plan"
	KindManifestValidate Kind = "manifest_validate"
)

type Status string

const (
	StatusQueued          Status = "queued"
	StatusRunning         Status = "running"
	StatusCancelRequested Status = "cancel_requested"
	// StatusStopping is the persisted transition after a cancellation was
	// observed by the worker and the outcome of the already-running command
	// is being determined (D02): requested -> stopping -> canceled |
	// already_committed. Non-terminal — the operation still holds its
	// runner slot while the receipt is checked.
	StatusStopping  Status = "stopping"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
	// StatusAlreadyCommitted is the honest terminal outcome of a
	// cancellation whose receipt shows the effect landed anyway (D02): the
	// cancellation arrived too late, the effect stood, and NO rollback was
	// performed. Not retryable — the work is done.
	StatusAlreadyCommitted Status = "already_committed"
	StatusInterrupted      Status = "interrupted"
)

func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusAlreadyCommitted, StatusInterrupted:
		return true
	default:
		return false
	}
}

type EventType string

const (
	EventStatus EventType = "status"
	EventStdout EventType = "stdout"
	EventStderr EventType = "stderr"
	// EventGap marks history that is known to be missing — a torn journal
	// tail after an unclean shutdown, a sequence hole, or events dropped by
	// retention compaction. It is a replay marker: clients render it (or
	// ignore it) like any other event, and its sequence occupies the first
	// missing position so subsequent sequences stay strictly increasing.
	EventGap EventType = "gap"
)

type Event struct {
	Sequence    uint64    `json:"sequence"`
	OperationID string    `json:"operation_id"`
	Type        EventType `json:"type"`
	Data        string    `json:"data"`
	CreatedAt   time.Time `json:"created_at"`
}

// Request is deliberately an allowlist. It cannot represent an arbitrary
// executable or arguments.
type Request struct {
	Kind             Kind              `json:"kind"`
	Server           string            `json:"server"`
	App              string            `json:"app,omitempty"`
	Mode             string            `json:"mode,omitempty"`
	ManifestRevision string            `json:"manifest_revision,omitempty"`
	Image            string            `json:"image,omitempty"`
	Domain           string            `json:"domain,omitempty"`
	Port             int               `json:"port,omitempty"`
	Template         string            `json:"template,omitempty"`
	Vars             map[string]string `json:"vars,omitempty"`
	Action           string            `json:"action,omitempty"`
	Purge            bool              `json:"purge,omitempty"`
	Redirect         string            `json:"redirect,omitempty"`
	// SourceID/SourceCommit pin a source-triggered operation to the forge
	// delivery that admitted it (D04): SourceID names the registered
	// source, SourceCommit is the AUTHENTICATED payload commit (the
	// delivery pin — never a mutable branch tip). They participate in the
	// request hash, so the same delivery replays while a new commit is new
	// work. Absent on every human/CI-submitted operation.
	SourceID     string `json:"source_id,omitempty"`
	SourceCommit string `json:"source_commit,omitempty"`
}

type Operation struct {
	ID             string   `json:"id"`
	Request        Request  `json:"request"`
	Metadata       Metadata `json:"metadata"`
	Target         string   `json:"target"`
	Status         Status   `json:"status"`
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
	// IdempotencyPrincipal is the principal namespace the IdempotencyKey was
	// issued in (D02 namespacing, A12/A13/R20): IdempotencyScope of the actor
	// that admitted the operation. Replays and conflicts resolve only within
	// one principal's namespace — two principals sharing a client key are two
	// independent operations. Empty on records written before namespacing
	// landed: those keys restore into a legacy namespace no namespaced replay
	// matches, so an upgrade can never replay one principal's operation to
	// another (the cost: a replay in flight across the upgrade enqueues new
	// work, bounded by the idempotency window).
	IdempotencyPrincipal string     `json:"idempotency_principal,omitempty"`
	RetryOf              string     `json:"retry_of,omitempty"`
	Attempt              int        `json:"attempt"`
	ExitCode             *int       `json:"exit_code,omitempty"`
	Error                string     `json:"error,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	FinishedAt           *time.Time `json:"finished_at,omitempty"`
	HasSecrets           bool       `json:"has_secrets,omitempty"`
	// AdmittedServer is the resolved host/user snapshot taken when the
	// operation was admitted. Before execution the current resolution is
	// compared against it; a server whose alias was repointed (or removed)
	// between admission and execution fails the operation instead of
	// silently redirecting queued work at the new target (A14). Nil on
	// records written before the field existed — those skip the check.
	AdmittedServer *Server `json:"admitted_server,omitempty"`
	// AdmissionSeq is the store-wide monotonic sequence assigned when the
	// operation was admitted. Per-target execution follows it in FIFO
	// order (A12/A27 core); persisting it makes the admission order durable
	// and gives listings a total order alongside CreatedAt.
	AdmissionSeq uint64 `json:"admission_seq,omitempty"`
	// Actor records which principal admitted the operation (A27 remainder).
	// Nil on records written before the field existed and on internal callers
	// with no principal to attribute.
	Actor *Actor `json:"actor,omitempty"`
	// Reconciliation records the receipt check for an outcome dash could not
	// observe itself (D02): interrupted by a restart, or canceled while the
	// command was mid-flight. Its state is the honest answer to "did the
	// effect land?" — reconciling until answered, then reconciled-applied /
	// reconciled-not-applied, or needs-manual-check with the exact reason
	// when no receipt can answer. Nil on records that never needed one
	// (including interrupted records from before D02, which stay retryable
	// exactly as before).
	Reconciliation *Reconciliation `json:"reconciliation,omitempty"`
	requestHash    string
}

type Metadata struct {
	Mode string `json:"mode"`
}

// ReconcileState is the lifecycle of a receipt check (D02).
type ReconcileState string

const (
	// ReconcileStateReconciling: the outcome is unknown and a receipt read
	// is (or will be) in flight. Retry is refused while this is the state.
	ReconcileStateReconciling ReconcileState = "reconciling"
	// ReconcileStateApplied: the receipt shows the effect landed.
	ReconcileStateApplied ReconcileState = "reconciled-applied"
	// ReconcileStateNotApplied: the receipt shows the effect did not land.
	ReconcileStateNotApplied ReconcileState = "reconciled-not-applied"
	// ReconcileStateManual: no receipt could answer (reader unavailable,
	// read failed, or the operation kind has no receipt semantics). Reason
	// carries the exact cause — the label is never a silent "failed".
	ReconcileStateManual ReconcileState = "needs-manual-check"
)

// Reconciliation is the durable record of one receipt check.
type Reconciliation struct {
	State     ReconcileState `json:"state"`
	Reason    string         `json:"reason,omitempty"`
	Evidence  string         `json:"evidence,omitempty"`
	CheckedAt time.Time      `json:"checked_at,omitempty"`
}

// Receipt is a read-side answer about one operation's effect: whether the
// target received it, plus the evidence the answer rests on. Produced by the
// injected ReceiptReader from the CLI's machine reads — never guessed.
type Receipt struct {
	Applied  bool
	Evidence string
}

// ReceiptReader answers whether an operation's effect reached its target.
// A non-nil error means the question could NOT be answered (transport
// failure, unsupported kind) — that maps to needs-manual-check, never to
// not-applied.
type ReceiptReader func(ctx context.Context, op *Operation) (Receipt, error)

type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

type Command struct {
	Args    []string
	Timeout time.Duration
	Secrets []string
	// Stdin, when set, is fed to the CLI process on its standard input —
	// how secret values travel instead of the argv (A11).
	Stdin string
}

type Server struct {
	Name string
	// ID is the server's stable CLI-recorded identity (X02 §1.3): minted
	// once by `teploy server add`, preserved across renames. Empty on
	// legacy servers.yml entries written before the field existed — the
	// name/host checks below remain the contract for those, and dash's
	// envelope IDs fall back to the name-hash.
	ID   string
	Host string
	User string
}

type Resolver func(name string) (Server, error)

// ResolverByID resolves a server by its stable CLI-recorded id. The second
// return is presence, not success: "no server with that id" is an expected
// answer in the rename-reconciliation path (X02 §1.3), not an error.
type ResolverByID func(id string) (Server, bool)

// ProjectResolver returns the immutable project directory registered for a
// manifest revision. Project paths are intentionally absent from Request.
type ProjectResolver func(server, app, revision string) (string, error)

type Executor func(ctx context.Context, command Command, emit func(Stream, string)) (int, error)

var (
	ErrNotFound            = errors.New("operation not found")
	ErrIdempotencyConflict = errors.New("idempotency key was already used for a different request")
	ErrNotCancelable       = errors.New("operation is not cancelable")
	ErrNotRetryable        = errors.New("operation is not retryable")
	// ErrReconciliationPending reports that an interrupted operation's
	// outcome is still being reconciled against the target's receipts (D02):
	// retry is refused until the answer lands so nobody re-runs work whose
	// effect may already stand.
	ErrReconciliationPending = errors.New("operation outcome is being reconciled against the server; retry after reconciliation completes")
	// ErrAdmissionBudget reports that the target's admission budget is
	// exhausted: too many non-terminal operations are already queued for it
	// (A12/A27 remainder). Bounded queues keep a runaway client (or a stuck
	// target) from admitting unbounded work; the caller should back off.
	ErrAdmissionBudget = errors.New("target admission budget exceeded — too many operations already queued for this target")
	// ErrShuttingDown reports that admission was closed by Shutdown (F022):
	// the manager seals admission before joining workers so no operation can
	// be added to (or be omitted from) the join — a request that outlives
	// the HTTP drain timeout gets a clean, retryable refusal instead of
	// racing the closing store.
	ErrShuttingDown = errors.New("server is shutting down; retry after restart")
)

// Actor attributes an operation to the principal that admitted it (A27
// remainder). It is deliberately NOT part of Request: the request hash keys
// idempotency, and who queued a request must not change its identity — only
// record it. Kind is "local" (password account), "sso" (OIDC principal),
// "mcp" (API token), or "webhook" (an authenticated forge delivery; Subject
// is "source/<id>" — its own idempotency namespace, D04). A nil Actor
// (records predating the field, or internal callers with no principal)
// means unknown.
type Actor struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject,omitempty"`
	Label   string `json:"label,omitempty"`
}

// NoAuthScope is the explicit idempotency principal for --no-auth installs
// and manager-internal callers with no authenticated actor (D02). In auth
// mode every API request carries a session before it can enqueue, so a nil
// actor at the boundary means no-auth: one local operator, named explicitly
// so its keys never collide with any authenticated principal's keys.
const NoAuthScope = "local:no-auth"

// IdempotencyScope derives an operation's idempotency namespace from the
// admitting actor (D02 namespacing): "local:<username>", "sso:<subject>",
// "mcp:mcp-token/<id>", or the explicit NoAuthScope for nil actors. The
// scope is persisted on the record so restart-time dedupe resolves within
// the same namespace.
func IdempotencyScope(actor *Actor) string {
	if actor == nil {
		return NoAuthScope
	}
	return actor.Kind + ":" + actor.Subject
}
