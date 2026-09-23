// Package source stores the canonical forge/repository identity behind
// D04's git-provider journeys: forge kind + normalized clone URL (never a
// bare owner/repo — the F03 lesson from teploy-ship, where an owner/name
// lookup returned a DIFFERENT forge's configuration), an as-of-added
// default-branch snapshot, a credential REFERENCE (the token value never
// enters the record), and a webhook delivery identity (secret in its own
// 0600 file, deliveries in an append-only ledger).
package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// Forge is the forge kind axis of a source's identity. Two sources with the
// same owner/repo on different forges are two identities; so are two kinds
// over one URL (a GitHub Enterprise install and a Forgejo can share a host
// string in principle — the kind is what the operator says the forge IS).
type Forge string

const (
	ForgeGitHub  Forge = "github"
	ForgeForgejo Forge = "forgejo"
	ForgeGitea   Forge = "gitea"
	ForgeGitLab  Forge = "gitlab"
	// ForgeGeneric covers forges without a dedicated kind (Gogs, forgotten
	// Gitea versions, bespoke cgit). Payload parsing treats it like the
	// GitHub push shape; GitLab-specific headers still work.
	ForgeGeneric Forge = "generic"
)

func ValidForge(f Forge) bool {
	switch f {
	case ForgeGitHub, ForgeForgejo, ForgeGitea, ForgeGitLab, ForgeGeneric:
		return true
	}
	return false
}

const maxCloneURLLength = 2048

// NormalizeCloneURL canonicalizes one repository's clone URL into the form
// dash stores and compares:
//
//   - credentials (userinfo, scp user) are STRIPPED before anything is
//     persisted — the record can never carry a token;
//   - scp-like (git@host:path) and ssh:// spellings become the https form —
//     transport is not identity, forge kind + host + path is;
//   - ".git" suffix and trailing slashes are dropped;
//   - scheme and host are lowercased; default ports (443/80/22) dropped;
//   - path case is PRESERVED in the display form (the identity key folds
//     it — the forges match owner/repo case-insensitively and redirect).
//
// http is accepted (self-hosted LAN forges) and stays a distinct identity
// from https on the same host. Query strings, fragments, and control
// characters are rejected.
func NormalizeCloneURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("clone URL is required")
	}
	if len(trimmed) > maxCloneURLLength {
		return "", fmt.Errorf("clone URL exceeds %d characters", maxCloneURLLength)
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("clone URL contains control characters")
		}
	}
	if !strings.Contains(trimmed, "://") {
		scp, err := normalizeSCP(trimmed)
		if err != nil {
			return "", err
		}
		trimmed = scp
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid clone URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https", "http", "ssh":
	default:
		return "", fmt.Errorf("clone URL scheme must be https or ssh")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", fmt.Errorf("clone URL must not contain query parameters or fragments")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("clone URL host is required")
	}
	port := parsed.Port()
	switch {
	case port == "" || (scheme == "https" && port == "443") || (scheme == "http" && port == "80") || (scheme == "ssh" && port == "22"):
		port = ""
	}
	path := strings.Trim(parsed.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if path == "" {
		return "", fmt.Errorf("clone URL path is required")
	}
	if strings.Contains(path, "//") || strings.Contains(path, "..") {
		return "", fmt.Errorf("clone URL path is not a repository path")
	}
	// Transport is not identity: ssh spellings canonicalize onto the https
	// form. The credential layer decides how a checkout actually happens.
	canonicalScheme := "https"
	if scheme == "http" {
		canonicalScheme = "http"
	}
	if port != "" {
		return canonicalScheme + "://" + host + ":" + port + "/" + path, nil
	}
	return canonicalScheme + "://" + host + "/" + path, nil
}

// normalizeSCP rewrites git@host:relative/path into https://host/path. The
// colon must sit in the host segment (before any slash), or the string is
// not an scp form.
func normalizeSCP(s string) (string, error) {
	at := strings.LastIndex(s, "@")
	if at <= 0 {
		return "", fmt.Errorf("invalid clone URL")
	}
	rest := s[at+1:]
	colon := strings.Index(rest, ":")
	if colon <= 0 {
		return "", fmt.Errorf("invalid clone URL")
	}
	host := rest[:colon]
	path := strings.TrimPrefix(rest[colon+1:], "/")
	if host == "" || path == "" || strings.ContainsAny(host, "/@:") {
		return "", fmt.Errorf("invalid clone URL")
	}
	return "https://" + strings.ToLower(host) + "/" + path, nil
}

// IdentityKey is the dedupe key: forge kind + the case-folded canonical
// URL, joined with NUL so no forge or URL byte can forge a collision.
func IdentityKey(forge Forge, rawCloneURL string) (string, error) {
	if !ValidForge(forge) {
		return "", fmt.Errorf("unknown forge kind %q", string(forge))
	}
	normalized, err := NormalizeCloneURL(rawCloneURL)
	if err != nil {
		return "", err
	}
	return string(forge) + "\x00" + strings.ToLower(normalized), nil
}

// CanonicalURL is the URL-only join key between sources and git-managed
// manifests: transport-normalized and case-folded, without the forge kind
// (the manifest's repository URL is already origin-qualified; F03's defect
// was bare owner/repo lookups, not URL lookups).
func CanonicalURL(rawCloneURL string) (string, error) {
	normalized, err := NormalizeCloneURL(rawCloneURL)
	if err != nil {
		return "", err
	}
	return strings.ToLower(normalized), nil
}

// DeriveID returns the stable source ID — the fleet-envelope convention
// (deterministic sha256 of the identity key, truncated to 16 hex chars).
func DeriveID(forge Forge, rawCloneURL string) (string, error) {
	key, err := IdentityKey(forge, rawCloneURL)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(key))
	return "src-" + hex.EncodeToString(digest[:])[:16], nil
}
