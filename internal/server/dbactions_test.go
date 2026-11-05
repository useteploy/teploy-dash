package server

// D05: database actions as DISTINCT operations. The invariants pinned
// here are the no-pretend rules: every D05 class appears exactly once,
// every unsupported entry carries a non-empty remedy (what to run and
// where), and nothing unsupported grows a dash action wired to a
// closest-match command.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDatabaseActionInventoryCoversD05Classes(t *testing.T) {
	actions := databaseActionInventory()
	want := []string{"restart", "version-upgrade", "credential-rotation", "data-restore", "destructive-removal"}
	if len(actions) != len(want) {
		t.Fatalf("inventory has %d actions, want the 5 D05 classes", len(actions))
	}
	seen := map[string]bool{}
	for _, a := range actions {
		if seen[a.ID] {
			t.Fatalf("duplicate action class %q", a.ID)
		}
		seen[a.ID] = true
		if a.Label == "" || a.BlastRadius == "" {
			t.Fatalf("action %q must carry a label and a blast-radius description", a.ID)
		}
		if a.Supported {
			// The no-pretend rule, mechanically: a supported entry must
			// name its real execution path; an unsupported one must not
			// have one dangling off it.
			if a.Command == "" || a.DashAction == "" {
				t.Fatalf("supported action %q must name its CLI command and dash action", a.ID)
			}
		} else {
			if a.DashAction != "" {
				t.Fatalf("unsupported action %q must not advertise a dash action", a.ID)
			}
			if a.Remedy == "" {
				t.Fatalf("unsupported action %q must render a remedy (what to run instead, and where)", a.ID)
			}
			if a.Confirmation != "" {
				t.Fatalf("unsupported action %q must not carry a confirmation prompt (nothing executes)", a.ID)
			}
		}
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("D05 class %q missing from the inventory", id)
		}
	}
}

// The inventory reflects the CLI surface this branch was built against
// (v0.1.52): data-restore is reachable in server-state mode — Dash's
// Backups tab runs `backup restore` as a durable operation, with a single
// database (--database) and the --no-backup escape hatch — while the other
// classes are not. When a class flips, this assertion flips with it (and the
// grounding comment in dbactions.go is updated in the same change).
func TestDatabaseActionInventoryMatchesCLISurface(t *testing.T) {
	supported := map[string]bool{}
	for _, a := range databaseActionInventory() {
		if a.Supported {
			supported[a.ID] = true
		}
	}
	if !supported["data-restore"] {
		t.Fatal("data-restore is wired from dash (backups/restore) and must be marked supported")
	}
	if len(supported) != 1 {
		t.Fatalf("only data-restore is CLI-reachable from dash's server-state mode today: %v — update this test together with the CLI surface", supported)
	}
	for _, a := range databaseActionInventory() {
		if a.ID != "data-restore" {
			continue
		}
		for _, flag := range []string{"--database", "--no-backup"} {
			if !strings.Contains(a.Command, flag) {
				t.Errorf("the restore command must name the v0.1.52 flags (%s missing): %s", flag, a.Command)
			}
		}
		if a.DashAction != "backups/restore" {
			t.Errorf("dash action = %s", a.DashAction)
		}
	}
}

func TestDatabaseActionsEndpointServesInventory(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	rec := httptest.NewRecorder()
	s.handleDatabaseActions(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/db-actions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data struct {
			Actions []dbAction `json:"actions"`
			Source  string     `json:"source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Actions) != 5 || envelope.Data.Source == "" {
		t.Fatalf("envelope = %#v", envelope.Data)
	}
	if rec.Header().Get("Content-Type") == "" {
		t.Fatal("content type must be set")
	}
}
