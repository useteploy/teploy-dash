// Package state reads the per-app state files that the teploy CLI writes to
// /deployments/<app>/state on each server.
//
// Format note: the CLI writes a plain key=value text file (NOT JSON):
//
//	current_port=49153
//	current_hash=v4
//	previous_port=49152
//	previous_hash=v3
//	domain=app.example.com
//
// See useteploy/teploy-cli internal/state/state.go Write() for the canonical
// writer. Previously this reader expected `state.json` with JSON contents,
// which silently returned an empty app list (the file never existed).
package state

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
		state, err := parseStateFile(filepath.Join(r.dir, entry.Name(), "state"))
		if err != nil {
			continue // skip apps without state files
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
	state, err := parseStateFile(filepath.Join(r.dir, name, "state"))
	if err != nil {
		return nil, err
	}
	if state.App == "" {
		state.App = name
	}
	return state, nil
}

// parseStateFile reads the CLI's key=value state format. Unknown keys are
// ignored (the CLI may add fields like current_ports / previous_ports for
// replicas which we don't surface in the dashboard list view).
func parseStateFile(path string) (*AppState, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := &AppState{}
	scanner := bufio.NewScanner(f)
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
