package operation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Retention defaults. The journal is append-only per operation and compacted
// only when its byte cap is crossed; records and journals of terminal
// operations are deleted once older than the age cap or when the total
// operation count exceeds the count cap (useteploy__teploy-dash-04).
const (
	defaultMaxJournalBytes = 4 << 20
	defaultMaxHistoryAge   = 30 * 24 * time.Hour
	defaultMaxOperations   = 5000
)

type fileStore struct {
	recordsDir string
	eventsDir  string
	maxEvents  int
	// maxJournalBytes caps one operation's on-disk journal before it is
	// compacted down to the retained suffix.
	maxJournalBytes int64
	// mu serializes journal mutation (append, compaction, truncation,
	// deletion) against loads, so a load never observes a half-written or
	// mid-compaction file. Appends hold it only for the duration of one
	// write(2).
	mu       sync.Mutex
	journals map[string]*openJournal
}

// openJournal is one operation's append handle plus its tracked size.
type openJournal struct {
	file *os.File
	size int64
}

// journalConfig carries the retention knobs openFileStore needs; the Manager
// fills defaults from Options before constructing the store.
type journalConfig struct {
	MaxEvents       int
	MaxJournalBytes int64
}

func openFileStore(dataDir string, cfg journalConfig) (*fileStore, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("operation data directory is required")
	}
	if cfg.MaxJournalBytes <= 0 {
		cfg.MaxJournalBytes = defaultMaxJournalBytes
	}
	root := filepath.Join(dataDir, "operations")
	s := &fileStore{
		recordsDir:      filepath.Join(root, "records"),
		eventsDir:       filepath.Join(root, "events"),
		maxEvents:       cfg.MaxEvents,
		maxJournalBytes: cfg.MaxJournalBytes,
		journals:        make(map[string]*openJournal),
	}
	for _, dir := range []string{s.recordsDir, s.eventsDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	return s, nil
}

type persistedOperation struct {
	Operation
	RequestHash string `json:"request_hash,omitempty"`
}

func (s *fileStore) loadOperations() (map[string]*Operation, error) {
	entries, err := os.ReadDir(s.recordsDir)
	if err != nil {
		return nil, err
	}
	operations := make(map[string]*Operation)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.recordsDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var stored persistedOperation
		if err := json.Unmarshal(data, &stored); err != nil {
			return nil, fmt.Errorf("load operation %s: %w", entry.Name(), err)
		}
		fileID := strings.TrimSuffix(entry.Name(), ".json")
		if !operationIDPattern.MatchString(stored.ID) || stored.ID != fileID {
			return nil, fmt.Errorf("load operation %s: invalid operation id", entry.Name())
		}
		stored.Operation.requestHash = stored.RequestHash
		operations[stored.ID] = &stored.Operation
	}
	return operations, nil
}

func (s *fileStore) saveOperation(op *Operation) error {
	stored := persistedOperation{Operation: *cloneOperation(op), RequestHash: op.requestHash}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.recordsDir, op.ID+".json"), append(data, '\n'))
}

// appendEvent appends one event to the operation's journal, creating the file
// on first use. The write is a single append(2) under the store mutex — no
// per-event rewrite, fsync, or directory sync. Events are advisory history:
// the operation RECORD is the commit point (A25), so a crash may lose the
// journal tail, which replay surfaces as a gap event rather than hiding.
// When the tracked size would cross the byte cap the journal is compacted to
// the retained suffix first, so the file stays bounded.
func (s *fileStore) appendEvent(id string, event Event) error {
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.journalLocked(id)
	if err != nil {
		return err
	}
	if j.size+int64(len(line)) > s.maxJournalBytes {
		if err := s.compactLocked(id); err != nil {
			return err
		}
		j, err = s.journalLocked(id)
		if err != nil {
			return err
		}
	}
	n, err := j.file.Write(line)
	if n > 0 {
		j.size += int64(n)
	}
	return err
}

