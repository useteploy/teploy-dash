package server

// authority.go — where an app's settings are edited. The CLI answers it
// (`config get --json`, spec §2.1): a Teploy-home app keeps its teploy.yml
// on its server, and an env key that config declares is changed in
// Settings, never by a competing env edit from dash.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/useteploy/teploy-dash/internal/cli"
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

// envSet performs one `env set` write through the injected runner seams
// (same argv and stdin contract as cli.EnvSet, which stays the reference
// implementation for non-server callers). A11/F015/R03: with the CLI's
// stdin contract the value never touches the argv; a probe that cannot
// answer fails closed; a verified-unsupported CLI refuses secret values
// unless the operator opted into the legacy argv transport.
func (s *Server) envSet(r *http.Request, serverName, appName, key, value string) (*cli.Result, error) {
	probe := s.config.EnvStdinSupport
	if probe == nil {
		probe = cli.EnvStdinSupport
	}
	stdinSupported, err := probe()
	if err != nil {
		return nil, err
	}
	target := []string{"--host", s.serverHost(serverName), "--app", appName}
	if u := s.serverUser(serverName); u != "" {
		target = append(target, "--user", u)
	}
	actor := s.requestActorArgs(r)
	if stdinSupported {
		args := append([]string{"env", "set", key, "--stdin"}, target...)
		return s.runCLIStdin(r.Context(), value, append(args, actor...)...)
	}
	if value != "" && !cli.LegacySecretArgVAllowed() {
		return nil, cli.ErrLegacySecretArgV
	}
	args := append([]string{"env", "set", key + "=" + value}, target...)
	result, err := s.runCLI(r.Context(), append(args, actor...)...)
	if err != nil {
		return result, err
	}
	return result, cli.CheckExit(result)
}

// envUnset performs one `env unset KEY` through the injected runner seam.
func (s *Server) envUnset(r *http.Request, serverName, appName, key string) (*cli.Result, error) {
	target := []string{"--host", s.serverHost(serverName), "--app", appName}
	if u := s.serverUser(serverName); u != "" {
		target = append(target, "--user", u)
	}
	args := append([]string{"env", "unset", key}, target...)
	args = append(args, s.requestActorArgs(r)...)
	result, err := s.runCLI(r.Context(), args...)
	if err != nil {
		return result, err
	}
	return result, cli.CheckExit(result)
}

// cliEnvGuardDeferred reports whether the installed CLI carries its own env
// config guard (token env-config-guard-v1): when it does, dash skips its
// local pre-check and lets the CLI refuse, surfacing the CLI's message (the
// CLI guard also covers homes dash's local parse cannot see).
func (s *Server) cliEnvGuardDeferred(r *http.Request) bool {
	return s.capabilities(r.Context()).Features.EnvConfigGuard
}

// cliEnvGuardRefusalPrefix is the stable first line of the CLI's
// env-config-guard refusal (env.go: "env operation refused (nothing
// written):"). Classified by message text like classifyServerError.
const cliEnvGuardRefusalPrefix = "env operation refused"

// writeEnvActionError maps a failed env set/unset. The CLI's config-guard
// refusal is an answer (409, message verbatim — it names the key and the
// remedy); any other completed non-zero exit keeps the CLI's message as a
// 400; transport failures are a bad gateway.
func writeEnvActionError(w http.ResponseWriter, res *cli.Result, err error) {
	var exited *cli.ExitStatusError
	if errors.As(err, &exited) {
		msg := strings.TrimSpace(err.Error())
		if strings.HasPrefix(msg, cliEnvGuardRefusalPrefix) {
			writeErrorStatus(w, msg, http.StatusConflict)
			return
		}
		writeError(w, msg)
		return
	}
	writeErrorStatus(w, err.Error(), http.StatusBadGateway)
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
