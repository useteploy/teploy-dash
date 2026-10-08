package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/source"
	"gopkg.in/yaml.v3"
)

func (s *Server) resolveSourceAccess(ctx context.Context, src *source.Source) (*source.Access, error) {
	if s.sourceAccessResolver != nil {
		return s.sourceAccessResolver(ctx, src)
	}
	return s.sourceProviders.Resolve(ctx, src)
}
func (s *Server) checkoutSource(ctx context.Context, a *source.Access, sha string) (string, func(), error) {
	if s.sourceCheckout != nil {
		return s.sourceCheckout(ctx, a, sha)
	}
	return a.Checkout(ctx, sha)
}

func (s *Server) executeSourceOperation(ctx context.Context, cmd operation.Command, emit func(operation.Stream, string), execute operation.Executor) (int, error) {
	req := cmd.SourceRequest
	src, e := s.sources.Get(req.SourceID)
	if e != nil {
		return -1, fmt.Errorf("source unavailable")
	}
	if src.Degraded && req.Kind != operation.KindSourcePreviewExpire {
		return -1, fmt.Errorf("source credentials are degraded")
	}
	cleanupKind := req.Kind == operation.KindSourcePreviewDestroy || req.Kind == operation.KindSourcePreviewExpire
	if cleanupKind {
		// Cleanup authority outlives the current manifest and server alias.
		owner, err := s.cleanupOwner(src, req.Server, req.App, req.SourcePullRequest)
		if err != nil {
			return -1, err
		}
		_ = owner
		// The present CLI has no atomic generation compare-and-destroy.
		// Never turn an unfenced list into permission to destroy a renewal.
		return -1, fmt.Errorf("preview cleanup requires CLI preview-compare-destroy-v1; ownership retained for retry")
	}
	doc, e := s.manifests.Get(req.Server, req.App)
	if e != nil || doc.Git == nil || (req.ManifestRevision != "" && doc.CurrentRevision != req.ManifestRevision) {
		return -1, fmt.Errorf("source manifest binding unavailable")
	}
	want, e := source.CanonicalURL(doc.Git.Repository)
	got, ge := source.CanonicalURL(src.CloneURL)
	if e != nil || ge != nil || want != got {
		return -1, fmt.Errorf("source manifest binding changed")
	}
	access, e := s.resolveSourceAccess(ctx, src)
	if e != nil {
		return -1, e
	}
	defer access.Close()
	preview := req.Kind == operation.KindSourcePreview
	if preview {
		if e = access.Pull(ctx, src, req.SourcePullRequest, req.SourceCommit, req.SourcePullUpdatedAt, false); e != nil {
			return -1, e
		}
	}
	dir, cleanup, e := s.checkoutSource(ctx, access, req.SourceCommit)
	if e != nil {
		return -1, e
	}
	defer cleanup()
	if preview {
		if e = writePreviewManifest(dir, req.App, access.Provider.Preview); e != nil {
			return -1, e
		}
	} else {
		dir, e = source.ProjectPath(dir, req.SourceManifestPath)
		if e != nil {
			return -1, e
		}
		if e = checkSourceApp(dir, req.App); e != nil {
			return -1, e
		}
	}
	cmd.SourceRequest = nil
	for i, arg := range cmd.Args {
		if arg == "--project-dir" && i+1 < len(cmd.Args) {
			cmd.Args[i+1] = dir
		}
	}
	if !preview {
		return execute(ctx, cmd, emit)
	}
	// CLI owns images, preview identity, route swap, expiry and volume ownership.
	// No deployment state is synthesized or written by Dash.
	policyBytes, e := os.ReadFile(filepath.Join(dir, "teploy.yml"))
	if e != nil {
		return -1, fmt.Errorf("installed preview policy unavailable")
	}
	admittedPolicy := sha256.Sum256(policyBytes)
	var target []string
	for i, a := range cmd.Args {
		if (a == "--host" || a == "--user") && i+1 < len(cmd.Args) {
			target = append(target, a, cmd.Args[i+1])
		}
	}
	branch := "dash-" + src.ID + "-pr-" + strconv.Itoa(req.SourcePullRequest)
	var output strings.Builder
	oversized := false
	code, e := execute(ctx, cmd, func(stream operation.Stream, text string) {
		if stream == operation.StreamStdout {
			if output.Len()+len(text)+1 <= 1<<20 {
				output.WriteString(text)
				output.WriteByte('\n')
			} else {
				oversized = true
			}
		} else {
			emit(stream, text)
		}
	})
	if e != nil || code != 0 {
		return code, e
	}
	var result struct {
		Image   string `json:"image"`
		Version string `json:"version"`
	}
	if oversized || json.Unmarshal([]byte(output.String()), &result) != nil || result.Image == "" || strings.ContainsAny(result.Image, "\r\n\x00 ") {
		return -1, fmt.Errorf("CLI build did not return a valid image receipt")
	}
	if len(result.Version) < 7 || !strings.HasPrefix(req.SourceCommit, result.Version) || result.Image != req.App+"-build-"+result.Version {
		return -1, fmt.Errorf("CLI image receipt does not match checked-out commit")
	}
	// Re-read credential policy after the potentially long build. Rotation,
	// revocation and changed preview access policy are checked before deployment.
	currentDoc, e := s.manifests.Get(req.Server, req.App)
	if e != nil || currentDoc.Git == nil || currentDoc.CurrentRevision != doc.CurrentRevision {
		return -1, fmt.Errorf("manifest binding changed during build")
	}
	latest, e := s.sources.Get(src.ID)
	if e != nil || latest.CredentialRef != src.CredentialRef || latest.Degraded {
		return -1, fmt.Errorf("source authority changed during build")
	}
	fresh, e := s.resolveSourceAccess(ctx, latest)
	if e != nil {
		return -1, e
	}
	defer fresh.Close()
	before, _ := json.Marshal(access.Provider)
	after, _ := json.Marshal(fresh.Provider)
	if string(before) != string(after) || access.AuthorityDigest != fresh.AuthorityDigest {
		return -1, fmt.Errorf("preview policy changed during build")
	}
	policyDir, e := os.MkdirTemp("", "teploy-preview-policy-recheck-")
	if e != nil {
		return -1, fmt.Errorf("policy recheck unavailable")
	}
	defer os.RemoveAll(policyDir)
	if e = writePreviewManifest(policyDir, req.App, fresh.Provider.Preview); e != nil {
		return -1, e
	}
	currentPolicy, e := os.ReadFile(filepath.Join(policyDir, "teploy.yml"))
	if e != nil || sha256.Sum256(currentPolicy) != admittedPolicy {
		return -1, fmt.Errorf("preview manifest policy changed during build")
	}
	access = fresh
	// Recheck after the potentially long build. The head may have moved or closed.
	if e = access.Pull(ctx, src, req.SourcePullRequest, req.SourceCommit, req.SourcePullUpdatedAt, false); e != nil {
		return -1, e
	}
	policy := access.Provider.Preview
	cmd.Args = []string{"--project-dir", dir, "preview", "deploy", branch, "--image", result.Image, "--ttl", policy.TTL, "--base-domain", policy.BaseDomain}
	for _, ip := range policy.AllowIPs {
		cmd.Args = append(cmd.Args, "--allow-ip", ip)
	}
	cmd.Args = append(cmd.Args, target...)
	return execute(ctx, cmd, emit)
}
func checkSourceApp(dir, app string) error {
	b, e := os.ReadFile(filepath.Join(dir, "teploy.yml"))
	if e != nil {
		return fmt.Errorf("source manifest unreadable")
	}
	var v struct {
		App string `yaml:"app"`
	}
	if yaml.Unmarshal(b, &v) != nil || v.App != app {
		return fmt.Errorf("checked out manifest app does not match admitted target")
	}
	return nil
}

