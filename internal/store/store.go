package store

import (
	"regexp"
	"time"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidID reports whether id is a safe monitor identifier: non-empty and only
// [A-Za-z0-9_-]. Monitor IDs are used directly as filenames in the file store,
// so anything else (path separators, "..", spaces) must be rejected to prevent
// path traversal / arbitrary file write. The frontend generates hex IDs, so
// this never rejects a legitimately-created monitor.
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// CheckResult represents the result of a single uptime check.
type CheckResult struct {
	MonitorID    string        `json:"monitor_id"`
	Status       string        `json:"status"` // "up", "down", "timeout"
	StatusCode   int           `json:"status_code,omitempty"`
	ResponseTime time.Duration `json:"response_time"`
	Message      string        `json:"message,omitempty"`
	CheckedAt    time.Time     `json:"checked_at"`
}

// Monitor represents an uptime monitor configuration.
type Monitor struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Type     string        `json:"type"` // "http", "tcp", "ping"
	Target   string        `json:"target"`
	Interval time.Duration `json:"interval"`
	Timeout  time.Duration `json:"timeout"`
	Enabled  bool          `json:"enabled"`
	// HTTP-specific
	ExpectedStatus int    `json:"expected_status,omitempty"`
	Method         string `json:"method,omitempty"`
}

// UptimeStats represents uptime statistics for a monitor over a period.
type UptimeStats struct {
	MonitorID     string        `json:"monitor_id"`
	TotalChecks   int           `json:"total_checks"`
	UpChecks      int           `json:"up_checks"`
	DownChecks    int           `json:"down_checks"`
	UptimePercent float64       `json:"uptime_percent"`
	AvgResponse   time.Duration `json:"avg_response"`
}

// Store is the interface for persisting monitor configs and check results.
type Store interface {
	// Monitors
	ListMonitors() ([]Monitor, error)
	GetMonitor(id string) (*Monitor, error)
	SaveMonitor(m Monitor) error
	DeleteMonitor(id string) error

	// Check results
	SaveCheck(result CheckResult) error
	GetChecks(monitorID string, since time.Time, limit int) ([]CheckResult, error)
	GetStats(monitorID string, since time.Time) (*UptimeStats, error)

	// Cleanup prunes check history older than RetentionDays. Implemented by
	// every backend so the retention ticker runs regardless of which is active.
	Cleanup() error

	// Lifecycle
	Close() error
}
