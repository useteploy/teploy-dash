package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RetentionDays is how many days of check history to keep in JSONL files.
// 7 days keeps files small and fast to scan. Use Nucleus for longer retention.
const RetentionDays = 7

// maxRecordBytes bounds one JSONL record while scanning history files. A
// record larger than this stops the scanner; Cleanup and GetChecks treat that
// as an error instead of silently rewriting a truncated file (A31).
const maxRecordBytes = 1 << 20

// FileStore implements Store using JSONL files. Fallback when Nucleus is not available.
type FileStore struct {
	dir string
	// initErr records a directory-creation failure. The constructor keeps its
	// signature (many callers), but the failure is not silent: InitErr lets
	// main refuse to start, and every operation on an uninitialized store
	// reports it (A32).
	initErr error
	mu      sync.RWMutex
}

// NewFileStore creates a file-based store in the given directory.
func NewFileStore(dir string) *FileStore {
	s := &FileStore{dir: dir}
	for _, sub := range []string{"monitors", "history", "restore-tests"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			s.initErr = fmt.Errorf("create %s: %w", filepath.Join(dir, sub), err)
			break
		}
	}
	return s
}

// InitErr reports a store-construction failure (e.g. unwritable data dir).
func (s *FileStore) InitErr() error { return s.initErr }

// Ping is the readiness probe (A39/A47): the data directory must still be
// present and owner-writable. Metadata only — no probe files are written.
func (s *FileStore) Ping() error {
	if s.initErr != nil {
		return s.initErr
	}
	info, err := os.Stat(s.dir)
	if err != nil {
		return fmt.Errorf("store directory %s: %w", s.dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("store path %s is not a directory", s.dir)
	}
	if info.Mode().Perm()&0200 == 0 {
		return fmt.Errorf("store directory %s is not writable", s.dir)
	}
	return nil
}

func (s *FileStore) ListMonitors() ([]Monitor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(filepath.Join(s.dir, "monitors"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var monitors []Monitor
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, "monitors", e.Name()))
		if err != nil {
			// A22: a damaged entry must at least be VISIBLE as skipped; the
			// partial-error envelope itself is deferred design.
			log.Printf("[store] skipping unreadable monitor entry %s: %v", e.Name(), err)
			continue
		}
		var m Monitor
		if err := json.Unmarshal(data, &m); err != nil {
			log.Printf("[store] skipping undecodable monitor entry %s: %v", e.Name(), err)
			continue
		} else {
			monitors = append(monitors, m)
		}
	}
	return monitors, nil
}

func (s *FileStore) GetMonitor(id string) (*Monitor, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("invalid monitor id %q", id)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(filepath.Join(s.dir, "monitors", id+".json"))
	if err != nil {
		return nil, err
	}
	var m Monitor
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *FileStore) SaveMonitor(m Monitor) error {
	if !ValidID(m.ID) {
		return fmt.Errorf("invalid monitor id %q", m.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, "monitors", m.ID+".json"), data, 0644)
}

func (s *FileStore) DeleteMonitor(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid monitor id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// An already-absent monitor is documented idempotent success; the history
	// file is still removed so a re-created monitor doesn't inherit stale
	// results. Every other remove error is returned (A32).
	for _, path := range []string{
		filepath.Join(s.dir, "monitors", id+".json"),
		filepath.Join(s.dir, "history", id+".jsonl"),
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *FileStore) ListRestoreTests() ([]RestoreTest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(filepath.Join(s.dir, "restore-tests"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var tests []RestoreTest
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, "restore-tests", e.Name()))
		if err != nil {
			log.Printf("[store] skipping unreadable restore-test entry %s: %v", e.Name(), err)
			continue
		}
		var t RestoreTest
		if err := json.Unmarshal(data, &t); err != nil {
			log.Printf("[store] skipping undecodable restore-test entry %s: %v", e.Name(), err)
			continue
		} else {
			tests = append(tests, t)
		}
	}
	return tests, nil
}

func (s *FileStore) GetRestoreTest(id string) (*RestoreTest, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("invalid restore test id %q", id)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(filepath.Join(s.dir, "restore-tests", id+".json"))
	if err != nil {
		return nil, err
	}
	var t RestoreTest
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *FileStore) SaveRestoreTest(t RestoreTest) error {
	if !ValidID(t.ID) {
		return fmt.Errorf("invalid restore test id %q", t.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, "restore-tests", t.ID+".json"), data, 0644)
}

