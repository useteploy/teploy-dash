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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/source"
)

// sourceTestServer builds a no-auth server whose push-head authority is a
// fixture that always confirms the delivered commit.
func sourceTestServer(t *testing.T, verifier func(context.Context, *source.Source) error) (*Server, *commandCapture) {
	t.Helper()
	capture := &commandCapture{}
	config := Config{
		DataDir: t.TempDir(), NoAuth: true,
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, ID: "srv-0123456789abcdef", Host: name + ".example", User: "deploy"}, nil
		},
		OperationExecutor: func(_ context.Context, command operation.Command, _ func(operation.Stream, string)) (int, error) {
			capture.record(command)
			return 0, nil
		},
		SourceCredentialVerifier: verifier,
	}
	server := New(config)
	server.sourcePushAuthority = func(context.Context, *source.Source, string, string) error { return nil }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		server.DrainOperations(ctx)
	})
	if server.sourceInitErr != nil {
		t.Fatalf("source init: %v", server.sourceInitErr)
	}
	return server, capture
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

// createSource registers a source through the API and returns its ID and
// the once-only webhook secret.
func createSource(t *testing.T, s *Server, forge, repoURL, defaultBranch string) (id, secret string) {
	t.Helper()
	body := fmt.Sprintf(`{"forge":%q,"clone_url":%q,"default_branch":%q,"display_name":"app repo"}`, forge, repoURL, defaultBranch)
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
	return created.Data.ID, created.Data.WebhookSecret
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

func sourceDetail(t *testing.T, s *Server, id string) string {
	t.Helper()
	detail := httptest.NewRecorder()
	s.handler().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/sources/"+id, nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	return detail.Body.String()
}

const noAppConnected = "no app is connected to this repository yet"

func TestSourceAPIRequiresAuthentication(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	authed := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	// Build an auth-mode server for this one assertion.
	config := Config{
		DataDir: t.TempDir(), AuthUser: "admin", AuthPass: "secret",
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, ID: "srv-0123456789abcdef", Host: name + ".example"}, nil
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
	id, secret := createSource(t, s, "github", "https://github.com/team/app", "main")
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

	// A GitLab token cannot authenticate a GitHub source.
	gitlab := httptest.NewRequest(http.MethodPost, "/hooks/sources/"+id, bytes.NewReader(body))
	gitlab.Header.Set("X-Gitlab-Event", "Push Hook")
	gitlab.Header.Set("X-Gitlab-Event-UUID", "d-gitlab")
	gitlab.Header.Set("X-Gitlab-Token", secret)
	gitlabResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(gitlabResponse, gitlab)
	if gitlabResponse.Code != http.StatusUnauthorized {
		t.Fatalf("cross-forge gitlab token status=%d body=%s", gitlabResponse.Code, gitlabResponse.Body.String())
	}
}

// An authenticated, verified push is durably recorded and ignored: nothing
// is enqueued until an app is connected to the repository. A replay of the
// same delivery id answers duplicate.
func TestSourceWebhookRecordsPushAsIgnoredAndDedupes(t *testing.T) {
	s, capture := sourceTestServer(t, nil)
	id, secret := createSource(t, s, "github", "https://github.com/team/app", "main")
	var probes []string
	s.sourcePushAuthority = func(_ context.Context, _ *source.Source, branch, commit string) error {
		probes = append(probes, branch+"@"+commit)
		return nil
	}
	commit := strings.Repeat("a", 40)
	first := pushDelivery(t, s, id, secret, "d-1", commit)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"disposition":"ignored"`) || !strings.Contains(first.Body.String(), noAppConnected) {
		t.Fatalf("first delivery status=%d body=%s", first.Code, first.Body.String())
	}
	if len(probes) != 1 || probes[0] != "main@"+commit {
		t.Fatalf("push head not verified before recording: %v", probes)
	}
	duplicate := pushDelivery(t, s, id, secret, "d-1", commit)
	if duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), `"status":"duplicate"`) {
		t.Fatalf("duplicate delivery status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	if len(probes) != 1 {
		t.Fatalf("duplicate re-ran verification: %v", probes)
	}
	if capture.len() != 0 || len(s.operations.List("", "", 0)) != 0 {
		t.Fatal("a delivery must not enqueue operations")
	}
	detail := sourceDetail(t, s, id)
	if !strings.Contains(detail, `"id":"d-1"`) || !strings.Contains(detail, commit) || !strings.Contains(detail, noAppConnected) {
		t.Fatalf("delivery not recorded: %s", detail)
	}

	// The record survives a store restart: the same id is still a duplicate.
	restarted, err := source.New(s.config.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	s.sources = restarted
	if again := pushDelivery(t, s, id, secret, "d-1", commit); !strings.Contains(again.Body.String(), `"status":"duplicate"`) {
		t.Fatalf("duplicate after restart: %s", again.Body.String())
	}
}

// Pings, tags/deletions (no commit pin), pushes to unwatched branches and
// pull request events are recorded-and-ignored, never silently dropped.
func TestSourceWebhookIgnoresNonDeployableDeliveries(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	id, secret := createSource(t, s, "github", "https://github.com/team/app", "main")
	probes := 0
	s.sourcePushAuthority = func(context.Context, *source.Source, string, string) error { probes++; return nil }
	deliver := func(event, deliveryID string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/hooks/sources/"+id, bytes.NewReader(body))
		request.Header.Set("X-GitHub-Event", event)
		request.Header.Set("X-GitHub-Delivery", deliveryID)
		request.Header.Set("X-Hub-Signature-256", signGitHub(secret, body))
		response := httptest.NewRecorder()
		s.handler().ServeHTTP(response, request)
		return response
	}
	for _, tc := range []struct {
		event, id, body, reason string
	}{
		{"ping", "d-ping", `{}`, "is not a push"},
		{"push", "d-branch", `{"ref":"refs/heads/feature","after":"` + strings.Repeat("a", 40) + `"}`, "does not match"},
		{"push", "d-tag", `{"ref":"refs/tags/v1","after":"` + strings.Repeat("a", 40) + `"}`, "no pinnable commit"},
		{"push", "d-del", `{"ref":"refs/heads/main","deleted":true,"after":"` + strings.Repeat("d", 40) + `"}`, "deletion"},
		{"pull_request", "d-pr", `{"action":"opened","number":1}`, "pull request previews are not supported yet"},
	} {
		response := deliver(tc.event, tc.id, []byte(tc.body))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"disposition":"ignored"`) || !strings.Contains(response.Body.String(), tc.reason) {
			t.Fatalf("%s status=%d body=%s", tc.id, response.Code, response.Body.String())
		}
	}
	if probes != 0 {
		t.Fatalf("non-push deliveries consulted push authority %d times", probes)
	}
	detail := sourceDetail(t, s, id)
	for _, want := range []string{"d-ping", "d-branch", "d-tag", "d-del", "d-pr"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("ignored delivery %s not recorded: %s", want, detail)
		}
	}
}

