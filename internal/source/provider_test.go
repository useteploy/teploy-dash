package source

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This fixture runs as an independent provider process. It verifies the real
// JWT signature and token request, instead of mocking Resolve or HTTP.Do.
func TestSourceProviderProcess(t *testing.T) {
	if os.Getenv("TEPLOY_PROVIDER_FIXTURE") != "1" {
		return
	}
	keyBytes, _ := os.ReadFile(os.Getenv("TEPLOY_PROVIDER_KEY"))
	block, _ := pem.Decode(keyBytes)
	key, e := x509.ParsePKCS1PrivateKey(block.Bytes)
	if e != nil {
		os.Exit(2)
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		os.Exit(2)
	}
	fmt.Println("http://" + ln.Addr().String())
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.HasPrefix(r.URL.Path, "/app/") {
			parts := strings.Split(auth, ".")
			if len(parts) != 3 {
				http.Error(w, "bad JWT", 401)
				return
			}
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig) != nil {
				http.Error(w, "bad signature", 401)
				return
			}
			b, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Issuer string `json:"iss"`
				IAT    int64  `json:"iat"`
				EXP    int64  `json:"exp"`
			}
			json.Unmarshal(b, &claims)
			if claims.Issuer != "42" || claims.IAT > time.Now().Unix() || claims.EXP <= time.Now().Unix() || claims.EXP-claims.IAT > 600 {
				http.Error(w, "bad claims", 401)
				return
			}
			if r.URL.Path == "/app/installations/7" {
				json.NewEncoder(w).Encode(map[string]any{"app_id": 42})
				return
			}
			if r.URL.Path != "/app/installations/7/access_tokens" || r.Method != "POST" {
				http.NotFound(w, r)
				return
			}
			var request struct {
				Repos       []string          `json:"repositories"`
				Permissions map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&request)
			if len(request.Repos) != 1 || request.Repos[0] != "repo" || request.Permissions["contents"] != "read" {
				http.Error(w, "scope", 403)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"token": "fixture-install-token", "expires_at": time.Now().UTC().Add(time.Hour), "repositories": []map[string]any{{"full_name": "owner/repo"}}, "permissions": request.Permissions})
			return
		}
		if auth != "fixture-install-token" && auth != "fixture-PAT" {
			http.Error(w, "fixture-PAT must not escape via remote errors", 403)
			return
		}
		switch r.URL.Path {
		case "/repos/owner/repo":
			json.NewEncoder(w).Encode(map[string]any{"full_name": "owner/repo"})
		case "/repos/owner/repo/branches":
			json.NewEncoder(w).Encode([]any{})
		case "/repos/owner/repo/pulls/9":
			json.NewEncoder(w).Encode(map[string]any{"state": "open", "updated_at": "2026-10-07T12:00:00Z", "head": map[string]any{"sha": strings.Repeat("a", 40), "repo": map[string]any{"full_name": "owner/repo"}}, "base": map[string]any{"repo": map[string]any{"full_name": "owner/repo"}}})
		case "/redirect":
			http.Redirect(w, r, "http://127.0.0.1:1/secret", 302)
		default:
			http.NotFound(w, r)
		}
	})
	http.Serve(ln, mux)
	os.Exit(0)
}
func startProviderProcess(t *testing.T, keyFile string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSourceProviderProcess$")
	cmd.Env = append(os.Environ(), "TEPLOY_PROVIDER_FIXTURE=1", "TEPLOY_PROVIDER_KEY="+keyFile)
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	ch := make(chan string, 1)
	go func() { var b [1024]byte; n, _ := stdout.Read(b[:]); ch <- strings.TrimSpace(string(b[:n])) }()
	select {
	case origin := <-ch:
		if !strings.HasPrefix(origin, "http://127.0.0.1:") {
			t.Fatalf("fixture startup %q", origin)
		}
		return origin
	case <-time.After(5 * time.Second):
		t.Fatal("fixture startup timed out")
	}
	return ""
}
func providerFixture(t *testing.T) (*Providers, *Source, string) {
	t.Helper()
	dir := t.TempDir()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	keyFile := filepath.Join(dir, "app.pem")
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600)
	origin := startProviderProcess(t, keyFile)
	p := &Providers{Path: filepath.Join(dir, "providers.json")}
	src := &Source{Forge: ForgeGitHub, CloneURL: "https://github.com/owner/repo", CredentialRef: "app"}
	v := Provider{Forge: ForgeGitHub, Repository: src.CloneURL, APIURL: origin, AllowPrivate: true, PrivateKeyFile: keyFile, AppID: 42, InstallationID: 7, Preview: &PreviewPolicy{}}
	b, _ := json.Marshal(map[string]Provider{"app": v})
	os.WriteFile(p.Path, b, 0600)
	return p, src, origin
}
func TestProviderAppRemoteContractAndReplayFence(t *testing.T) {
	p, src, _ := providerFixture(t)
	ctx := context.Background()
	if e := p.Verify(ctx, src); e != nil {
		t.Fatal(e)
	}
	a, e := p.Resolve(ctx, src)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	sha := strings.Repeat("a", 40)
	stamp := "2026-10-07T12:00:00Z"
	if e = a.Pull(ctx, src, 9, sha, stamp, false); e != nil {
		t.Fatal(e)
	}
	for _, input := range []struct {
		sha, stamp string
		closed     bool
	}{{strings.Repeat("b", 40), stamp, false}, {sha, "2026-10-06T12:00:00Z", false}, {sha, stamp, true}} {
		if e = a.Pull(ctx, src, 9, input.sha, input.stamp, input.closed); e == nil {
			t.Fatal("stale or closed delivery passed")
		}
	}
	// Rotation is read at use. No cached token can hide installation removal.
	b, _ := os.ReadFile(p.Path)
	os.WriteFile(p.Path, []byte(strings.ReplaceAll(string(b), `"installation_id":7`, `"installation_id":8`)), 0600)
	if _, e = p.Resolve(ctx, src); e == nil {
		t.Fatal("revoked installation passed")
	}
}
func TestProviderScopeHostRedactionAndCancellation(t *testing.T) {
	p, src, origin := providerFixture(t)
	wrong := *src
	wrong.CloneURL = "https://github.com/other/repo"
	if _, e := p.Resolve(context.Background(), &wrong); e == nil {
		t.Fatal("cross-repo credential use passed")
	}
	client, e := providerClient(origin, false)
	if e == nil {
		if _, e = client.Get(origin + "/repos/owner/repo"); e == nil {
			t.Fatal("unadmitted private address passed")
		}
		client.CloseIdleConnections()
	}
	client, e = providerClient(origin, true)
	if e != nil {
		t.Fatal(e)
	}
	defer client.CloseIdleConnections()
	a := &Access{Provider: Provider{APIURL: origin, Forge: ForgeGitHub}, Token: "fixture-secret", Client: client}
	e = a.call(context.Background(), "GET", "/repos/owner/repo", nil, nil)
	if e == nil || strings.Contains(e.Error(), "fixture") {
		t.Fatalf("remote secret response leaked: %v", e)
	}
	a.Token = "fixture-PAT"
	if e = a.call(context.Background(), "GET", "/redirect", nil, nil); e == nil {
		t.Fatal("redirect accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e = a.call(ctx, "GET", "/repos/owner/repo", nil, nil); e != context.Canceled {
		t.Fatalf("cancellation lost: %v", e)
	}
	if e = os.Chmod(p.Path, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Policy(src); e == nil {
		t.Fatal("public provider config accepted")
	}
}

func TestPushHeadNativeForgeContracts(t *testing.T) {
	for _, forge := range []Forge{ForgeGitHub, ForgeGitLab, ForgeGitea, ForgeForgejo} {
		t.Run(string(forge), func(t *testing.T) {
			sha := strings.Repeat("b", 40)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				expected := "/repos/team/app/branches/feature/scope"
				if forge == ForgeGitLab {
					expected = "/projects/team/app/repository/branches/feature/scope"
				}
				if r.URL.Path != expected {
					t.Errorf("native branch authority path %q want %q", r.URL.Path, expected)
				}
				if forge == ForgeGitLab || forge == ForgeGitea || forge == ForgeForgejo {
					fmt.Fprintf(w, `{"commit":{"id":%q}}`, sha)
				} else {
					fmt.Fprintf(w, `{"commit":{"sha":%q}}`, sha)
				}
			}))
			defer server.Close()
			a := &Access{Provider: Provider{Forge: forge, APIURL: server.URL}, Client: server.Client(), Token: "fixture"}
			src := &Source{Forge: forge, CloneURL: "https://forge.test/team/app"}
			if e := a.PushHead(context.Background(), src, "feature/scope", sha); e != nil {
				t.Fatal(e)
			}
			if e := a.PushHead(context.Background(), src, "feature/scope", strings.Repeat("a", 40)); e == nil {
				t.Fatal("old head admitted")
			}
		})
	}
}

