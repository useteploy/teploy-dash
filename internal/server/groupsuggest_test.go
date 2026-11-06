package server

// groupsuggest_test.go — Programme B item 7: the suggestion diff. Pure-core
// tests over fleet apps + groups.json shapes, plus the endpoint read.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/remote"
)

func TestGroupSuggestionsByRegistrableDomain(t *testing.T) {
	apps := []fleetAppLite{
		{App: "akiroo-lite", Server: "infra", Domain: "akiroo.com,livekit.akiroo.com"},
		{App: "akiroo-api", Server: "infra", Domain: "api.akiroo.com"},
		{App: "fylun-web", Server: "edge", Domain: "fylun.ai"},
	}
	sugg := groupSuggestions(apps, nil)
	if len(sugg) != 1 {
		t.Fatalf("suggestions = %+v, want only akiroo.com (fylun.ai is a singleton and drops)", sugg)
	}
	ak := sugg[0]
	if ak.Name != "akiroo.com" || ak.Basis != "domain" || len(ak.Apps) != 2 || ak.Exists {
		t.Fatalf("akiroo suggestion = %+v", ak)
	}
}

func TestGroupSuggestionsFallbackHyphenToken(t *testing.T) {
	apps := []fleetAppLite{
		{App: "fylun-web", Server: "edge"}, // no domains: fallback name token
		{App: "fylun-api", Server: "edge"},
		{App: "lullmail-web", Server: "edge"},
		{App: "plain", Server: "edge"}, // no hyphen, no domain: no suggestion
	}
	sugg := groupSuggestions(apps, nil)
	if len(sugg) != 1 || sugg[0].Name != "fylun" || sugg[0].Basis != "name" || len(sugg[0].Apps) != 2 {
		t.Fatalf("suggestions = %+v, want fylun (name basis, 2 apps)", sugg)
	}
}

// A domain beats the name token; several distinct registrable domains pick
// the first deterministically; placeholders and IP literals are not domains.
func TestGroupSuggestionsDomainBeatsNameAndJunkIgnored(t *testing.T) {
	apps := []fleetAppLite{
		{App: "fylun-web", Server: "edge", Domain: "shop.fylun.ai,app.other.org"},
		{App: "fylun-api", Server: "edge", Domain: "app.other.org"},
		{App: "docs-x", Server: "edge", Domain: "docs.example.com"}, // placeholder
		{App: "docs-y", Server: "edge", Domain: "192.0.2.10"},       // IP literal
		{App: "docs-z", Server: "edge", Domain: "docs.localhost"},   // placeholder suffix
	}
	sugg := groupSuggestions(apps, nil)
	if len(sugg) != 2 {
		t.Fatalf("suggestions = %+v, want other.org and docs (the placeholder/IP apps fall back to the name token)", sugg)
	}
	byName := map[string]groupSuggestion{}
	for _, s := range sugg {
		byName[s.Name] = s
	}
	if o := byName["other.org"]; len(o.Apps) != 2 {
		t.Fatalf("other.org suggestion = %+v", o)
	}
	if d := byName["docs"]; d.Basis != "name" || len(d.Apps) != 3 {
		t.Fatalf("docs name-fallback suggestion = %+v, want all three placeholder-domain apps", d)
	}
}

func TestGroupSuggestionsNeverMoveGroupedApps(t *testing.T) {
	groups := []groupEntry{
		{Name: "akiroo", Apps: []string{"akiroo-lite"}},
		{Name: "other", ServerApps: []groupAppRef{{ServerID: "srv-1", App: "akiroo-api"}}},
	}
	apps := []fleetAppLite{
		{App: "akiroo-lite", Server: "infra", Domain: "akiroo.com"},
		{App: "akiroo-api", Server: "infra", Domain: "api.akiroo.com"},
		{App: "akiroo-web", Server: "infra", Domain: "www.akiroo.com"},
	}
	sugg := groupSuggestions(apps, groups)
	// Both grouped apps are excluded, so akiroo.com cannot reach two —
	// nothing is suggested rather than half a group.
	if len(sugg) != 0 {
		t.Fatalf("suggestions = %+v, want none (the two grouped apps leave a singleton)", sugg)
	}
}

func TestGroupSuggestionsAddToExistingGroup(t *testing.T) {
	groups := []groupEntry{{Name: "fylun.ai", Apps: []string{"old-fylun"}}}
	apps := []fleetAppLite{
		{App: "fylun-web", Server: "edge", Domain: "www.fylun.ai"},
		{App: "fylun-api", Server: "edge", Domain: "api.fylun.ai"},
	}
	sugg := groupSuggestions(apps, groups)
	if len(sugg) != 1 || !sugg[0].Exists || sugg[0].Name != "fylun.ai" || len(sugg[0].Apps) != 2 {
		t.Fatalf("suggestions = %+v, want an add-to-existing diff", sugg)
	}
}

func TestHandleGroupSuggestionsServesDiff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := New(Config{DataDir: t.TempDir(), NoAuth: true})
	publishFleetApps(s, []remote.AppState{
		{App: "akiroo-lite", Server: "infra", Domain: "akiroo.com"},
		{App: "akiroo-api", Server: "infra", Domain: "api.akiroo.com"},
	})
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/group-suggestions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/group-suggestions = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Suggestions []groupSuggestion `json:"suggestions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data.Suggestions) != 1 || out.Data.Suggestions[0].Name != "akiroo.com" {
		t.Fatalf("suggestions = %+v", out.Data.Suggestions)
	}
	if !strings.Contains(rec.Body.String(), `"server":"infra"`) {
		t.Fatal("the suggestion carries the server binding for the accept step")
	}
}
