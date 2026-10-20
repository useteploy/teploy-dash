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

	"golang.org/x/crypto/bcrypt"

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
		{"maintenance on", "/api/apps/prod/web/maintenance/on", `{}`, func(w http.ResponseWriter, r *http.Request) {
			server.handleAppPost(w, r, "prod", "web", "maintenance/on")
		}},
		{"maintenance off", "/api/apps/prod/web/maintenance/off", `{}`, func(w http.ResponseWriter, r *http.Request) {
			server.handleAppPost(w, r, "prod", "web", "maintenance/off")
		}},
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

// The removed manifest/source request surface cannot be re-submitted:
// unknown fields and removed kinds are both refused before anything runs.
func TestRemovedOperationSurfaceRefused(t *testing.T) {
	server := operationTestServer(t, false)
	for _, body := range []string{
		`{"kind":"manifest_apply","server":"prod","app":"web","project_dir":"/tmp/evil"}`,
		`{"kind":"manifest_apply","server":"prod","app":"web","manifest_revision":"` + strings.Repeat("a", 64) + `"}`,
		`{"kind":"deploy","server":"prod","app":"web","source_id":"src-0123456789abcdef","source_commit":"` + strings.Repeat("a", 40) + `"}`,
		`{"kind":"source_preview","server":"prod","app":"web"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/operations", strings.NewReader(body))
		response := httptest.NewRecorder()
		server.handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("removed surface %s status=%d body=%s", body, response.Code, response.Body.String())
		}
		if got := len(server.operations.List("", "", 0)); got != 0 {
			t.Fatalf("removed surface admitted %d operations", got)
		}
	}
}

func jsonNumber(value uint64) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// Lock/unlock are instant state-file toggles with no output worth streaming,
// so they deliberately answer inline instead of enqueueing an operation.
// Pinned as a test so the distinction survives future refactors.
func TestInstantTogglesStaySynchronous(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/teploy", []byte("#!/bin/sh\necho '{}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	server := operationTestServer(t, false)

	for _, action := range []string{"lock", "unlock"} {
		t.Run(action, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/apps/prod/web/"+action, strings.NewReader(`{}`))
			response := httptest.NewRecorder()
			server.handleAppPost(response, request, "prod", "web", action)
			if response.Code == http.StatusAccepted {
				t.Fatalf("%s should answer inline, not enqueue an operation (202)", action)
			}
			if strings.Contains(response.Body.String(), `"status":"queued"`) {
				t.Fatalf("%s returned a queued operation: %s", action, response.Body.String())
			}
		})
	}
}

// A12/A27 remainder: the enqueued operation records WHICH principal admitted
// it — the session identity travels onto the operation's actor, and the
// admission-budget rejection maps to 429.
func TestOperationAPIAttributesActorFromSession(t *testing.T) {
	server := operationTestServer(t, true)
	bcryptCost = bcrypt.MinCost
	t.Cleanup(func() { bcryptCost = bcrypt.DefaultCost })
	if err := server.gate.createUser("dana", "danapass123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	cookie := loginCookie(t, server.gate, "dana", "danapass123")

	body := bytes.NewBufferString(`{"kind":"deploy","server":"prod","app":"web","image":"example/web:1"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/operations", body)
	request.AddCookie(cookie)
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
	if envelope.Data.Actor == nil {
		t.Fatal("operation carries no actor")
	}
	if envelope.Data.Actor.Kind != "local" || envelope.Data.Actor.Subject != "dana" || envelope.Data.Actor.Label != "dana" {
		t.Fatalf("actor = %+v, want local/dana", envelope.Data.Actor)
	}
	// Join the operation before returning so its terminal persist cannot
	// race t.TempDir cleanup.
	waitForTerminalOperation(t, server, envelope.Data.ID)
}

// waitForTerminalOperation polls until the operation reaches a terminal
// status, so tests never leave live runners writing behind them.
func waitForTerminalOperation(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if op, err := s.operations.Get(id); err == nil && op.Status.Terminal() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("operation %s never reached a terminal status", id)
}

func TestOperationAdmissionBudgetMapsTo429(t *testing.T) {
	blocker := make(chan struct{})
	config := Config{
		DataDir: t.TempDir(), NoAuth: true,
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: name + ".example", User: "deploy"}, nil
		},
		OperationExecutor: func(_ context.Context, _ operation.Command, _ func(operation.Stream, string)) (int, error) {
			<-blocker
			return 0, nil
		},
		OperationMaxQueued: 1,
	}
	server := New(config)
	post := func(image string) *httptest.ResponseRecorder {
		body := bytes.NewBufferString(`{"kind":"deploy","server":"prod","app":"web","image":"` + image + `"}`)
		rec := httptest.NewRecorder()
		server.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/operations", body))
		return rec
	}
	if rec := post("img:1"); rec.Code != http.StatusAccepted {
		t.Fatalf("first enqueue status = %d body=%s", rec.Code, rec.Body.String())
	}
	// Wait until the first op is running (holding the target's budget).
	var firstID string
	deadline := time.Now().Add(3 * time.Second)
	running := false
	for time.Now().Before(deadline) && !running {
		for _, op := range server.operations.List("", "", 0) {
			firstID = op.ID
			if op.Status == operation.StatusRunning {
				running = true
			}
		}
		if !running {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if !running {
		t.Fatal("first operation never started running")
	}
	rec := post("img:2")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("budget-exceeded status = %d body=%s, want 429", rec.Code, rec.Body.String())
	}
	close(blocker)
	waitForTerminalOperation(t, server, firstID)
	server.DrainOperations(context.Background())
}

// D02 idempotency namespacing (A12/A13/R20): the Idempotency-Key header is
// scoped to the authenticated principal. Two sessions submitting the SAME key
// enqueue two independent operations; the same session replays.
func TestOperationIdempotencyNamesBySessionPrincipal(t *testing.T) {
	server := operationTestServer(t, true)
	bcryptCost = bcrypt.MinCost
	t.Cleanup(func() { bcryptCost = bcrypt.DefaultCost })
	if err := server.gate.createUser("dana", "danapass123", RoleEditor); err != nil {
		t.Fatal(err)
	}
	if err := server.gate.createUser("evan", "evanpass123", RoleEditor); err != nil {
		t.Fatal(err)
	}
	dana := loginCookie(t, server.gate, "dana", "danapass123")
	evan := loginCookie(t, server.gate, "evan", "evanpass123")

	post := func(cookie *http.Cookie, image, key string) (int, string, string) {
		body := bytes.NewBufferString(`{"kind":"deploy","server":"prod","app":"web","image":"` + image + `"}`)
		request := httptest.NewRequest(http.MethodPost, "/api/operations", body)
		request.AddCookie(cookie)
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		server.handler().ServeHTTP(response, request)
		var envelope struct {
			Data operation.Operation `json:"data"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &envelope)
		return response.Code, envelope.Data.ID, response.Header().Get("Idempotency-Replayed")
	}

	code, danaID, replayed := post(dana, "example/web:1", "release-7")
	if code != http.StatusAccepted || danaID == "" || replayed == "true" {
		t.Fatalf("dana's enqueue: code=%d id=%s replayed=%s", code, danaID, replayed)
	}
	waitForTerminalOperation(t, server, danaID)

	// Same key, DIFFERENT principal: a distinct operation, not a replay.
	code, evanID, replayed := post(evan, "example/web:1", "release-7")
	if code != http.StatusAccepted || evanID == "" || evanID == danaID || replayed == "true" {
		t.Fatalf("evan's same-key enqueue must be independent: code=%d dana=%s evan=%s replayed=%s", code, danaID, evanID, replayed)
	}
	waitForTerminalOperation(t, server, evanID)

	// Same principal, same key: replay.
	code, replayID, replayed := post(dana, "example/web:1", "release-7")
	if code != http.StatusAccepted || replayID != danaID || replayed != "true" {
		t.Fatalf("dana's replay: code=%d id=%s replayed=%s", code, replayID, replayed)
	}
}
