package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/operation"
)

// operationKindDedicatedRoutes names, for every supported operation kind, a
// dedicated route that admits it. The cleanup remedy route admits
// route_remove, releases_prune and server_prune (and also remove/redeploy).
var operationKindDedicatedRoutes = map[operation.Kind]string{
	operation.KindDeploy:          "/api/apps/prod/web/deploy",
	operation.KindRollback:        "/api/apps/prod/web/rollback",
	operation.KindRemove:          "/api/apps/prod/web/remove",
	operation.KindTemplateInstall: "/api/templates/install",
	operation.KindAppLifecycle:    "/api/apps/prod/web/restart",
	operation.KindMaintenance:     "/api/apps/prod/web/maintenance/on",
	operation.KindRedeploy:        "/api/apps/prod/web/redeploy",
	operation.KindBackupCreate:    "/api/apps/prod/web/backups/create",
	operation.KindBackupRestore:   "/api/apps/prod/web/backups/restore",
	operation.KindServerAudit:     "/api/cleanup/prod/audit",
	operation.KindRouteRemove:     "/api/cleanup/prod/remedy",
	operation.KindReleasesPrune:   "/api/cleanup/prod/remedy",
	operation.KindServerPrune:     "/api/cleanup/prod/remedy",
}

func allowsAll(held caps.Set, required []string) (string, bool) {
	for _, c := range required {
		if !held.Allow(c) {
			return c, false
		}
	}
	return "", true
}

// D-02: the generic POST /api/operations must never admit a kind its own
// dedicated route refuses — e.g. a backup restore without restore.data.
func TestOperationKindCapabilityParity(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)
	genericGate := requiredCapabilities(http.MethodPost, "/api/operations")

	for _, kind := range operation.SupportedKinds() {
		table, ok := operationKindCapabilities[kind]
		if !ok {
			t.Fatalf("operation kind %q has no entry in operationKindCapabilities", kind)
		}
		route, ok := operationKindDedicatedRoutes[kind]
		if !ok {
			t.Fatalf("operation kind %q has no dedicated route named in this test", kind)
		}
		// Static parity: the table holds everything the dedicated route's
		// gate requires, so generic (gate + table) >= dedicated (gate + table).
		dedicatedGate := requiredCapabilities(http.MethodPost, route)
		for _, c := range dedicatedGate {
			if !caps.NewSet(table...).Allow(c) {
				t.Errorf("kind %q: dedicated route %s requires %s, the kind table does not", kind, route, c)
			}
		}
		dedicated := append(append([]string{}, dedicatedGate...), table...)
		generic := append(append([]string{}, genericGate...), table...)

		for _, u := range []struct{ name, role string }{
			{"v", RoleViewer}, {"op", RoleEditor}, {"admin", RoleAdmin},
		} {
			held := caps.PresetForRole(u.role)
			_, dedicatedOK := allowsAll(held, dedicated)
			missing, genericOK := allowsAll(held, generic)
			if genericOK && !dedicatedOK {
				t.Errorf("kind %q as %s: generic route admits what %s refuses", kind, u.role, route)
			}
			body := fmt.Sprintf(`{"kind":%q,"server":"nowhere","app":"web"}`, kind)
			rec := capDo(t, s, u.name, u.role, http.MethodPost, "/api/operations", body)
			if genericOK {
				if rec.Code == http.StatusForbidden {
					t.Errorf("kind %q as %s: generic = 403 (%s), want admitted past the capability checks", kind, u.role, rec.Body.String())
				}
				continue
			}
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), missing) {
				t.Errorf("kind %q as %s: generic = %d (%s), want 403 naming %s", kind, u.role, rec.Code, rec.Body.String(), missing)
			}
		}
	}
}

// An editor's preset has execute.deploy but not restore.data, so a retry of
// a failed restore is refused to them; an admin may retry it.
func TestOperationRetryRequiresTheKindsCapabilities(t *testing.T) {
	// A failing executor: the restore ends failed, which is retryable.
	s := New(Config{
		DataDir:      t.TempDir(),
		CLIInstalled: func() bool { return true },
		CLIRunner:    newCapsRunner(),
		OperationExecutor: func(context.Context, operation.Command, func(operation.Stream, string)) (int, error) {
			return 1, nil
		},
	})
	t.Cleanup(func() { s.DrainOperations(context.Background()) })
	seedPresetUsers(t, s)

	rec := capDo(t, s, "admin", RoleAdmin, http.MethodPost, "/api/operations", `{"kind":"backup_restore","server":"prod","app":"web","backup_id":"20261019-120000"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("admin restore = %d (%s), want 202", rec.Code, rec.Body.String())
	}
	id := strings.TrimPrefix(rec.Header().Get("Location"), "/api/operations/")
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := s.operations.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == operation.StatusFailed {
			break
		}
		if got.Status.Terminal() || time.Now().After(deadline) {
			t.Fatalf("restore ended %s, want failed", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	rec = capDo(t, s, "op", RoleEditor, http.MethodPost, "/api/operations/"+id+"/retry", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(capError(t, rec), caps.RestoreData) {
		t.Fatalf("editor retry of a restore = %d (%s), want 403 naming restore.data", rec.Code, rec.Body.String())
	}
	rec = capDo(t, s, "admin", RoleAdmin, http.MethodPost, "/api/operations/"+id+"/retry", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("admin retry of a restore = %d (%s), want 202", rec.Code, rec.Body.String())
	}
}
