package operation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A record written by the removed manifest/source pipeline, with the
// request fields that no longer exist, still loads and lists; retrying it
// is refused cleanly.
func TestHistoricalManifestRecordLoadsButIsNotRetryable(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	// Recent enough that default age retention (30d) keeps it: this test is
	// about the removed kind, not retention, and a fixed date expires.
	createdAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	record := `{
  "id": "` + id + `",
  "request": {
    "kind": "manifest_apply",
    "server": "prod",
    "app": "web",
    "mode": "git-managed",
    "manifest_revision": "` + strings.Repeat("a", 64) + `",
    "source_id": "src-0123456789abcdef",
    "source_commit": "` + strings.Repeat("b", 40) + `",
    "source_manifest_path": "teploy.yml",
    "source_pull_updated_at": "2026-10-01T00:00:00Z",
    "source_pull_request": 7
  },
  "metadata": {"mode": "git-managed"},
  "target": "server:prod/app:web",
  "status": "failed",
  "error": "source manifest binding changed",
  "created_at": "` + createdAt + `",
  "attempt": 1,
  "record_version": 2
}
`
	records := filepath.Join(dir, "operations", "records")
	if err := os.MkdirAll(records, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(records, id+".json"), []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	executor := func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil }
	manager, err := New(dir, Options{MaxEvents: 100, Resolver: testResolver, Executor: executor})
	if err != nil {
		t.Fatalf("historical record must not block startup: %v", err)
	}
	defer manager.Shutdown(context.Background())
	op, err := manager.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if op.Request.Kind != "manifest_apply" || op.Status != StatusFailed || op.Error != "source manifest binding changed" {
		t.Fatalf("historical record = %+v", op)
	}
	if list := manager.List("", "", 0); len(list) != 1 || list[0].ID != id {
		t.Fatalf("historical record missing from list: %+v", list)
	}
	if _, err := manager.Retry(id, nil); err != ErrNotRetryable {
		t.Fatalf("Retry of removed kind = %v, want ErrNotRetryable", err)
	}
	if _, _, err := manager.Enqueue(Request{Kind: "manifest_apply", Server: "prod", App: "web"}, "", nil); err == nil {
		t.Fatal("removed kind enqueued")
	}
}
