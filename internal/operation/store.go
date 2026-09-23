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

// currentRecordVersion is the operation record schema version this build
// writes and fully understands (X02 §5 row 6). Absent/0 means version 1 —
// records from before the field existed. A record carrying a HIGHER
// version cannot be safely interpreted or rewritten: the manager starts
// read-only instead of executing or mutating records it may corrupt.
const currentRecordVersion = 2

type persistedOperation struct {
	Operation
	RequestHash   string `json:"request_hash,omitempty"`
	RecordVersion int    `json:"record_version,omitempty"`
}

// futureRecord names one stored operation record whose record_version
// exceeds currentRecordVersion.
type futureRecord struct {
	File    string
	Version int
}

func (s *fileStore) loadOperations() (map[string]*Operation, []futureRecord, error) {
	entries, err := os.ReadDir(s.recordsDir)
	if err != nil {
		return nil, nil, err
	}
	operations := make(map[string]*Operation)
	var future []futureRecord
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.recordsDir, entry.Name()))
		if err != nil {
			return nil, nil, err
		}
		var stored persistedOperation
		if err := json.Unmarshal(data, &stored); err != nil {
			return nil, nil, fmt.Errorf("load operation %s: %w", entry.Name(), err)
		}
		fileID := strings.TrimSuffix(entry.Name(), ".json")
		if !operationIDPattern.MatchString(stored.ID) || stored.ID != fileID {
			return nil, nil, fmt.Errorf("load operation %s: invalid operation id", entry.Name())
		}
		if stored.RecordVersion > currentRecordVersion {
			// Readable enough to display, not safe to mutate: flag it and
			// let the manager refuse mutations (X02 §5 row 6's
			// refuse-downgrade rule — never a hard startup failure, which
			// would take the reads away too).
			future = append(future, futureRecord{File: entry.Name(), Version: stored.RecordVersion})
		}
		stored.Operation.requestHash = stored.RequestHash
		operations[stored.ID] = &stored.Operation
	}
	return operations, future, nil
}

