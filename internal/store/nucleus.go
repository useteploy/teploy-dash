package store

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
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
	// saveMu + incarnationClock serialize the read-bump-upsert in SaveMonitor
	// so two concurrent saves never assign the same incarnation, and the
	// number only moves forward within a process (D08: delete+recreate never
	// reuses the deleted incarnation — no ABA against SaveCheck's CAS).
	saveMu           sync.Mutex
	incarnationClock uint64
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

// Ping is the readiness probe (A39/A47): a cheap round trip with its own
// short deadline, independent of any caller's context.
func (s *NucleusStore) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.pool.Ping(ctx)
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
		// D08: incarnation column for the revision-conditional check commit
		// (same additive pattern; pre-D08 rows read as incarnation 0 and are
		// superseded by the first post-upgrade save).
		`ALTER TABLE monitors ADD COLUMN IF NOT EXISTS incarnation BIGINT NOT NULL DEFAULT 0`,
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
		"SELECT id, name, type, target, interval_ms, timeout_ms, enabled, expected_status, method, allow_internal, incarnation FROM monitors")
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
			&intervalMs, &timeoutMs, &m.Enabled, &expectedStatus, &method, &m.AllowInternal, &m.Incarnation)
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
		"SELECT id, name, type, target, interval_ms, timeout_ms, enabled, expected_status, method, allow_internal, incarnation FROM monitors WHERE id = $1", id,
	).Scan(&m.ID, &m.Name, &m.Type, &m.Target,
		&intervalMs, &timeoutMs, &m.Enabled, &m.ExpectedStatus, &m.Method, &m.AllowInternal, &m.Incarnation)
	if err != nil {
		return nil, err
	}

	m.Interval = time.Duration(intervalMs) * time.Millisecond
	m.Timeout = time.Duration(timeoutMs) * time.Millisecond
	return &m, nil
}

// SaveMonitor upserts configuration and ASSIGNS the next incarnation under
// saveMu (D08): monotonic across edits and delete+recreate, and never the
// client's value.
func (s *NucleusStore) SaveMonitor(m *Monitor) error {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()

	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	var current uint64
	err := s.pool.QueryRow(ctx,
		"SELECT incarnation FROM monitors WHERE id = $1", m.ID).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	next := s.incarnationClock + 1
	if current >= next {
		next = current + 1
	}
	m.Incarnation = next
	s.incarnationClock = next

	_, err = s.pool.Exec(ctx,
		`INSERT INTO monitors (id, name, type, target, interval_ms, timeout_ms, enabled, expected_status, method, allow_internal, incarnation)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (id) DO UPDATE SET
		   name = EXCLUDED.name, type = EXCLUDED.type, target = EXCLUDED.target,
		   interval_ms = EXCLUDED.interval_ms, timeout_ms = EXCLUDED.timeout_ms,
		   enabled = EXCLUDED.enabled, expected_status = EXCLUDED.expected_status,
		   method = EXCLUDED.method, allow_internal = EXCLUDED.allow_internal,
		   incarnation = EXCLUDED.incarnation`,
		m.ID, m.Name, m.Type, m.Target,
		m.Interval.Milliseconds(), m.Timeout.Milliseconds(),
		m.Enabled, m.ExpectedStatus, m.Method, m.AllowInternal, m.Incarnation,
	)
	return err
}

func (s *NucleusStore) DeleteMonitor(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	// Both deletes commit or roll back together — separate statements could
	// leave checks behind for a removed monitor (or remove checks for a
	// monitor whose config row failed to delete) (A33).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "DELETE FROM monitors WHERE id = $1", id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM checks WHERE monitor_id = $1", id); err != nil {
		return err
	}
	return tx.Commit(ctx)
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

