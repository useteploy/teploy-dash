package source

// Provider configuration is operator-owned, never supplied by a webhook or API.
// It is re-read for every use so revocation and key rotation need no restart.
import (
	"bytes"
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
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrStaleAuthority = errors.New("source event no longer authoritative")

func IsStaleAuthority(err error) bool { return errors.Is(err, ErrStaleAuthority) }

type Provider struct {
	Forge          Forge          `json:"forge"`
	Repository     string         `json:"repository"`
	APIURL         string         `json:"api_url,omitempty"`
	TokenFile      string         `json:"token_file,omitempty"`
	TokenEnv       string         `json:"token_env,omitempty"`
	Username       string         `json:"username,omitempty"`
	AppID          int64          `json:"app_id,omitempty"`
	InstallationID int64          `json:"installation_id,omitempty"`
	PrivateKeyFile string         `json:"private_key_file,omitempty"`
	AllowPrivate   bool           `json:"allow_private,omitempty"`
	Preview        *PreviewPolicy `json:"preview,omitempty"`
}
type PreviewPolicy struct {
	ManifestFile string   `json:"manifest_file"`
	BaseDomain   string   `json:"base_domain"`
	AllowIPs     []string `json:"allow_ips"`
	TTL          string   `json:"ttl"`
}
type Providers struct{ Path string }
type Access struct {
	// Opaque in-memory credential authority identity; never journaled.
	AuthorityDigest string
	Provider        Provider
	Token           string
	Username        string
	Client          *http.Client
}

func privateRead(path string) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, fmt.Errorf("credential file unavailable")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 1<<20 {
		return nil, fmt.Errorf("credential file must be a private regular file, at most 1 MiB")
	}
	b, e := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if e != nil {
		return nil, fmt.Errorf("credential file unreadable")
	}
	return b, nil
}
func (p *Providers) Policy(src *Source) (Provider, error) {
	if p == nil || p.Path == "" {
		return Provider{}, fmt.Errorf("source providers are not configured")
	}
	b, e := privateRead(p.Path)
	if e != nil {
		return Provider{}, e
	}
	var entries map[string]Provider
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&entries) != nil || d.Decode(new(any)) != io.EOF {
		return Provider{}, fmt.Errorf("invalid source provider configuration")
	}
	v, ok := entries[src.CredentialRef]
	if !ok {
		return Provider{}, fmt.Errorf("unknown named source provider")
	}
	repo, e := CanonicalURL(v.Repository)
	want, we := CanonicalURL(src.CloneURL)
	if e != nil || we != nil || repo != want || v.Forge != src.Forge {
		return Provider{}, fmt.Errorf("source provider repository or forge scope mismatch")
	}
	u, e := url.Parse(v.Repository)
	if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && v.AllowPrivate)) {
		return Provider{}, fmt.Errorf("source provider requires an admitted HTTPS repository")
	}
	if v.TokenEnv != "" && !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(v.TokenEnv) {
		return Provider{}, fmt.Errorf("credential environment variable name invalid")
	}
	modes := 0
	if v.TokenEnv != "" {
		modes++
	}
	if v.TokenFile != "" {
		modes++
	}
	if v.PrivateKeyFile != "" {
		modes++
	}
	if modes != 1 {
		return Provider{}, fmt.Errorf("source provider requires exactly one credential source")
	}
	if v.PrivateKeyFile != "" && (v.Forge != ForgeGitHub || v.AppID <= 0 || v.InstallationID <= 0) {
		return Provider{}, fmt.Errorf("GitHub App requires positive app and installation IDs")
	}
	if v.PrivateKeyFile == "" && (v.AppID != 0 || v.InstallationID != 0) {
		return Provider{}, fmt.Errorf("App IDs require an App private key")
	}
	if v.APIURL == "" {
		switch v.Forge {
		case ForgeGitHub:
			if u.Host == "github.com" {
				v.APIURL = "https://api.github.com"
			} else {
				v.APIURL = u.Scheme + "://" + u.Host + "/api/v3"
			}
		case ForgeGitLab:
			v.APIURL = u.Scheme + "://" + u.Host + "/api/v4"
		case ForgeGitea, ForgeForgejo:
			v.APIURL = u.Scheme + "://" + u.Host + "/api/v1"
		}
	}
	if v.Forge != ForgeGeneric {
		if _, e := admittedURL(v.APIURL, v.AllowPrivate); e != nil {
			return Provider{}, e
		}
	}
	return v, nil
}
func admittedURL(raw string, private bool) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(private && u.Scheme == "http")) {
		return nil, fmt.Errorf("provider origin is not admitted")
	}
	return u, nil
}

