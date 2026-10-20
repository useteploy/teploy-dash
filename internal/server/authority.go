package server

// authority.go — where an app's settings are edited. The CLI answers it
// (`config get --json`, spec §2.1): a Teploy-home app keeps its teploy.yml
// on its server, and an env key that config declares is changed in
// Settings, never by a competing env edit from dash.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/useteploy/teploy-dash/internal/configkeys"
)

// configAuthorityResponse is the per-app answer.
type configAuthorityResponse struct {
	DeclaredEnvKeys []string `json:"declared_env_keys"`
	// Home is where the app's settings are edited, from the CLI (spec
	// §2.1): "folder" or "teploy" ("" when the CLI cannot say). For a
	// Teploy-home app DeclaredEnvKeys are its config's env keys and
	// ConfigCommit/AppliedCommit tell whether the config changed since the
	// running release was deployed.
	Home          string `json:"home,omitempty"`
	ConfigCommit  string `json:"config_commit,omitempty"`
	AppliedCommit string `json:"applied_commit,omitempty"`
}

// cliConfigHome asks the CLI where an app's settings live; ok is false when
// the installed CLI has no config homes or the read failed.
func (s *Server) cliConfigHome(ctx context.Context, serverName, appName string) (home, commit, applied, content string, ok bool) {
	if !s.cliInstalled() || !s.capabilities(ctx).Features.ConfigHome {
		return "", "", "", "", false
	}
	res, err := s.cliAppRun(ctx, serverName, appName, "config", "get", "--json")
	if err != nil {
		return "", "", "", "", false
	}
	var dto struct {
		Home          string `json:"home"`
		Commit        string `json:"commit"`
		AppliedCommit string `json:"applied_commit"`
		Content       string `json:"content"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &dto) != nil || dto.Home == "" {
		return "", "", "", "", false
	}
	return dto.Home, dto.Commit, dto.AppliedCommit, dto.Content, true
}

// configOwnedEnvKeyError is the env guard for Teploy-home apps (spec P2,
// "env guard via CLI"): a key the app's server config declares under env:
// is edited in Settings. nil means the edit may proceed.
func (s *Server) configOwnedEnvKeyError(ctx context.Context, serverName, appName, key string) error {
	home, _, _, content, ok := s.cliConfigHome(ctx, serverName, appName)
	if !ok || home != "teploy" {
		return nil
	}
	keys, err := configkeys.EnvKeys([]byte(content))
	if err != nil {
		return nil
	}
	for _, k := range keys {
		if k == key {
			return fmt.Errorf("environment variable %s is set in this app's Teploy settings; change it in Settings", key)
		}
	}
	return nil
}

func (s *Server) refuseConfigOwnedEnvEdit(w http.ResponseWriter, r *http.Request, serverName, appName, key string) bool {
	if err := s.configOwnedEnvKeyError(r.Context(), serverName, appName, key); err != nil {
		writeErrorStatus(w, err.Error(), http.StatusConflict)
		return true
	}
	return false
}

// handleConfigAuthority serves GET /api/apps/{server}/{app}/config-authority
// (viewer-visible metadata: names, never values).
func (s *Server) handleConfigAuthority(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	resp := configAuthorityResponse{DeclaredEnvKeys: []string{}}
	if home, commit, applied, content, ok := s.cliConfigHome(r.Context(), serverName, appName); ok {
		resp.Home, resp.ConfigCommit, resp.AppliedCommit = home, commit, applied
		if home == "teploy" {
			if keys, err := configkeys.EnvKeys([]byte(content)); err == nil && keys != nil {
				resp.DeclaredEnvKeys = keys
			}
		}
	}
	writeData(w, resp)
}

// handleManifestsGone answers every /api/manifests route: the manifest store
// was removed and app settings now live on the app's server.
func (s *Server) handleManifestsGone(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	writeErrorStatus(w, "the manifests API was removed; app settings live on the app's server (Settings tab / teploy config)", http.StatusGone)
}
