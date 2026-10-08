package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/source"
)

func wrapperGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	b, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("git fixture %v %s", e, b)
	}
	return strings.TrimSpace(string(b))
}
func TestProductionSourceWrapperImmutableCheckoutAndReceipts(t *testing.T) {
	repo := t.TempDir()
	wrapperGit(t, repo, "init", "--quiet")
	os.WriteFile(filepath.Join(repo, "teploy.yml"), []byte("app: web\nport: 80\n"), 0600)
	wrapperGit(t, repo, "add", ".")
	wrapperGit(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "--quiet", "-m", "admitted")
	sha := wrapperGit(t, repo, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(repo, "teploy.yml"), []byte("app: moved\n"), 0600)
	wrapperGit(t, repo, "add", ".")
	wrapperGit(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "--quiet", "-m", "moving tip")
	for _, scenario := range []string{"valid", "bad-image", "bad-version", "oversized", "execute-fail", "closed-after-build", "rotated-key", "policy-file-changed", "wrong-app", "escaping-path", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := sourceTestServer(t, nil)
			id, _ := createBoundSource(t, s, "https://github.com/team/app", "main")
			src, _ := s.sources.Get(id)
			_ = src
			policyFile := filepath.Join(t.TempDir(), "preview.yml")
			os.WriteFile(policyFile, []byte("app: web\nport: 80\n"), 0600)
			p := source.Provider{Forge: source.ForgeGitHub, Repository: "https://github.com/team/app", PrivateKeyFile: "fixture-key", Preview: &source.PreviewPolicy{ManifestFile: policyFile, TTL: "24h", BaseDomain: "preview.test", AllowIPs: []string{"127.0.0.1"}}}
			var closed atomic.Bool
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				state := "open"
				if closed.Load() {
					state = "closed"
				}
				fmt.Fprintf(w, `{"state":%q,"updated_at":"2026-10-07T12:00:00Z","head":{"sha":%q,"repo":{"full_name":"team/app"}},"base":{"repo":{"full_name":"team/app"}}}`, state, sha)
			}))
			defer api.Close()
			p.APIURL = api.URL
			resolves := 0
			s.sourceAccessResolver = func(context.Context, *source.Source) (*source.Access, error) {
				resolves++
				digest := "key-before"
				if resolves > 1 && scenario == "rotated-key" {
					digest = "key-after"
				}
				return &source.Access{Provider: p, AuthorityDigest: digest, Client: api.Client()}, nil
			}
			workspace := ""
			s.sourceCheckout = func(ctx context.Context, a *source.Access, commit string) (string, func(), error) {
				if commit != sha {
					t.Fatal("wrapper lost admitted SHA")
				}
				root, e := os.MkdirTemp("", "dash-wrapper-fixture-")
				if e != nil {
					return "", nil, e
				}
				workspace = root
				cleanup := func() { os.RemoveAll(root) }
				dir := filepath.Join(root, "repo")
				os.Mkdir(dir, 0700)
				env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
				if e = source.CheckoutRepository(ctx, dir, repo, commit, []string{"-c", "protocol.file.allow=always"}, env); e != nil {
					cleanup()
					return "", nil, e
				}
				if wrapperGit(t, dir, "rev-parse", "HEAD") != sha {
					t.Fatal("moving tip checkout")
				}
				return dir, cleanup, nil
			}
			deploys := 0
			calls := 0
			execute := func(ctx context.Context, cmd operation.Command, emit func(operation.Stream, string)) (int, error) {
				calls++
				if cmd.SourceRequest != nil {
					t.Fatal("source wrapper leaked recursion")
				}
				if ctx.Err() != nil {
					return -1, ctx.Err()
				}
				var dir string
				for i, arg := range cmd.Args {
					if arg == "--project-dir" {
						dir = cmd.Args[i+1]
					}
				}
				if dir == "" || !strings.HasPrefix(dir, workspace) {
					t.Fatal("registered manifest bypassed immutable checkout")
				}
				for _, file := range []string{"password", "username", "askpass"} {
					if _, e := os.Stat(filepath.Join(workspace, file)); !os.IsNotExist(e) {
						t.Fatal("checkout credential survived before CLI")
					}
				}
				manifest, _ := os.ReadFile(filepath.Join(dir, "teploy.yml"))
				if !strings.Contains(string(manifest), "app: web") || strings.Contains(string(manifest), "moved") {
					t.Fatal("immutable/policy app binding lost")
				}
				if strings.Contains(strings.Join(cmd.Args, " "), "preview deploy") {
					deploys++
					return 0, nil
				}
				if scenario == "execute-fail" {
					return 9, fmt.Errorf("fixture executor failure")
				}
				version := sha[:12]
				image := "web-build-" + version
				if scenario == "bad-image" {
					image = "other-build-" + version
				}
				if scenario == "bad-version" {
					version = strings.Repeat("e", 12)
					image = "web-build-" + version
				}
				receipt, _ := json.Marshal(map[string]string{"image": image, "version": version})
				if scenario == "oversized" {
					emit(operation.StreamStdout, strings.Repeat("x", 1<<20))
				}
				emit(operation.StreamStdout, string(receipt))
				if scenario == "closed-after-build" {
					closed.Store(true)
				}
				if scenario == "policy-file-changed" {
					os.WriteFile(policyFile, []byte("app: web\nport: 80\ncontext: changed\n"), 0600)
				}
				return 0, nil
			}
			req := &operation.Request{Kind: operation.KindSourcePreview, SourceID: id, SourceCommit: sha, Server: "prod", App: "web", SourcePullRequest: 9, SourcePullUpdatedAt: "2026-10-07T12:00:00Z"}
			if scenario == "wrong-app" || scenario == "escaping-path" {
				req.Kind = operation.KindManifestApply
				req.SourceManifestPath = "teploy.yml"
				if scenario == "wrong-app" {
					req.App = "other"
				} else {
					req.SourceManifestPath = "../../escape/teploy.yml"
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			if scenario == "canceled" {
				cancel()
			}
			defer cancel()
			cmd := operation.Command{SourceRequest: req, Args: []string{"--project-dir", "/registered/bypass", "build", "--host", "prod.example", "--json"}}
			code, e := s.executeSourceOperation(ctx, cmd, func(operation.Stream, string) {}, execute)
			if scenario == "valid" {
				if e != nil || code != 0 || deploys != 1 || calls != 2 {
					t.Fatalf("valid wrapper %d %v calls=%d deploys=%d", code, e, calls, deploys)
				}
			} else if e == nil || deploys != 0 {
				t.Fatalf("unsafe scenario %s passed: code=%d error=%v deploys=%d", scenario, code, e, deploys)
			}
			if workspace != "" {
				if _, e = os.Stat(workspace); !os.IsNotExist(e) {
					t.Fatal("wrapper workspace leaked")
				}
			}
		})
	}
}