var _, cgnat, _ = net.ParseCIDR("100.64.0.0/10")

func allowedIP(ip net.IP, private bool) bool {
	if private {
		return true
	}
	return ip != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified() && !ip.IsMulticast() && ip.IsGlobalUnicast() && !cgnat.Contains(ip)
}
func providerClient(origin string, private bool) (*http.Client, error) {
	u, e := admittedURL(origin, private)
	if e != nil {
		return nil, e
	}
	tr := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(addr)
		if e != nil || host != u.Hostname() {
			return nil, fmt.Errorf("provider host not admitted")
		}
		ips, e := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if e != nil || len(ips) == 0 {
			return nil, fmt.Errorf("provider DNS unavailable")
		}
		for _, ip := range ips {
			if !allowedIP(ip, private) {
				return nil, fmt.Errorf("provider address not admitted")
			}
		}
		var dial net.Dialer
		dial.Timeout = 5 * time.Second
		return dial.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
	return &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("provider redirects are refused") }}, nil
}
func (a *Access) call(ctx context.Context, method, path string, body any, result any) error {
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return fmt.Errorf("provider request invalid")
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.Provider.APIURL, "/")+path, reader)
	if e != nil {
		return fmt.Errorf("provider request invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	switch a.Provider.Forge {
	case ForgeGitLab:
		req.Header.Set("PRIVATE-TOKEN", a.Token)
	case ForgeGitea, ForgeForgejo:
		req.Header.Set("Authorization", "token "+a.Token)
	default:
		req.Header.Set("Authorization", "Bearer "+a.Token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, e := a.Client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("provider request failed")
	}
	defer resp.Body.Close()
	// Never include remote bodies, headers, URLs or transport errors in persisted verdicts.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider rejected repository access (HTTP %d)", resp.StatusCode)
	}
	if result == nil {
		return nil
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if e != nil || len(b) > 1<<20 {
		return fmt.Errorf("provider response exceeds bounds")
	}
	if json.Unmarshal(b, result) != nil {
		return fmt.Errorf("provider response invalid")
	}
	return nil
}
func (p *Providers) Resolve(ctx context.Context, src *Source) (*Access, error) {
	v, e := p.Policy(src)
	if e != nil {
		return nil, e
	}
	a := &Access{Provider: v, Username: v.Username}
	if a.Username == "" {
		a.Username = "oauth2"
		if v.Forge == ForgeGitHub {
			a.Username = "x-access-token"
		}
	}
	if v.Forge != ForgeGeneric {
		a.Client, e = providerClient(v.APIURL, v.AllowPrivate)
		if e != nil {
			return nil, e
		}
	}
	if v.PrivateKeyFile == "" {
		if v.TokenFile != "" {
			b, e := privateRead(v.TokenFile)
			if e != nil {
				return nil, e
			}
			a.Token = strings.TrimSpace(string(b))
		} else {
			a.Token = os.Getenv(v.TokenEnv)
		}
	} else {
		b, e := privateRead(v.PrivateKeyFile)
		if e != nil {
			return nil, e
		}
		authoritySum := sha256.Sum256(b)
		a.AuthorityDigest = fmt.Sprintf("%x", authoritySum)
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, fmt.Errorf("App signing key invalid")
		}
		key, e := x509.ParsePKCS1PrivateKey(block.Bytes)
		if e != nil {
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("App signing key invalid")
			}
			var ok bool
			key, ok = k.(*rsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("App signing key must be RSA")
			}
		}
		if key.N.BitLen() < 2048 {
			return nil, fmt.Errorf("App signing key too small")
		}
		enc := base64.RawURLEncoding.EncodeToString
		now := time.Now()
		claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(8 * time.Minute).Unix(), "iss": strconv.FormatInt(v.AppID, 10)})
		unsigned := enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(claims)
		sum := sha256.Sum256([]byte(unsigned))
		sig, e := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if e != nil {
			return nil, fmt.Errorf("App signing failed")
		}
		a.Token = unsigned + "." + enc(sig)
		var install struct {
			AppID       int64      `json:"app_id"`
			SuspendedAt *time.Time `json:"suspended_at"`
		}
		if e = a.call(ctx, "GET", "/app/installations/"+strconv.FormatInt(v.InstallationID, 10), nil, &install); e != nil {
			return nil, e
		}
		if install.AppID != v.AppID || install.SuspendedAt != nil {
			return nil, fmt.Errorf("App installation scope invalid or suspended")
		}
		u, _ := url.Parse(src.CloneURL)
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 2 {
			return nil, fmt.Errorf("GitHub repository must be owner/name")
		}
		permissions := map[string]string{"contents": "read"}
		if v.Preview != nil {
			permissions["pull_requests"] = "read"
		}
		var token struct {
			Token        string            `json:"token"`
			Expires      time.Time         `json:"expires_at"`
			Permissions  map[string]string `json:"permissions"`
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		e = a.call(ctx, "POST", "/app/installations/"+strconv.FormatInt(v.InstallationID, 10)+"/access_tokens", map[string]any{"repositories": []string{parts[1]}, "permissions": permissions}, &token)
		if e != nil {
			return nil, e
		}
		if token.Token == "" || token.Expires.Before(now.Add(time.Minute)) || token.Expires.After(now.Add(65*time.Minute)) || len(token.Repositories) != 1 || !strings.EqualFold(token.Repositories[0].FullName, strings.Join(parts, "/")) || token.Permissions["contents"] != "read" {
			return nil, fmt.Errorf("App token repository scope or expiry invalid")
		}
		for k, val := range token.Permissions {
			if (k == "metadata" && val != "read") || (k != "metadata" && permissions[k] != val) {
				return nil, fmt.Errorf("App token permissions exceed requested scope")
			}
		}
		if v.Preview != nil && token.Permissions["pull_requests"] != "read" {
			return nil, fmt.Errorf("App token lacks pull request permission")
		}
		a.Token = token.Token
	}
	if v.PrivateKeyFile == "" {
		sum := sha256.Sum256([]byte(a.Token))
		a.AuthorityDigest = fmt.Sprintf("%x", sum)
	}
	if a.Token == "" || len(a.Token) > 16384 || strings.ContainsAny(a.Token, "\r\n\x00") {
		return nil, fmt.Errorf("source credential missing or invalid")
	}
	return a, nil
}
func (p *Providers) Verify(ctx context.Context, src *Source) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	a, e := p.Resolve(ctx, src)
	if e != nil {
		return e
	}
	defer a.Close()
	if src.Forge == ForgeGeneric {
		return a.verifyGit(ctx)
	}
	u, _ := url.Parse(src.CloneURL)
	repo := strings.Trim(u.Path, "/")
	path := "/repos/" + repo
	if src.Forge == ForgeGitLab {
		path = "/projects/" + url.PathEscape(repo)
	}
	var result struct {
		FullName string `json:"full_name"`
		Path     string `json:"path_with_namespace"`
	}
	if e = a.call(ctx, "GET", path, nil, &result); e != nil {
		return e
	}
	got := result.FullName
	if src.Forge == ForgeGitLab {
		got = result.Path
	}
	if !strings.EqualFold(got, repo) {
		return fmt.Errorf("provider returned a different repository")
	}
	var branches []any
	contentPath := path + "/branches?per_page=1"
	if src.Forge == ForgeGitLab {
		contentPath = path + "/repository/branches?per_page=1"
	}
	return a.call(ctx, "GET", contentPath, nil, &branches)
}
func (a *Access) Close() {
	if a.Client != nil {
		a.Client.CloseIdleConnections()
	}
	a.Token = ""
}

