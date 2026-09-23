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

// The inventory reflects the CLI surface this branch was built against:
// only stop/start/logs (and verify-backup via restore tests) are reachable
// in server-state mode, so NO D05 class is executable from dash today —
// and the endpoint must say that rather than wire closest-match commands.
// When a class flips to supported, this assertion flips with it (and the
// grounding comment in dbactions.go is updated in the same change).
func TestDatabaseActionInventoryMatchesCLISurface(t *testing.T) {
	for _, a := range databaseActionInventory() {
		if a.Supported {
			t.Fatalf("action %q marked supported; no D05 database-action class is CLI-reachable from dash's server-state mode today — update this test together with the CLI surface", a.ID)
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
