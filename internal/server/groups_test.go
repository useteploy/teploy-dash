package server

import (
	"encoding/json"
	"os"
	"path/filepath"
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
