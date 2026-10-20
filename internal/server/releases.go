package server

// releases.go — plan D5: the app's release history with rollback-to and the
// redeploy action. Dash stores none of it: the list is `teploy releases
// --app X --host S --json` (releases-json-v1), a rollback to a chosen
// release is the durable rollback operation with --to (rollback-json-v1),
// and redeploy is its own operation kind (redeploy-v1). Every mutation goes
// through internal/operation; nothing mutates inline.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/useteploy/teploy-dash/internal/operation"
)

// handleReleasesList serves GET /api/apps/{server}/{app}/releases: the CLI's
// releases envelope (newest first, each row marked current and
// rollback-able).
func (s *Server) handleReleasesList(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Releases }, "Releases", "v0.1.45") {
		return
	}
	res, err := s.cliAppRun(r.Context(), serverName, appName, "releases", "--json")
	if err != nil {
		writeError(w, err.Error())
		return
	}
	writeRawJSON(w, res.Stdout)
}

// decodeRollbackToBody reads the optional {to} body of a rollback POST. The
// verb predates the field (rollback-to is rollback-json-v1), so an empty body
// still means "the previous release" exactly as before. Unknown fields and a
// second JSON value are refused, like strictDecode.
func decodeRollbackToBody(r *http.Request) (string, error) {
	var body struct {
		To string `json:"to"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		if err == io.EOF {
			return "", nil // no body: the classic previous-release rollback
		}
		return "", err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return "", errors.New("request body must contain exactly one JSON object")
		}
		return "", err
	}
	return body.To, nil
}

// handleRedeploy serves POST /api/apps/{server}/{app}/redeploy: a durable
// redeploy of the current release with env re-resolved (the operation that
// makes a secret or env edit reach containers).
func (s *Server) handleRedeploy(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Redeploy }, "Redeploy", "v0.1.45") {
		return
	}
	s.enqueueOperation(w, r, operation.Request{Kind: operation.KindRedeploy, Server: serverName, App: appName})
}
