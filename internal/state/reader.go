package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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

// ListApps returns all deployed apps by reading state files.
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

		stateFile := filepath.Join(r.dir, entry.Name(), "state.json")
		data, err := os.ReadFile(stateFile)
		if err != nil {
			continue // skip apps without state files
		}

		var state AppState
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}

		if state.App == "" {
			state.App = entry.Name()
		}
		apps = append(apps, state)
	}

	sort.Slice(apps, func(i, j int) bool {
		return apps[i].App < apps[j].App
	})

	return apps, nil
}

// GetApp returns the state for a specific app.
func (r *Reader) GetApp(name string) (*AppState, error) {
	stateFile := filepath.Join(r.dir, name, "state.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, err
	}

	var state AppState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	if state.App == "" {
		state.App = name
	}
	return &state, nil
}
