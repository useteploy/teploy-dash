package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
)

func withTempGroupsFile(t *testing.T) func() {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// ensure ~/.teploy/ exists for the saveGroupsFile path
	os.MkdirAll(filepath.Join(tmp, ".teploy"), 0755)
	return func() {}
}

func TestGroupsFile_EmptyWhenMissing(t *testing.T) {
	withTempGroupsFile(t)
	data, err := loadGroupsFile()
	if err != nil {
		t.Fatalf("loadGroupsFile with no file: %v", err)
	}
	if len(data.Groups) != 0 {
		t.Errorf("expected 0 groups, got %d", len(data.Groups))
	}
}

func TestGroupsFile_RoundTrip(t *testing.T) {
	withTempGroupsFile(t)

	original := groupData{
		Groups: []groupEntry{
			{Name: "prod", Apps: []string{"web", "api"}},
			{Name: "staging", Apps: []string{"web-staging"}},
		},
	}
	if err := saveGroupsFile(original); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadGroupsFile()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Groups) != 2 {
		t.Errorf("expected 2 groups, got %d", len(loaded.Groups))
	}
	if loaded.Groups[0].Name != "prod" {
		t.Errorf("expected first group 'prod', got %q", loaded.Groups[0].Name)
	}
}

func TestGroupsFile_AcceptsBareArrayFormat(t *testing.T) {
	// Legacy format: top-level array instead of {groups: [...]}
	// Should still load correctly for backwards compat with older CLI UI.
	withTempGroupsFile(t)
	path := groupsFilePath()
	raw, _ := json.Marshal([]groupEntry{
		{Name: "legacy", Apps: []string{"myapp"}},
	})
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := loadGroupsFile()
	if err != nil {
		t.Fatalf("load bare array: %v", err)
	}
	if len(loaded.Groups) != 1 || loaded.Groups[0].Name != "legacy" {
		t.Errorf("bare array not parsed correctly: %+v", loaded.Groups)
	}
}

// R12: renaming group A onto existing group B answers 409 and leaves the
// document byte-for-byte unchanged; unroute-safe names are rejected at
// create and rename.
func TestGroupRenameCollisionAndNameGrammar(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	data := groupData{Groups: []groupEntry{
		{Name: "alpha", Apps: []string{}},
		{Name: "beta", Apps: []string{}},
	}}
	if err := saveGroupsFile(data); err != nil {
		t.Fatal(err)
	}
	before, err := loadGroupsFile()
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	put := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleGroupAction(rec, req)
		return rec
	}

	if rec := put("/api/groups/alpha", `{"name":"beta"}`); rec.Code != http.StatusConflict {
		t.Fatalf("rename onto existing name must 409, got %d (%s)", rec.Code, rec.Body.String())
	}
	after, err := loadGroupsFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Groups) != len(before.Groups) || after.Groups[0].Name != "alpha" || after.Groups[1].Name != "beta" {
		t.Fatalf("conflicting rename must not change state: %+v", after.Groups)
	}

	for _, bad := range []string{`{"name":"a/b"}`, `{"name":".."}`, `{"name":".hidden"}`, `{"name":"  "}`, `{"name":"x/y/z"}`} {
		if rec := put("/api/groups/alpha", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("unroute-safe name %s must 400, got %d", bad, rec.Code)
		}
	}

	// POST create with a slash name is rejected before any write.
	req := httptest.NewRequest(http.MethodPost, "/api/groups", strings.NewReader(`{"name":"evil/name"}`))
	rec := httptest.NewRecorder()
	s.handleGroups(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with slash name must 400, got %d", rec.Code)
	}
	final, _ := loadGroupsFile()
	if len(final.Groups) != 2 {
		t.Fatalf("no writes may happen for rejected names, got %+v", final.Groups)
	}
}

