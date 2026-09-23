package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/source"
)

// sourceTestServer wires the D04 webhook surface the way production does:
// an operations manager with an injected resolver/executor (commands are
// captured, never executed) and the manifest store the webhook joins
// sources against.
func sourceTestServer(t *testing.T, verifier func(context.Context, *source.Source) error) (*Server, *commandCapture) {
	t.Helper()
	capture := &commandCapture{}
	config := Config{
		DataDir: t.TempDir(), NoAuth: true,
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: name + ".example", User: "deploy"}, nil
		},
		OperationExecutor: func(_ context.Context, command operation.Command, _ func(operation.Stream, string)) (int, error) {
			capture.record(command)
			return 0, nil
		},
		SourceCredentialVerifier: verifier,
	}
	server := New(config)
	if server.sourceInitErr != nil {
		t.Fatalf("source init: %v", server.sourceInitErr)
	}
	return server, capture
}

// gatedTestServer holds every CLI execution until release is closed, so a
// target can be kept BUSY (running operation) while later deliveries queue
// behind it — the deterministic shape of the reorder-convergence scenario.
func gatedTestServer(t *testing.T, verifier func(context.Context, *source.Source) error) (*Server, *commandCapture, chan struct{}) {
	t.Helper()
	capture := &commandCapture{}
	release := make(chan struct{})
	config := Config{
		DataDir: t.TempDir(), NoAuth: true,
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: name + ".example", User: "deploy"}, nil
		},
		OperationExecutor: func(ctx context.Context, command operation.Command, _ func(operation.Stream, string)) (int, error) {
			select {
			case <-release:
				capture.record(command)
				return 0, nil
			case <-ctx.Done():
				return -1, ctx.Err()
			}
		},
		SourceCredentialVerifier: verifier,
	}
	server := New(config)
	if server.sourceInitErr != nil {
		t.Fatalf("source init: %v", server.sourceInitErr)
	}
	return server, capture, release
}

type commandCapture struct {
	mu       sync.Mutex
	commands []operation.Command
}

func (c *commandCapture) record(command operation.Command) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commands = append(c.commands, command)
}

func (c *commandCapture) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.commands)
}

func signGitHub(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// createBoundSource registers a source for the repository and a git-managed
// manifest bound to it, returning the source ID + webhook secret.
func createBoundSource(t *testing.T, s *Server, repoURL, defaultBranch string) (id, secret string) {
	t.Helper()
	body := fmt.Sprintf(`{"forge":"github","clone_url":%q,"default_branch":%q,"display_name":"app repo"}`, repoURL, defaultBranch)
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sources", strings.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create source status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Data source.CreatedSource `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id = created.Data.ID
	secret = created.Data.WebhookSecret

	manifestBody := `{"mode":"git-managed","git":{"repository":` + jsonQuote(repoURL) + `,"revision":"0123456789abcdef0123456789abcdef01234567"},"manifest":"app: web\nimage: example/web:1\ndomain: web.example.com\n"}`
	manifestResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(manifestResponse, httptest.NewRequest(http.MethodPut, "/api/manifests/prod/web", strings.NewReader(manifestBody)))
	if manifestResponse.Code != http.StatusCreated {
		t.Fatalf("create manifest status=%d body=%s", manifestResponse.Code, manifestResponse.Body.String())
	}
	return id, secret
}

func jsonQuote(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

func pushDelivery(t *testing.T, s *Server, id, secret, deliveryID, commit string) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(`{"ref":"refs/heads/main","after":"` + commit + `"}`)
	request := httptest.NewRequest(http.MethodPost, "/hooks/sources/"+id, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	request.Header.Set("X-Hub-Signature-256", signGitHub(secret, body))
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, request)
	return response
}

func waitTerminal(t *testing.T, s *Server, opID string) *operation.Operation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		op, err := s.operations.Get(opID)
		if err != nil {
			t.Fatal(err)
		}
		if op.Status.Terminal() {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return nil
}

func TestSourceAPIRequiresAuthentication(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	authed := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	// Build an auth-mode server for this one assertion.
	config := Config{
		DataDir: t.TempDir(), AuthUser: "admin", AuthPass: "secret",
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: name + ".example"}, nil
		},
		OperationExecutor: func(context.Context, operation.Command, func(operation.Stream, string)) (int, error) { return 0, nil },
	}
	_ = authed
	authServer := New(config)
	response := httptest.NewRecorder()
	authServer.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sources", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status=%d, want 401", response.Code)
	}
	// The no-auth test server still lists fine.
	ok := httptest.NewRecorder()
	s.handler().ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/api/sources", nil))
	if ok.Code != http.StatusOK {
		t.Fatalf("no-auth list status=%d body=%s", ok.Code, ok.Body.String())
	}
}