// A push-head verification failure is unavailable, not stale: it is not
// recorded, so the forge's retry with the same delivery id re-runs it. A
// proven mismatch is an honest durable ignore. The authority here is the
// production path (resolveSourceAccess -> Access.PushHead) against a real
// generic-forge git transport.
func TestSourceWebhookPushAuthorityFailureStaysRetryable(t *testing.T) {
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("git required")
	}
	s, _ := sourceTestServer(t, nil)
	id, secret := createSource(t, s, "generic", "https://git.example/team/app", "main")
	s.sourcePushAuthority = nil
	pointAccessAt := func(repository string) {
		s.sourceAccessResolver = func(context.Context, *source.Source) (*source.Access, error) {
			return &source.Access{Provider: source.Provider{Forge: source.ForgeGeneric, Repository: repository, AllowPrivate: true}, Username: "fixture", Token: "fixture"}, nil
		}
	}
	advertise := func(sha, ref string) *httptest.Server {
		line := func(s string) []byte {
			b := []byte(s)
			out := []byte(fmt.Sprintf("%04x", len(b)+4))
			return append(out, b...)
		}
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			body := line("# service=git-upload-pack\n")
			body = append(body, '0', '0', '0', '0')
			body = append(body, line(sha+" "+ref+"\n")...)
			body = append(body, '0', '0', '0', '0')
			w.Write(body)
		}))
	}

	pointAccessAt("http://127.0.0.1:1/repo.git")
	failed := pushDelivery(t, s, id, secret, "d-transport", strings.Repeat("b", 40))
	if failed.Code != http.StatusServiceUnavailable || !strings.Contains(failed.Body.String(), "push authority unavailable") {
		t.Fatalf("transport failure status=%d body=%s", failed.Code, failed.Body.String())
	}
	retry := pushDelivery(t, s, id, secret, "d-transport", strings.Repeat("b", 40))
	if retry.Code != http.StatusServiceUnavailable || strings.Contains(retry.Body.String(), "duplicate") {
		t.Fatalf("retry swallowed as duplicate: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if strings.Contains(sourceDetail(t, s, id), "d-transport") {
		t.Fatal("unavailable authority must not durably record the delivery")
	}

	mismatch := advertise(strings.Repeat("c", 40), "refs/heads/main")
	defer mismatch.Close()
	pointAccessAt(mismatch.URL + "/repo.git")
	stale := pushDelivery(t, s, id, secret, "d-stale", strings.Repeat("a", 40))
	if stale.Code != http.StatusOK || !strings.Contains(stale.Body.String(), `"disposition":"ignored"`) || !strings.Contains(stale.Body.String(), "no longer authoritative") {
		t.Fatalf("proven mismatch status=%d body=%s", stale.Code, stale.Body.String())
	}

	healed := advertise(strings.Repeat("b", 40), "refs/heads/main")
	defer healed.Close()
	pointAccessAt(healed.URL + "/repo.git")
	recovered := pushDelivery(t, s, id, secret, "d-transport", strings.Repeat("b", 40))
	if recovered.Code != http.StatusOK || !strings.Contains(recovered.Body.String(), noAppConnected) {
		t.Fatalf("same-id recovery status=%d body=%s", recovered.Code, recovered.Body.String())
	}
	if len(s.operations.List("", "", 0)) != 0 {
		t.Fatal("a delivery must not enqueue operations")
	}
}

