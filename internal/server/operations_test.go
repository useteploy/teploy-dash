package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/operation"
)

func operationTestServer(t *testing.T, auth bool) *Server {
	t.Helper()
	config := Config{
		DataDir: t.TempDir(), NoAuth: !auth, AuthUser: "admin", AuthPass: "secret",
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: name + ".example", User: "deploy"}, nil
		},
		OperationExecutor: func(_ context.Context, _ operation.Command, emit func(operation.Stream, string)) (int, error) {
			emit(operation.StreamStdout, "started")
			return 0, nil
		},
	}
	return New(config)
}

func TestOperationRoutesRequireAuthentication(t *testing.T) {
	server := operationTestServer(t, true)
	request := httptest.NewRequest(http.MethodGet, "/api/operations", nil)
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestOperationAPIEnqueueGetAndSSEReplay(t *testing.T) {
	server := operationTestServer(t, false)
	body := bytes.NewBufferString(`{"kind":"deploy","server":"prod","app":"web","image":"example/web:1"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/operations", body)
	request.Header.Set("Idempotency-Key", "api-key")
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("enqueue status = %d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Header().Get("Location") != "/api/operations/"+envelope.Data.ID {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		op, _ := server.operations.Get(envelope.Data.ID)
		if op.Status == operation.StatusSucceeded {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	get := httptest.NewRequest(http.MethodGet, "/api/operations/"+envelope.Data.ID, nil)
	getResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("get status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}

	events, _ := server.operations.EventsAfter(envelope.Data.ID, 0)
	sse := httptest.NewRequest(http.MethodGet, "/api/operations/"+envelope.Data.ID+"/events", nil)
	sse.Header.Set("Last-Event-ID", "1")
	sseResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(sseResponse, sse)
	if sseResponse.Code != http.StatusOK || sseResponse.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE status=%d headers=%v", sseResponse.Code, sseResponse.Header())
	}
	if strings.Contains(sseResponse.Body.String(), "id: 1\n") {
		t.Fatalf("Last-Event-ID replayed event 1: %s", sseResponse.Body.String())
	}
	if len(events) < 2 || !strings.Contains(sseResponse.Body.String(), "id: "+jsonNumber(events[len(events)-1].Sequence)) {
		t.Fatalf("SSE did not replay latest event: %s", sseResponse.Body.String())
	}

	replay := httptest.NewRequest(http.MethodPost, "/api/operations", bytes.NewBufferString(`{"kind":"deploy","server":"prod","app":"web","image":"example/web:1"}`))
	replay.Header.Set("Idempotency-Key", "api-key")
	replayResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusAccepted || replayResponse.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("idempotent replay status=%d headers=%v body=%s", replayResponse.Code, replayResponse.Header(), replayResponse.Body.String())
	}
}

func TestLegacyMutationFlowsReturnAcceptedOperation(t *testing.T) {
	dir := t.TempDir()
	cliPath := dir + "/teploy"
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\necho '{}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	server := operationTestServer(t, false)

	tests := []struct {
		name   string
		target string
		body   string
		call   func(http.ResponseWriter, *http.Request)
	}{
		{"deploy", "/api/deploy", `{"server":"prod","app":"web","image":"web:1"}`, server.handleDeploy},
		{"rollback", "/api/apps/prod/web/rollback", `{}`, func(w http.ResponseWriter, r *http.Request) { server.handleAppPost(w, r, "prod", "web", "rollback") }},
		{"remove", "/api/apps/prod/web/remove", `{"purge":true}`, func(w http.ResponseWriter, r *http.Request) { server.handleAppPost(w, r, "prod", "web", "remove") }},
		{"template install", "/api/templates/install", `{"template":"postgres","domain":"db.example","server":"prod"}`, server.handleTemplateInstall},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			test.call(response, request)
			if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"status":"queued"`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var result struct {
				Data operation.Operation `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if test.name == "deploy" && result.Data.Metadata.Mode != "ad-hoc" {
				t.Fatalf("deploy operation mode = %q", result.Data.Metadata.Mode)
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				current, err := server.operations.Get(result.Data.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.Status.Terminal() {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Fatal("operation did not finish")
		})
	}
}

func TestManifestAPIAndApplyOperation(t *testing.T) {
	commandCh := make(chan operation.Command, 1)
	server := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: name + ".example", User: "deploy"}, nil
		},
		OperationExecutor: func(_ context.Context, command operation.Command, _ func(operation.Stream, string)) (int, error) {
			commandCh <- command
			return 0, nil
		},
	})
	manifestBody := `{"mode":"dash-managed","manifest":"app: web\nimage: example/web:1\ndomain: web.example.com\nenv:\n  DB_PASSWORD: secret:db#password\n"}`
	put := httptest.NewRequest(http.MethodPut, "/api/manifests/prod/web", strings.NewReader(manifestBody))
	putResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(putResponse, put)
	if putResponse.Code != http.StatusCreated {
		t.Fatalf("put status=%d body=%s", putResponse.Code, putResponse.Body.String())
	}
	var stored struct {
		Data struct {
			CurrentRevision string `json:"current_revision"`
		} `json:"data"`
	}
	if err := json.Unmarshal(putResponse.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Data.CurrentRevision == "" || putResponse.Header().Get("ETag") != `"`+stored.Data.CurrentRevision+`"` {
		t.Fatalf("stored response=%s headers=%v", putResponse.Body.String(), putResponse.Header())
	}

	conflict := httptest.NewRequest(http.MethodPut, "/api/manifests/prod/web", strings.NewReader(manifestBody))
	conflict.Header.Set("If-Match", `"`+strings.Repeat("0", 64)+`"`)
	conflictResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(conflictResponse, conflict)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", conflictResponse.Code, conflictResponse.Body.String())
	}

	apply := httptest.NewRequest(http.MethodPost, "/api/manifests/prod/web/apply", nil)
	apply.Header.Set("If-Match", `"`+stored.Data.CurrentRevision+`"`)
	applyResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(applyResponse, apply)
	if applyResponse.Code != http.StatusAccepted {
		t.Fatalf("apply status=%d body=%s", applyResponse.Code, applyResponse.Body.String())
	}
	var applied struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(applyResponse.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	select {
	case command := <-commandCh:
		if len(command.Args) < 6 || command.Args[0] != "--project-dir" || command.Args[2] != "deploy" ||
			command.Args[3] != "--host" || command.Args[4] != "prod.example" {
			t.Fatalf("apply command=%v", command.Args)
		}
		if strings.Contains(strings.Join(command.Args, " "), "../") {
			t.Fatalf("unsafe apply command=%v", command.Args)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("manifest apply command did not execute")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		current, err := server.operations.Get(applied.Data.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("manifest apply operation did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}

	export := httptest.NewRequest(http.MethodGet, "/api/manifests/prod/web/export", nil)
	exportResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(exportResponse, export)
	if exportResponse.Code != http.StatusOK || exportResponse.Header().Get("Content-Type") != "application/yaml; charset=utf-8" ||
		!strings.Contains(exportResponse.Body.String(), "app: web") {
		t.Fatalf("export status=%d headers=%v body=%s", exportResponse.Code, exportResponse.Header(), exportResponse.Body.String())
	}
}

func TestManifestAPIRejectsTraversalSecretsAndProjectDir(t *testing.T) {
	server := operationTestServer(t, false)
	for _, target := range []string{
		"/api/manifests/../web",
		"/api/manifests/prod/%2e%2e",
		"/api/manifests/prod/web/revisions/../../outside",
	} {
		request := httptest.NewRequest(http.MethodPut, target, strings.NewReader(`{"mode":"dash-managed","manifest":"app: web\nimage: web:1\ndomain: web.example.com\n"}`))
		response := httptest.NewRecorder()
		server.handler().ServeHTTP(response, request)
		if response.Code >= 200 && response.Code < 300 {
			t.Fatalf("traversal target %q returned %d", target, response.Code)
		}
	}
	secret := httptest.NewRequest(http.MethodPut, "/api/manifests/prod/web", strings.NewReader(`{"mode":"dash-managed","manifest":"app: web\nimage: web:1\ndomain: web.example.com\nenv:\n  API_TOKEN: plaintext-secret\n"}`))
	secretResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(secretResponse, secret)
	if secretResponse.Code != http.StatusUnprocessableEntity || strings.Contains(secretResponse.Body.String(), "plaintext-secret") {
		t.Fatalf("secret response status=%d body=%s", secretResponse.Code, secretResponse.Body.String())
	}
	directPath := httptest.NewRequest(http.MethodPost, "/api/operations", strings.NewReader(`{"kind":"manifest_apply","server":"prod","app":"web","project_dir":"/tmp/evil"}`))
	directPathResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(directPathResponse, directPath)
	if directPathResponse.Code != http.StatusBadRequest {
		t.Fatalf("project_dir status=%d body=%s", directPathResponse.Code, directPathResponse.Body.String())
	}
}

func jsonNumber(value uint64) string {
	data, _ := json.Marshal(value)
	return string(data)
}