// R11: two concurrent assignments against one group both survive — the
// read-modify-write runs inside the transaction lock.
func TestConcurrentGroupAssignmentsBothSurvive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := saveGroupsFile(groupData{Groups: []groupEntry{{Name: "g", Apps: []string{}}}}); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"app":"app-%d"}`, i)
			req := httptest.NewRequest(http.MethodPost, "/api/groups/g/apps", strings.NewReader(body))
			s.handleGroupAction(httptest.NewRecorder(), req)
		}(i)
	}
	wg.Wait()

	data, err := loadGroupsFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Groups[0].Apps) != 8 {
		t.Fatalf("all 8 concurrent assignments must survive, got %d: %v", len(data.Groups[0].Apps), data.Groups[0].Apps)
	}
}

// ── X02 §5 row 7: server-scoped app refs ───────────────────────────────────

// groupBindingServer builds a Server whose fleet is the given server-list
// JSON (swappable mid-test to simulate renames/rewrites).
func groupBindingServer(t *testing.T, list *atomic.Value) *Server {
	t.Helper()
	return New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			if len(args) >= 2 && args[0] == "server" && args[1] == "list" {
				return &cli.Result{Stdout: list.Load().(string)}, nil
			}
			return &cli.Result{}, nil
		},
	})
}

func groupPost(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleGroupAction(rec, req)
	return rec
}

func groupDelete(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	rec := httptest.NewRecorder()
	s.handleGroupAction(rec, req)
	return rec
}

// Binding records the server's envelope id: the CLI's stable id when the
// entry carries one, never the name.
func TestGroupAppBindingRecordsStableID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	list := &atomic.Value{}
	list.Store(`{"prod":{"id":"srv-aaaaaaaaaaaaaaaa","host":"10.0.0.1"}}`)
	s := groupBindingServer(t, list)
	if err := saveGroupsFile(groupData{Groups: []groupEntry{{Name: "g", Apps: []string{}}}}); err != nil {
		t.Fatal(err)
	}

	if rec := groupPost(t, s, "/api/groups/g/apps", `{"app":"web","server":"prod"}`); rec.Code != http.StatusOK {
		t.Fatalf("assign: %d (%s)", rec.Code, rec.Body.String())
	}
	data, err := loadGroupsFile()
	if err != nil {
		t.Fatal(err)
	}
	refs := data.Groups[0].ServerApps
	if len(refs) != 1 || refs[0].ServerID != "srv-aaaaaaaaaaaaaaaa" || refs[0].App != "web" {
		t.Fatalf("binding = %+v, want {srv-aaaaaaaaaaaaaaaa web}", refs)
	}
	if rec := groupPost(t, s, "/api/groups/g/apps", `{"app":"web","server":"prod"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "already assigned") {
		t.Fatalf("re-assign: %d (%s)", rec.Code, rec.Body.String())
	}
	data, _ = loadGroupsFile()
	if len(data.Groups[0].ServerApps) != 1 {
		t.Fatalf("re-assign duplicated the binding: %+v", data.Groups[0].ServerApps)
	}
}

// A legacy id-less server binds by the name-hash fallback — the envelope id
// dash already uses everywhere for it.
func TestGroupAppBindingUsesNameHashFallbackForLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	list := &atomic.Value{}
	list.Store(`{"prod":{"host":"10.0.0.1"}}`)
	s := groupBindingServer(t, list)
	if err := saveGroupsFile(groupData{Groups: []groupEntry{{Name: "g", Apps: []string{}}}}); err != nil {
		t.Fatal(err)
	}

	if rec := groupPost(t, s, "/api/groups/g/apps", `{"app":"web","server":"prod"}`); rec.Code != http.StatusOK {
		t.Fatalf("assign: %d (%s)", rec.Code, rec.Body.String())
	}
	data, _ := loadGroupsFile()
	refs := data.Groups[0].ServerApps
	if len(refs) != 1 || refs[0].ServerID != serverStableID("prod") {
		t.Fatalf("legacy binding = %+v, want name-hash fallback", refs)
	}
}

