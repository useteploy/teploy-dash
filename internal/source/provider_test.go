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
	v := Provider{Forge: ForgeGitHub, Repository: src.CloneURL, APIURL: origin, AllowPrivate: true, PrivateKeyFile: keyFile, AppID: 42, InstallationID: 7}
	b, _ := json.Marshal(map[string]Provider{"app": v})
	// A pre-removal file carrying the old preview policy still parses.
	b = []byte(strings.Replace(string(b), `"forge":`, `"preview":{"manifest_file":"/etc/teploy/preview.yml","base_domain":"preview.example","allow_ips":["192.0.2.1"],"ttl":"24h"},"forge":`, 1))
	os.WriteFile(p.Path, b, 0600)
	return p, src, origin
}
func TestProviderAppRemoteContractAndRotation(t *testing.T) {
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
	if a.Token != "fixture-install-token" {
		t.Fatal("App installation token not minted")
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

func TestProviderPolicyIgnoresLegacyPreviewKeyButNotOtherUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	src := &Source{Forge: ForgeGitHub, CloneURL: "https://github.com/owner/repo", CredentialRef: "pat"}
	legacy := `{"pat":{"forge":"github","repository":"https://github.com/owner/repo","token_env":"FIXTURE_TOKEN","preview":{"manifest_file":"/x","base_domain":"p.example","allow_ips":["192.0.2.1"],"ttl":"1h"}}}`
	if e := os.WriteFile(path, []byte(legacy), 0600); e != nil {
		t.Fatal(e)
	}
	p := &Providers{Path: path}
	v, e := p.Policy(src)
	if e != nil {
		t.Fatalf("legacy preview key refused: %v", e)
	}
	if v.TokenEnv != "FIXTURE_TOKEN" || v.APIURL != "https://api.github.com" {
		t.Fatalf("policy = %+v", v)
	}
	unknown := `{"pat":{"forge":"github","repository":"https://github.com/owner/repo","token_env":"FIXTURE_TOKEN","surprise":true}}`
	if e = os.WriteFile(path, []byte(unknown), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Policy(src); e == nil {
		t.Fatal("unknown provider field accepted")
	}
}

// forgeTestServer is a minimal forge API double for the registration
// surface: it records POST bodies and answers listings with whatever was
// registered, so dedupe-by-content can be exercised.
func forgeTestServer(t *testing.T, forge Forge) (*httptest.Server, *[]map[string]interface{}) {
	t.Helper()
	registered := &[]map[string]interface{}{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "keys") && r.Method == "GET":
			rows := []map[string]interface{}{}
			for _, body := range *registered {
				if _, ok := body["key"]; ok {
					rows = append(rows, body)
				}
			}
			json.NewEncoder(w).Encode(rows)
		case strings.HasSuffix(r.URL.Path, "keys") && r.Method == "POST":
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			*registered = append(*registered, body)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": len(*registered)})
		case strings.HasSuffix(r.URL.Path, "/hooks") && r.Method == "GET":
			rows := []map[string]interface{}{}
			for _, body := range *registered {
				if _, ok := body["config"]; ok {
					rows = append(rows, map[string]interface{}{"config": body["config"]})
				} else if _, ok := body["url"]; ok {
					rows = append(rows, map[string]interface{}{"url": body["url"]})
				}
			}
			json.NewEncoder(w).Encode(rows)
		case strings.HasSuffix(r.URL.Path, "/hooks") && r.Method == "POST":
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			*registered = append(*registered, body)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": len(*registered)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, registered
}

func registrationAccess(t *testing.T, forge Forge, origin string) *Access {
	t.Helper()
	client, err := providerClient(origin, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return &Access{Provider: Provider{Forge: forge, APIURL: origin}, Token: "test-token", Client: client}
}

func TestRegisterDeployKeyAndWebhookForEachge(t *testing.T) {
	const publicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeF"
	for _, tc := range []struct {
		forge Forge
		url   string
	}{
		{ForgeGitHub, "https://github.com/owner/repo"},
		{ForgeForgejo, "https://forge.example/owner/repo"},
		{ForgeGitLab, "https://gitlab.example/group/sub/repo"},
	} {
		t.Run(string(tc.forge), func(t *testing.T) {
			server, registered := forgeTestServer(t, tc.forge)
			access := registrationAccess(t, tc.forge, server.URL)
			src := &Source{Forge: tc.forge, CloneURL: tc.url}
			ctx := context.Background()
			if err := access.RegisterDeployKey(ctx, src, "teploy w6probe", publicKey); err != nil {
				t.Fatal(err)
			}
			// The same key material registers exactly once.
			if err := access.RegisterDeployKey(ctx, src, "teploy w6probe", publicKey); err != nil {
				t.Fatal(err)
			}
			if err := access.RegisterWebhook(ctx, src, "https://dash.example/hooks/sources/src-0123456789abcdef", "whsec", "main"); err != nil {
				t.Fatal(err)
			}
			if err := access.RegisterWebhook(ctx, src, "https://dash.example/hooks/sources/src-0123456789abcdef", "whsec", "main"); err != nil {
				t.Fatal(err)
			}
			keys, hooks := 0, 0
			for _, body := range *registered {
				if _, ok := body["key"]; ok {
					keys++
					if body["key"] != publicKey {
						t.Fatalf("key body = %v", body)
					}
					if tc.forge == ForgeGitLab {
						if body["can_push"] != false {
							t.Fatalf("gitlab key not read-only: %v", body)
						}
					} else if body["read_only"] != true {
						t.Fatalf("key not read-only: %v", body)
					}
					continue
				}
				hooks++
				switch tc.forge {
				case ForgeGitLab:
					if body["url"] != "https://dash.example/hooks/sources/src-0123456789abcdef" || body["token"] != "whsec" || body["push_events_branch_filter"] != "main" {
						t.Fatalf("gitlab hook body = %v", body)
					}
				default:
					config, ok := body["config"].(map[string]interface{})
					if !ok || config["url"] != "https://dash.example/hooks/sources/src-0123456789abcdef" || config["secret"] != "whsec" {
						t.Fatalf("hook body = %v", body)
					}
					if tc.forge == ForgeForgejo && (body["branch_filter"] != "main" || body["type"] != "forgejo") {
						t.Fatalf("forgejo hook body = %v", body)
					}
				}
			}
			if keys != 1 || hooks != 1 {
				t.Fatalf("registered keys=%d hooks=%d, want 1 and 1 (dedupe failed)", keys, hooks)
			}
		})
	}
}

func TestRegistrationRefusesGenericForgeAndBadURLs(t *testing.T) {
	server, _ := forgeTestServer(t, ForgeGeneric)
	access := registrationAccess(t, ForgeGeneric, server.URL)
	src := &Source{Forge: ForgeGeneric, CloneURL: "https://forge.example/owner/repo"}
	if err := access.RegisterDeployKey(context.Background(), src, "t", "k"); err == nil {
		t.Fatal("generic forge accepted a deploy key registration")
	}
	if err := access.RegisterWebhook(context.Background(), src, "https://dash.example/hooks/x", "s", ""); err == nil {
		t.Fatal("generic forge accepted a webhook registration")
	}
	forgejo, _ := forgeTestServer(t, ForgeForgejo)
	real := registrationAccess(t, ForgeForgejo, forgejo.URL)
	if err := real.RegisterWebhook(context.Background(), &Source{Forge: ForgeForgejo, CloneURL: "https://forge.example/owner/repo"}, "javascript:alert(1)", "s", ""); err == nil {
		t.Fatal("non-http webhook url accepted")
	}
	if err := real.RegisterWebhook(context.Background(), &Source{Forge: ForgeForgejo, CloneURL: "https://forge.example/owner"}, "https://dash.example/h", "s", ""); err == nil {
		t.Fatal("repository without an owner/name path accepted")
	}
}

func TestCredentialRefForFindsMatchingProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	os.WriteFile(path, []byte(`{
		"alpha": {"forge":"github","repository":"git@github.com:owner/repo.git","token_env":"TOK"},
		"beta": {"forge":"forgejo","repository":"https://forge.example/other/repo","token_env":"TOK2"}
	}`), 0600)
	providers := &Providers{Path: path}
	if ref, provider, ok := providers.CredentialRefFor("https://github.com/OWNER/Repo.git"); !ok || ref != "alpha" || provider.Forge != ForgeGitHub {
		t.Fatalf("lookup = %q %+v %v", ref, provider, ok)
	}
	if ref, _, ok := providers.CredentialRefFor("https://forge.example/other/repo/"); !ok || ref != "beta" {
		t.Fatalf("lookup = %q %v", ref, ok)
	}
	if ref, _, ok := providers.CredentialRefFor("ssh://git@forge.example:2222/other/repo.git"); !ok || ref != "beta" {
		t.Fatalf("ssh spelling on another port missed the HTTPS provider: %q %v", ref, ok)
	}
	if _, _, ok := providers.CredentialRefFor("https://github.com/nothing/repo"); ok {
		t.Fatal("unrelated repository matched a provider")
	}
	if _, _, ok := (&Providers{}).CredentialRefFor("https://github.com/owner/repo"); ok {
		t.Fatal("unconfigured providers matched")
	}
	os.WriteFile(path, []byte("{damage"), 0600)
	if _, _, ok := providers.CredentialRefFor("https://github.com/owner/repo"); ok {
		t.Fatal("damaged provider file matched")
	}
}

func TestProviderPolicyAcceptsSSHSourceOnAnotherPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	os.WriteFile(path, []byte(`{"forge":{"forge":"forgejo","repository":"http://forge.lan:49152/tyler/app","allow_private":true,"token_env":"TOK"}}`), 0600)
	providers := &Providers{Path: path}
	src := &Source{Forge: ForgeForgejo, CloneURL: "https://forge.lan:2222/tyler/app", CredentialRef: "forge"}
	policy, err := providers.Policy(src)
	if err != nil {
		t.Fatal(err)
	}
	// The API is reached on the HTTP port, never the SSH one.
	if policy.APIURL != "http://forge.lan:49152/api/v1" {
		t.Fatalf("api url = %q", policy.APIURL)
	}
	src.CloneURL = "https://forge.lan:2222/tyler/other"
	if _, err := providers.Policy(src); err == nil {
		t.Fatal("credential applied to another repository")
	}
}