// Pull authority is queried at execution, including retries. A signed old delivery
// cannot redeploy after close or after the review head changes.
func (a *Access) Pull(ctx context.Context, src *Source, number int, sha string, updatedAt string, closed bool) error {
	if a.Provider.Forge != ForgeGitHub || a.Provider.PrivateKeyFile == "" || a.Provider.Preview == nil {
		return fmt.Errorf("previews require a configured GitHub App policy")
	}
	u, _ := url.Parse(src.CloneURL)
	repo := strings.Trim(u.Path, "/")
	var pr struct {
		State     string    `json:"state"`
		UpdatedAt time.Time `json:"updated_at"`
		Head      struct {
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	}
	if number <= 0 {
		return fmt.Errorf("invalid pull request number")
	}
	if e := a.call(ctx, "GET", "/repos/"+repo+"/pulls/"+strconv.Itoa(number), nil, &pr); e != nil {
		return e
	}
	admittedTime, timestampErr := time.Parse(time.RFC3339Nano, updatedAt)
	if timestampErr != nil || !pr.UpdatedAt.Equal(admittedTime) {
		return fmt.Errorf("%w: pull request lifecycle delivery is stale", ErrStaleAuthority)
	}
	if !strings.EqualFold(pr.Base.Repo.FullName, repo) {
		return fmt.Errorf("%w: pull request base repository mismatch", ErrStaleAuthority)
	}
	if closed {
		if pr.State != "closed" {
			return fmt.Errorf("%w: pull request is not closed", ErrStaleAuthority)
		}
		return nil
	}
	if pr.State != "open" || pr.Head.SHA != sha || pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, repo) {
		return fmt.Errorf("%w: pull request head is stale, closed, or from an untrusted fork", ErrStaleAuthority)
	}
	return nil
}

