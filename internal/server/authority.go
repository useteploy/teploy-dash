package server

// authority.go — D07 config-authority indicators: which side owns an app's
// configuration. The settled model is the manifest store (X02/D04): an app
// with a REGISTERED manifest is configured by it — git-managed mode means
// the repository owns the content (dash holds a pinned revision), and a
// dash-side edit of a manifest-declared setting would be a silent competing
// source of truth. The API answers the authority question per app, the env
// surface refuses source-owned edits explicitly (409 with the remedy), and
// the UI renders the indicators.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/useteploy/teploy-dash/internal/manifest"
	"github.com/useteploy/teploy-dash/internal/source"
)

// configAuthorityResponse is the per-app authority answer. Registered is
// false when no manifest exists for the app (nothing registered the config
// anywhere) — the UI then treats every setting as local/dash-side, which is
// the pre-manifest truth.
type configAuthorityResponse struct {
	Registered       bool                   `json:"registered"`
	Mode             string                 `json:"mode,omitempty"`
	Git              *manifest.GitReference `json:"git,omitempty"`
	ManifestRevision string                 `json:"manifest_revision,omitempty"`
	// RepositoryURL is the normalized (browsable) form of the git reference's
	// clone URL — the "create a source change" affordance links there. Empty
	// when normalization fails; the raw repository string stays in Git.
	RepositoryURL   string   `json:"repository_url,omitempty"`
	DeclaredEnvKeys []string `json:"declared_env_keys,omitempty"`
}

// handleConfigAuthority serves GET /api/apps/{server}/{app}/config-authority
// (viewer-visible metadata: names and repo references, never values).
func (s *Server) handleConfigAuthority(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	resp := configAuthorityResponse{}
	if s.manifests != nil {
		if doc, err := s.manifests.Get(serverName, appName); err == nil {
			resp.Registered = true
			resp.Mode = string(doc.Mode)
			resp.Git = doc.Git
			resp.ManifestRevision = doc.CurrentRevision
			resp.DeclaredEnvKeys = manifest.EnvKeys([]byte(doc.Manifest))
			if doc.Git != nil && doc.Git.Repository != "" {
				if normalized, err := source.NormalizeCloneURL(doc.Git.Repository); err == nil {
					resp.RepositoryURL = normalized
				}
			}
		} else if !errors.Is(err, manifest.ErrNotFound) {
			writeError(w, err.Error())
			return
		}
	}
	if resp.DeclaredEnvKeys == nil {
		resp.DeclaredEnvKeys = []string{}
	}
	writeData(w, resp)
}

// envKeySourceOwned reports whether an env mutation target is owned by a
// GIT-MANAGED manifest (the source of truth for that key). The boolean is
// the verdict; the string is the repository to point the operator at.
func (s *Server) envKeySourceOwned(serverName, appName, key string) (bool, string) {
	if s.manifests == nil {
		return false, ""
	}
	doc, err := s.manifests.Get(serverName, appName)
	if err != nil || doc.Mode != manifest.ModeGitManaged {
		return false, ""
	}
	for _, declared := range manifest.EnvKeys([]byte(doc.Manifest)) {
		if declared == key {
			repo := ""
			if doc.Git != nil {
				repo = doc.Git.Repository
				if normalized, err := source.NormalizeCloneURL(repo); err == nil {
					repo = normalized
				}
			}
			return true, repo
		}
	}
	return false, ""
}

// refuseSourceOwnedEnvEdit is the guard on env mutations (D07: "Git-managed
// edits should create a source change or be explicitly read-only; they must
// not create a silent competing source of truth"). It refuses with 409 and
// the remedy — never a silent override of source-owned configuration. Keys
// NOT declared by the manifest stay dash-settable: secret VALUES cannot live
// in a manifest (Validate rejects them), so the secret:... inputs a
// git-managed manifest references are set here on purpose.
func (s *Server) refuseSourceOwnedEnvEdit(w http.ResponseWriter, serverName, appName, key string) bool {
	owned, repo := s.envKeySourceOwned(serverName, appName, key)
	if !owned {
		return false
	}
	var where strings.Builder
	where.WriteString("environment variable ")
	where.WriteString(key)
	where.WriteString(" is declared in the git-managed manifest for this app")
	if repo != "" {
		where.WriteString(" (")
		where.WriteString(repo)
		where.WriteString(")")
	}
	where.WriteString("; change it at the source — a dash-side edit would compete with the repository")
	writeErrorStatus(w, where.String(), http.StatusConflict)
	return true
}
