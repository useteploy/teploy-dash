package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RetentionDays is how many days of check history to keep in JSONL files.
// 7 days keeps files small and fast to scan. Use Nucleus for longer retention.
const RetentionDays = 7

// FileStore implements Store using JSONL files. Fallback when Nucleus is not available.
type FileStore struct {
	dir string
	mu  sync.RWMutex
}

// NewFileStore creates a file-based store in the given directory.
func NewFileStore(dir string) *FileStore {
	os.MkdirAll(filepath.Join(dir, "monitors"), 0755)
	os.MkdirAll(filepath.Join(dir, "history"), 0755)
	return &FileStore{dir: dir}
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
			continue
		}
		var m Monitor
		if json.Unmarshal(data, &m) == nil {
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
	return os.WriteFile(filepath.Join(s.dir, "monitors", m.ID+".json"), data, 0644)
}

func (s *FileStore) DeleteMonitor(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid monitor id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	os.Remove(filepath.Join(s.dir, "monitors", id+".json"))
	os.Remove(filepath.Join(s.dir, "history", id+".jsonl"))
	return nil
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
	for scanner.Scan() {
		var r CheckResult
		if json.Unmarshal(scanner.Bytes(), &r) == nil {
			if r.CheckedAt.After(since) {
				results = append(results, r)
			}
		}
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
		} else {
			stats.DownChecks++
		}
		totalResponse += c.ResponseTime
	}

	if stats.TotalChecks > 0 {
		stats.UptimePercent = float64(stats.UpChecks) / float64(stats.TotalChecks) * 100
		stats.AvgResponse = totalResponse / time.Duration(stats.TotalChecks)
	}

	return stats, nil
}

// Cleanup removes check history older than RetentionDays.
// Should be called periodically (e.g. daily).
func (s *FileStore) Cleanup() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -RetentionDays)

	entries, err := os.ReadDir(filepath.Join(s.dir, "history"))
	if err != nil {
		return nil
	}

	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}

		path := filepath.Join(s.dir, "history", e.Name())
		tmpPath := path + ".tmp"

		inFile, err := os.Open(path)
		if err != nil {
			continue
		}

		outFile, err := os.Create(tmpPath)
		if err != nil {
			inFile.Close()
			continue
		}

		scanner := bufio.NewScanner(inFile)
		kept := 0
		for scanner.Scan() {
			var r CheckResult
			if json.Unmarshal(scanner.Bytes(), &r) == nil {
				if r.CheckedAt.After(cutoff) {
					outFile.Write(scanner.Bytes())
					outFile.Write([]byte("\n"))
					kept++
				}
			}
		}

		inFile.Close()
		outFile.Close()

		os.Rename(tmpPath, path)
	}

	return nil
}

func (s *FileStore) Close() error {
	return nil
}
