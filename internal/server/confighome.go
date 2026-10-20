package server

// confighome.go — server-authority spec P2: the dash surface over the CLI's
// config homes and server event history. Dash keeps no copy of either: every
// read and write is a `teploy config ...` / `teploy events` call against the
// app's server, so a laptop, CI and this dashboard see one config and one
// history. Each surface is gated on the CLI capability token that provides
// it; with an older CLI on PATH the routes answer 501 with the remedy and the
// UI hides them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/operation"
)

var configCommitPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// maxConfigBody bounds a submitted teploy.yml (the CLI's limit is 1 MiB).
const maxConfigBody = 1 << 20

// dashActorArgs names the dashboard principal in the target's event
// history. The label is the session's user (client-asserted on the
// server; the authenticated identity stays in the operation record, linked
// by --actor-op).
func dashActorArgs(actor *operation.Actor, opID string) []string {
	label := "dashboard"
	if actor != nil && actor.Label != "" {
		label = actor.Label
	}
	args := []string{"--actor", "dash:" + label}
	if opID != "" {
		args = append(args, "--actor-op", opID)
	}
	return args
}

// requestActorArgs is dashActorArgs for an inline (non-operation) CLI call.
func (s *Server) requestActorArgs(r *http.Request) []string {
	if !s.capabilities(r.Context()).Features.ActorFlag {
		return nil
	}
	return dashActorArgs(actorFromRequest(r), "")
}

// canWritePrivilegedConfig reports whether the session may save config that
// reaches the host (spec §2.3, gate 8): the admin capability. With auth
// disabled there is no principal to narrow.
func canWritePrivilegedConfig(r *http.Request) bool {
	si, ok := currentUser(r)
	if !ok {
		return true
	}
	return si.caps != nil && (si.caps.Allow(caps.AdministerCredentials) || si.caps.Allow(caps.AdministerUsers))
}

// requireFeature answers 501 with the remedy when the installed CLI lacks a
// token-gated surface.
func (s *Server) requireFeature(w http.ResponseWriter, r *http.Request, ok func(machineFeatures) bool, what string) bool {
	if !s.cliInstalled() {
		writeError(w, "teploy CLI not installed")
		return false
	}
	if !ok(s.capabilities(r.Context()).Features) {
		writeErrorStatus(w, what+" needs a newer teploy CLI on the dashboard host (upgrade teploy to v0.1.44 or later)", http.StatusNotImplemented)
		return false
	}
	return true
}

// appTargetArgs are the --host/--app/--user arguments for an app command.
func (s *Server) appTargetArgs(ctx context.Context, serverName, appName string) ([]string, error) {
	srv, err := s.lookupServerStrict(ctx, serverName)
	if err != nil {
		return nil, err
	}
	args := []string{"--host", srv.Host, "--app", appName}
	if srv.User != "" {
		args = append(args, "--user", srv.User)
	}
	return args, nil
}

// cliMachineError reads the machine error envelope a failed --json command
// wrote to stderr: its code and detail ("" when there is none).
func cliMachineError(res *cli.Result) (code, detail string) {
	if res == nil {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(res.Stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var env struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &env) == nil && env.Code != "" {
			return env.Code, env.Detail
		}
	}
	return "", strings.TrimSpace(res.Stderr)
}

// handleAppConfigRoutes dispatches /api/apps/{server}/{app}/config... and
// /events. Reports false when the action is not one of them.
func (s *Server) handleAppConfigRoutes(w http.ResponseWriter, r *http.Request, serverName, appName, action string) bool {
	switch {
	case action == "config" && r.Method == http.MethodGet:
		s.handleConfigGet(w, r, serverName, appName)
	case action == "config" && r.Method == http.MethodPut:
		s.handleConfigPut(w, r, serverName, appName)
	case action == "config/history" && r.Method == http.MethodGet:
		if !s.requireFeature(w, r, func(f machineFeatures) bool { return f.ConfigHome }, "Config history") {
			return true
		}
		res, err := s.cliAppRun(r.Context(), serverName, appName, "config", "history", "--json")
		if err != nil {
			writeError(w, err.Error())
			return true
		}
		writeRawJSON(w, res.Stdout)
	case action == "config/restore" && r.Method == http.MethodPost:
		s.handleConfigRestore(w, r, serverName, appName)
	case action == "config/home" && r.Method == http.MethodPost:
		s.handleConfigHome(w, r, serverName, appName)
	case action == "events" && r.Method == http.MethodGet:
		if !s.requireFeature(w, r, func(f machineFeatures) bool { return f.Events }, "Server event history") {
			return true
		}
		args := []string{"events", "--json"}
		if v := r.URL.Query().Get("since_seq"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				writeError(w, "since_seq must be a non-negative integer")
				return true
			}
			args = append(args, "--since-seq", strconv.FormatInt(n, 10))
		}
		res, err := s.cliAppRun(r.Context(), serverName, appName, args...)
		if err != nil {
			writeError(w, err.Error())
			return true
		}
		writeRawJSON(w, res.Stdout)
	default:
		return false
	}
	return true
}

// handleConfigGet serves GET .../config[?at=<commit>]: the app's home, its
// Teploy config (content + commit) and the commit the running release was
// deployed from. A folder-home app answers home "folder" with no content.
func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeature(w, r, func(f machineFeatures) bool { return f.ConfigHome }, "Settings") {
		return
	}
	args := []string{"config", "get", "--json"}
	if at := r.URL.Query().Get("at"); at != "" {
		if !configCommitPattern.MatchString(at) {
			writeError(w, "invalid commit")
			return
		}
		args = append(args, "--at", at)
	}
	res, err := s.cliAppRun(r.Context(), serverName, appName, args...)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	writeRawJSON(w, res.Stdout)
}