func (s *fileStore) saveOperation(op *Operation) error {
	stored := persistedOperation{Operation: *cloneOperation(op), RequestHash: op.requestHash, RecordVersion: currentRecordVersion}
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
// When the tracked size would cross the byte cap the journal is compacted
// first (F018: to a LOWER watermark that reserves the incoming record, so
// the post-compaction file plus the append is back under the cap and the
// next append does not immediately cross it again).
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
		if err := s.compactLocked(id, line); err != nil {
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

// journalLocked returns (opening if needed) the append handle for id. When
// an existing journal does not end in '\n' (a valid final record from an
// unclean shutdown), the newline is appended BEFORE the handle is reused —
// otherwise the next event would concatenate a second JSON object onto that
// record and corrupt the journal permanently (F017).
func (s *fileStore) journalLocked(id string) (*openJournal, error) {
	if j, ok := s.journals[id]; ok {
		return j, nil
	}
	path := s.journalPath(id)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	size := info.Size()
	if size > 0 {
		appended, err := ensureTrailingNewline(path, size)
		if err != nil {
			file.Close()
			return nil, err
		}
		if appended {
			size++
		}
	}
	j := &openJournal{file: file, size: size}
	s.journals[id] = j
	return j, nil
}

// ensureTrailingNewline appends '\n' to path when its last byte is not
// already a newline. size is the current file size (caller just stat'ed).
// Reports whether a newline was appended.
func ensureTrailingNewline(path string, size int64) (bool, error) {
	reader, err := os.Open(path)
	if err != nil {
		return false, err
	}
	var last [1]byte
	_, rerr := reader.ReadAt(last[:], size-1)
	cerr := reader.Close()
	if rerr != nil || cerr != nil {
		return false, errors.Join(rerr, cerr)
	}
	if last[0] == '\n' {
		return false, nil
	}
	writer, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	_, werr := writer.Write([]byte{'\n'})
	wcerr := writer.Close()
	if err := errors.Join(werr, wcerr); err != nil {
		return false, err
	}
	log.Printf("[operation] journal %s: appended missing final newline before write", filepath.Base(path))
	return true, nil
}

// compactLocked rewrites the journal down to the longest suffix that fits
// both the event-count cap and a byte budget derived from the cap's LOWER
// watermark minus the incoming record (F018), always keeping the newest
// event — including when that single event alone exceeds the budget (a
// deliberate policy: the payload bound at ingestion is 16 KiB, so this only
// arises for pathologically small configured caps, and dropping the newest
// event would lose the live tail). The rewrite is the same
// temp+sync+rename atomicWrite records use; it runs once per cap crossing,
// not per event.
func (s *fileStore) compactLocked(id string, incoming []byte) error {
	events, _, err := s.scanLocked(id)
	if err != nil {
		return err
	}
	s.closeJournalLocked(id)
	budget := s.maxJournalBytes*3/4 - int64(len(incoming))
	keep := retainedSuffix(events, s.maxEvents, budget)
	var buf bytes.Buffer
	buf.Grow(int(min64(int64(budget), s.maxJournalBytes)))
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

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// retainedSuffix picks the longest suffix of events that has at most maxEvents
// entries and encodes to at most budget bytes, always keeping the newest
// entry. Sizes are computed once (F018 — the previous re-serialization per
// candidate suffix turned every compaction into quadratic work).
func retainedSuffix(events []Event, maxEvents int, budget int64) []Event {
	if len(events) <= 1 {
		return events
	}
	kept := events
	if maxEvents > 0 && len(kept) > maxEvents {
		kept = kept[len(kept)-maxEvents:]
	}
	if budget < 0 {
		budget = 0
	}
	sizes := make([]int64, len(kept))
	total := int64(0)
	for i, event := range kept {
		line, err := json.Marshal(event)
		if err != nil {
			continue
		}
		sizes[i] = int64(len(line)) + 1
		total += sizes[i]
	}
	start := 0
	for start < len(kept)-1 && total > budget {
		total -= sizes[start]
		start++
	}
	return kept[start:]
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

// discardJournal closes any cached append handle for id and REMOVES its
// journal file (F019). Used when admission failed before its record
// committed: the record is the commit point (A25) and recovery enumerates
// records, never event files, so this journal is a definite orphan —
// without this, repeated storage failures leaked both the descriptor (the
// cached handle was never closed) and history files invisible to retention.
func (s *fileStore) discardJournal(id string) {
	s.mu.Lock()
	s.closeJournalLocked(id)
	s.mu.Unlock()
	_ = os.Remove(s.journalPath(id))
}

func (s *fileStore) journalPath(id string) string {
	return filepath.Join(s.eventsDir, id+".jsonl")
}

// loadEvents replays the operation's journal. Synthetic EventGap entries are
// inserted wherever history is known to be missing: a sequence hole, or a
// torn/corrupt tail (unclean shutdown or external damage) — the file is
// truncated at the damage point so future appends continue from a clean end.
// F016: real events and gap markers are merged in SEQUENCE order, so the
// replay view is chronological and the manager's next-sequence derivation
// cannot rewind (a hole at 2 used to append the gap marker after the real
// tail, making the next emitted sequence collide with an existing one).
// A missing journal is an empty history, not an error.
func (s *fileStore) loadEvents(id string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events, gaps, err := s.scanLocked(id)
	if err != nil {
		return nil, err
	}
	if len(gaps) > 0 {
		events = append(events, gaps...)
		sort.Slice(events, func(i, j int) bool {
			return events[i].Sequence < events[j].Sequence
		})
	}
	return events, nil
}

// scanLocked reads the journal once, detecting holes and tail damage. It
// returns the parsed events plus gap markers, and truncates the file at the
// damage offset when the tail is corrupt so the journal never grows behind
// a dead line (a gap marker is emitted once per load; appends after
// truncation extend the clean prefix).
//
// F016: sequence numbers must be strictly increasing in an append-only
// journal. A zero, duplicate, or regressing sequence is treated as damage
// at that offset (it can only arise from external interference or the
// ordering bug this fixes) — without the check, a regressing sequence made
// the hole arithmetic underflow and the next append reuse an existing ID.
//
// F017: framing damage (unparseable record, wrong operation, oversized
// line) is distinct from a storage READ failure. Only the former truncates;
// an I/O error returns without touching the file — repairing a transient
// read failure by deleting bytes would destroy good history. A valid final
// record without its terminating newline is repaired by appending the
// newline (never by concatenating the next record onto it).
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
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := info.Size()

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
		if err := json.Unmarshal(line, &event); err != nil || event.OperationID != id || event.Sequence == 0 || event.Sequence <= lastSeq {
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
		if errors.Is(err, bufio.ErrTooLong) {
			// An oversized record is framing damage (payloads are bounded at
			// ingestion, so this is external); truncation at the record's
			// start offset is the same repair as any other corrupt frame.
			damage = fmt.Sprintf("journal record exceeds the size limit at byte %d", offset)
		} else {
			// Storage read failure: do NOT truncate (F017).
			return nil, nil, fmt.Errorf("reading journal %s at byte %d: %w", path, offset, err)
		}
	}
	if damage == "" && offset == size+1 {
		// The final record parsed cleanly but had no terminating newline
		// (F017). Append one so the next event append cannot concatenate a
		// second JSON object onto this record — which would corrupt the
		// journal permanently on its next read.
		if err := appendNewlineLocked(path); err != nil {
			return nil, nil, fmt.Errorf("repairing unterminated journal %s: %w", path, err)
		}
		log.Printf("[operation] journal %s: appended missing final newline", id)
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

// appendNewlineLocked appends a single '\n' to the journal file. Caller must
// hold s.mu (the repair runs inside scanLocked).
func appendNewlineLocked(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, werr := f.Write([]byte{'\n'})
	cerr := f.Close()
	return errors.Join(werr, cerr)
}

// Health probes whether the record and journal directories are still usable
// (present and owner-writable) for readiness reporting (A39/A47). It checks
// metadata only — no probe files are written.
func (s *fileStore) Health() error {
	for _, dir := range []string{s.recordsDir, s.eventsDir} {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("operation storage %s: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("operation storage %s is not a directory", dir)
		}
		if info.Mode().Perm()&0200 == 0 {
			return fmt.Errorf("operation storage %s is not writable", dir)
		}
	}
	return nil
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
// operations first. Non-terminal operations are never touched. F020: the two
// policies apply INDEPENDENTLY — the count pass previously filtered its
// candidates through the age predicate, so configuring age retention off
// (negative) silently disabled the count bound too.
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
	removedSet := make(map[string]bool)
	for _, rec := range records {
		if rec.terminal && maxAge > 0 && now.Sub(rec.createdAt) > maxAge {
			if err := s.deleteOperation(rec.id); err == nil {
				removed = append(removed, rec.id)
				removedSet[rec.id] = true
			}
		}
	}
	if maxCount > 0 {
		var terminal []candidate
		for _, rec := range records {
			if rec.terminal {
				terminal = append(terminal, rec)
			}
		}
		sort.Slice(terminal, func(i, j int) bool {
			if terminal[i].createdAt.Equal(terminal[j].createdAt) {
				return terminal[i].id < terminal[j].id
			}
			return terminal[i].createdAt.Before(terminal[j].createdAt)
		})
		remaining := len(records) - len(removed)
		for _, rec := range terminal {
			if remaining <= maxCount {
				break
			}
			if removedSet[rec.id] {
				continue
			}
			if err := s.deleteOperation(rec.id); err == nil {
				removed = append(removed, rec.id)
				removedSet[rec.id] = true
				remaining--
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
