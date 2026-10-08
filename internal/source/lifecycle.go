package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/useteploy/teploy-dash/internal/durable"
	"io"
	"os"
	"path/filepath"
)

// Lifecycle survives delivery/operation retention. A watermark is a recoverable
// admission intent: retrying exactly that intent is allowed after partial batch
// admission or journal failure; an older intent never replaces it.
type Lifecycle struct {
	LegacyAuditRequired bool                        `json:"legacy_audit_required,omitempty"`
	Version             int                         `json:"schema"`
	Watermarks          map[string]Watermark        `json:"watermarks"`
	Previews            map[string]PreviewOwnership `json:"previews"`
}
type Watermark struct {
	UpdatedAt string `json:"updated_at"`
	Commit    string `json:"commit"`
	Closed    bool   `json:"closed"`
	Binding   string `json:"binding,omitempty"`
	Complete  bool   `json:"complete,omitempty"`
}
type PreviewOwnership struct {
	ID       string        `json:"id"`
	ServerID string        `json:"server_id"`
	Server   string        `json:"server"`
	App      string        `json:"app"`
	Branch   string        `json:"branch"`
	Pull     int           `json:"pull"`
	Policy   PreviewPolicy `json:"policy"`
	Manifest string        `json:"manifest"`
}

func (s *Store) Lifecycle(id string) (*Lifecycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadSource(id); err != nil {
		return nil, err
	}
	return s.lifecycleLocked(id)
}
func (s *Store) lifecycleLocked(id string) (*Lifecycle, error) {
	state := &Lifecycle{Version: 1, LegacyAuditRequired: true, Watermarks: map[string]Watermark{}, Previews: map[string]PreviewOwnership{}}
	b, err := os.ReadFile(filepath.Join(s.dir(id), "lifecycle.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	var extra json.RawMessage
	if len(b) > 16<<20 || decoder.Decode(state) != nil || decoder.Decode(&extra) != io.EOF || state.Version != 1 || state.Watermarks == nil || state.Previews == nil {
		return nil, fmt.Errorf("source lifecycle authority invalid")
	}
	return state, nil
}

// SaveLifecycle replaces one serialized source admission transaction. Server's
// sourceActionMu owns read/modify/write and excludes source deletion.
func (s *Store) SaveLifecycle(id string, state *Lifecycle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadSource(id); err != nil {
		return err
	}
	if state == nil || state.Version != 1 || state.Watermarks == nil || state.Previews == nil {
		return fmt.Errorf("source lifecycle authority invalid")
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(b) > 16<<20 {
		return fmt.Errorf("source lifecycle authority exceeds storage budget; reconcile cleanup")
	}
	return durable.Replace(filepath.Join(s.dir(id), "lifecycle.json"), append(b, '\n'), 0600)
}