func TestSourceCreateNormalizesAndCollides(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	created := httptest.NewRecorder()
	s.handler().ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/sources", strings.NewReader(`{"forge":"github","clone_url":"git@github.com:tyler/akiroo.git"}`)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var envelope struct {
		Data source.CreatedSource `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.CloneURL != "https://github.com/tyler/akiroo" {
		t.Fatalf("normalized clone URL = %q", envelope.Data.CloneURL)
	}
	if envelope.Data.WebhookSecret == "" {
		t.Fatal("create must return the webhook secret exactly once")
	}

	// The same repository under any spelling collides (409).
	duplicate := httptest.NewRecorder()
	s.handler().ServeHTTP(duplicate, httptest.NewRequest(http.MethodPost, "/api/sources", strings.NewReader(`{"forge":"github","clone_url":"https://GITHUB.COM/Tyler/Akiroo"}`)))
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}

	// The secret never appears again.
	list := httptest.NewRecorder()
	s.handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/sources", nil))
	if strings.Contains(list.Body.String(), envelope.Data.WebhookSecret) {
		t.Fatal("webhook secret must never appear in later reads")
	}
	if !strings.Contains(list.Body.String(), `"webhook_secret_set":true`) {
		t.Fatalf("list should advertise the secret exists: %s", list.Body.String())
	}

	// A different forge with the same owner/repo does NOT collide (F03).
	other := httptest.NewRecorder()
	s.handler().ServeHTTP(other, httptest.NewRequest(http.MethodPost, "/api/sources", strings.NewReader(`{"forge":"forgejo","clone_url":"https://forge.example/tyler/akiroo"}`)))
	if other.Code != http.StatusCreated {
		t.Fatalf("second forge status=%d body=%s", other.Code, other.Body.String())
	}
}

func TestSourceWebhookAuthentication(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	id, secret := createBoundSource(t, s, "https://github.com/team/app", "main")
	body := []byte(`{"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567"}`)

	bad := httptest.NewRequest(http.MethodPost, "/hooks/sources/"+id, bytes.NewReader(body))
	bad.Header.Set("X-GitHub-Event", "push")
	bad.Header.Set("X-GitHub-Delivery", "d-bad")
	bad.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	badResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status=%d body=%s", badResponse.Code, badResponse.Body.String())
	}

	unsigned := httptest.NewRequest(http.MethodPost, "/hooks/sources/"+id, bytes.NewReader(body))
	unsigned.Header.Set("X-GitHub-Event", "push")
	unsigned.Header.Set("X-GitHub-Delivery", "d-unsigned")
	unsignedResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(unsignedResponse, unsigned)
	if unsignedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned status=%d", unsignedResponse.Code)
	}

	// GitLab's token scheme verifies against the same secret.
	gitlab := httptest.NewRequest(http.MethodPost, "/hooks/sources/"+id, bytes.NewReader(body))
	gitlab.Header.Set("X-Gitlab-Event", "Push Hook")
	gitlab.Header.Set("X-Gitlab-Event-UUID", "d-gitlab")
	gitlab.Header.Set("X-Gitlab-Token", secret)
	gitlabResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(gitlabResponse, gitlab)
	if gitlabResponse.Code != http.StatusOK {
		t.Fatalf("gitlab token status=%d body=%s", gitlabResponse.Code, gitlabResponse.Body.String())
	}
}

// The load-bearing D04 convergence assertions: duplicate deliveries are
// deduped by delivery id, and while a delivery's operation is still QUEUED
// (the target is busy with a running deploy) a newer delivery supersedes
// it — C02's newest-wins riding the D02 queue, never a second queue and
// never an interruption of the RUNNING operation.
func TestSourceWebhookDedupeAndReorderConverge(t *testing.T) {
	s, capture, release := gatedTestServer(t, nil)
	id, secret := createBoundSource(t, s, "https://github.com/team/app", "main")

	// Hold the target busy with a manual deploy (blocked in the executor).
	manual := httptest.NewRecorder()
	s.handler().ServeHTTP(manual, httptest.NewRequest(http.MethodPost, "/api/operations", strings.NewReader(`{"kind":"deploy","server":"prod","app":"web","image":"example/web:0"}`)))
	if manual.Code != http.StatusAccepted {
		t.Fatalf("manual deploy status=%d body=%s", manual.Code, manual.Body.String())
	}
	var manualEnvelope struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(manual.Body.Bytes(), &manualEnvelope); err != nil {
		t.Fatal(err)
	}

	first := pushDelivery(t, s, id, secret, "d-1", strings.Repeat("a", 40))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"status":"admitted"`) {
		t.Fatalf("first delivery status=%d body=%s", first.Code, first.Body.String())
	}
	var admitted struct {
		OperationIDs []string `json:"operation_ids"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	if len(admitted.OperationIDs) != 1 {
		t.Fatalf("one bound manifest must admit one operation: %s", first.Body.String())
	}

	// Duplicate delivery id: no second operation, explicit duplicate ack.
	duplicate := pushDelivery(t, s, id, secret, "d-1", strings.Repeat("a", 40))
	if duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), `"status":"duplicate"`) {
		t.Fatalf("duplicate delivery status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}

	// A newer delivery (different id, different commit) supersedes the
	// still-QUEUED operation from d-1 and admits its own. The RUNNING
	// manual deploy is never touched.
	newer := pushDelivery(t, s, id, secret, "d-2", strings.Repeat("b", 40))
	if newer.Code != http.StatusOK || !strings.Contains(newer.Body.String(), `"status":"admitted"`) {
		t.Fatalf("newer delivery status=%d body=%s", newer.Code, newer.Body.String())
	}
	var newerAdmitted struct {
		OperationIDs []string `json:"operation_ids"`
	}
	if err := json.Unmarshal(newer.Body.Bytes(), &newerAdmitted); err != nil {
		t.Fatal(err)
	}

	// Release the executor: the manual deploy completes first (FIFO), then
	// the newest delivery's operation runs.
	close(release)

	superseded := waitTerminal(t, s, admitted.OperationIDs[0])
	if superseded.Status != operation.StatusCanceled {
		t.Fatalf("superseded queued operation status=%s, want canceled", superseded.Status)
	}
	if manualOp := waitTerminal(t, s, manualEnvelope.Data.ID); manualOp.Status != operation.StatusSucceeded {
		t.Fatalf("running manual deploy status=%s, want succeeded (never interrupted)", manualOp.Status)
	}
	final := waitTerminal(t, s, newerAdmitted.OperationIDs[0])
	if final.Status != operation.StatusSucceeded {
		t.Fatalf("newest operation status=%s, want succeeded", final.Status)
	}
	if final.Request.SourceCommit != strings.Repeat("b", 40) {
		t.Fatalf("newest operation pinned commit %q", final.Request.SourceCommit)
	}

	// Convergence: the manual deploy + the NEWEST delivery's command —
	// d-1's superseded operation never executed.
	if got := capture.len(); got != 2 {
		t.Fatalf("executed commands = %d, want 2 (manual + newest)", got)
	}

	// The delivery ledger shows both deliveries with honest dispositions.
	detail := httptest.NewRecorder()
	s.handler().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/sources/"+id, nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status=%d", detail.Code)
	}
	for _, want := range []string{`"disposition":"admitted"`, `"disposition":"superseded"`} {
		if !strings.Contains(detail.Body.String(), want) {
			t.Fatalf("delivery ledger missing %s: %s", want, detail.Body.String())
		}
	}
}

// The forwarded operation carries the source identity and the payload pin
// (D04: "builds against the exact commit"), attributed to the webhook
// principal namespace.
func TestSourceWebhookOperationCarriesSourceAndCommit(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	id, secret := createBoundSource(t, s, "https://github.com/team/app", "main")
	commit := strings.Repeat("c", 40)
	response := pushDelivery(t, s, id, secret, "d-pin", commit)
	if response.Code != http.StatusOK {
		t.Fatalf("delivery status=%d body=%s", response.Code, response.Body.String())
	}
	var admitted struct {
		OperationIDs []string `json:"operation_ids"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	op := waitTerminal(t, s, admitted.OperationIDs[0])
	if op.Request.SourceID != id {
		t.Fatalf("operation source id = %q, want %q", op.Request.SourceID, id)
	}
	if op.Request.SourceCommit != commit {
		t.Fatalf("operation commit = %q", op.Request.SourceCommit)
	}
	if op.Request.Kind != operation.KindManifestApply || op.Request.Mode != "git-managed" {
		t.Fatalf("operation = %s/%s", op.Request.Kind, op.Request.Mode)
	}
	if op.Actor == nil || op.Actor.Kind != "webhook" || op.Actor.Subject != "source/"+id {
		t.Fatalf("operation actor = %#v", op.Actor)
	}
}

