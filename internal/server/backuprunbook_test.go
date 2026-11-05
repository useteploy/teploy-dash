package server

// backuprunbook_test.go — the rebuild runbook (plan P2.7) is a document the
// owner will read on the worst day of the fleet's life. These checks keep it
// true to the code: every dash-owned path it names is a path the code really
// writes, and the essentials of a key-only recovery are all in it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const runbookPath = "../../docs/rebuild-dash.md"

// runbookNamedPaths are the file and directory names the runbook's table
// names, each paired with the package tree whose non-test source must contain
// the literal (the code that writes it). nil means the name is not dash's to
// write (the CLI's or the operator's) and only its presence in the doc is
// checked.
var runbookNamedPaths = []struct {
	name     string
	codeDirs []string
}{
	{"auth.json", []string{"internal/server"}},
	{"sessions.json", []string{"internal/server"}},
	{"homepage.json", []string{"internal/server"}},
	{"backups.json", []string{"internal/server"}},
	{"incidents.json", []string{"internal/incident"}},
	{"silences.json", []string{"internal/server"}},
	{"audit.jsonl", []string{"internal/audit"}},
	{"domain-health.json", []string{"internal/server"}},
	{"sources", []string{"internal/source"}},
	{"operations", []string{"internal/operation"}},
	{"notifications.json", []string{"internal/server"}},
	{"servers.yml", nil},     // the teploy CLI's file
	{".ssh/id_ed25519", nil}, // the operator's key
}

func TestRebuildRunbook_NamesOnlyRealPaths(t *testing.T) {
	raw, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatalf("reading the runbook: %v", err)
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(runbookPath), ".."))
	doc := string(raw)
	for _, named := range runbookNamedPaths {
		if !strings.Contains(doc, named.name) {
			t.Errorf("the runbook does not name %s", named.name)
			continue
		}
		if named.codeDirs == nil {
			continue
		}
		found := false
		for _, dir := range named.codeDirs {
			entries, err := os.ReadDir(filepath.Join(repo, dir))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
					continue
				}
				body, err := os.ReadFile(filepath.Join(repo, dir, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(body), named.name) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("the runbook names %s, but the code never writes it — one of the two is stale", named.name)
		}
	}
}

func TestRebuildRunbook_KeyOnlyRecovery(t *testing.T) {
	raw, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	essentials := []string{
		"backup fetch",   // the drill command, for real
		"--identity",     // the recovery key is the way back
		"AGE-SECRET-KEY", // what the owner must have kept
		"age -r",         // the interim job encrypts to the owner key alone
		"CLI gap",        // why the job is not a policy entry yet
		"test-send",      // the alert path is checked after a rebuild
	}
	for _, want := range essentials {
		if !strings.Contains(doc, want) {
			t.Errorf("a key-only rebuild needs %q in the runbook", want)
		}
	}
}

// The page surfaces the gap and points at the runbook instead of pretending.
func TestBackupSelfView_GapAndRunbook(t *testing.T) {
	v := backupSelfGap()
	if v.Available {
		t.Fatal("the self-backup must not claim to exist yet")
	}
	if !strings.Contains(v.Reason, "cannot yet") || !strings.Contains(v.Reason, "recovery key alone") {
		t.Fatalf("reason = %q", v.Reason)
	}
	if v.Runbook != "docs/rebuild-dash.md" {
		t.Fatalf("runbook = %q", v.Runbook)
	}
	if _, err := os.Stat(runbookPath); err != nil {
		t.Fatalf("the named runbook must exist: %v", err)
	}
}
