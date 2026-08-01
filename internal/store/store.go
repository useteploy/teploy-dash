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
	// AllowInternal opts this monitor out of the default network policy that
	// rejects loopback/private/link-local/cloud-metadata targets (DASH-010).
	// Internal fleet monitoring (e.g. over Tailscale) is a real, supported use
	// case — this makes that reachability an explicit admin choice per
	// monitor rather than a default every monitor gets.
	AllowInternal bool `json:"allow_internal,omitempty"`
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

// RestoreTest is a scheduled backup verification: on its interval, dash runs
// `teploy accessory verify-backup` against the accessory's latest S3 backup,
// which restores it into a scratch container on the server and proves it's
// usable. Only the most recent result is persisted on the entity itself —
// these run hourly/daily, so a per-run history table would be noise.
type RestoreTest struct {
	ID            string `json:"id"`
	Server        string `json:"server"` // server name from servers.yml
	App           string `json:"app"`
	Accessory     string `json:"accessory"`
	Bucket        string `json:"bucket"`
	Region        string `json:"region"`
	IntervalHours int    `json:"interval_hours"`
	Enabled       bool   `json:"enabled"`
	// Last result
	LastRunAt      time.Time `json:"last_run_at"`
	LastOK         bool      `json:"last_ok"`
	LastDetail     string    `json:"last_detail,omitempty"`
	LastMetric     string    `json:"last_metric,omitempty"`
	LastDate       string    `json:"last_date,omitempty"` // backup timestamp that was verified
	LastDurationMs int64     `json:"last_duration_ms,omitempty"`
}

// Store is the interface for persisting monitor configs and check results.
type Store interface {
	// Monitors
	ListMonitors() ([]Monitor, error)
	GetMonitor(id string) (*Monitor, error)
	SaveMonitor(m Monitor) error
	DeleteMonitor(id string) error

	// Restore tests (scheduled backup verification)
	ListRestoreTests() ([]RestoreTest, error)
	GetRestoreTest(id string) (*RestoreTest, error)
	SaveRestoreTest(t RestoreTest) error
	DeleteRestoreTest(id string) error

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