func TestSourceQueueUsesProductionWrapperSeam(t *testing.T) {
	repo := t.TempDir()
	wrapperGit(t, repo, "init", "--quiet")
	os.WriteFile(filepath.Join(repo, "teploy.yml"), []byte("app: web\nimage: example/web:1\n"), 0600)
	wrapperGit(t, repo, "add", ".")
	wrapperGit(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "--quiet", "-m", "pinned")
	sha := wrapperGit(t, repo, "rev-parse", "HEAD")
	var consumed, workspace string
	config := Config{DataDir: t.TempDir(), NoAuth: true, OperationResolver: func(name string) (operation.Server, error) {
		return operation.Server{Name: name, ID: "srv-stable", Host: "fixture.example"}, nil
	},
		SourceAccessResolver: func(context.Context, *source.Source) (*source.Access, error) {
			return &source.Access{Provider: source.Provider{Forge: source.ForgeGitHub, Repository: "https://github.com/team/app"}}, nil
		},
		SourceCheckout: func(ctx context.Context, a *source.Access, commit string) (string, func(), error) {
			if commit != sha {
				return "", nil, fmt.Errorf("wrong admitted commit")
			}
			dir, e := os.MkdirTemp("", "dash-wrapper-queue-")
			if e != nil {
				return "", nil, e
			}
			workspace = dir
			cleanup := func() { os.RemoveAll(dir) }
			env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
			if e = source.CheckoutRepository(ctx, dir, repo, commit, []string{"-c", "protocol.file.allow=always"}, env); e != nil {
				cleanup()
				return "", nil, e
			}
			return dir, cleanup, nil
		},
		SourceCLIExecutor: func(ctx context.Context, cmd operation.Command, emit func(operation.Stream, string)) (int, error) {
			if cmd.SourceRequest != nil {
				return -1, fmt.Errorf("wrapper not consumed")
			}
			for i, arg := range cmd.Args {
				if arg == "--project-dir" {
					consumed = cmd.Args[i+1]
				}
			}
			// The wrapper pins --project-dir to the symlink-RESOLVED checkout
			// root (source.ProjectPath fences escaping roots), so the seam
			// must compare against the resolved workspace, not the raw temp
			// path the fixture created.
			resolved, e := filepath.EvalSymlinks(workspace)
			if e != nil || consumed != resolved {
				return -1, fmt.Errorf("registered project bypass")
			}
			return 0, nil
		}}
	s := New(config)
	t.Cleanup(func() { s.DrainOperations(context.Background()) })
	id, _ := createBoundSource(t, s, "https://github.com/team/app", "main")
	doc, _ := s.manifests.Get("prod", "web")
	op, _, e := s.operations.Enqueue(operation.Request{Kind: operation.KindManifestApply, Server: "prod", App: "web", Mode: "git-managed", ManifestRevision: doc.CurrentRevision, SourceID: id, SourceCommit: sha}, "queue-wrapper", nil)
	if e != nil {
		t.Fatal(e)
	}
	result := waitTerminal(t, s, op.ID)
	if result.Status != operation.StatusSucceeded || consumed == "" {
		t.Fatalf("production queue wrapper failed %+v consumed=%q", result, consumed)
	}
	if _, e = os.Stat(workspace); !os.IsNotExist(e) {
		t.Fatal("queue wrapper workspace leaked")
	}
}
