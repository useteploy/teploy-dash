package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"github.com/useteploy/teploy-dash/internal/manifest"
	"github.com/useteploy/teploy-dash/internal/source"
)

func previewOwnershipKey(sourceID, serverID, app string, number int) string {
	sum := sha256.Sum256([]byte("dash-preview-ownership-v1\x00" + sourceID + "\x00" + serverID + "\x00" + app + "\x00" + strconv.Itoa(number)))
	return hex.EncodeToString(sum[:])
}
func (s *Server) retainPreviewOwnership(ctx context.Context, src *source.Source, m manifest.Metadata, n int) error {
	srv, err := s.operationsServer(m.Server)
	if err != nil || srv.ID == "" {
		return fmt.Errorf("preview requires stable server identity")
	}
	state, err := s.sources.Lifecycle(src.ID)
	if err != nil {
		return err
	}
	if state.LegacyAuditRequired {
		return fmt.Errorf("former-target preview census required before new admission")
	}
	key := previewOwnershipKey(src.ID, srv.ID, m.App, n)
	// Repeated creation may update policy only after prior remote cleanup. Until
	// the remote fence seam is shipped retain the original cleanup authority.
	if _, ok := state.Previews[key]; ok {
		return nil
	}
	p, err := s.sourceProviders.Policy(src)
	if err != nil || p.Preview == nil {
		return fmt.Errorf("preview policy unavailable")
	}
	dir, err := os.MkdirTemp("", "teploy-preview-admission-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err = writePreviewManifest(dir, m.App, p.Preview); err != nil {
		return err
	}
	b, err := os.ReadFile(dir + "/teploy.yml")
	if err != nil {
		return err
	}
	state.Previews[key] = source.PreviewOwnership{ID: key, ServerID: srv.ID, Server: m.Server, App: m.App, Pull: n, Branch: "dash-" + src.ID + "-pr-" + strconv.Itoa(n), Policy: *p.Preview, Manifest: string(b)}
	return s.sources.SaveLifecycle(src.ID, state)
}
func (s *Server) operationsServer(name string) (operationServer, error) {
	if s.config.OperationResolver != nil {
		srv, e := s.config.OperationResolver(name)
		return operationServer{ID: srv.ID, Name: srv.Name, Host: srv.Host, User: srv.User}, e
	}
	srv, e := s.resolveOperationServer(name)
	return operationServer{ID: srv.ID, Name: srv.Name, Host: srv.Host, User: srv.User}, e
}

type operationServer struct{ ID, Name, Host, User string }

// cleanupOwner resolves only the retained stable ID; aliases are never fallback.
func (s *Server) cleanupOwner(src *source.Source, server, app string, n int) (*source.PreviewOwnership, error) {
	state, e := s.sources.Lifecycle(src.ID)
	if e != nil {
		return nil, e
	}
	for _, owner := range state.Previews {
		if owner.App != app || owner.Pull != n {
			continue
		}
		srv, ok := s.resolveOperationServerByID(owner.ServerID)
		if s.config.OperationResolverByID != nil {
			x, found := s.config.OperationResolverByID(owner.ServerID)
			srv = x
			ok = found
		}
		if ok && srv.Name == server {
			return &owner, nil
		}
	}
	return nil, fmt.Errorf("retained preview target identity unavailable")
}

// previewTargets includes removed/rebound manifests. Unknown stable identities
// fail closed, so deleting a manifest cannot make an existing target disappear.
func (s *Server) previewTargets(src *source.Source) ([]manifest.Metadata, error) {
	state, e := s.sources.Lifecycle(src.ID)
	if e != nil {
		return nil, e
	}
	if state.LegacyAuditRequired {
		return nil, fmt.Errorf("source predates durable preview ownership; former-target audit required")
	}
	targets := []manifest.Metadata{}
	seen := map[string]bool{}
	for _, owner := range state.Previews {
		srv, ok := s.resolveOperationServerByID(owner.ServerID)
		if s.config.OperationResolverByID != nil {
			x, found := s.config.OperationResolverByID(owner.ServerID)
			srv = x
			ok = found
		}
		if !ok || srv.ID != owner.ServerID {
			return nil, fmt.Errorf("retained preview server unavailable")
		}
		key := owner.ServerID + "/" + owner.App
		if !seen[key] {
			targets = append(targets, manifest.Metadata{Server: srv.Name, App: owner.App, Mode: manifest.ModeGitManaged})
			seen[key] = true
		}
	}
	return targets, nil
}
func retainedPreviewManifest(dir string, owner *source.PreviewOwnership) error {
	path := dir + "/retained-policy.yml"
	if e := os.WriteFile(path, []byte(owner.Manifest), 0600); e != nil {
		return e
	}
	policy := owner.Policy
	policy.ManifestFile = path
	return writePreviewManifest(dir, owner.App, &policy)
}

func (s *Server) previewTargetManifest(dir string, src *source.Source, server, app string) error {
	state, err := s.sources.Lifecycle(src.ID)
	if err != nil {
		return err
	}
	for _, owner := range state.Previews {
		if owner.App != app {
			continue
		}
		actual, err := s.cleanupOwner(src, server, app, owner.Pull)
		if err == nil && actual.ID == owner.ID {
			return retainedPreviewManifest(dir, actual)
		}
	}
	return fmt.Errorf("retained preview policy unavailable")
}
