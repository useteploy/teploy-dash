package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/source"
)

type sourcePreviewRow struct {
	Branch    string    `json:"branch"`
	ExpiresAt time.Time `json:"expires_at"`
}

func previewCommand(cmd operation.Command, dir, action, branch string) operation.Command {
	args := []string{"--project-dir", dir, "preview", action}
	if branch != "" {
		args = append(args, branch)
	} else {
		args = append(args, "--json")
	}
	for i, a := range cmd.Args {
		if (a == "--host" || a == "--user") && i+1 < len(cmd.Args) {
			args = append(args, a, cmd.Args[i+1])
		}
	}
	cmd.Args = args
	cmd.SourceRequest = nil
	return cmd
}
func (s *Server) executePreviewExpiry(ctx context.Context, src *source.Source, cmd operation.Command, emit func(operation.Stream, string), execute operation.Executor) (int, error) {
	req := cmd.SourceRequest
	p, e := s.sourceProviders.Policy(src)
	if e != nil || p.Preview == nil {
		return -1, fmt.Errorf("preview cleanup policy unavailable")
	}
	dir, e := os.MkdirTemp("", "teploy-preview-expiry-")
	if e != nil {
		return -1, fmt.Errorf("preview cleanup workspace unavailable")
	}
	defer os.RemoveAll(dir)
	if e = writePreviewManifest(dir, req.App, p.Preview); e != nil {
		return -1, e
	}
	var output strings.Builder
	code, e := execute(ctx, previewCommand(cmd, dir, "list", ""), func(stream operation.Stream, text string) {
		if stream == operation.StreamStdout {
			if output.Len()+len(text) < 1<<20 {
				output.WriteString(text)
				output.WriteByte('\n')
			}
		} else {
			emit(stream, text)
		}
	})
	if e != nil || code != 0 {
		return code, e
	}
	var rows []sourcePreviewRow
	if json.Unmarshal([]byte(output.String()), &rows) != nil {
		return -1, fmt.Errorf("CLI preview expiry receipt invalid")
	}
	branch := "dash-" + src.ID + "-pr-" + strconv.Itoa(req.SourcePullRequest)
	for _, row := range rows {
		if row.Branch == branch {
			// A queued expiry cannot delete a preview renewed by a newer deployment.
			if row.ExpiresAt.IsZero() || time.Now().Before(row.ExpiresAt) {
				return 0, nil
			}
			return -1, fmt.Errorf("preview expiry requires atomic CLI generation compare-and-destroy; ownership retained")
		}
	}
	return 0, nil
}
func (s *Server) startSourceMaintenance() {
	s.sourceMaintenanceOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.sourceMaintenanceCancel = cancel
		s.sourceMaintenanceDone = make(chan struct{})
		go func() {
			defer close(s.sourceMaintenanceDone)
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.queueExpiredSourcePreviews(ctx)
				}
			}
		}()
	})
}
func (s *Server) stopSourceMaintenance(ctx context.Context) {
	// Seal startup even when Stop arrives before Serve.
	s.sourceMaintenanceOnce.Do(func() {})
	if s.sourceMaintenanceCancel != nil {
		s.sourceMaintenanceCancel()
	}
	if s.sourceMaintenanceDone != nil {
		select {
		case <-s.sourceMaintenanceDone:
		case <-ctx.Done():
		}
	}
}
func (s *Server) queueExpiredSourcePreviews(ctx context.Context) {
	if s.sources == nil || s.manifests == nil || s.operations == nil {
		return
	}
	all, e := s.sources.List()
	if e != nil {
		log.Printf("[source] preview maintenance source read failed")
		return
	}
	for _, src := range all {
		if ctx.Err() != nil {
			return
		}
		bound, e := s.previewTargets(&src)
		if e != nil {
			log.Printf("[source] preview maintenance binding failed for %s", src.ID)
			continue
		}
		for _, m := range bound {
			probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			dir, e := os.MkdirTemp("", "teploy-preview-expiry-probe-")
			if e != nil {
				cancel()
				return
			}
			e = s.previewTargetManifest(dir, &src, m.Server, m.App)
			if e != nil {
				os.RemoveAll(dir)
				cancel()
				continue
			}
			srv, e := s.lookupServerStrict(probeCtx, m.Server)
			if e != nil {
				os.RemoveAll(dir)
				cancel()
				continue
			}
			args := []string{"--project-dir", dir, "preview", "list", "--json", "--host", srv.Host}
			if srv.User != "" {
				args = append(args, "--user", srv.User)
			}
			result, e := s.runCLI(probeCtx, args...)
			os.RemoveAll(dir)
			cancel()
			if e != nil || result == nil || result.ExitCode != 0 {
				log.Printf("[source] preview expiry probe failed for %s", src.ID)
				continue
			}
			var rows []sourcePreviewRow
			if json.Unmarshal([]byte(result.Stdout), &rows) != nil {
				log.Printf("[source] invalid preview expiry receipt for %s", src.ID)
				continue
			}
			for _, row := range rows {
				prefix := "dash-" + src.ID + "-pr-"
				if !strings.HasPrefix(row.Branch, prefix) || row.ExpiresAt.IsZero() || time.Now().Before(row.ExpiresAt) {
					continue
				}
				n, e := strconv.Atoi(strings.TrimPrefix(row.Branch, prefix))
				if e != nil || n <= 0 {
					continue
				}
				s.sourceActionMu.Lock()
				if _, err := s.sources.Get(src.ID); err != nil {
					s.sourceActionMu.Unlock()
					continue
				}
				op, _, admissionErr := s.operations.Enqueue(operation.Request{Kind: operation.KindSourcePreviewExpire, Server: m.Server, App: m.App, Mode: "git-managed", ManifestRevision: m.CurrentRevision, SourceID: src.ID, SourcePullRequest: n}, "expire:"+row.Branch+":"+row.ExpiresAt.UTC().Format(time.RFC3339), &operation.Actor{Kind: "system", Subject: "source/" + src.ID, Label: "Preview expiry"})
				e = admissionErr
				if e == nil && op != nil && (op.Status == operation.StatusFailed || op.Status == operation.StatusInterrupted || op.Status == operation.StatusCanceled) {
					_, e = s.operations.Retry(op.ID, &operation.Actor{Kind: "system", Subject: "source/" + src.ID, Label: "Preview expiry retry"})
				}
				s.sourceActionMu.Unlock()
				if e != nil {
					log.Printf("[source] preview expiry admission failed for %s", src.ID)
				}
			}
		}
	}
}