// The same app name on two servers stays two distinct bindings; removing
// by the bare name is then ambiguous and refuses naming the servers.
func TestGroupSameAppTwoServersDistinctAndAmbiguousRemoval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	list := &atomic.Value{}
	list.Store(`{"prod":{"id":"srv-aaaaaaaaaaaaaaaa","host":"10.0.0.1"},"staging":{"id":"srv-bbbbbbbbbbbbbbbb","host":"10.0.0.2"}}`)
	s := groupBindingServer(t, list)
	if err := saveGroupsFile(groupData{Groups: []groupEntry{{Name: "g", Apps: []string{}}}}); err != nil {
		t.Fatal(err)
	}

	groupPost(t, s, "/api/groups/g/apps", `{"app":"web","server":"prod"}`)
	groupPost(t, s, "/api/groups/g/apps", `{"app":"web","server":"staging"}`)
	data, _ := loadGroupsFile()
	if len(data.Groups[0].ServerApps) != 2 {
		t.Fatalf("same app on two servers must be two bindings, got %+v", data.Groups[0].ServerApps)
	}

	rec := groupDelete(t, s, "/api/groups/g/apps/web")
	if rec.Code != http.StatusConflict {
		t.Fatalf("ambiguous removal must 409, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "srv-aaaaaaaaaaaaaaaa") || !strings.Contains(rec.Body.String(), "srv-bbbbbbbbbbbbbbbb") {
		t.Fatalf("ambiguity must name both servers: %s", rec.Body.String())
	}
	data, _ = loadGroupsFile()
	if len(data.Groups[0].ServerApps) != 2 {
		t.Fatalf("ambiguous refusal changed state: %+v", data.Groups[0].ServerApps)
	}
}

// A binding against an unconfigured server is refused before any write.
func TestGroupAppBindingUnknownServerRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	list := &atomic.Value{}
	list.Store(`{"prod":{"id":"srv-aaaaaaaaaaaaaaaa","host":"10.0.0.1"}}`)
	s := groupBindingServer(t, list)
	if err := saveGroupsFile(groupData{Groups: []groupEntry{{Name: "g", Apps: []string{}}}}); err != nil {
		t.Fatal(err)
	}

	if rec := groupPost(t, s, "/api/groups/g/apps", `{"app":"web","server":"nosuch"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown server must 404, got %d (%s)", rec.Code, rec.Body.String())
	}
	data, _ := loadGroupsFile()
	if len(data.Groups[0].ServerApps) != 0 || len(data.Groups[0].Apps) != 0 {
		t.Fatalf("refused assignment wrote state: %+v", data.Groups[0])
	}
}

// X02 §1.3 rule 5: two servers swap names — existing bindings keep their
// original ids (nothing re-keys), and a NEW binding under a swapped name
// follows the id the name now resolves to, not the name's history.
func TestGroupBindingSurvivesNameSwap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	list := &atomic.Value{}
	list.Store(`{"prod":{"id":"srv-aaaaaaaaaaaaaaaa","host":"10.0.0.1"},"staging":{"id":"srv-bbbbbbbbbbbbbbbb","host":"10.0.0.2"}}`)
	s := groupBindingServer(t, list)
	if err := saveGroupsFile(groupData{Groups: []groupEntry{{Name: "g", Apps: []string{}}}}); err != nil {
		t.Fatal(err)
	}

	groupPost(t, s, "/api/groups/g/apps", `{"app":"web","server":"prod"}`)

	// The two servers swap names.
	list.Store(`{"prod":{"id":"srv-bbbbbbbbbbbbbbbb","host":"10.0.0.2"},"staging":{"id":"srv-aaaaaaaaaaaaaaaa","host":"10.0.0.1"}}`)

	data, _ := loadGroupsFile()
	if refs := data.Groups[0].ServerApps; len(refs) != 1 || refs[0].ServerID != "srv-aaaaaaaaaaaaaaaa" {
		t.Fatalf("swap must not re-key existing bindings: %+v", refs)
	}
	// A new binding under the name "prod" now binds the server that name
	// currently resolves to — the other id.
	groupPost(t, s, "/api/groups/g/apps", `{"app":"api","server":"prod"}`)
	data, _ = loadGroupsFile()
	var apiID string
	for _, ref := range data.Groups[0].ServerApps {
		if ref.App == "api" {
			apiID = ref.ServerID
		}
	}
	if apiID != "srv-bbbbbbbbbbbbbbbb" {
		t.Fatalf("new binding followed the name's history, not its current id: %q", apiID)
	}
	// And the unambiguous single match removes cleanly by name.
	if rec := groupDelete(t, s, "/api/groups/g/apps/api"); rec.Code != http.StatusOK {
		t.Fatalf("single-match removal: %d (%s)", rec.Code, rec.Body.String())
	}
}
