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
	// The legacy-census fence: PR preview admission is refused with the
	// documented error — the source predates durable preview ownership and
	// no former-target census is recorded (nothing in the current tree can
	// record one). The refusal is a refused, retryable delivery, never a
	// seen one: the same delivery id re-runs admission (C02 rollback rule).
	refused := send("synchronize", "B", sha, stamp)
	if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), "former-target preview census required before new admission") {
		t.Fatalf("B: %d %s", refused.Code, refused.Body.String())
	}
	retry := send("synchronize", "B", sha, stamp)
	if retry.Code != http.StatusBadRequest || !strings.Contains(retry.Body.String(), "former-target preview census required before new admission") {
		t.Fatalf("refused delivery swallowed the retry: %d %s", retry.Code, retry.Body.String())
	}
	// The refused delivery still recorded its admission-intent watermark,
	// so lifecycle chronology keeps ordering deliveries: an older update is
	// ignored as a stale watermark...
	old := send("synchronize", "A", strings.Repeat("a", 40), "2026-10-07T12:01:00Z")
	if !strings.Contains(old.Body.String(), "ignored") {
		t.Fatal(old.Body.String())
	}
	// ...and an equal timestamp with another SHA is resolved by provider
	// authority (the live head wins; the delivery is ignored).
	equal := send("synchronize", "equal-old", strings.Repeat("a", 40), stamp)
	if !strings.Contains(equal.Body.String(), "ignored") {
		t.Fatal(equal.Body.String())
	}
	mu.Lock()
	state = "closed"
	stamp = "2026-10-07T12:03:00Z"
	mu.Unlock()
	// A closed delivery must resolve its targets from the durable preview
	// ownership record; the census fence refuses that enumeration
	// retryably instead of guessing from manifest bindings.
	closed := send("closed", "close", sha, stamp)
	if closed.Code != http.StatusServiceUnavailable || !strings.Contains(closed.Body.String(), "source predates durable preview ownership; former-target audit required") {
		t.Fatalf("close: %d %s", closed.Code, closed.Body.String())
	}
	// The watermark and the fence both survive a store restart: the older
	// re-open stays ignored and admission stays refused.
	reopenedStore, e := source.New(s.config.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	s.sources = reopenedStore
	if stale := send("opened", "restart-old-open", sha, "2026-10-07T12:02:00Z"); !strings.Contains(stale.Body.String(), "ignored") {
		t.Fatal(stale.Body.String())
	}
	mu.Lock()
	state = "open"
	stamp = "2026-10-07T12:04:00Z"
	sha = strings.Repeat("c", 40)
	mu.Unlock()
	afterRestart := send("reopened", "reopen", sha, stamp)
	if afterRestart.Code != http.StatusBadRequest || !strings.Contains(afterRestart.Body.String(), "former-target preview census required before new admission") {
		t.Fatalf("restart admitted without a census: %d %s", afterRestart.Code, afterRestart.Body.String())
	}
	// Refused lifecycle persistence answers 503 without admitting anything
	// or corrupting the durable state; the fence is intact afterwards.
	lifecyclePath := filepath.Join(s.config.DataDir, "sources", id, "lifecycle.json")
	saved, _ := os.ReadFile(lifecyclePath)
	os.Remove(lifecyclePath)
	os.Mkdir(lifecyclePath, 0700)
	mu.Lock()
	stamp = "2026-10-07T12:05:00Z"
	sha = strings.Repeat("d", 40)
	mu.Unlock()
	persistence := send("synchronize", "journal-fail", sha, "2026-10-07T12:05:00Z")
	if persistence.Code != 503 {
		t.Fatalf("persist refusal %d %s", persistence.Code, persistence.Body.String())
	}
	os.Remove(lifecyclePath)
	os.WriteFile(lifecyclePath, saved, 0600)
	// Nothing was ever admitted for the source; only the blocker exists and
	// the fence never disturbed unrelated queue execution.
	for _, op := range s.operations.List("", "", 0) {
		if op.Request.SourceID != "" {
			t.Fatalf("fence admitted work: %+v", op.Request)
		}
	}
	close(release)
	if op := waitTerminal(t, s, blocker.ID); op.Status != operation.StatusSucceeded {
		t.Fatalf("blocker did not execute: %+v", op)
	}
}

func TestSourceOwnershipOutlivesBindingAndRetention(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	id, _ := createBoundSource(t, s, "https://github.com/team/app", "main")
	src, _ := s.sources.Get(id)
	// Legacy-census fence: until the former-target census is durably
	// recorded, the source predates durable preview ownership and target
	// enumeration refuses rather than guessing from manifest bindings.
	if _, e := s.previewTargets(src); e == nil || !strings.Contains(e.Error(), "source predates durable preview ownership; former-target audit required") {
		t.Fatalf("legacy census fence absent: %v", e)
	}
	state, _ := s.sources.Lifecycle(id)
	// Seed retained preview ownership. The census flag cannot be cleared
	// through any current writer (a legacy_audit_required=false value is
	// dropped by omitempty on save and the loader defaults it back), so the
	// seeded record stays behind the census fence — exactly like production.
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
	// Enumeration stays fenced even with retained ownership on record: the
	// census gate is authoritative, never best-effort, and deleting the
	// manifest binding cannot manufacture a census.
	if _, e := s.previewTargets(src); e == nil || !strings.Contains(e.Error(), "source predates durable preview ownership; former-target audit required") {
		t.Fatalf("retained ownership bypassed the census fence: %v", e)
	}
	var e error
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
