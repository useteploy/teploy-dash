package operation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// X02 §5 row 6: operation records carry record_version (absent = 1); this
// build writes 2 and refuses MUTATIONS when it finds a record from a newer
// schema — reads keep working, so history stays inspectable while the
// remedy (upgrade) is pending.

// writeFutureRecord plants a minimal but structurally valid operation
// record carrying the given record_version.
func writeFutureRecord(t *testing.T, dataDir, id string, version int) {
	t.Helper()
	op := map[string]interface{}{
		"id":             id,
		"request":        map[string]interface{}{"kind": "deploy", "server": "prod", "app": "web", "image": "example/web:1", "mode": "ad-hoc"},
		"metadata":       map[string]interface{}{"mode": "ad-hoc"},
		"target":         "server:prod/app:web",
		"status":         "failed",
		"created_at":     "2026-09-23T00:00:00Z",
		"attempt":        1,
		"record_version": version,
	}
	raw, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, "operations", "records")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFutureRecordVersionStartsReadOnly(t *testing.T) {
	dir := t.TempDir()
	writeFutureRecord(t, dir, "0123456789abcdef0123456789abcdef", 3)

	executor := func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil }
	manager, err := New(dir, Options{MaxEvents: 100, Resolver: testResolver, Executor: executor})
	if err != nil {
		t.Fatalf("New must start (read-only), not fail: %v", err)
	}

	// Reads work: the future record is visible for inspection.
	op, err := manager.Get("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusFailed {
		t.Fatalf("future record status = %q, want failed (readable)", op.Status)
	}

	// Every mutation refuses with ErrReadOnly.
	if _, _, err := manager.Enqueue(deployRequest("web", "example/web:9"), "", nil); err == nil || !strings.Contains(err.Error(), ErrReadOnly.Error()) {
		t.Fatalf("Enqueue under future version: err = %v, want ErrReadOnly", err)
	}
	if _, err := manager.Retry("0123456789abcdef0123456789abcdef", nil); err == nil || !strings.Contains(err.Error(), ErrReadOnly.Error()) {
		t.Fatalf("Retry under future version: err = %v, want ErrReadOnly", err)
	}
	if _, err := manager.Cancel("0123456789abcdef0123456789abcdef"); err == nil || !strings.Contains(err.Error(), ErrReadOnly.Error()) {
		t.Fatalf("Cancel under future version: err = %v, want ErrReadOnly", err)
	}

	// The banner carries the remediation.
	if h := manager.Health(); h.ReadOnly == "" || !strings.Contains(h.ReadOnly, "upgrade") {
		t.Fatalf("Health().ReadOnly = %q, want the upgrade remedy", h.ReadOnly)
	}
	manager.Shutdown(context.Background())
}

func TestRecordVersionStampedAndReloadedMutable(t *testing.T) {
	dir := t.TempDir()
	manager := newTestManager(t, dir, 100, func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil })
	op, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, op.ID, StatusSucceeded)
	manager.Shutdown(context.Background())

	// The written record is stamped with the current version.
	raw, err := os.ReadFile(filepath.Join(dir, "operations", "records", op.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), fmt.Sprintf(`"record_version": %d`, currentRecordVersion)) {
		t.Fatalf("record not stamped with record_version %d: %s", currentRecordVersion, raw)
	}

	// Reloading those records starts MUTABLE (own-version records never
	// trip the read-only gate).
	restarted, err := New(dir, Options{MaxEvents: 100, Resolver: testResolver, Executor: func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown(context.Background())
	if h := restarted.Health(); h.ReadOnly != "" {
		t.Fatalf("own-version reload started read-only: %q", h.ReadOnly)
	}
	if _, _, err := restarted.Enqueue(deployRequest("api", "example/api:1"), "", nil); err != nil {
		t.Fatalf("mutation after own-version reload: %v", err)
	}
}

// The version check must fire on the RECORD's version, not the file name or
// an adjacent field: a version-2 record with extra unknown fields stays
// mutable (additive-within-version is the contract).
func TestUnknownFieldsWithinVersionStayMutable(t *testing.T) {
	dir := t.TempDir()
	writeFutureRecord(t, dir, "0123456789abcdef0123456789abcdee", currentRecordVersion)
	manager, err := New(dir, Options{MaxEvents: 100, Resolver: testResolver, Executor: func(context.Context, Command, func(Stream, string)) (int, error) { return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	if h := manager.Health(); h.ReadOnly != "" {
		t.Fatalf("own-version record with unknown fields tripped read-only: %q", h.ReadOnly)
	}
}