// Pings, tags/deletions (no commit pin), and pushes to unwatched branches
// are recorded-and-ignored, never silently dropped and never admitted.
func TestSourceWebhookIgnoresNonDeployableDeliveries(t *testing.T) {
	s, capture := sourceTestServer(t, nil)
	id, secret := createBoundSource(t, s, "https://github.com/team/app", "main")

	ping := []byte(`{}`)
	request := httptest.NewRequest(http.MethodPost, "/hooks/sources/"+id, bytes.NewReader(ping))
	request.Header.Set("X-GitHub-Event", "ping")
	request.Header.Set("X-GitHub-Delivery", "d-ping")
	request.Header.Set("X-Hub-Signature-256", signGitHub(secret, ping))
	pingResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(pingResponse, request)
	if pingResponse.Code != http.StatusOK || !strings.Contains(pingResponse.Body.String(), `"status":"ignored"`) {
		t.Fatalf("ping status=%d body=%s", pingResponse.Code, pingResponse.Body.String())
	}

	otherBranch := []byte(`{"ref":"refs/heads/feature","after":"` + strings.Repeat("a", 40) + `"}`)
	request = httptest.NewRequest(http.MethodPost, "/hooks/sources/"+id, bytes.NewReader(otherBranch))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", "d-branch")
	request.Header.Set("X-Hub-Signature-256", signGitHub(secret, otherBranch))
	branchResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(branchResponse, request)
	if branchResponse.Code != http.StatusOK || !strings.Contains(branchResponse.Body.String(), "does not match") {
		t.Fatalf("branch filter status=%d body=%s", branchResponse.Code, branchResponse.Body.String())
	}

	if capture.len() != 0 {
		t.Fatalf("non-deployable deliveries must not execute anything, got %d", capture.len())
	}
	// Both deliveries are visible in the ledger.
	detail := httptest.NewRecorder()
	s.handler().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/sources/"+id, nil))
	if !strings.Contains(detail.Body.String(), "d-ping") || !strings.Contains(detail.Body.String(), "d-branch") {
		t.Fatalf("ignored deliveries must be recorded: %s", detail.Body.String())
	}
}

