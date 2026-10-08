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

	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/source"
)

func TestSourcePRChronologyBeforeSupersedeAndRestart(t *testing.T) {
	s, _, release := gatedTestServer(t, nil)
	id, secret := createBoundSource(t, s, "https://github.com/team/app", "main")
	ref := "fixture"
	s.sources.Update(id, source.UpdateInput{CredentialRef: &ref})
	policyFile := filepath.Join(t.TempDir(), "preview.yml")
	os.WriteFile(policyFile, []byte("app: web\nport: 80\n"), 0600)
	policy := source.Provider{Forge: source.ForgeGitHub, Repository: "https://github.com/team/app", PrivateKeyFile: "fixture.pem", AppID: 42, InstallationID: 7, Preview: &source.PreviewPolicy{ManifestFile: policyFile, TTL: "24h", BaseDomain: "preview.test", AllowIPs: []string{"127.0.0.1"}}}
	providerPath := filepath.Join(t.TempDir(), "providers.json")
	b, _ := json.Marshal(map[string]source.Provider{ref: policy})
	os.WriteFile(providerPath, b, 0600)
	s.sourceProviders.Path = providerPath
	var mu sync.Mutex
	state, sha, stamp := "open", strings.Repeat("b", 40), "2026-10-07T12:02:00Z"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, `{"state":%q,"updated_at":%q,"head":{"sha":%q,"repo":{"full_name":"team/app"}},"base":{"repo":{"full_name":"team/app"}}}`, state, stamp, sha)
	}))
	defer api.Close()
	s.sourceAccessResolver = func(context.Context, *source.Source) (*source.Access, error) {
		p := policy
		p.APIURL = api.URL
		return &source.Access{Provider: p, Client: api.Client()}, nil
	}
	s.config.OperationResolverByID = func(id string) (operation.Server, bool) {
		return operation.Server{ID: id, Name: "prod", Host: "prod.example"}, true
	}
	blocker, _, e := s.operations.Enqueue(operation.Request{Kind: operation.KindDeploy, Server: "prod", App: "web", Image: "example/web:1"}, "blocker", nil)
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		op, _ := s.operations.Get(blocker.ID)
		if op.Status == operation.StatusRunning {
			break
		}
		time.Sleep(time.Millisecond)
	}
	send := func(action, delivery, commit, updated string) *httptest.ResponseRecorder {
		body := []byte(fmt.Sprintf(`{"action":%q,"number":9,"installation":{"id":7},"repository":{"full_name":"team/app"},"pull_request":{"updated_at":%q,"head":{"sha":%q,"repo":{"full_name":"team/app"}},"base":{"repo":{"full_name":"team/app"}}}}`, action, updated, commit))
		req := httptest.NewRequest("POST", "/hooks/sources/"+id, strings.NewReader(string(body)))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-Hub-Signature-256", signGitHub(secret, body))
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, req)
		return w
	}
	current := send("synchronize", "B", sha, stamp)
	if current.Code != 200 || !strings.Contains(current.Body.String(), "admitted") {
		t.Fatalf("B: %s", current.Body.String())
	}
	var envelope struct {
		IDs []string `json:"operation_ids"`
	}
	json.Unmarshal(current.Body.Bytes(), &envelope)
	assertQueued := func(id string) {
		t.Helper()
		op, e := s.operations.Get(id)
		if e != nil || op.Status != operation.StatusQueued {
			t.Fatalf("newest lost: %+v %v", op, e)
		}
	}
	assertQueued(envelope.IDs[0])
	old := send("synchronize", "A", strings.Repeat("a", 40), "2026-10-07T12:01:00Z")
	if !strings.Contains(old.Body.String(), "ignored") {
		t.Fatal(old.Body.String())
	}
	assertQueued(envelope.IDs[0])
	// Equal timestamp with another SHA is resolved by provider authority.
	equal := send("synchronize", "equal-old", strings.Repeat("a", 40), stamp)
	if !strings.Contains(equal.Body.String(), "ignored") {
		t.Fatal(equal.Body.String())
	}
	assertQueued(envelope.IDs[0])
	mu.Lock()
	state = "closed"
	stamp = "2026-10-07T12:03:00Z"
	mu.Unlock()
	closed := send("closed", "close", sha, stamp)
	if !strings.Contains(closed.Body.String(), "admitted") {
		t.Fatal(closed.Body.String())
	}
	json.Unmarshal(closed.Body.Bytes(), &envelope)
	closeID := envelope.IDs[0]
	send("opened", "old-open", sha, "2026-10-07T12:02:00Z")
	assertQueued(closeID)
	reopenedStore, e := source.New(s.config.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	s.sources = reopenedStore
	send("opened", "restart-old-open", sha, "2026-10-07T12:02:00Z")
	assertQueued(closeID)
	mu.Lock()
	state = "open"
	stamp = "2026-10-07T12:04:00Z"
	sha = strings.Repeat("c", 40)
	mu.Unlock()
	reopened := send("reopened", "reopen", sha, stamp)
	if !strings.Contains(reopened.Body.String(), "admitted") {
		t.Fatal(reopened.Body.String())
	}
	json.Unmarshal(reopened.Body.Bytes(), &envelope)
	assertQueued(envelope.IDs[0])
	// Refused persistence cannot cancel the queued newest operation.
	lifecyclePath := filepath.Join(s.config.DataDir, "sources", id, "lifecycle.json")
	saved, _ := os.ReadFile(lifecyclePath)
	os.Remove(lifecyclePath)
	os.Mkdir(lifecyclePath, 0700)
	mu.Lock()
	stamp = "2026-10-07T12:05:00Z"
	sha = strings.Repeat("d", 40)
	mu.Unlock()
	refused := send("synchronize", "journal-fail", sha, stamp)
	if refused.Code != 503 {
		t.Fatalf("persist refusal %d %s", refused.Code, refused.Body.String())
	}
	assertQueued(envelope.IDs[0])
	os.Remove(lifecyclePath)
	os.WriteFile(lifecyclePath, saved, 0600)
	close(release)
	if op := waitTerminal(t, s, envelope.IDs[0]); op.Status != operation.StatusSucceeded {
		t.Fatalf("newest did not execute: %+v", op)
	}
}

