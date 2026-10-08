package source

import (
	"strings"
	"testing"
)

func TestSourceMonorepoPushPathsPreserveCLIGrammar(t *testing.T) {
	body := []byte(`{"commits":[{"added":["apps/web/a.ts"],"modified":["README.md"],"removed":["apps/web/old.ts"]}]}`)
	for _, p := range []string{"apps/web/**", "apps/web/*.ts", "README.md", "**"} {
		if !PushMatchesPaths(body, []string{p}) {
			t.Fatalf("lost relevant change: %s", p)
		}
	}
	if PushMatchesPaths(body, []string{"apps/api/**"}) {
		t.Fatal("unrelated monorepo app admitted")
	}
	for _, b := range []string{`{}`, `{"commits":[]}`, `{"total_commits_count":30,"commits":[{"added":["other"]}]}`, `{"commits":[` + strings.TrimSuffix(strings.Repeat(`{"added":["other"]},`, 20), ",") + `]}`} {
		if !PushMatchesPaths([]byte(b), []string{"apps/api/**"}) {
			t.Fatal("incomplete file list skipped deployment")
		}
	}
}
