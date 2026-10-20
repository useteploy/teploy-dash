package server

// secrets.go — plan D5: the app's secrets as the CLI holds them, write-only
// from dash. `teploy secret list --app X --host S --json` answers key names
// with masked values; dash never reads a value back and never displays one.
// Writes go through `secret set KEY --stdin` (value on stdin, never argv,
// where every local process could read it) and `secret rm KEY`. Gated on the
// secret-stdin-v1 capability token; with an older CLI the surface answers
// 501 and the UI hides it.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/useteploy/teploy-dash/internal/cli"
)

// handleSecretsList serves GET /api/apps/{server}/{app}/secrets: the secret
// key names (the CLI masks every value as "***"). Metadata — names only.
func (s *Server) handleSecretsList(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Secrets }, "Secrets", "v0.1.45") {
		return
	}
	res, err := s.cliAppRun(r.Context(), serverName, appName, "secret", "list", "--json")
	if err != nil {
		writeError(w, err.Error())
		return
	}
	writeRawJSON(w, res.Stdout)
}

// handleSecretsSet serves POST /api/apps/{server}/{app}/secrets {key, value}:
// `secret set KEY --stdin`. The value travels on the CLI's stdin exactly as
// given and is never echoed back — the answer carries only the key.
func (s *Server) handleSecretsSet(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Secrets }, "Secrets", "v0.1.45") {
		return
	}
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !validEnvKey(body.Key) {
		writeError(w, "invalid secret name")
		return
	}
	if !s.cliInstalled() {
		writeError(w, "teploy CLI not installed")
		return
	}
	target, err := s.appTargetArgs(r.Context(), serverName, appName)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	args := append([]string{"secret", "set", body.Key, "--stdin"}, target...)
	args = append(args, s.requestActorArgs(r)...)
	res, err := s.runCLIStdin(r.Context(), body.Value, args...)
	if err != nil {
		writeSecretsError(w, res, err)
		return
	}
	writeData(w, map[string]string{"key": body.Key, "output": strings.TrimSpace(res.Stdout)})
}

// handleSecretsDelete serves DELETE /api/apps/{server}/{app}/secrets/{key}:
// `secret rm KEY`. The CLI reports an unset key as a no-op, so re-running a
// removal stays safe.
func (s *Server) handleSecretsDelete(w http.ResponseWriter, r *http.Request, serverName, appName, key string) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Secrets }, "Secrets", "v0.1.45") {
		return
	}
	if !validEnvKey(key) {
		writeError(w, "invalid secret name")
		return
	}
	if !s.cliInstalled() {
		writeError(w, "teploy CLI not installed")
		return
	}
	parts := append([]string{"secret", "rm", key}, s.requestActorArgs(r)...)
	res, err := s.cliAppRun(r.Context(), serverName, appName, parts...)
	if err != nil {
		writeSecretsError(w, res, err)
		return
	}
	writeData(w, map[string]string{"key": key, "output": strings.TrimSpace(res.Stdout)})
}

// writeSecretsError maps a failed secrets write: a completed non-zero exit
// is the CLI's answer (OpenBao's rm refusal, a locked store) and surfaces as
// its message; a transport failure is a bad gateway.
func writeSecretsError(w http.ResponseWriter, res *cli.Result, err error) {
	var exited *cli.ExitStatusError
	if errors.As(err, &exited) {
		writeErrorStatus(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeErrorStatus(w, err.Error(), http.StatusBadGateway)
}
