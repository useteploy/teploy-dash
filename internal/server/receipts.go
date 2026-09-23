package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/remote"
)

// D02 receipt slice. Dash cannot observe what a canceled or interrupted CLI
// command did on the target — "interrupted" describes the coordinator, not
// the remote. The receipt reader answers "did the effect land?" from the
// machine reads the fleet already uses (`teploy app list --host <host>
// --json`, machine.go), against the operation's ADMITTED target snapshot
// (A14) so a repointed alias can never make a receipt describe a different
// server. Where the read cannot answer for an operation kind, it returns an
// error — the caller records needs-manual-check with the exact reason, never
// a guess and never a silent "failed".

// operationReceipt is the manager's ReceiptReader (D02). It answers for the
// kinds whose effect `app list` can decide:
//
//   - deploy: the app must be present AND (when the request pinned an image)
//     run by a container reporting exactly that image. Presence without the
//     image is ambiguous (an older release may be running) and stays manual.
//   - remove: the app must be absent.
//
// Every other kind answers "no receipt reader for kind" today — recorded as
// needs-manual-check with this exact reason.
func (s *Server) operationReceipt(ctx context.Context, op *operation.Operation) (operation.Receipt, error) {
	if op.AdmittedServer == nil || op.AdmittedServer.Host == "" {
		return operation.Receipt{}, fmt.Errorf("operation record predates target pinning; the server identity is unknown — verify the server state manually")
	}
	conn := remote.ServerConn{
		Name: op.AdmittedServer.Name,
		Host: op.AdmittedServer.Host,
		User: op.AdmittedServer.User,
	}
	switch op.Request.Kind {
	case operation.KindDeploy:
		return deployReceipt(ctx, s, conn, op.Request.App, op.Request.Image)
	case operation.KindRemove:
		return removeReceipt(ctx, s, conn, op.Request.App)
	default:
		return operation.Receipt{}, fmt.Errorf("no receipt reader for operation kind %q; verify the server state manually", op.Request.Kind)
	}
}

func deployReceipt(ctx context.Context, s *Server, conn remote.ServerConn, app, image string) (operation.Receipt, error) {
	state, err := findApp(ctx, s, conn, app)
	if err != nil {
		return operation.Receipt{}, err
	}
	if state == nil {
		return operation.Receipt{
			Applied:  false,
			Evidence: fmt.Sprintf("app %q is absent from teploy app list on %s", app, conn.Host),
		}, nil
	}
	if image == "" {
		return operation.Receipt{}, fmt.Errorf("app %q is present on %s but the request pinned no image to verify against; verify the release manually", app, conn.Host)
	}
	for _, container := range state.Containers {
		if container.Image == image {
			return operation.Receipt{
				Applied: true,
				Evidence: fmt.Sprintf("app %q on %s runs image %s (current release %s)",
					app, conn.Host, image, releaseLabel(state.CurrentHash)),
			}, nil
		}
	}
	return operation.Receipt{}, fmt.Errorf("app %q is present on %s (current release %s) but no container reports the requested image %s",
		app, conn.Host, releaseLabel(state.CurrentHash), image)
}

func removeReceipt(ctx context.Context, s *Server, conn remote.ServerConn, app string) (operation.Receipt, error) {
	state, err := findApp(ctx, s, conn, app)
	if err != nil {
		return operation.Receipt{}, err
	}
	if state != nil {
		return operation.Receipt{
			Applied:  false,
			Evidence: fmt.Sprintf("app %q is still present in teploy app list on %s (current release %s)", app, conn.Host, releaseLabel(state.CurrentHash)),
		}, nil
	}
	return operation.Receipt{
		Applied:  true,
		Evidence: fmt.Sprintf("app %q is gone from teploy app list on %s", app, conn.Host),
	}, nil
}

// findApp reads the machine app list for the admitted target and returns the
// app's state, or nil when the app is not deployed there.
func findApp(ctx context.Context, s *Server, conn remote.ServerConn, app string) (*remote.AppState, error) {
	apps, err := s.readMachineApps(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("reading teploy app list on %s: %w", conn.Host, err)
	}
	for i := range apps {
		if apps[i].App == app {
			return &apps[i], nil
		}
	}
	return nil, nil
}

func releaseLabel(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "unknown"
	}
	return version
}
