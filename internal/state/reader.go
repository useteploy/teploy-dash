// Package state reads the per-app state files that the teploy CLI writes on
// each server: /deployments/<app>/state.json (schema v2+, every current CLI)
// and, for apps last deployed by an old CLI, the legacy
// /deployments/<app>/state. state.json wins when both exist.
//
// Legacy format: a plain key=value text file:
//
//	current_port=49153
//	current_hash=v4
//	previous_port=49152
//	previous_hash=v3
//	domain=app.example.com
//
// See useteploy/teploy-cli internal/state/state.go for the canonical writer.
package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AppState represents the deployment state of an app (written by teploy CLI).
type AppState struct {
	App          string `json:"app"`
	CurrentHash  string `json:"current_hash"`
	PreviousHash string `json:"previous_hash"`
	Port         int    `json:"port"`
	Domain       string `json:"domain"`
	DeployedAt   string `json:"deployed_at"`
	Status       string `json:"status"`
}

// Reader reads teploy CLI state files from the deployments directory.
type Reader struct {
	dir string
}

// NewReader creates a state reader for the given deployments directory.
func NewReader(deploymentsDir string) *Reader {
	return &Reader{dir: deploymentsDir}
}

// ListApps returns all deployed apps by reading per-app state files.
// Apps with missing or unparseable state files are skipped silently.
func (r *Reader) ListApps() ([]AppState, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var apps []AppState
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := readAppDir(filepath.Join(r.dir, entry.Name()))
		if err != nil {
			continue // skip apps without (or with unreadable) state, and tombstones
		}
		if state.App == "" {
			state.App = entry.Name()
		}
		apps = append(apps, *state)
	}

	sort.Slice(apps, func(i, j int) bool { return apps[i].App < apps[j].App })
	return apps, nil
}

// GetApp returns the state for a specific app.
func (r *Reader) GetApp(name string) (*AppState, error) {
	state, err := readAppDir(filepath.Join(r.dir, name))
	if err != nil {
		return nil, err
	}
	if state.App == "" {
		state.App = name
	}
	return state, nil
}

// ErrTombstone marks a state.json the CLI keeps for a removed or never-
// deployed app (schema v3 `tombstone: true`); readers treat the app as gone.
var ErrTombstone = errors.New("app is tombstoned")

// MaxStateSchema is the newest state.json schema this dash understands.
const MaxStateSchema = 3

// readAppDir reads state.json when present, else the legacy state file.
func readAppDir(dir string) (*AppState, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		return ParseJSON(raw)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	return parseStateFile(filepath.Join(dir, "state"))
}

// ParseJSON reads the CLI's state.json. A newer schema than MaxStateSchema
// is refused rather than guessed at; a tombstone returns ErrTombstone.
func ParseJSON(raw []byte) (*AppState, error) {
	var doc struct {
		SchemaVersion int       `json:"schema_version"`
		Tombstone     bool      `json:"tombstone"`
		CurrentHash   string    `json:"current_hash"`
		PreviousHash  string    `json:"previous_hash"`
		CurrentPort   int       `json:"current_port"`
		Domain        string    `json:"domain"`
		UpdatedAt     time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing state.json: %w", err)
	}
	if doc.SchemaVersion > MaxStateSchema {
		return nil, fmt.Errorf("unsupported state schema version %d (this dash reads up to %d)", doc.SchemaVersion, MaxStateSchema)
	}
	if doc.Tombstone {
		return nil, ErrTombstone
	}
	s := &AppState{CurrentHash: doc.CurrentHash, PreviousHash: doc.PreviousHash, Port: doc.CurrentPort, Domain: doc.Domain}
	if !doc.UpdatedAt.IsZero() {
		s.DeployedAt = doc.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return s, nil
}

// parseStateFile reads the CLI's key=value state file from disk.
func parseStateFile(path string) (*AppState, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads the CLI's key=value state format from r. Unknown keys are
// ignored (the CLI may add fields like previous_port / current_ports for
// replicas which we don't surface in the dashboard list view).
//
// This is the single canonical parser for the CLI state format — the SSH
// fleet path (internal/remote) reads the file over the wire and feeds the
// raw bytes here, so the two paths can't drift on key names.
func Parse(r io.Reader) (*AppState, error) {
	s := &AppState{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, val, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "current_hash":
			s.CurrentHash = val
		case "previous_hash":
			s.PreviousHash = val
		case "current_port":
			if n, err := strconv.Atoi(val); err == nil {
				s.Port = n
			}
		case "domain":
			s.Domain = val
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return s, nil
}