// handleConfigPut serves PUT .../config {content, expect}: a compare-and-
// swap write of the whole teploy.yml. A user without the admin capability
// writes with --unprivileged, so a change that reaches the host is refused
// by the CLI (gate 8). A stale expect answers 409 with the CLI's key-level
// merge against the current config (merged content + the conflicting keys)
// for the editor to resolve.
func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeature(w, r, func(f machineFeatures) bool { return f.ConfigHome && f.ConfigUnprivileged }, "Editing settings") {
		return
	}
	var body struct {
		Content string `json:"content"`
		Expect  string `json:"expect"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigBody+4096)
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !configCommitPattern.MatchString(body.Expect) && body.Expect != strings.Repeat("0", 40) {
		writeError(w, "expect must name the commit the edit started from")
		return
	}
	if strings.TrimSpace(body.Content) == "" || len(body.Content) > maxConfigBody {
		writeError(w, "content must be a non-empty teploy.yml of at most 1 MiB")
		return
	}
	target, err := s.appTargetArgs(r.Context(), serverName, appName)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	args := append([]string{"config", "set", "--file", "-", "--expect", body.Expect, "--json"}, target...)
	if !canWritePrivilegedConfig(r) {
		args = append(args, "--unprivileged")
	}
	args = append(args, s.requestActorArgs(r)...)
	res, err := s.runCLIStdin(r.Context(), body.Content, args...)
	if err == nil {
		s.fleet.invalidate()
		writeRawJSON(w, res.Stdout)
		return
	}
	var exited *cli.ExitStatusError
	if !errors.As(err, &exited) {
		writeErrorStatus(w, err.Error(), http.StatusBadGateway)
		return
	}
	code, detail := cliMachineError(res)
	switch code {
	case "conflict":
		if !s.capabilities(r.Context()).Features.ConfigMerge {
			writeErrorStatus(w, detail, http.StatusConflict)
			return
		}
		margs := append([]string{"config", "merge", "--base", body.Expect, "--file", "-", "--json"}, target...)
		mres, merr := s.runCLIStdin(r.Context(), body.Content, margs...)
		if merr != nil {
			writeErrorStatus(w, detail, http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": detail, "merge": json.RawMessage(strings.TrimSpace(mres.Stdout))})
	case "config-invalid":
		writeErrorStatus(w, detail, http.StatusUnprocessableEntity)
	default:
		if detail == "" {
			detail = err.Error()
		}
		writeErrorStatus(w, detail, http.StatusBadGateway)
	}
}

// handleConfigRestore serves POST .../config/restore {commit}: an earlier
// config becomes the current one as a new commit (same privilege rule).
func (s *Server) handleConfigRestore(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeature(w, r, func(f machineFeatures) bool { return f.ConfigHome && f.ConfigUnprivileged }, "Restoring settings") {
		return
	}
	var body struct {
		Commit string `json:"commit"`
	}
	if err := strictDecode(r, &body); err != nil || !configCommitPattern.MatchString(body.Commit) {
		writeError(w, "commit is required")
		return
	}
	args := []string{"config", "restore", body.Commit}
	if !canWritePrivilegedConfig(r) {
		args = append(args, "--unprivileged")
	}
	args = append(args, s.requestActorArgs(r)...)
	res, err := s.cliAppRun(r.Context(), serverName, appName, args...)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	writeData(w, map[string]string{"output": strings.TrimSpace(res.Stdout)})
}

// handleConfigHome serves POST .../config/home {home}:
//
//   - "teploy": "Make editable here" — the config is rebuilt from the
//     server's records and adopted only if it reproduces the running
//     release exactly (`config home teploy --from-server`);
//   - "folder": back to folder home; the server config is archived and its
//     last content is returned so the operator can save it as teploy.yml.
func (s *Server) handleConfigHome(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeature(w, r, func(f machineFeatures) bool { return f.ConfigHome }, "Changing where settings are edited") {
		return
	}
	var body struct {
		Home string `json:"home"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	switch body.Home {
	case "teploy":
		args := append([]string{"config", "home", "teploy", "--from-server"}, s.requestActorArgs(r)...)
		res, err := s.cliAppRun(r.Context(), serverName, appName, args...)
		if err != nil {
			writeErrorStatus(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		s.fleet.invalidate()
		writeData(w, map[string]string{"output": strings.TrimSpace(res.Stdout)})
	case "folder":
		dir, err := os.MkdirTemp("", "teploy-dash-home-*")
		if err != nil {
			writeError(w, err.Error())
			return
		}
		defer os.RemoveAll(dir)
		file := filepath.Join(dir, "teploy.yml")
		args := append([]string{"config", "home", "folder", "--file", file}, s.requestActorArgs(r)...)
		res, err := s.cliAppRun(r.Context(), serverName, appName, args...)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		s.fleet.invalidate()
		content, _ := os.ReadFile(file)
		writeData(w, map[string]string{"output": strings.TrimSpace(res.Stdout), "teploy_yml": string(content)})
	default:
		writeError(w, fmt.Sprintf("home must be teploy or folder, got %q", body.Home))
	}
}
