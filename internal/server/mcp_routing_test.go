package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/operation"
)

// A19: MCP deploy/rollback/lifecycle enqueue validated operations instead of
// shelling out directly, and unknown servers are refused before anything runs.
func TestMCPMutationsRouteThroughOperations(t *testing.T) {
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		OperationResolver: func(name string) (operation.Server, error) {
			if name == "prod" {
				return operation.Server{Name: name, Host: "10.0.0.9", User: "deploy"}, nil
			}
			return operation.Server{}, operation.ErrNotFound
		},
		OperationExecutor: func(_ context.Context, _ operation.Command, _ func(operation.Stream, string)) (int, error) {
			return 0, nil
		},
	})
	b := mcpBackend{s: s}

	if out, err := b.Deploy(context.Background(), "prod", "web", "example/web:1", "web.test", 8080); err != nil {
		t.Fatalf("deploy: %v", err)
	} else if !strings.Contains(out, `"kind": "deploy"`) || !strings.Contains(out, `"id"`) {
		t.Fatalf("deploy did not return an operation record: %s", out)
	}
	if _, err := b.Rollback(context.Background(), "prod", "web"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := b.AppAction(context.Background(), "prod", "web", "restart"); err != nil {
		t.Fatalf("restart: %v", err)
	}

	// Wait for TERMINAL records, not mere existence: the workers persist
	// their terminal state after the records appear, and the test's TempDir
	// is removed the moment it returns — leaving the persist racing the
	// cleanup.
	deadline := time.Now().Add(3 * time.Second)
	kinds := map[string]bool{}
	for time.Now().Before(deadline) {
		kinds = map[string]bool{}
		for _, op := range s.operations.List("", "", 0) {
			if op.Status.Terminal() {
				kinds[string(op.Request.Kind)] = true
			}
		}
		if kinds["deploy"] && kinds["rollback"] && kinds["app_lifecycle"] {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, want := range []string{"deploy", "rollback", "app_lifecycle"} {
		if !kinds[want] {
			t.Fatalf("operation kind %q not journaled; have %v", want, kinds)
		}
	}

	// Unknown server is refused before admission.
	before := len(s.operations.List("", "", 0))
	if _, err := b.Deploy(context.Background(), "ghost", "web", "example/web:1", "", 0); err == nil {
		t.Fatal("deploy against an unknown server succeeded")
	}
	if _, err := b.SetEnv(context.Background(), "ghost", "web", "KEY", "v"); err == nil {
		t.Fatal("SetEnv against an unknown server succeeded")
	}
	after := len(s.operations.List("", "", 0))
	if after != before {
		t.Fatalf("unknown-server mutations were admitted: %d -> %d", before, after)
	}
}