// A preview runs only this operator-approved, secret-free manifest. PR code
// cannot retarget an app, mount production data or inject secret references.
func writePreviewManifest(dir, app string, p *source.PreviewPolicy) error {
	if p == nil || p.ManifestFile == "" || !filepath.IsAbs(p.ManifestFile) {
		return fmt.Errorf("preview requires an absolute trusted manifest file")
	}
	ttl, e := time.ParseDuration(p.TTL)
	if e != nil || ttl < time.Minute || ttl > 7*24*time.Hour {
		return fmt.Errorf("preview TTL must be 1 minute to 7 days")
	}
	if p.BaseDomain == "" || strings.ContainsAny(p.BaseDomain, "/ :@\r\n") || len(p.AllowIPs) == 0 {
		return fmt.Errorf("preview requires an explicit domain and access allowlist")
	}
	for _, ip := range p.AllowIPs {
		if net.ParseIP(ip) == nil {
			if _, _, e = net.ParseCIDR(ip); e != nil {
				return fmt.Errorf("preview access allowlist invalid")
			}
		}
	}
	f, e := os.Open(p.ManifestFile)
	if e != nil {
		return fmt.Errorf("preview manifest unavailable")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 1<<20 {
		return fmt.Errorf("preview manifest must be a private bounded regular file")
	}
	var doc map[string]any
	d := yaml.NewDecoder(f)
	var extra yaml.Node
	if d.Decode(&doc) != nil || d.Decode(&extra) != io.EOF {
		return fmt.Errorf("preview manifest invalid")
	}
	allowed := map[string]bool{"app": true, "port": true, "dockerfile": true, "context": true, "domain": true, "healthcheck": true, "health_check": true, "resources": true, "ingress": true}
	for k := range doc {
		if !allowed[k] {
			return fmt.Errorf("preview manifest field %q is not allowed", k)
		}
	}
	// The current CLI preview consumer maps container port 80. Refuse a
	// different manifest port rather than promise a route it cannot serve.
	if port, ok := doc["port"]; ok && port != 80 {
		return fmt.Errorf("current CLI preview contract requires container port 80")
	}
	if doc["app"] != app {
		return fmt.Errorf("preview manifest app scope mismatch")
	}
	for _, k := range []string{"context", "dockerfile"} {
		if v, ok := doc[k]; ok {
			path, ok := v.(string)
			if !ok || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(filepath.Clean(path), "../") {
				return fmt.Errorf("preview build path escapes checkout")
			}
		}
	}
	b, e := yaml.Marshal(doc)
	if e != nil || strings.Contains(string(b), "secret:") {
		return fmt.Errorf("preview manifest contains a secret reference")
	}
	// Remove the repository's manifest first: it may be a symlink to another
	// checked-out file. Never follow it when installing trusted policy bytes.
	path := filepath.Join(dir, "teploy.yml")
	if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
		return fmt.Errorf("preview manifest replacement failed")
	}
	if e = os.WriteFile(path, b, 0600); e != nil {
		return fmt.Errorf("preview manifest write failed")
	}
	return nil
}
