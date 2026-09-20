package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