func TestSourceOwnershipOutlivesBindingAndRetention(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	id, _ := createBoundSource(t, s, "https://github.com/team/app", "main")
	src, _ := s.sources.Get(id)
	state, _ := s.sources.Lifecycle(id)
	key := previewOwnershipKey(id, "srv-stable", "web", 9)
	state.Previews[key] = source.PreviewOwnership{ID: key, ServerID: "srv-stable", Server: "prod", App: "web", Pull: 9, Branch: "dash-" + id + "-pr-9", Manifest: "app: web\nport: 80\n", Policy: source.PreviewPolicy{TTL: "24h", BaseDomain: "preview.test", AllowIPs: []string{"127.0.0.1"}}}
	if e := s.sources.SaveLifecycle(id, state); e != nil {
		t.Fatal(e)
	}
	s.config.OperationResolverByID = func(id string) (operation.Server, bool) {
		return operation.Server{ID: id, Name: "renamed", Host: "same.example"}, id == "srv-stable"
	}
	doc, _ := s.manifests.Get("prod", "web")
	if e := s.manifests.Delete("prod", "web", &doc.CurrentRevision); e != nil {
		t.Fatal(e)
	}
	targets, e := s.previewTargets(src)
	if e != nil || len(targets) != 1 || targets[0].Server != "renamed" {
		t.Fatalf("former target lost: %v %v", targets, e)
	}
	if _, e = s.cleanupOwner(src, "prod", "web", 9); e == nil {
		t.Fatal("reused name authorized cleanup")
	}
	if _, e = s.cleanupOwner(src, "renamed", "web", 9); e != nil {
		t.Fatal(e)
	}
	s.sources, e = source.New(s.config.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.sourceDeletionReady(context.Background(), id); e == nil {
		t.Fatal("source deletion lost retained ownership")
	}
	dir := t.TempDir()
	if e = s.previewTargetManifest(dir, src, "renamed", "web"); e != nil {
		t.Fatal(e)
	}
	s.config.OperationResolverByID = func(string) (operation.Server, bool) { return operation.Server{}, false }
	if _, e = s.previewTargets(src); e == nil {
		t.Fatal("unknown target treated absent")
	}
}

func TestSourcePrincipalBackpressureHTTP429(t *testing.T) {
	s, _, release := gatedTestServer(t, nil, 1)
	defer close(release)
	id, secret := createBoundSource(t, s, "https://github.com/team/app", "main")
	send := func(delivery, sha string) *httptest.ResponseRecorder {
		body := []byte(fmt.Sprintf(`{"ref":"refs/heads/main","after":%q}`, sha))
		r := httptest.NewRequest("POST", "/hooks/sources/"+id, strings.NewReader(string(body)))
		r.Header.Set("X-GitHub-Event", "push")
		r.Header.Set("X-GitHub-Delivery", delivery)
		r.Header.Set("X-Hub-Signature-256", signGitHub(secret, body))
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w
	}
	first := send("first", strings.Repeat("a", 40))
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	second := send("second", strings.Repeat("b", 40))
	if second.Code != 429 {
		t.Fatalf("principal budget %d %s", second.Code, second.Body.String())
	}
	for _, op := range s.operations.List("", "", 0) {
		if op.Request.SourceCommit == strings.Repeat("a", 40) && op.Status == operation.StatusCanceled {
			t.Fatal("refused replacement canceled admitted work")
		}
	}
}

func TestProductionSourceCleanupRequiresFencedOwnership(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	id, _ := createBoundSource(t, s, "https://github.com/team/app", "main")
	state, _ := s.sources.Lifecycle(id)
	key := previewOwnershipKey(id, "srv-stable", "web", 9)
	state.Previews[key] = source.PreviewOwnership{ID: key, ServerID: "srv-stable", Server: "old", App: "web", Pull: 9}
	if e := s.sources.SaveLifecycle(id, state); e != nil {
		t.Fatal(e)
	}
	s.config.OperationResolverByID = func(id string) (operation.Server, bool) {
		return operation.Server{ID: id, Name: "renamed", Host: "actual.example"}, true
	}
	req := &operation.Request{Kind: operation.KindSourcePreviewDestroy, SourceID: id, App: "web", Server: "renamed", SourcePullRequest: 9}
	called := false
	_, err := s.executeSourceOperation(context.Background(), operation.Command{SourceRequest: req}, func(operation.Stream, string) {}, func(context.Context, operation.Command, func(operation.Stream, string)) (int, error) {
		called = true
		return 0, nil
	})
	if err == nil || !strings.Contains(err.Error(), "preview-compare-destroy-v1") || called {
		t.Fatalf("unfenced wrapper executed: %v called=%v", err, called)
	}
	retained, _ := s.sources.Lifecycle(id)
	if len(retained.Previews) != 1 {
		t.Fatal("refusal discarded ownership")
	}
}

// Native header/authentication and real provider HTTP authority are exercised
// together; the queue executor is a capture fixture, not a live CLI result.
func TestSourceNativePushAuthorityAndReplay(t *testing.T) {
	for _, forge := range []source.Forge{source.ForgeGitHub, source.ForgeGitLab, source.ForgeGitea, source.ForgeForgejo} {
		t.Run(string(forge), func(t *testing.T) {
			s, _ := sourceTestServer(t, nil)
			s.sourcePushAuthority = nil
			created, err := s.sources.Create(source.CreateInput{Forge: forge, CloneURL: "https://forge.test/team/app", DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			id, secret := created.ID, created.WebhookSecret
			doc := `{"mode":"git-managed","git":{"repository":"https://forge.test/team/app","revision":"0123456789abcdef0123456789abcdef01234567"},"manifest":"app: web\nimage: example/web:1\n"}`
			w := httptest.NewRecorder()
			s.handler().ServeHTTP(w, httptest.NewRequest("PUT", "/api/manifests/prod/web", strings.NewReader(doc)))
			if w.Code != 201 {
				t.Fatal(w.Body.String())
			}
			sha := strings.Repeat("b", 40)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"commit":{"sha":%q,"id":%q}}`, sha, sha)
			}))
			defer api.Close()
			s.sourceAccessResolver = func(context.Context, *source.Source) (*source.Access, error) {
				return &source.Access{Provider: source.Provider{Forge: forge, Repository: "https://forge.test/team/app", APIURL: api.URL}, Client: api.Client()}, nil
			}
			send := func(delivery, commit string) *httptest.ResponseRecorder {
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
				r.Header.Set(eventHeader, "push")
				r.Header.Set(idHeader, delivery)
				r.Header.Set(sigHeader, signature)
				result := httptest.NewRecorder()
				s.handler().ServeHTTP(result, r)
				return result
			}
			current := send("native-B", sha)
			if current.Code != 200 || !strings.Contains(current.Body.String(), "admitted") {
				t.Fatalf("native admission %d %s", current.Code, current.Body.String())
			}
			var admitted struct {
				IDs []string `json:"operation_ids"`
			}
			json.Unmarshal(current.Body.Bytes(), &admitted)
			if op := waitTerminal(t, s, admitted.IDs[0]); op.Status != operation.StatusSucceeded {
				t.Fatal(op)
			}
			// Restart the durable registry and use new delivery identities: identity
			// dedupe is separate from the bounded delivery-ID ledger.
			s.sources, err = source.New(s.config.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			replay := send("fresh-delivery-same-identity", sha)
			if !strings.Contains(replay.Body.String(), "already admitted") {
				t.Fatal(replay.Body.String())
			}
			old := send("native-A", strings.Repeat("a", 40))
			if !strings.Contains(old.Body.String(), "no longer authoritative") {
				t.Fatal(old.Body.String())
			}
			if got := len(s.operations.List("", "", 0)); got != 1 {
				t.Fatalf("replay/stale queued %d operations", got)
			}
		})
	}
}

func TestPreviewOwnershipSeparatesSourcesAtSameTarget(t *testing.T) {
	if previewOwnershipKey("source-a", "stable-server", "web", 9) == previewOwnershipKey("source-b", "stable-server", "web", 9) {
		t.Fatal("source ownership identities collide")
	}
}
