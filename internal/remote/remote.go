package remote

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/state"
	uissh "github.com/useteploy/teploy-dash/internal/ssh"
)

const deploymentsDir = "/deployments"

// shellQuote single-quotes s for safe interpolation into a remote /bin/sh
// command, escaping any embedded single quotes. Defense-in-depth: HTTP handlers
// already reject non-identifier app/server names, but a value must never reach a
// remote shell unquoted.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// AppState is the state of a deployed app on a remote server.
type AppState struct {
	App          string    `json:"app"`
	Server       string    `json:"server"`
	Domain       string    `json:"domain"`
	CurrentHash  string    `json:"current_hash"`
	PreviousHash string    `json:"previous_hash"`
	CurrentPort  int       `json:"port"`
	Status       string    `json:"status"` // "running", "stopped", "unknown"
	DeployedAt   time.Time `json:"deployed_at"`
}

// ServerConn holds connection details for a server.
type ServerConn struct {
	Name    string
	Host    string
	User    string
	KeyPath string
}

// ListApps SSHes to a server and returns all deployed app states.
func ListApps(ctx context.Context, srv ServerConn) ([]AppState, error) {
	c, err := uissh.Connect(ctx, srv.Host, srv.User, srv.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", srv.Name, err)
	}
	defer c.Close()

	// Get list of app directories.
	out, err := c.Run(ctx, fmt.Sprintf("ls -1 %s 2>/dev/null", deploymentsDir))
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, nil
	}

	var apps []AppState
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || name == "teploy.log" {
			continue
		}

		state, err := readAppState(ctx, c, srv.Name, name)
		if err != nil || state == nil {
			continue
		}
		apps = append(apps, *state)
	}

	return apps, nil
}

// readAppState reads the KEY=VALUE state file and docker status for one app.
func readAppState(ctx context.Context, c *uissh.Client, serverName, appName string) (*AppState, error) {
	statePath := fmt.Sprintf("%s/%s/state", deploymentsDir, appName)
	raw, err := c.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", statePath))
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	// Parse with the single canonical CLI-state parser (internal/state) so the
	// SSH fleet path and the local-disk path can't drift on key names.
	parsed, err := state.Parse(strings.NewReader(raw))
	if err != nil {
		return nil, nil
	}

	st := &AppState{
		App:          appName,
		Server:       serverName,
		Status:       "unknown",
		CurrentHash:  parsed.CurrentHash,
		PreviousHash: parsed.PreviousHash,
		CurrentPort:  parsed.Port,
		Domain:       parsed.Domain,
	}

	// Get state file mtime as deployed_at.
	ts, err := c.Run(ctx, fmt.Sprintf("stat -c %%Y %s 2>/dev/null", statePath))
	if err == nil && ts != "" {
		if unix, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64); err == nil {
			st.DeployedAt = time.Unix(unix, 0).UTC()
		}
	}

	// Check live container status.
	if st.CurrentHash != "" {
		containerFilter := fmt.Sprintf("name=%s-", appName)
		running, err := c.Run(ctx, fmt.Sprintf(
			"docker ps -q --filter %q 2>/dev/null | wc -l",
			containerFilter,
		))
		if err == nil {
			count, _ := strconv.Atoi(strings.TrimSpace(running))
			if count > 0 {
				st.Status = "running"
			} else {
				st.Status = "stopped"
			}
		}
	}

	return st, nil
}

// StopApp stops all containers for an app on a server.
func StopApp(ctx context.Context, srv ServerConn, appName string) error {
	return runDockerOp(ctx, srv, appName, "stop")
}

// StartApp starts all stopped containers for an app on a server.
func StartApp(ctx context.Context, srv ServerConn, appName string) error {
	return runDockerOp(ctx, srv, appName, "start")
}

// RestartApp restarts all containers for an app on a server.
func RestartApp(ctx context.Context, srv ServerConn, appName string) error {
	return runDockerOp(ctx, srv, appName, "restart")
}

func runDockerOp(ctx context.Context, srv ServerConn, appName, op string) error {
	c, err := uissh.Connect(ctx, srv.Host, srv.User, srv.KeyPath)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", srv.Name, err)
	}
	defer c.Close()

	// Find containers whose names start with the app name.
	ids, err := c.Run(ctx, fmt.Sprintf(
		"docker ps -aq --filter %s 2>/dev/null",
		shellQuote("name="+appName+"-"),
	))
	if err != nil || strings.TrimSpace(ids) == "" {
		return fmt.Errorf("no containers found for app %q on %s", appName, srv.Name)
	}

	idList := strings.Join(strings.Fields(strings.TrimSpace(ids)), " ")
	_, err = c.Run(ctx, fmt.Sprintf("docker %s %s", op, idList))
	return err
}

// StreamLogs streams docker logs for an app to w until ctx is cancelled.
func StreamLogs(ctx context.Context, srv ServerConn, appName string, w io.Writer) error {
	c, err := uissh.Connect(ctx, srv.Host, srv.User, srv.KeyPath)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", srv.Name, err)
	}
	defer c.Close()

	// Get the name of the current (most recently started) web container.
	container, err := c.Run(ctx, fmt.Sprintf(
		"docker ps --filter %s --format '{{.Names}}' | head -1 2>/dev/null",
		shellQuote("name="+appName+"-web-"),
	))
	if err != nil || strings.TrimSpace(container) == "" {
		return fmt.Errorf("no running container found for app %q", appName)
	}

	return c.Stream(ctx, fmt.Sprintf("docker logs -f --tail=200 %s", strings.TrimSpace(container)), w)
}