// Revoked-permission handling: a failing credential verification marks the
// source degraded (visible, exact reason), deliveries on a degraded source
// are recorded and refused admission — never silently dropped — and a
// later successful verification clears it.
func TestSourceRevokedCredentialDegradesVisibly(t *testing.T) {
	s, capture := sourceTestServer(t, func(_ context.Context, src *source.Source) error {
		if src.CredentialRef == "good-token" {
			return nil
		}
		return fmt.Errorf("forge 403: token revoked (ref %q)", src.CredentialRef)
	})
	id, secret := createBoundSource(t, s, "https://github.com/team/app", "main")

	// No credential reference: nothing to verify.
	noRef := httptest.NewRecorder()
	s.handler().ServeHTTP(noRef, httptest.NewRequest(http.MethodPost, "/api/sources/"+id+"/verify", nil))
	if noRef.Code != http.StatusBadRequest {
		t.Fatalf("verify without credential ref status=%d body=%s", noRef.Code, noRef.Body.String())
	}

	patch := httptest.NewRecorder()
	s.handler().ServeHTTP(patch, httptest.NewRequest(http.MethodPatch, "/api/sources/"+id, strings.NewReader(`{"credential_ref":"revoked-token"}`)))
	if patch.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", patch.Code, patch.Body.String())
	}

	verify := httptest.NewRecorder()
	s.handler().ServeHTTP(verify, httptest.NewRequest(http.MethodPost, "/api/sources/"+id+"/verify", nil))
	if verify.Code != http.StatusOK || !strings.Contains(verify.Body.String(), "degraded") {
		t.Fatalf("verify status=%d body=%s", verify.Code, verify.Body.String())
	}

	list := httptest.NewRecorder()
	s.handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/sources", nil))
	if !strings.Contains(list.Body.String(), `"degraded":true`) || !strings.Contains(list.Body.String(), "token revoked") {
		t.Fatalf("degraded source must be visible with the exact reason: %s", list.Body.String())
	}

	delivery := pushDelivery(t, s, id, secret, "d-degraded", strings.Repeat("a", 40))
	if delivery.Code != http.StatusOK || !strings.Contains(delivery.Body.String(), "source degraded") {
		t.Fatalf("delivery on degraded source status=%d body=%s", delivery.Code, delivery.Body.String())
	}
	if capture.len() != 0 {
		t.Fatal("a degraded source must not admit operations")
	}
	detail := httptest.NewRecorder()
	s.handler().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/sources/"+id, nil))
	if !strings.Contains(detail.Body.String(), `"disposition":"degraded"`) {
		t.Fatalf("the refused delivery must be recorded: %s", detail.Body.String())
	}

	// Recovery: a good credential clears the degraded state and deliveries
	// admit again.
	fix := httptest.NewRecorder()
	s.handler().ServeHTTP(fix, httptest.NewRequest(http.MethodPatch, "/api/sources/"+id, strings.NewReader(`{"credential_ref":"good-token"}`)))
	if fix.Code != http.StatusOK {
		t.Fatalf("fix patch status=%d", fix.Code)
	}
	recovered := httptest.NewRecorder()
	s.handler().ServeHTTP(recovered, httptest.NewRequest(http.MethodPost, "/api/sources/"+id+"/verify", nil))
	if recovered.Code != http.StatusOK || strings.Contains(recovered.Body.String(), `"degraded":true`) {
		t.Fatalf("recovery verify status=%d body=%s", recovered.Code, recovered.Body.String())
	}
	ok := pushDelivery(t, s, id, secret, "d-recovered", strings.Repeat("d", 40))
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), `"status":"admitted"`) {
		t.Fatalf("recovered delivery status=%d body=%s", ok.Code, ok.Body.String())
	}
}

func TestSourceWebhookUnknownSource(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/hooks/sources/src-0123456789abcdef", strings.NewReader(`{}`)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown source status=%d", response.Code)
	}
}

func TestSourceDelete(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	id, _ := createBoundSource(t, s, "https://github.com/team/app", "main")
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sources/"+id, nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	gone := httptest.NewRecorder()
	s.handler().ServeHTTP(gone, httptest.NewRequest(http.MethodGet, "/api/sources/"+id, nil))
	if gone.Code != http.StatusNotFound {
		t.Fatalf("get after delete status=%d", gone.Code)
	}
	// The webhook endpoint is gone too.
	hook := httptest.NewRecorder()
	s.handler().ServeHTTP(hook, httptest.NewRequest(http.MethodPost, "/hooks/sources/"+id, strings.NewReader(`{}`)))
	if hook.Code != http.StatusNotFound {
		t.Fatalf("hook after delete status=%d", hook.Code)
	}
}