// Native headers and signatures per forge, verified against real provider
// HTTP branch authority: current head is recorded, a stale head is ignored.
func TestSourceNativePushAuthorityPerForge(t *testing.T) {
	for _, forge := range []source.Forge{source.ForgeGitHub, source.ForgeGitLab, source.ForgeGitea, source.ForgeForgejo} {
		t.Run(string(forge), func(t *testing.T) {
			s, _ := sourceTestServer(t, nil)
			s.sourcePushAuthority = nil
			created, err := s.sources.Create(source.CreateInput{Forge: forge, CloneURL: "https://forge.test/team/app", DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			id, secret := created.ID, created.WebhookSecret
			sha := strings.Repeat("b", 40)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"commit":{"sha":%q,"id":%q}}`, sha, sha)
			}))
			defer api.Close()
			s.sourceAccessResolver = func(context.Context, *source.Source) (*source.Access, error) {
				return &source.Access{Provider: source.Provider{Forge: forge, Repository: "https://forge.test/team/app", APIURL: api.URL}, Client: api.Client()}, nil
			}
			send := func(delivery, commit string, corrupt bool) *httptest.ResponseRecorder {
				body := []byte(fmt.Sprintf(`{"ref":"refs/heads/main","after":%q,"checkout_sha":%q}`, commit, commit))
				r := httptest.NewRequest("POST", "/hooks/sources/"+id, strings.NewReader(string(body)))
				eventHeader, idHeader, sigHeader := "X-GitHub-Event", "X-GitHub-Delivery", "X-Hub-Signature-256"
				signature := signGitHub(secret, body)
				switch forge {
				case source.ForgeGitLab:
					eventHeader, idHeader, sigHeader = "X-Gitlab-Event", "X-Gitlab-Event-UUID", "X-Gitlab-Signature"
				case source.ForgeGitea:
					eventHeader, idHeader, sigHeader = "X-Gitea-Event", "X-Gitea-Delivery", "X-Gitea-Signature"
					signature = strings.TrimPrefix(signature, "sha256=")
				case source.ForgeForgejo:
					eventHeader, idHeader, sigHeader = "X-Forgejo-Event", "X-Forgejo-Delivery", "X-Forgejo-Signature"
					signature = strings.TrimPrefix(signature, "sha256=")
				}
				if corrupt {
					signature = strings.Repeat("0", len(signature))
				}
				r.Header.Set(eventHeader, "push")
				r.Header.Set(idHeader, delivery)
				r.Header.Set(sigHeader, signature)
				result := httptest.NewRecorder()
				s.handler().ServeHTTP(result, r)
				return result
			}
			if bad := send("native-bad", sha, true); bad.Code != http.StatusUnauthorized {
				t.Fatalf("bad signature %d %s", bad.Code, bad.Body.String())
			}
			current := send("native-B", sha, false)
			if current.Code != 200 || !strings.Contains(current.Body.String(), noAppConnected) {
				t.Fatalf("native delivery %d %s", current.Code, current.Body.String())
			}
			old := send("native-A", strings.Repeat("a", 40), false)
			if old.Code != 200 || !strings.Contains(old.Body.String(), "no longer authoritative") {
				t.Fatalf("stale delivery %d %s", old.Code, old.Body.String())
			}
			if strings.Contains(sourceDetail(t, s, id), "native-bad") {
				t.Fatal("unauthenticated delivery recorded")
			}
		})
	}
}

func TestSourceWebhookAuthenticationIsForgeScoped(t *testing.T) {
	body := []byte(`{"after":"fixture"}`)
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Gitlab-Token", "fixture-secret")
	if deliveryAuthenticatedFor(source.ForgeGitHub, r, "fixture-secret", body) {
		t.Fatal("GitLab token header authenticated a GitHub source")
	}
	if !deliveryAuthenticatedFor(source.ForgeGitLab, r, "fixture-secret", body) {
		t.Fatal("GitLab token rejected")
	}
	r.Header.Del("X-Gitlab-Token")
	r.Header.Set("X-Hub-Signature-256", signGitHub("fixture-secret", body))
	if !deliveryAuthenticatedFor(source.ForgeGitHub, r, "fixture-secret", body) {
		t.Fatal("GitHub signature rejected")
	}
	if deliveryAuthenticatedFor(source.ForgeGitLab, r, "fixture-secret", body) {
		t.Fatal("GitHub signature authenticated a GitLab source")
	}
}

func TestGitLabSHA256AndTokenAuthentication(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main","after":"1111111111111111111111111111111111111111"}`)
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Gitlab-Signature", signGitHub("fixture", body))
	if !deliveryAuthenticatedFor(source.ForgeGitLab, r, "fixture", body) {
		t.Fatal("GitLab SHA256 variant refused")
	}
	if deliveryAuthenticatedFor(source.ForgeGitHub, r, "fixture", body) {
		t.Fatal("GitLab variant crossed forge boundary")
	}
	if deliveryAuthenticatedFor(source.ForgeGitLab, r, "fixture", append(body, ' ')) {
		t.Fatal("signature accepted altered body")
	}
	r.Header.Set("X-Gitlab-Token", "wrong")
	if deliveryAuthenticatedFor(source.ForgeGitLab, r, "fixture", body) {
		t.Fatal("bad explicit token fell back to signature")
	}
}

func TestProductionSourceVerifierFailsClosedWithoutConfiguration(t *testing.T) {
	t.Setenv("TEPLOY_DASH_SOURCE_PROVIDERS", "")
	s, _ := sourceTestServer(t, nil)
	if s.sourceVerifier == nil {
		t.Fatal("production verifier remains nil")
	}
	e := s.sourceVerifier(context.Background(), &source.Source{CredentialRef: "absent"})
	if e == nil || !strings.Contains(e.Error(), "not configured") {
		t.Fatalf("missing configuration passed: %v", e)
	}
}

// Rotation returns the new secret once; deliveries signed with the old
// secret are refused afterwards.
func TestSourceRotateSecret(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	id, oldSecret := createSource(t, s, "github", "https://github.com/team/app", "main")
	rotate := httptest.NewRecorder()
	s.handler().ServeHTTP(rotate, httptest.NewRequest(http.MethodPost, "/api/sources/"+id+"/rotate-secret", nil))
	if rotate.Code != http.StatusOK {
		t.Fatalf("rotate status=%d body=%s", rotate.Code, rotate.Body.String())
	}
	var rotated struct {
		Data struct {
			WebhookSecret string `json:"webhook_secret"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rotate.Body.Bytes(), &rotated); err != nil || rotated.Data.WebhookSecret == "" || rotated.Data.WebhookSecret == oldSecret {
		t.Fatalf("rotate body invalid: %v", err)
	}
	if strings.Contains(sourceDetail(t, s, id), rotated.Data.WebhookSecret) {
		t.Fatal("rotated secret appeared in a later read")
	}
	if stale := pushDelivery(t, s, id, oldSecret, "d-old", strings.Repeat("a", 40)); stale.Code != http.StatusUnauthorized {
		t.Fatalf("old secret status=%d", stale.Code)
	}
	if fresh := pushDelivery(t, s, id, rotated.Data.WebhookSecret, "d-new", strings.Repeat("a", 40)); fresh.Code != http.StatusOK {
		t.Fatalf("new secret status=%d body=%s", fresh.Code, fresh.Body.String())
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
	id, secret := createSource(t, s, "github", "https://github.com/team/app", "main")

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
		t.Fatal("a degraded source must not execute anything")
	}
	detail := httptest.NewRecorder()
	s.handler().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/sources/"+id, nil))
	if !strings.Contains(detail.Body.String(), `"disposition":"degraded"`) {
		t.Fatalf("the refused delivery must be recorded: %s", detail.Body.String())
	}

	// Recovery: a good credential clears the degraded state and deliveries
	// pass verification again.
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
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), noAppConnected) {
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
	id, secret := createSource(t, s, "github", "https://github.com/team/app", "main")
	if response := pushDelivery(t, s, id, secret, "d-1", strings.Repeat("a", 40)); response.Code != http.StatusOK {
		t.Fatalf("delivery status=%d", response.Code)
	}
	// A lifecycle.json left by the removed pipeline does not block deletion.
	legacy := filepath.Join(s.config.DataDir, "sources", id, "lifecycle.json")
	if err := os.WriteFile(legacy, []byte(`{"schema":1,"legacy_audit_required":true,"watermarks":{},"previews":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	deleted := httptest.NewRecorder()
	s.handler().ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/api/sources/"+id, nil))
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	gone := httptest.NewRecorder()
	s.handler().ServeHTTP(gone, httptest.NewRequest(http.MethodGet, "/api/sources/"+id, nil))
	if gone.Code != http.StatusNotFound {
		t.Fatalf("get after delete status=%d", gone.Code)
	}
	if hook := pushDelivery(t, s, id, secret, "d-2", strings.Repeat("a", 40)); hook.Code != http.StatusNotFound {
		t.Fatalf("hook after delete status=%d", hook.Code)
	}
	if previews := httptest.NewRecorder(); true {
		s.handler().ServeHTTP(previews, httptest.NewRequest(http.MethodGet, "/api/sources/"+id+"/previews", nil))
		if previews.Code != http.StatusNotFound {
			t.Fatalf("previews route status=%d, want 404", previews.Code)
		}
	}
}
