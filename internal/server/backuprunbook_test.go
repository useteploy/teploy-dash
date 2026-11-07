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
	"time"
)

const runbookPath = "../../docs/recovery.md"

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
		"backup fetch",           // the proof, for real
		"--identity",             // the recovery key is the way back
		"AGE-SECRET-KEY",         // what the owner must have kept
		"age -r",                 // a non-template Dash encrypts to the owner key alone
		"test-send",              // the alert path is checked after a rebuild
		"has not been rehearsed", // says so until someone has
		"Read Only",              // the read-only console key setup never showed
		"Everything is lost",     // the four cases the page covers
		"server A's data onto server B",
		"Dash itself",
		"Hidden copies",
	}
	for _, want := range essentials {
		if !strings.Contains(doc, want) {
			t.Errorf("a key-only rebuild needs %q in the runbook", want)
		}
	}
}

// The page reports the real dash/volumes entry and points at the recovery page, and
// the old claim that Dash cannot be backed up is gone.
func TestBackupSelfStatus_ReportsTheRealEntry(t *testing.T) {
	good := time.Now()
	covered := []backupRowView{{Server: "infra", Name: "dash/volumes", Kind: "volumes", App: "dash", State: "covered", LastGood: &good, Encrypted: true}}
	v := backupSelfStatus("dash", "/var/teploy-dash", covered, true)
	if !v.Available || v.Entry != "dash/volumes" || !strings.Contains(v.Reason, "recovery key opens them on its own") {
		t.Fatalf("covered: %+v", v)
	}
	if strings.Contains(v.Reason, "cannot yet") {
		t.Fatalf("the old claim survives: %q", v.Reason)
	}
	if v.Runbook != "docs/recovery.md" {
		t.Fatalf("runbook = %q", v.Runbook)
	}
	if _, err := os.Stat(runbookPath); err != nil {
		t.Fatalf("the named recovery page must exist: %v", err)
	}

	plain := []backupRowView{{Server: "infra", Name: "dash/volumes", Kind: "volumes", App: "dash", State: "covered", LastGood: &good}}
	if v := backupSelfStatus("dash", "/var/teploy-dash", plain, false); v.Available || !strings.Contains(v.Warning, "SSH key") {
		t.Fatalf("unencrypted volumes must warn: %+v", v)
	}
	excluded := []backupRowView{{Server: "infra", Name: "dash/volumes", Kind: "volumes", App: "dash", State: "excluded"}}
	if v := backupSelfStatus("dash", "/var/teploy-dash", excluded, false); v.Available || !strings.Contains(v.Reason, "no recovery key") {
		t.Fatalf("excluded for want of a key: %+v", v)
	}
	if v := backupSelfStatus("", "/srv/dash", nil, false); v.Available || !strings.Contains(v.Reason, "not deployed from the teploy template") || !strings.Contains(strings.Join(v.Uncovered, " "), "/srv/dash") {
		t.Fatalf("a non-template Dash reports its paths as uncovered: %+v", v)
	}
}

func TestBackupSelfExcludes(t *testing.T) {
	if got := backupSelfExcludes("dash"); len(got) != 1 || got[0] != "dash/volumes" {
		t.Fatalf("template name: %v", got)
	}
	if got := backupSelfExcludes(""); len(got) != 1 || got[0] != "dash/volumes" {
		t.Fatalf("an older deploy has no TEPLOY_APP: %v", got)
	}
	if got := strings.Join(backupSelfExcludes("control"), ","); got != "control/volumes,dash/volumes" {
		t.Fatalf("named app: %v", got)
	}
}
