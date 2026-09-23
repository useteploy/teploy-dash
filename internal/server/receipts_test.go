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
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/operation"
)

// D02 server slice: reconciliation and honest cancellation outcomes read the
// CLI machine interface (app list receipts) — mocked here at the same
// CLIRunner boundary the fleet tests use.

// appListResult builds one complete machine app-list response whose app runs
// the given image in a container (the shape readMachineApps requires).
func appListResult(host, app, image string) *cli.Result {
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	containers := "[]"
	if image != "" {
		containers = fmt.Sprintf(`[{"name":"%s-1","image":"%s","state":"running","status":"Up"}]`, app, image)
	}
	return &cli.Result{Stdout: `{"host":"` + host + `","observed_at":"` + observedAt + `","errors":[],"apps":[` +
		`{"app":"` + app + `","observed_at":"` + observedAt + `",` +
		`"current_release":{"version":"v42","ports":[3000]},` +
		`"previous_release":{"version":"v41","ports":[]},` +
		`"containers":` + containers + `,"processes":[],"errors":[]}]}`}
}

// receiptRunner answers the app-list reads receipts use. A nil answer for a
// host makes the read fail (the way an unreachable server does).
func receiptRunner(host string, answer *cli.Result) func(context.Context, ...string) (*cli.Result, error) {
	return func(_ context.Context, args ...string) (*cli.Result, error) {
		if len(args) >= 4 && args[0] == "app" && args[1] == "list" && args[2] == "--host" && args[3] == host {
			if answer == nil {
				return nil, context.DeadlineExceeded
			}
			return answer, nil
		}
		return nil, fmt.Errorf("unexpected CLI call: %v", args)
	}
}

func receiptTestResolver(name string) (operation.Server, error) {
	return operation.Server{Name: name, Host: name + ".example", User: "deploy"}, nil
}

// seedInterruptedOperationRecord writes a running-operation record directly,
// simulating what a process that died mid-flight leaves on disk.
func seedInterruptedOperationRecord(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "operations", "records"), 0700); err != nil {
		t.Fatal(err)
	}
	record := `{
		"id": "` + id + `",
		"request": {"kind": "deploy", "server": "prod", "app": "web", "image": "example/web:1", "mode": "ad-hoc"},
		"metadata": {"mode": "ad-hoc"},
		"target": "server:prod/app:web",
		"status": "running",
		"attempt": 1,
		"created_at": "` + time.Now().UTC().Format(time.RFC3339Nano) + `",
		"admitted_server": {"Name": "prod", "Host": "prod.example", "User": "deploy"},
		"admission_seq": 1
	}`
	if err := os.WriteFile(filepath.Join(dir, "operations", "records", id+".json"), []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
}

