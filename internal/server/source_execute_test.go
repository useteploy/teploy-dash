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
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/source"
)

func TestPreviewPolicyRejectsProductionAuthority(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "preview.yml")
	policy := &source.PreviewPolicy{ManifestFile: path, TTL: "24h", BaseDomain: "preview.example", AllowIPs: []string{"100.64.0.0/10"}}
	for _, extra := range []string{"env:\n  PASSWORD: secret:production\n", "volumes:\n  /data: production\n", "accessories:\n  db:\n    image: postgres\n", "hooks:\n  pre_deploy: steal\n", "context: ../../etc\n", "image: production\n", "unknown: true\n"} {
		os.WriteFile(path, []byte("app: fixture\nport: 80\n"+extra), 0600)
		if e := writePreviewManifest(t.TempDir(), "fixture", policy); e == nil {
			t.Fatalf("unsafe preview policy admitted: %s", extra)
		}
	}
	os.WriteFile(path, []byte("app: fixture\nport: 80\ndockerfile: Dockerfile\n"), 0600)
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	os.WriteFile(victim, []byte("unchanged"), 0600)
	os.Symlink(victim, filepath.Join(dir, "teploy.yml"))
	if e := writePreviewManifest(dir, "fixture", policy); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(victim)
	if string(b) != "unchanged" {
		t.Fatal("trusted manifest replacement followed repository symlink")
	}
	if e := writePreviewManifest(t.TempDir(), "other-app", policy); e == nil {
		t.Fatal("cross-app preview policy admitted")
	}
}
func TestPreviewExpiryRechecksReceiptAndScopesBranch(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "preview.yml")
	os.WriteFile(manifestPath, []byte("app: fixture\nport: 80\n"), 0600)
	configPath := filepath.Join(dir, "providers.json")
	p := source.Provider{Forge: source.ForgeGitHub, Repository: "https://github.com/owner/repo", TokenEnv: "FIXTURE_UNUSED", Preview: &source.PreviewPolicy{ManifestFile: manifestPath, TTL: "24h", BaseDomain: "preview.example", AllowIPs: []string{"100.64.0.0/10"}}}
	b, _ := json.Marshal(map[string]source.Provider{"fixture": p})
	os.WriteFile(configPath, b, 0600)
	s := &Server{sourceProviders: &source.Providers{Path: configPath}}
	src := &source.Source{ID: "src-0123456789abcdef", Forge: source.ForgeGitHub, CloneURL: p.Repository, CredentialRef: "fixture"}
	req := &operation.Request{Kind: operation.KindSourcePreviewExpire, App: "fixture", SourceID: src.ID, SourcePullRequest: 9}
	branch := "dash-" + src.ID + "-pr-9"
	for _, expired := range []bool{false, true} {
		destroyed := false
		executor := func(_ context.Context, cmd operation.Command, emit func(operation.Stream, string)) (int, error) {
			args := strings.Join(cmd.Args, " ")
			if strings.Contains(args, "preview list") {
				deadline := time.Now().Add(time.Hour)
				if expired {
					deadline = time.Now().Add(-time.Hour)
				}
				b, _ := json.Marshal([]sourcePreviewRow{{Branch: branch, ExpiresAt: deadline}, {Branch: "unrelated", ExpiresAt: time.Now().Add(-time.Hour)}})
				emit(operation.StreamStdout, string(b))
				return 0, nil
			}
			if !strings.Contains(args, "preview destroy "+branch) || !strings.Contains(args, "--host fixture.example") {
				t.Fatalf("cleanup changed scope: %s", args)
			}
			destroyed = true
			return 0, nil
		}
		cmd := operation.Command{SourceRequest: req, Args: []string{"--host", "fixture.example", "--user", "fixture"}}
		_, e := s.executePreviewExpiry(context.Background(), src, cmd, func(operation.Stream, string) {}, executor)
		if (e != nil) != expired || destroyed {
			t.Fatalf("unfenced cleanup expired=%v destroyed=%v error=%v", expired, destroyed, e)
		}
	}
}
func TestSourceMaintenanceStopSealsLateStart(t *testing.T) {
	s := &Server{}
	s.stopSourceMaintenance(context.Background())
	s.startSourceMaintenance()
	if s.sourceMaintenanceDone != nil {
		t.Fatal("maintenance started after stop")
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

func TestSourcePullWebhookPinsInstallationHeadAndLifecycle(t *testing.T) {
	s, _ := sourceTestServer(t, nil)
	id, secret := createBoundSource(t, s, "https://github.com/team/app", "main")
	ref := "app-installation"
	if _, e := s.sources.Update(id, source.UpdateInput{CredentialRef: &ref}); e != nil {
		t.Fatal(e)
	}
	configPath := filepath.Join(t.TempDir(), "providers.json")
	policy := source.Provider{Forge: source.ForgeGitHub, Repository: "https://github.com/team/app", PrivateKeyFile: "/fixture/not-used-by-capture.pem", AppID: 42, InstallationID: 7, Preview: &source.PreviewPolicy{ManifestFile: filepath.Join(t.TempDir(), "preview.yml"), TTL: "24h", BaseDomain: "preview.example", AllowIPs: []string{"100.64.0.0/10"}}}
	os.WriteFile(policy.Preview.ManifestFile, []byte("app: web\nport: 80\n"), 0600)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"state":"open","updated_at":"2026-10-07T12:00:00Z","head":{"sha":"%s","repo":{"full_name":"team/app"}},"base":{"repo":{"full_name":"team/app"}}}`, strings.Repeat("a", 40))
	}))
	defer api.Close()
	s.sourceAccessResolver = func(context.Context, *source.Source) (*source.Access, error) {
		p := policy
		p.APIURL = api.URL
		return &source.Access{Provider: p, Client: api.Client()}, nil
	}
	b, _ := json.Marshal(map[string]source.Provider{ref: policy})
	os.WriteFile(configPath, b, 0600)
	s.sourceProviders.Path = configPath
	send := func(installation int, delivery string) *httptest.ResponseRecorder {
		body := []byte(fmt.Sprintf(`{"action":"synchronize","number":9,"installation":{"id":%d},"repository":{"full_name":"team/app"},"pull_request":{"updated_at":"2026-10-07T12:00:00Z","head":{"sha":"%s","repo":{"full_name":"team/app"}},"base":{"repo":{"full_name":"team/app"}}}}`, installation, strings.Repeat("a", 40)))
		r := httptest.NewRequest("POST", "/hooks/sources/"+id, strings.NewReader(string(body)))
		r.Header.Set("X-GitHub-Event", "pull_request")
		r.Header.Set("X-GitHub-Delivery", delivery)
		r.Header.Set("X-Hub-Signature-256", signGitHub(secret, body))
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w
	}
	bad := send(8, "wrong-installation")
	if bad.Code != 200 || !strings.Contains(bad.Body.String(), `"status":"ignored"`) {
		t.Fatalf("wrong installation accepted: %s", bad.Body.String())
	}
	good := send(7, "trusted-head")
	var result struct {
		IDs []string `json:"operation_ids"`
	}
	json.Unmarshal(good.Body.Bytes(), &result)
	if good.Code != 200 || len(result.IDs) != 1 {
		t.Fatalf("trusted lifecycle refused: %d %s", good.Code, good.Body.String())
	}
	op := waitTerminal(t, s, result.IDs[0])
	if op.Request.Kind != operation.KindSourcePreview || op.Request.SourcePullRequest != 9 || op.Request.SourceCommit != strings.Repeat("a", 40) || op.Request.SourcePullUpdatedAt != "2026-10-07T12:00:00Z" {
		t.Fatalf("immutable lifecycle provenance missing: %+v", op.Request)
	}
	replay := send(7, "trusted-head")
	if !strings.Contains(replay.Body.String(), `"status":"duplicate"`) {
		t.Fatalf("delivery replay admitted: %s", replay.Body.String())
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