func (s *FileStore) DeleteRestoreTest(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid restore test id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(filepath.Join(s.dir, "restore-tests", id+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SaveRestoreTestResult applies only the Last* fields of `result` onto the
// stored configuration (A15): config edits made while the run was in flight
// survive, and a deleted test stays deleted.
func (s *FileStore) SaveRestoreTestResult(id string, result RestoreTest) (bool, error) {
	if !ValidID(id) {
		return false, fmt.Errorf("invalid restore test id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.dir, "restore-tests", id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // deleted mid-run: do not resurrect
		}
		return false, err
	}
	var stored RestoreTest
	if err := json.Unmarshal(data, &stored); err != nil {
		return false, fmt.Errorf("reading stored restore test: %w", err)
	}
	// A24: a result only applies to the configuration that produced it. If
	// the test was retargeted (or deleted and recreated) while the run was
	// in flight, the stored identity no longer matches and the result is
	// dropped rather than attributed to the new target. F037: region is
	// part of the identity — the same bucket name in a different region is
	// a different target.
	if stored.Server != result.Server || stored.App != result.App || stored.Accessory != result.Accessory ||
		stored.Bucket != result.Bucket || stored.Region != result.Region {
		return false, nil
	}
	stored.LastRunAt = result.LastRunAt
	stored.LastOK = result.LastOK
	stored.LastDetail = result.LastDetail
	stored.LastMetric = result.LastMetric
	stored.LastDate = result.LastDate
	stored.LastDurationMs = result.LastDurationMs
	out, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return false, err
	}
	return true, atomicWrite(path, out, 0644)
}

func (s *FileStore) SaveCheck(result CheckResult) error {
	if !ValidID(result.MonitorID) {
		return fmt.Errorf("invalid monitor id %q", result.MonitorID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(
		filepath.Join(s.dir, "history", result.MonitorID+".jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644,
	)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%s\n", data)
	return err
}

func (s *FileStore) GetChecks(monitorID string, since time.Time, limit int) ([]CheckResult, error) {
	if !ValidID(monitorID) {
		return nil, fmt.Errorf("invalid monitor id %q", monitorID)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	f, err := os.Open(filepath.Join(s.dir, "history", monitorID+".jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var results []CheckResult
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxRecordBytes)
	for scanner.Scan() {
		var r CheckResult
		if json.Unmarshal(scanner.Bytes(), &r) == nil {
			if r.CheckedAt.After(since) {
				results = append(results, r)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// A read failure (oversized or unreadable record) must surface, not
		// silently return a truncated history as if it were complete (A31).
		return nil, fmt.Errorf("reading history for %s: %w", monitorID, err)
	}

	// Return the most recent N, newest-first — matching the Nucleus store's
	// `ORDER BY checked_at DESC` so callers (and the UI) see a consistent order
	// regardless of backend. The unlimited path (limit==0, used by GetStats) is
	// left ascending since it only aggregates.
	if limit > 0 {
		if len(results) > limit {
			results = results[len(results)-limit:]
		}
		for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
			results[i], results[j] = results[j], results[i]
		}
	}
	return results, nil
}

func (s *FileStore) GetStats(monitorID string, since time.Time) (*UptimeStats, error) {
	checks, err := s.GetChecks(monitorID, since, 0)
	if err != nil {
		return nil, err
	}

	stats := &UptimeStats{MonitorID: monitorID}
	var totalResponse time.Duration

	for _, c := range checks {
		stats.TotalChecks++
		if c.Status == "up" {
			stats.UpChecks++
			// Average only successful-check latency; a down check's response
			// time is noise (often 0 or a timeout) and skews the average.
			totalResponse += c.ResponseTime
		} else {
			stats.DownChecks++
		}
	}

	if stats.TotalChecks > 0 {
		stats.UptimePercent = float64(stats.UpChecks) / float64(stats.TotalChecks) * 100
	}
	if stats.UpChecks > 0 {
		stats.AvgResponse = totalResponse / time.Duration(stats.UpChecks)
	}

	return stats, nil
}

// Cleanup removes check history older than RetentionDays.
// Should be called periodically (e.g. daily).
//
// The rewrite is checked end to end (A31): every scan, decode, write, sync,
// and close error aborts THAT file's rewrite and preserves the original — the
// previous version ignored all of them and renamed a possibly-truncated
// temporary file over good history (an oversized record silently reduced a
// 71KB file to 20 bytes in the audit's reproduction).
func (s *FileStore) Cleanup() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -RetentionDays)

	entries, err := os.ReadDir(filepath.Join(s.dir, "history"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var firstErr error
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}

		path := filepath.Join(s.dir, "history", e.Name())

		if err := rewriteHistoryFile(path, cutoff); err != nil {
			// One bad file must not stop the others from being cleaned, but
			// the failure is reported rather than swallowed.
			if firstErr == nil {
				firstErr = fmt.Errorf("cleanup %s: %w", path, err)
			}
		}
	}
	return firstErr
}

// rewriteHistoryFile rewrites one history file without records older than
// cutoff, preserving the original on any failure.
//
// Retained records are copied as their ORIGINAL bytes (A21): re-encoding a
// decoded struct drops unknown future fields, and the old corrupt-record
// branch re-encoded the ZERO-VALUE of a failed decode — replacing evidence
// with a fabricated record whose zero timestamp a later cleanup then
// dropped, destroying data while claiming to preserve it. A malformed line
// now aborts the whole rewrite (the original stays intact and the error is
// reported for operator repair).
func rewriteHistoryFile(path string, cutoff time.Time) error {
	inFile, err := os.Open(path)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".cleanup-*")
	if err != nil {
		inFile.Close()
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		inFile.Close()
		if !committed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	scanner := bufio.NewScanner(inFile)
	scanner.Buffer(make([]byte, 64<<10), maxRecordBytes)
	writer := bufio.NewWriter(tmp)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r CheckResult
		if err := json.Unmarshal(line, &r); err != nil || r.CheckedAt.IsZero() {
			return fmt.Errorf("malformed history record (%d bytes); original retained", len(line))
		}
		if r.CheckedAt.Before(cutoff) {
			continue
		}
		if _, err := writer.Write(line); err != nil {
			return err
		}
		if err := writer.WriteByte('\n'); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// atomicWrite writes data to path via a same-directory temp file + rename, so
// a partial write can never replace a complete file (A32).
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// F004 discipline: the rename is only durable once the directory entry
	// is synced too.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *FileStore) Close() error {
	return nil
}