// A restart over an operation that was mid-flight when the previous process
// died reconciles against the machine app list: the API serves interrupted +
// reconciled-applied (with evidence) once the receipt lands, retry was
// refused while reconciling, and the retry afterwards is a normal enqueue.
func TestRestartReconciliationThroughMachineReceipts(t *testing.T) {
	dir := t.TempDir()
	// Instance one "dies" mid-flight: its executor never returns.
	block := make(chan struct{})
	first := New(Config{
		DataDir: dir, NoAuth: true,
		OperationResolver: receiptTestResolver,
		OperationExecutor: func(context.Context, operation.Command, func(operation.Stream, string)) (int, error) {
			<-block
			return 0, nil
		},
	})
	body := strings.NewReader(`{"kind":"deploy","server":"prod","app":"web","image":"example/web:1"}`)
	rec := httptest.NewRecorder()
	first.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/operations", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enqueue status=%d body=%s", rec.Code, rec.Body.String())
	}
	var admitted struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	waitForOperationStatus(t, first, admitted.Data.ID, operation.StatusRunning)

	// "Crash": a second instance over the same data dir, whose CLI reports
	// the app running the requested image.
	second := New(Config{
		DataDir: dir, NoAuth: true,
		CLIInstalled:      func() bool { return true },
		CLIRunner:         receiptRunner("prod.example", appListResult("prod.example", "web", "example/web:1")),
		OperationResolver: receiptTestResolver,
		OperationExecutor: func(context.Context, operation.Command, func(operation.Stream, string)) (int, error) { return 0, nil },
	})
	defer second.DrainOperations(context.Background())

	// The API surfaces the honest states distinctly: interrupted (coordinator
	// state) plus reconciliation reconciling -> reconciled-applied.
	var served operation.Operation
	deadline := time.Now().Add(5 * time.Second)
	sawReconciling := false
	for time.Now().Before(deadline) {
		get := httptest.NewRecorder()
		second.handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/operations/"+admitted.Data.ID, nil))
		if get.Code != http.StatusOK {
			t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
		}
		var envelope struct {
			Data operation.Operation `json:"data"`
		}
		if err := json.Unmarshal(get.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		served = envelope.Data
		if served.Status != operation.StatusInterrupted {
			t.Fatalf("served status = %s, want interrupted", served.Status)
		}
		if served.Reconciliation == nil {
			t.Fatal("interrupted operation carries no reconciliation")
		}
		if served.Reconciliation.State == operation.ReconcileStateReconciling {
			sawReconciling = true
			// While the outcome is unresolved, retry is refused (409).
			retry := httptest.NewRecorder()
			second.handler().ServeHTTP(retry, httptest.NewRequest(http.MethodPost, "/api/operations/"+admitted.Data.ID+"/retry", nil))
			if retry.Code != http.StatusConflict || !strings.Contains(retry.Body.String(), "reconcil") {
				t.Fatalf("retry while reconciling status=%d body=%s, want 409 reconciliation pending", retry.Code, retry.Body.String())
			}
		}
		if served.Reconciliation.State == operation.ReconcileStateApplied {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if served.Reconciliation == nil || served.Reconciliation.State != operation.ReconcileStateApplied {
		t.Fatalf("final reconciliation = %+v, want reconciled-applied", served.Reconciliation)
	}
	if !strings.Contains(served.Reconciliation.Evidence, "example/web:1") {
		t.Fatalf("evidence = %q, want the observed image", served.Reconciliation.Evidence)
	}
	_ = sawReconciling // the reconciling window may be missed on a fast read; the gate above pins it manager-side

	// After the answer lands, retry is a normal explicit re-authorization.
	retry := httptest.NewRecorder()
	second.handler().ServeHTTP(retry, httptest.NewRequest(http.MethodPost, "/api/operations/"+admitted.Data.ID+"/retry", nil))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry after reconciliation status=%d body=%s", retry.Code, retry.Body.String())
	}
	var retried struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(retry.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	waitForTerminalOperation(t, second, retried.Data.ID)
}

// A receipt read that fails (unreachable server) leaves the interrupted
// operation labeled needs-manual-check with the exact reason — never
// silently failed, never retried.
func TestRestartReconciliationNeedsManualCheckOnReadFailure(t *testing.T) {
	dir := t.TempDir()
	const id = "abcdef0123456789abcdef0123456789"
	seedInterruptedOperationRecord(t, dir, id)

	s := New(Config{
		DataDir: dir, NoAuth: true,
		CLIInstalled:      func() bool { return true },
		CLIRunner:         receiptRunner("prod.example", nil), // every read fails
		OperationResolver: receiptTestResolver,
		OperationExecutor: func(context.Context, operation.Command, func(operation.Stream, string)) (int, error) { return 0, nil },
	})
	defer s.DrainOperations(context.Background())

	deadline := time.Now().Add(5 * time.Second)
	var served operation.Operation
	for time.Now().Before(deadline) {
		get := httptest.NewRecorder()
		s.handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/operations/"+id, nil))
		var envelope struct {
			Data operation.Operation `json:"data"`
		}
		if err := json.Unmarshal(get.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		served = envelope.Data
		if served.Reconciliation != nil && served.Reconciliation.State == operation.ReconcileStateManual {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if served.Reconciliation == nil || served.Reconciliation.State != operation.ReconcileStateManual {
		t.Fatalf("reconciliation = %+v, want needs-manual-check", served.Reconciliation)
	}
	if !strings.Contains(served.Reconciliation.Reason, "reading teploy app list") {
		t.Fatalf("manual-check reason = %q, want the exact read failure", served.Reconciliation.Reason)
	}
	if served.Status != operation.StatusInterrupted {
		t.Fatalf("status = %s, want interrupted (never relabeled failed)", served.Status)
	}
}

// A cancel that lands mid-flight, where the CLI effect had already committed,
// surfaces as already_committed with the receipt evidence — the API never
// pretends a rollback happened.
func TestCancelDuringRunSurfacesAlreadyCommitted(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	var once sync.Once
	s := New(Config{
		DataDir: dir, NoAuth: true,
		CLIInstalled:      func() bool { return true },
		CLIRunner:         receiptRunner("prod.example", appListResult("prod.example", "web", "example/web:1")),
		OperationResolver: receiptTestResolver,
		OperationExecutor: func(ctx context.Context, _ operation.Command, _ func(operation.Stream, string)) (int, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return -1, ctx.Err()
		},
	})
	defer s.DrainOperations(context.Background())

	body := strings.NewReader(`{"kind":"deploy","server":"prod","app":"web","image":"example/web:1"}`)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/operations", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enqueue status=%d body=%s", rec.Code, rec.Body.String())
	}
	var admitted struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	<-started

	cancel := httptest.NewRecorder()
	s.handler().ServeHTTP(cancel, httptest.NewRequest(http.MethodPost, "/api/operations/"+admitted.Data.ID+"/cancel", nil))
	if cancel.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", cancel.Code, cancel.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	var served operation.Operation
	for time.Now().Before(deadline) {
		get := httptest.NewRecorder()
		s.handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/operations/"+admitted.Data.ID, nil))
		var envelope struct {
			Data operation.Operation `json:"data"`
		}
		if err := json.Unmarshal(get.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		served = envelope.Data
		if served.Status.Terminal() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if served.Status != operation.StatusAlreadyCommitted {
		t.Fatalf("status = %s, want already_committed", served.Status)
	}
	if served.Reconciliation == nil || served.Reconciliation.State != operation.ReconcileStateApplied {
		t.Fatalf("reconciliation = %+v, want reconciled-applied with evidence", served.Reconciliation)
	}
	if !strings.Contains(served.Reconciliation.Evidence, "example/web:1") || !strings.Contains(served.Error, "no rollback was performed") {
		t.Fatalf("record = error %q evidence %q", served.Error, served.Reconciliation.Evidence)
	}
	// Retry is not offered for work whose effect stood.
	retry := httptest.NewRecorder()
	s.handler().ServeHTTP(retry, httptest.NewRequest(http.MethodPost, "/api/operations/"+admitted.Data.ID+"/retry", nil))
	if retry.Code != http.StatusConflict {
		t.Fatalf("already-committed retry status=%d, want 409", retry.Code)
	}
}

func waitForOperationStatus(t *testing.T, s *Server, id string, status operation.Status) operation.Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if op, err := s.operations.Get(id); err == nil && op.Status == status {
			return *op
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("operation %s never reached %s", id, status)
	return operation.Operation{}
}