// lsRemoteAdvertisement builds a minimal git smart-HTTP v0 advertisement
// carrying exactly one ref, so a completed generic ls-remote observes it.
func lsRemoteAdvertisement(sha, ref string) []byte {
	line := func(s string) []byte {
		b := []byte(s)
		out := []byte(fmt.Sprintf("%04x", len(b)+4))
		return append(out, b...)
	}
	body := line("# service=git-upload-pack\n")
	body = append(body, '0', '0', '0', '0')
	body = append(body, line(sha+" "+ref+"\n")...)
	body = append(body, '0', '0', '0', '0')
	return body
}

// Generic-forge push authority must separate three outcomes (R5-02). A
// COMPLETED ls-remote advertising exactly the watched ref at another valid
// commit is the only stale outcome; failed or canceled transport, a ref the
// advertisement does not carry, garbage responses and malformed output stay
// retryable-unavailable, so a transient failure is never durably recorded
// as a stale-delivery ignore that swallows the retry.
func TestPushHeadGenericAuthorityClassifications(t *testing.T) {
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("git required")
	}
	pinned := strings.Repeat("a", 40)
	moved := strings.Repeat("b", 40)
	src := &Source{Forge: ForgeGeneric, CloneURL: "https://git.example/team/app"}
	advertise := func(sha, ref string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			w.Write(lsRemoteAdvertisement(sha, ref))
		}))
	}
	authority := func(repository string) *Access {
		return &Access{Provider: Provider{Forge: ForgeGeneric, Repository: repository, AllowPrivate: true}, Username: "fixture", Token: "fixture"}
	}

	// A proven mismatch on a valid observation stays genuinely stale.
	movedServer := advertise(moved, "refs/heads/main")
	defer movedServer.Close()
	if e := authority(movedServer.URL+"/repo.git").PushHead(context.Background(), src, "main", pinned); !IsStaleAuthority(e) {
		t.Fatalf("proven mismatch classified %v, want stale", e)
	}

	// The watched ref at the pinned commit admits.
	sameServer := advertise(pinned, "refs/heads/main")
	defer sameServer.Close()
	if e := authority(sameServer.URL+"/repo.git").PushHead(context.Background(), src, "main", pinned); e != nil {
		t.Fatalf("current head refused: %v", e)
	}

	// Unreachable repository: transport failure is unavailable, not stale.
	if e := authority("http://127.0.0.1:1/repo.git").PushHead(context.Background(), src, "main", pinned); e == nil || IsStaleAuthority(e) {
		t.Fatalf("transport failure classified %v, want retryable unavailable", e)
	}

	// An advertisement that does not carry the watched ref makes ls-remote
	// exit nonzero with no output. The flattened error cannot distinguish
	// this from transport failure, so absence is not inferred from it.
	absentServer := advertise(moved, "refs/heads/other")
	defer absentServer.Close()
	if e := authority(absentServer.URL+"/repo.git").PushHead(context.Background(), src, "main", pinned); e == nil || IsStaleAuthority(e) {
		t.Fatalf("no-match exit classified %v, want retryable unavailable", e)
	}

	// A non-git response is a failed observation, never an authoritative one.
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not a git advertisement")
	}))
	defer garbage.Close()
	if e := authority(garbage.URL+"/repo.git").PushHead(context.Background(), src, "main", pinned); e == nil || IsStaleAuthority(e) {
		t.Fatalf("garbage response classified %v, want retryable unavailable", e)
	}

	// An in-flight ls-remote canceled by deadline surfaces the context
	// error, never staleness.
	hangDone := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hangDone
	}))
	defer hang.Close()
	defer close(hangDone)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if e := authority(hang.URL+"/repo.git").PushHead(ctx, src, "main", pinned); !errors.Is(e, context.DeadlineExceeded) || IsStaleAuthority(e) {
		t.Fatalf("canceled ls-remote classified %v, want retryable cancellation", e)
	}

	// Completed but malformed output is an invalid observation — neither a
	// successful authority check nor a proven mismatch.
	for _, tc := range []struct{ name, text string }{
		{"empty output", ""},
		{"single field", moved},
		{"wrong ref name", moved + "\trefs/heads/other"},
		{"extra field", moved + "\trefs/heads/main\textra"},
		{"non-hex oid", "not-a-commit\trefs/heads/main"},
		{"zero oid", strings.Repeat("0", 40) + "\trefs/heads/main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if e := classifyLsRemote(tc.text, "main", pinned); e == nil || IsStaleAuthority(e) {
				t.Fatalf("malformed output %q classified %v, want retryable invalid observation", tc.text, e)
			}
		})
	}
}
