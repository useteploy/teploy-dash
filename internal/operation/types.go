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
	StatusSucceeded       Status = "succeeded"
	StatusFailed          Status = "failed"
	StatusCanceled        Status = "canceled"
	StatusInterrupted     Status = "interrupted"
)

func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusInterrupted:
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
}

type Operation struct {
	ID             string     `json:"id"`
	Request        Request    `json:"request"`
	Metadata       Metadata   `json:"metadata"`
	Target         string     `json:"target"`
	Status         Status     `json:"status"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	RetryOf        string     `json:"retry_of,omitempty"`
	Attempt        int        `json:"attempt"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	HasSecrets     bool       `json:"has_secrets,omitempty"`
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
	requestHash  string
}

type Metadata struct {
	Mode string `json:"mode"`
}

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
	Host string
	User string
}

type Resolver func(name string) (Server, error)

// ProjectResolver returns the immutable project directory registered for a
// manifest revision. Project paths are intentionally absent from Request.
type ProjectResolver func(server, app, revision string) (string, error)

type Executor func(ctx context.Context, command Command, emit func(Stream, string)) (int, error)

var (
	ErrNotFound            = errors.New("operation not found")
	ErrIdempotencyConflict = errors.New("idempotency key was already used for a different request")
	ErrNotCancelable       = errors.New("operation is not cancelable")
	ErrNotRetryable        = errors.New("operation is not retryable")
)
