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
	StatusQueued      Status = "queued"
	StatusRunning     Status = "running"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusCanceled    Status = "canceled"
	StatusInterrupted Status = "interrupted"
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
	requestHash    string
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
