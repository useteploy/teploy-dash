package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/operation"
)

// The CLI list is authoritative for access, expiry and lifecycle. Operation
// history preserves the immutable head and failure trail after target cleanup.
func (s *Server) handleSourcePreviews(w http.ResponseWriter, r *http.Request, id string) {
	if !s.operationsAvailableFor(w, http.MethodGet) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	src, e := s.sources.Get(id)
	if e != nil {
		writeSourceError(w, e)
		return
	}
	bound, e := s.previewTargets(src)
	if e != nil {
		writeErrorStatus(w, "manifest binding unavailable", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	views := []map[string]any{}
	for _, m := range bound {
		dir, e := os.MkdirTemp("", "teploy-preview-status-")
		if e != nil {
			writeError(w, "preview status workspace unavailable")
			return
		}
		e = s.previewTargetManifest(dir, src, m.Server, m.App)
		if e != nil {
			os.RemoveAll(dir)
			writeError(w, e.Error())
			return
		}
		srv, e := s.lookupServerStrict(ctx, m.Server)
		if e != nil {
			os.RemoveAll(dir)
			writeErrorStatus(w, "preview server unavailable", 502)
			return
		}
		args := []string{"--project-dir", dir, "preview", "list", "--json", "--host", srv.Host}
		if srv.User != "" {
			args = append(args, "--user", srv.User)
		}
		result, e := s.runCLI(ctx, args...)
		os.RemoveAll(dir)
		if e != nil || result == nil || result.ExitCode != 0 {
			writeErrorStatus(w, "CLI preview status unavailable", 502)
			return
		}
		var rows []map[string]any
		if json.Unmarshal([]byte(result.Stdout), &rows) != nil {
			writeErrorStatus(w, "CLI preview status invalid", 502)
			return
		}
		for _, row := range rows {
			branch, _ := row["branch"].(string)
			if strings.HasPrefix(branch, "dash-"+id+"-pr-") {
				views = append(views, map[string]any{"server": m.Server, "app": m.App, "preview": row})
			}
		}
	}
	history := []*operation.Operation{}
	for _, op := range s.operations.List("", "", 0) {
		if op.Request.SourceID == id && op.Request.SourcePullRequest > 0 {
			history = append(history, op)
		}
	}
	writeData(w, map[string]any{"live": views, "operations": history, "review_base": fmt.Sprintf("%s/pull/", src.CloneURL)})
}

func (s *Server) sourceDeletionReady(ctx context.Context, id string) error {
	if s.operations != nil {
		for _, op := range s.operations.List("", "", 0) {
			if op.Request.SourceID == id && !op.Status.Terminal() {
				return fmt.Errorf("source has active operations; wait for cleanup before deletion")
			}
		}
	}
	src, e := s.sources.Get(id)
	if e != nil {
		return e
	}
	state, e := s.sources.Lifecycle(src.ID)
	if e != nil {
		return fmt.Errorf("preview ownership unavailable; source deletion refused")
	}
	if state.LegacyAuditRequired {
		return fmt.Errorf("source predates durable preview ownership; former-target audit and fenced reconciliation required before deletion")
	}
	if len(state.Previews) > 0 {
		return fmt.Errorf("source retains preview ownership; fenced remote cleanup and absence reconciliation required before deletion")
	}
	return nil
}
