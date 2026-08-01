package store

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// nucleusTimeout bounds each Nucleus query so a wedged/slow DB can't block a
// request (or a monitor check) indefinitely. Generous enough for a 24h history
// scan.
const nucleusTimeout = 10 * time.Second

// randomID returns a random positive int64 for the checks primary key. The
// previous time.Now().UnixNano() collided when two checks landed in the same
// nanosecond (concurrent monitors), and the PK violation silently dropped a
// check. A 63-bit random value makes that astronomically unlikely for a
// retention-bounded table, and needs no schema migration (still a BIGINT).
func randomID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.BigEndian.Uint64(b[:]) >> 1)
}

// NucleusStore implements Store using Nucleus (via pgwire/pgx).
// Uses time-series model for check history, SQL for monitor configs.
type NucleusStore struct {
	pool *pgxpool.Pool
}

// NewNucleusStore connects to a Nucleus instance and initializes tables.
func NewNucleusStore(url string) (*NucleusStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Nucleus: %w", err)
	}

	// Verify connection
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping Nucleus: %w", err)
	}

	s := &NucleusStore{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return s, nil
}

func (s *NucleusStore) migrate(ctx context.Context) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS monitors (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			target TEXT NOT NULL,
			interval_ms BIGINT NOT NULL DEFAULT 60000,
			timeout_ms BIGINT NOT NULL DEFAULT 10000,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			expected_status INT DEFAULT 200,
			method TEXT DEFAULT 'GET',
			allow_internal BOOLEAN NOT NULL DEFAULT FALSE
		)`,
		// Additive migration for pre-existing installs whose monitors table
		// predates allow_internal — CREATE TABLE IF NOT EXISTS above is a
		// no-op once the table already exists.
		`ALTER TABLE monitors ADD COLUMN IF NOT EXISTS allow_internal BOOLEAN NOT NULL DEFAULT FALSE`,
		`CREATE TABLE IF NOT EXISTS checks (
			id BIGINT PRIMARY KEY,
			monitor_id TEXT NOT NULL,
			status TEXT NOT NULL,
			status_code INT DEFAULT 0,
			response_time_ms BIGINT NOT NULL,
			message TEXT DEFAULT '',
			checked_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		// Serves the (monitor_id =) + (checked_at range / ORDER BY) access pattern
		// in GetChecks/GetStats; without it every query is a full scan.
		`CREATE INDEX IF NOT EXISTS idx_checks_monitor_time ON checks (monitor_id, checked_at DESC)`,
		`CREATE TABLE IF NOT EXISTS restore_tests (
			id TEXT PRIMARY KEY,
			server TEXT NOT NULL,
			app TEXT NOT NULL,
			accessory TEXT NOT NULL,
			bucket TEXT NOT NULL,
			region TEXT NOT NULL DEFAULT 'us-east-1',
			interval_hours INT NOT NULL DEFAULT 24,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			last_run_ms BIGINT NOT NULL DEFAULT 0,
			last_ok BOOLEAN NOT NULL DEFAULT FALSE,
			last_detail TEXT NOT NULL DEFAULT '',
			last_metric TEXT NOT NULL DEFAULT '',
			last_date TEXT NOT NULL DEFAULT '',
			last_duration_ms BIGINT NOT NULL DEFAULT 0
		)`,
	}

	for _, q := range queries {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *NucleusStore) ListMonitors() ([]Monitor, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		"SELECT id, name, type, target, interval_ms, timeout_ms, enabled, expected_status, method, allow_internal FROM monitors")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var monitors []Monitor
	for rows.Next() {
		var m Monitor
		var intervalMs, timeoutMs int64
		var expectedStatus int
		var method string

		err := rows.Scan(&m.ID, &m.Name, &m.Type, &m.Target,
			&intervalMs, &timeoutMs, &m.Enabled, &expectedStatus, &method, &m.AllowInternal)
		if err != nil {
			continue
		}

		m.Interval = time.Duration(intervalMs) * time.Millisecond
		m.Timeout = time.Duration(timeoutMs) * time.Millisecond
		m.ExpectedStatus = expectedStatus
		m.Method = method
		monitors = append(monitors, m)
	}
	if err := rows.Err(); err != nil {
		// Surface a mid-stream failure instead of silently returning a
		// truncated monitor list (which would stop monitoring some targets).
		return nil, err
	}
	return monitors, nil
}