// SaveRestoreTest upserts configuration. R36: the last-result columns are
// preserved from the CURRENT row inside the same statement when the target
// identity is unchanged (and reset when it changed, F038) — the handler used
// to round-trip stale Last* values through the API, letting a config save
// overwrite a verification that completed while the edit was in flight.
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
		   last_run_ms = CASE WHEN restore_tests.server = EXCLUDED.server
		                       AND restore_tests.app = EXCLUDED.app
		                       AND restore_tests.accessory = EXCLUDED.accessory
		                       AND restore_tests.bucket = EXCLUDED.bucket
		                       AND restore_tests.region = EXCLUDED.region
		                      THEN restore_tests.last_run_ms ELSE EXCLUDED.last_run_ms END,
		   last_ok = CASE WHEN restore_tests.server = EXCLUDED.server
		                   AND restore_tests.app = EXCLUDED.app
		                   AND restore_tests.accessory = EXCLUDED.accessory
		                   AND restore_tests.bucket = EXCLUDED.bucket
		                   AND restore_tests.region = EXCLUDED.region
		                  THEN restore_tests.last_ok ELSE EXCLUDED.last_ok END,
		   last_detail = CASE WHEN restore_tests.server = EXCLUDED.server
		                       AND restore_tests.app = EXCLUDED.app
		                       AND restore_tests.accessory = EXCLUDED.accessory
		                       AND restore_tests.bucket = EXCLUDED.bucket
		                       AND restore_tests.region = EXCLUDED.region
		                      THEN restore_tests.last_detail ELSE EXCLUDED.last_detail END,
		   last_metric = CASE WHEN restore_tests.server = EXCLUDED.server
		                       AND restore_tests.app = EXCLUDED.app
		                       AND restore_tests.accessory = EXCLUDED.accessory
		                       AND restore_tests.bucket = EXCLUDED.bucket
		                       AND restore_tests.region = EXCLUDED.region
		                      THEN restore_tests.last_metric ELSE EXCLUDED.last_metric END,
		   last_date = CASE WHEN restore_tests.server = EXCLUDED.server
		                     AND restore_tests.app = EXCLUDED.app
		                     AND restore_tests.accessory = EXCLUDED.accessory
		                     AND restore_tests.bucket = EXCLUDED.bucket
		                     AND restore_tests.region = EXCLUDED.region
		                    THEN restore_tests.last_date ELSE EXCLUDED.last_date END,
		   last_duration_ms = CASE WHEN restore_tests.server = EXCLUDED.server
		                            AND restore_tests.app = EXCLUDED.app
		                            AND restore_tests.accessory = EXCLUDED.accessory
		                            AND restore_tests.bucket = EXCLUDED.bucket
		                            AND restore_tests.region = EXCLUDED.region
		                           THEN restore_tests.last_duration_ms ELSE EXCLUDED.last_duration_ms END`,
		t.ID, t.Server, t.App, t.Accessory, t.Bucket, t.Region, t.IntervalHours, t.Enabled,
		lastRunMs, t.LastOK, t.LastDetail, t.LastMetric, t.LastDate, t.LastDurationMs,
	)
	return err
}

// SaveRestoreTestResult applies only the last-result columns (A15): config
// edits made while the run was in flight survive, and a deleted test stays
// deleted (zero rows affected). The WHERE clause also requires the identity
// columns to match the run's target (A24): a retargeted or recreated test
// never inherits the old target's verification result. F037: region is part
// of the identity, and zero affected rows are reported as applied=false
// (dropped) rather than masquerading as success.
func (s *NucleusStore) SaveRestoreTestResult(id string, result RestoreTest) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	var lastRunMs int64
	if !result.LastRunAt.IsZero() {
		lastRunMs = result.LastRunAt.UnixMilli()
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE restore_tests
		 SET last_run_ms = $1, last_ok = $2, last_detail = $3,
		     last_metric = $4, last_date = $5, last_duration_ms = $6
		 WHERE id = $7 AND server = $8 AND app = $9 AND accessory = $10 AND bucket = $11 AND region = $12`,
		lastRunMs, result.LastOK, result.LastDetail, result.LastMetric,
		result.LastDate, result.LastDurationMs, id,
		result.Server, result.App, result.Accessory, result.Bucket, result.Region,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *NucleusStore) DeleteRestoreTest(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	_, err := s.pool.Exec(ctx, "DELETE FROM restore_tests WHERE id = $1", id)
	return err
}

// SaveCheck commits a result only when the monitor row still exists at the
// result's incarnation (D08/A19/R34): INSERT ... SELECT against the monitors
// table is the CAS, and zero affected rows report an intentionally dropped
// result (deleted, edited, or recreated mid-check).
func (s *NucleusStore) SaveCheck(result CheckResult) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nucleusTimeout)
	defer cancel()
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO checks (id, monitor_id, status, status_code, response_time_ms, message, checked_at)
		 SELECT $1, $2, $3, $4, $5, $6, $7
		 WHERE EXISTS (SELECT 1 FROM monitors WHERE id = $8 AND incarnation = $9)`,
		randomID(), result.MonitorID, result.Status,
		result.StatusCode, result.ResponseTime.Milliseconds(),
		result.Message, result.CheckedAt,
		result.MonitorID, result.Incarnation,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
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
			// F040: a row that cannot be decoded is a storage failure, not a
			// silently smaller history — a partial read used to look like a
			// complete (shorter) one.
			return nil, fmt.Errorf("decode check row for %s: %w", monitorID, err)
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

	// SUM over an empty set is NULL in PostgreSQL while COUNT is 0; scanning
	// NULL into an ordinary int fails. COALESCE both aggregates to explicit
	// zeros (A33).
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN status = 'up' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN status != 'up' THEN 1 ELSE 0 END), 0),
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

	// Scale before converting: time.Duration(avgMs) truncates sub-millisecond
	// precision to zero (A33).
	return &UptimeStats{
		MonitorID:     monitorID,
		TotalChecks:   total,
		UpChecks:      up,
		DownChecks:    down,
		UptimePercent: uptimePct,
		AvgResponse:   time.Duration(avgMs * float64(time.Millisecond)),
	}, nil
}

func (s *NucleusStore) Close() error {
	s.pool.Close()
	return nil
}