// journalLocked returns (opening if needed) the append handle for id.
func (s *fileStore) journalLocked(id string) (*openJournal, error) {
	if j, ok := s.journals[id]; ok {
		return j, nil
	}
	file, err := os.OpenFile(s.journalPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	j := &openJournal{file: file, size: info.Size()}
	s.journals[id] = j
	return j, nil
}

// compactLocked rewrites the journal down to the longest suffix that fits both
// the event-count and byte caps (always keeping at least the newest event).
// The rewrite is the same temp+sync+rename atomicWrite records use; it runs
// once per cap crossing, not per event.
func (s *fileStore) compactLocked(id string) error {
	events, _, err := s.scanLocked(id)
	if err != nil && events == nil {
		return err
	}
	s.closeJournalLocked(id)
	keep := retainedSuffix(events, s.maxEvents, s.maxJournalBytes)
	var buf bytes.Buffer
	for _, event := range keep {
		line, err := json.Marshal(event)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return atomicWrite(s.journalPath(id), buf.Bytes())
}

// retainedSuffix picks the longest suffix of events that has at most maxEvents
// entries and encodes to at most maxBytes, always keeping the newest entry.
func retainedSuffix(events []Event, maxEvents int, maxBytes int64) []Event {
	if len(events) <= 1 {
		return events
	}
	kept := events
	if maxEvents > 0 && len(kept) > maxEvents {
		kept = kept[len(kept)-maxEvents:]
	}
	for len(kept) > 1 {
		var size int64
		for _, event := range kept {
			line, err := json.Marshal(event)
			if err != nil {
				continue
			}
			size += int64(len(line)) + 1
		}
		if size <= maxBytes {
			break
		}
		kept = kept[1:]
	}
	return kept
}

// closeJournalLocked closes and forgets the append handle for id, if open.
func (s *fileStore) closeJournalLocked(id string) {
	if j, ok := s.journals[id]; ok {
		j.file.Close()
		delete(s.journals, id)
	}
}

// CloseJournal releases the append handle for a finished operation so long
// histories do not pin file descriptors; a later append reopens on demand.
func (s *fileStore) CloseJournal(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeJournalLocked(id)
}

func (s *fileStore) journalPath(id string) string {
	return filepath.Join(s.eventsDir, id+".jsonl")
}

// loadEvents replays the operation's journal. Synthetic EventGap entries are
// inserted wherever history is known to be missing: a sequence hole, or a
// torn/corrupt tail (unclean shutdown or external damage) — the file is
// truncated at the damage point so future appends continue from a clean end.
// A missing journal is an empty history, not an error.
func (s *fileStore) loadEvents(id string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events, gaps, err := s.scanLocked(id)
	if len(gaps) > 0 {
		events = append(events, gaps...)
	}
	return events, err
}

// scanLocked reads the journal once, detecting holes and tail damage. It
// returns the parsed events plus gap markers for position-aware callers, and
// truncates the file at the damage offset when the tail is corrupt so the
// journal never grows behind a dead line (a gap marker is emitted once per
// load; appends after truncation extend the clean prefix).
func (s *fileStore) scanLocked(id string) ([]Event, []Event, error) {
	path := s.journalPath(id)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	var events []Event
	var gaps []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var offset int64
	var lastSeq uint64
	damage := ""
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			offset += int64(len(line)) + 1
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil || event.OperationID != id {
			damage = fmt.Sprintf("journal corrupt at byte %d", offset)
			break
		}
		if len(events) > 0 && event.Sequence != lastSeq+1 {
			missing := event.Sequence - lastSeq - 1
			gaps = append(gaps, Event{
				Sequence: lastSeq + 1, OperationID: id, Type: EventGap,
				Data: fmt.Sprintf("%d event(s) missing from the journal", missing),
			})
		}
		lastSeq = event.Sequence
		events = append(events, event)
		offset += int64(len(line)) + 1
	}
	if err := scanner.Err(); err != nil {
		damage = fmt.Sprintf("journal unreadable at byte %d (%v)", offset, err)
	}
	if damage != "" {
		s.closeJournalLocked(id)
		if truncErr := os.Truncate(path, offset); truncErr != nil {
			log.Printf("[operation] journal %s: %v; truncate failed: %v", id, damage, truncErr)
		}
		gaps = append(gaps, Event{
			Sequence: lastSeq + 1, OperationID: id, Type: EventGap,
			Data: damage + "; later events were lost",
		})
	}
	return events, gaps, nil
}

// deleteOperation removes an operation's record and journal entirely
// (retention). In-memory state is the caller's responsibility.
func (s *fileStore) deleteOperation(id string) error {
	s.mu.Lock()
	s.closeJournalLocked(id)
	s.mu.Unlock()
	var errs []string
	if err := os.Remove(filepath.Join(s.recordsDir, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err.Error())
	}
	if err := os.Remove(s.journalPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("delete operation %s: %s", id, strings.Join(errs, "; "))
	}
	return nil
}

// sweepRetention deletes terminal operations past the retention caps and
// returns their ids. maxAge removes terminal operations older than the cutoff;
// maxCount bounds the total record count by removing the oldest terminal
// operations first. Non-terminal operations are never touched.
func (s *fileStore) sweepRetention(now time.Time, maxAge time.Duration, maxCount int) []string {
	type candidate struct {
		id        string
		createdAt time.Time
		terminal  bool
	}
	entries, err := os.ReadDir(s.recordsDir)
	if err != nil {
		return nil
	}
	var records []candidate
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.recordsDir, entry.Name()))
		if err != nil {
			continue
		}
		var stored persistedOperation
		if err := json.Unmarshal(data, &stored); err != nil {
			continue
		}
		records = append(records, candidate{
			id:        stored.ID,
			createdAt: stored.CreatedAt,
			terminal:  stored.Status.Terminal(),
		})
	}
	var removed []string
	for _, rec := range records {
		if rec.terminal && maxAge > 0 && now.Sub(rec.createdAt) > maxAge {
			if err := s.deleteOperation(rec.id); err == nil {
				removed = append(removed, rec.id)
			}
		}
	}
	if maxCount > 0 {
		var terminal []candidate
		for _, rec := range records {
			if rec.terminal && now.Sub(rec.createdAt) <= maxAge {
				terminal = append(terminal, rec)
			}
		}
		sort.Slice(terminal, func(i, j int) bool {
			if terminal[i].createdAt.Equal(terminal[j].createdAt) {
				return terminal[i].id < terminal[j].id
			}
			return terminal[i].createdAt.Before(terminal[j].createdAt)
		})
		for _, rec := range terminal {
			if len(records)-len(removed) <= maxCount {
				break
			}
			if err := s.deleteOperation(rec.id); err == nil {
				removed = append(removed, rec.id)
			}
		}
	}
	return removed
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
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
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
