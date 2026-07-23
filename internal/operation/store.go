package operation

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type fileStore struct {
	recordsDir string
	eventsDir  string
	maxEvents  int
}

func openFileStore(dataDir string, maxEvents int) (*fileStore, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("operation data directory is required")
	}
	root := filepath.Join(dataDir, "operations")
	s := &fileStore{
		recordsDir: filepath.Join(root, "records"),
		eventsDir:  filepath.Join(root, "events"),
		maxEvents:  maxEvents,
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

func (s *fileStore) loadEvents(id string) ([]Event, error) {
	file, err := os.Open(filepath.Join(s.eventsDir, id+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var events []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}

func (s *fileStore) saveEvents(id string, events []Event) error {
	if len(events) > s.maxEvents {
		events = events[len(events)-s.maxEvents:]
	}
	var data strings.Builder
	encoder := json.NewEncoder(&data)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			return err
		}
	}
	return atomicWrite(filepath.Join(s.eventsDir, id+".jsonl"), []byte(data.String()))
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