func (s *NucleusStore) GetMonitor(id string) (*Monitor, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	var m Monitor
	var intervalMs, timeoutMs int64

	err := s.pool.QueryRow(ctx,
		"SELECT id, name, type, target, interval_ms, timeout_ms, enabled, expected_status, method, allow_internal FROM monitors WHERE id = $1", id,
	).Scan(&m.ID, &m.Name, &m.Type, &m.Target,
		&intervalMs, &timeoutMs, &m.Enabled, &m.ExpectedStatus, &m.Method, &m.AllowInternal)
	if err != nil {
		return nil, err
	}

	m.Interval = time.Duration(intervalMs) * time.Millisecond
	m.Timeout = time.Duration(timeoutMs) * time.Millisecond
	return &m, nil
}

func (s *NucleusStore) SaveMonitor(m Monitor) error {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO monitors (id, name, type, target, interval_ms, timeout_ms, enabled, expected_status, method, allow_internal)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (id) DO UPDATE SET
		   name = EXCLUDED.name, type = EXCLUDED.type, target = EXCLUDED.target,
		   interval_ms = EXCLUDED.interval_ms, timeout_ms = EXCLUDED.timeout_ms,
		   enabled = EXCLUDED.enabled, expected_status = EXCLUDED.expected_status,
		   method = EXCLUDED.method, allow_internal = EXCLUDED.allow_internal`,
		m.ID, m.Name, m.Type, m.Target,
		m.Interval.Milliseconds(), m.Timeout.Milliseconds(),
		m.Enabled, m.ExpectedStatus, m.Method, m.AllowInternal,
	)
	return err
}

func (s *NucleusStore) DeleteMonitor(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	_, err := s.pool.Exec(ctx, "DELETE FROM monitors WHERE id = $1", id)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, "DELETE FROM checks WHERE monitor_id = $1", id)
	return err
}

func (s *NucleusStore) ListRestoreTests() ([]RestoreTest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT id, server, app, accessory, bucket, region, interval_hours, enabled,
		        last_run_ms, last_ok, last_detail, last_metric, last_date, last_duration_ms
		 FROM restore_tests`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tests []RestoreTest
	for rows.Next() {
		t, err := scanRestoreTest(rows.Scan)
		if err != nil {
			continue
		}
		tests = append(tests, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tests, nil
}

func (s *NucleusStore) GetRestoreTest(id string) (*RestoreTest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	row := s.pool.QueryRow(ctx,
		`SELECT id, server, app, accessory, bucket, region, interval_hours, enabled,
		        last_run_ms, last_ok, last_detail, last_metric, last_date, last_duration_ms
		 FROM restore_tests WHERE id = $1`, id)
	t, err := scanRestoreTest(row.Scan)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// scanRestoreTest maps a restore_tests row onto the struct; shared between
// List and Get so the column order lives in one place.
func scanRestoreTest(scan func(dest ...any) error) (RestoreTest, error) {
	var t RestoreTest
	var lastRunMs int64
	err := scan(&t.ID, &t.Server, &t.App, &t.Accessory, &t.Bucket, &t.Region,
		&t.IntervalHours, &t.Enabled, &lastRunMs, &t.LastOK,
		&t.LastDetail, &t.LastMetric, &t.LastDate, &t.LastDurationMs)
	if err != nil {
		return t, err
	}
	if lastRunMs > 0 {
		t.LastRunAt = time.UnixMilli(lastRunMs)
	}
	return t, nil
}

func (s *NucleusStore) SaveRestoreTest(t RestoreTest) error {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	var lastRunMs int64
	if !t.LastRunAt.IsZero() {
		lastRunMs = t.LastRunAt.UnixMilli()
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO restore_tests (id, server, app, accessory, bucket, region, interval_hours, enabled,
		                            last_run_ms, last_ok, last_detail, last_metric, last_date, last_duration_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 ON CONFLICT (id) DO UPDATE SET
		   server = EXCLUDED.server, app = EXCLUDED.app, accessory = EXCLUDED.accessory,
		   bucket = EXCLUDED.bucket, region = EXCLUDED.region,
		   interval_hours = EXCLUDED.interval_hours, enabled = EXCLUDED.enabled,
		   last_run_ms = EXCLUDED.last_run_ms, last_ok = EXCLUDED.last_ok,
		   last_detail = EXCLUDED.last_detail, last_metric = EXCLUDED.last_metric,
		   last_date = EXCLUDED.last_date, last_duration_ms = EXCLUDED.last_duration_ms`,
		t.ID, t.Server, t.App, t.Accessory, t.Bucket, t.Region, t.IntervalHours, t.Enabled,
		lastRunMs, t.LastOK, t.LastDetail, t.LastMetric, t.LastDate, t.LastDurationMs,
	)
	return err
}

func (s *NucleusStore) DeleteRestoreTest(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	_, err := s.pool.Exec(ctx, "DELETE FROM restore_tests WHERE id = $1", id)
	return err
}

func (s *NucleusStore) SaveCheck(result CheckResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO checks (id, monitor_id, status, status_code, response_time_ms, message, checked_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		randomID(), result.MonitorID, result.Status,
		result.StatusCode, result.ResponseTime.Milliseconds(),
		result.Message, result.CheckedAt,
	)
	return err
}

// Cleanup deletes check history older than RetentionDays. Without this the
// NucleusStore checks table grew unbounded — the daily cleanup ticker only ran
// for the file backend.
func (s *NucleusStore) Cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	cutoff := time.Now().AddDate(0, 0, -RetentionDays)
	_, err := s.pool.Exec(ctx, `DELETE FROM checks WHERE checked_at < $1`, cutoff)
	return err
}

func (s *NucleusStore) GetChecks(monitorID string, since time.Time, limit int) ([]CheckResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	query := "SELECT monitor_id, status, status_code, response_time_ms, message, checked_at FROM checks WHERE monitor_id = $1 AND checked_at > $2 ORDER BY checked_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.pool.Query(ctx, query, monitorID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []CheckResult
	for rows.Next() {
		var r CheckResult
		var responseMs int64
		err := rows.Scan(&r.MonitorID, &r.Status, &r.StatusCode, &responseMs, &r.Message, &r.CheckedAt)
		if err != nil {
			continue
		}
		r.ResponseTime = time.Duration(responseMs) * time.Millisecond
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *NucleusStore) GetStats(monitorID string, since time.Time) (*UptimeStats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()

	var total, up, down int
	var avgMs float64

	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*),
		        SUM(CASE WHEN status = 'up' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status != 'up' THEN 1 ELSE 0 END),
		        COALESCE(AVG(response_time_ms) FILTER (WHERE status = 'up'), 0)
		 FROM checks WHERE monitor_id = $1 AND checked_at > $2`,
		monitorID, since,
	).Scan(&total, &up, &down, &avgMs)
	if err != nil {
		return nil, err
	}

	var uptimePct float64
	if total > 0 {
		uptimePct = float64(up) / float64(total) * 100
	}

	return &UptimeStats{
		MonitorID:     monitorID,
		TotalChecks:   total,
		UpChecks:      up,
		DownChecks:    down,
		UptimePercent: uptimePct,
		AvgResponse:   time.Duration(avgMs) * time.Millisecond,
	}, nil
}

func (s *NucleusStore) Close() error {
	s.pool.Close()
	return nil
}