// PushHead binds a signed payload to the live forge ref. Commit timestamps are
// not delivery clocks; current branch authority rejects old replays even after
// the bounded delivery journal has compacted. Generic HTTPS uses ls-remote.
func (a *Access) PushHead(ctx context.Context, src *Source, branch, sha string) error {
	if branch == "" || !ValidCommit(sha) {
		return fmt.Errorf("push authority invalid")
	}
	if src.Forge == ForgeGeneric {
		root, args, env, cleanup, e := a.gitSession(ctx)
		if e != nil {
			return e
		}
		defer cleanup()
		text, e := gitRun(ctx, root, append(args, "ls-remote", "--exit-code", a.Provider.Repository, "refs/heads/"+branch), env)
		if e != nil {
			// A failed, canceled or otherwise unusable ls-remote is not an
			// authoritative observation. gitRun flattens transport, auth,
			// cancellation, process and bounded-output failures — and the
			// --exit-code no-match exit — into one error, so absence cannot
			// be inferred either. The refusal stays retryable-unavailable
			// instead of a durable stale-delivery ignore.
			return fmt.Errorf("push authority unavailable: %w", e)
		}
		return classifyLsRemote(text, branch, sha)
	}
	u, _ := url.Parse(src.CloneURL)
	repo := strings.Trim(u.Path, "/")
	path := "/repos/" + repo + "/branches/" + url.PathEscape(branch)
	if src.Forge == ForgeGitLab {
		path = "/projects/" + url.PathEscape(repo) + "/repository/branches/" + url.PathEscape(branch)
	}
	var row struct {
		Commit struct {
			SHA string `json:"sha"`
			ID  string `json:"id"`
		} `json:"commit"`
	}
	if e := a.call(ctx, "GET", path, nil, &row); e != nil {
		return e
	}
	got := row.Commit.SHA
	if got == "" {
		got = row.Commit.ID
	}
	if !ValidCommit(got) {
		return fmt.Errorf("provider branch authority invalid")
	}
	if got != sha {
		return fmt.Errorf("%w: push head no longer authoritative", ErrStaleAuthority)
	}
	return nil
}

// classifyLsRemote interprets one completed generic ls-remote observation.
// Only a well-formed advertisement of exactly the watched ref — a valid
// commit id under refs/heads/<branch> — proves a mismatch, which is the
// sole stale outcome (an honest durable ignore). Malformed output is an
// invalid authority observation and stays retryable-unavailable, never
// stale: garbage must not permanently swallow a current delivery.
func classifyLsRemote(text, branch, sha string) error {
	fields := strings.Fields(text)
	if len(fields) != 2 || fields[1] != "refs/heads/"+branch || !ValidCommit(fields[0]) {
		return fmt.Errorf("push authority observation invalid")
	}
	if fields[0] != sha {
		return fmt.Errorf("%w: push head no longer authoritative", ErrStaleAuthority)
	}
	return nil
}
