package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/useteploy/teploy-dash/internal/manifest"
	"github.com/useteploy/teploy-dash/internal/operation"
)

type manifestUpdateRequest struct {
	Mode             manifest.Mode          `json:"mode"`
	Git              *manifest.GitReference `json:"git,omitempty"`
	Manifest         string                 `json:"manifest"`
	ExpectedRevision *string                `json:"expected_revision,omitempty"`
}

func (s *Server) handleManifests(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !s.manifestsAvailable(w) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	items, err := s.manifests.List()
	if err != nil {
		writeManifestError(w, err)
		return
	}
	writeData(w, items)
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !s.manifestsAvailable(w) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/manifests/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || !manifest.ValidIdentifier(parts[0]) || !manifest.ValidIdentifier(parts[1]) {
		writeErrorStatus(w, "invalid manifest server or app", http.StatusBadRequest)
		return
	}
	server, app := parts[0], parts[1]
	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet:
			document, err := s.manifests.Get(server, app)
			if err != nil {
				writeManifestError(w, err)
				return
			}
			w.Header().Set("ETag", quoteETag(document.CurrentRevision))
			writeData(w, document)
		case http.MethodPut:
			s.putManifest(w, r, server, app)
		case http.MethodDelete:
			expected, err := expectedRevision(r, nil)
			if err != nil {
				writeErrorStatus(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := s.manifests.Delete(server, app, expected); err != nil {
				writeManifestError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	if len(parts) == 3 {
		switch parts[2] {
		case "history":
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			history, err := s.manifests.History(server, app)
			if err != nil {
				writeManifestError(w, err)
				return
			}
			writeData(w, history)
		case "export":
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.exportManifest(w, server, app, "")
		case "apply":
			s.enqueueManifestOperationRoute(w, r, server, app, operation.KindManifestApply)
		case "plan":
			s.enqueueManifestOperationRoute(w, r, server, app, operation.KindManifestPlan)
		case "validate":
			s.enqueueManifestOperationRoute(w, r, server, app, operation.KindManifestValidate)
		default:
			writeErrorStatus(w, "manifest route not found", http.StatusNotFound)
		}
		return
	}
	if len(parts) == 4 && parts[2] == "revisions" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.exportManifest(w, server, app, parts[3])
		return
	}
	writeErrorStatus(w, "manifest route not found", http.StatusNotFound)
}

func (s *Server) putManifest(w http.ResponseWriter, r *http.Request, server, app string) {
	var request manifestUpdateRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeErrorStatus(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeErrorStatus(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeErrorStatus(w, "invalid request body", http.StatusBadRequest)
		return
	}
	expected, err := expectedRevision(r, request.ExpectedRevision)
	if err != nil {
		writeErrorStatus(w, err.Error(), http.StatusBadRequest)
		return
	}
	document, created, err := s.manifests.Put(server, app, manifest.Update{
		Mode: request.Mode, Git: request.Git, Manifest: request.Manifest, ExpectedRevision: expected,
	})
	if err != nil {
		writeManifestError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", quoteETag(document.CurrentRevision))
	w.Header().Set("Location", "/api/manifests/"+url.PathEscape(server)+"/"+url.PathEscape(app))
	if created {
		w.WriteHeader(http.StatusCreated)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"data": document})
}

func (s *Server) enqueueManifestOperationRoute(w http.ResponseWriter, r *http.Request, server, app string, kind operation.Kind) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.enqueueManifestOperation(w, r, server, app, kind)
}

func (s *Server) exportManifest(w http.ResponseWriter, server, app, revision string) {
	var content []byte
	if revision == "" {
		document, err := s.manifests.Get(server, app)
		if err != nil {
			writeManifestError(w, err)
			return
		}
		revision = document.CurrentRevision
		content = []byte(document.Manifest)
	} else {
		var err error
		content, err = s.manifests.Export(server, app, revision)
		if err != nil {
			writeManifestError(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, app+"-teploy.yml"))
	w.Header().Set("ETag", quoteETag(revision))
	w.Write(content)
}

func (s *Server) enqueueManifestOperation(w http.ResponseWriter, r *http.Request, server, app string, kind operation.Kind) {
	if !s.operationsAvailable(w) {
		return
	}
	document, err := s.manifests.Get(server, app)
	if err != nil {
		writeManifestError(w, err)
		return
	}
	expected, err := expectedRevision(r, nil)
	if err != nil {
		writeErrorStatus(w, err.Error(), http.StatusBadRequest)
		return
	}
	if expected != nil {
		if *expected != document.CurrentRevision {
			writeErrorStatus(w, "manifest revision conflict", http.StatusConflict)
			return
		}
	}
	s.enqueueOperation(w, r, operation.Request{
		Kind: kind, Server: server, App: app, Mode: string(document.Mode), ManifestRevision: document.CurrentRevision,
	})
}

func expectedRevision(r *http.Request, body *string) (*string, error) {
	header := strings.TrimSpace(r.Header.Get("If-Match"))
	if header != "" {
		if strings.HasPrefix(header, "W/") || strings.Contains(header, ",") {
			return nil, fmt.Errorf("If-Match must contain one strong revision ETag")
		}
		header = strings.Trim(header, `"`)
		if header != "" && !manifest.ValidRevision(header) {
			return nil, fmt.Errorf("invalid expected revision")
		}
		if body != nil && *body != header {
			return nil, fmt.Errorf("If-Match and expected_revision disagree")
		}
		return &header, nil
	}
	if body != nil && *body != "" && !manifest.ValidRevision(*body) {
		return nil, fmt.Errorf("invalid expected revision")
	}
	if body != nil {
		copy := *body
		return &copy, nil
	}
	if query := r.URL.Query().Get("expected_revision"); query != "" {
		if !manifest.ValidRevision(query) {
			return nil, fmt.Errorf("invalid expected revision")
		}
		return &query, nil
	}
	return nil, nil
}

func quoteETag(revision string) string {
	return `"` + revision + `"`
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request body must contain one JSON object")
	}
	return nil
}

func (s *Server) manifestsAvailable(w http.ResponseWriter) bool {
	if s.manifests != nil {
		return true
	}
	message := "manifest service unavailable"
	if s.manifestInitErr != nil {
		message += ": " + s.manifestInitErr.Error()
	}
	writeErrorStatus(w, message, http.StatusServiceUnavailable)
	return false
}

func writeManifestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manifest.ErrNotFound), errors.Is(err, manifest.ErrRevisionNotFound):
		writeErrorStatus(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, manifest.ErrConflict):
		writeErrorStatus(w, err.Error(), http.StatusConflict)
	case errors.Is(err, manifest.ErrExpectedRevision):
		writeErrorStatus(w, err.Error(), http.StatusPreconditionRequired)
	case errors.Is(err, manifest.ErrSecrets):
		writeErrorStatus(w, manifest.ErrSecrets.Error(), http.StatusUnprocessableEntity)
	default:
		writeError(w, err.Error())
	}
}
