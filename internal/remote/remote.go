package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	uissh "github.com/useteploy/teploy-dash/internal/ssh"
	"github.com/useteploy/teploy-dash/internal/state"
)

const deploymentsDir = "/deployments"

// shellQuote single-quotes s for safe interpolation into a remote /bin/sh
// command, escaping any embedded single quotes. Defense-in-depth: HTTP handlers
// already reject non-identifier app/server names, but a value must never reach a
// remote shell unquoted.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// validAppName matches the deployment app-name charset (^[A-Za-z0-9._-]+$).
var validAppNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validAppName(s string) bool { return s != "." && s != ".." && validAppNameRe.MatchString(s) }

// AppState is the state of a deployed app on a remote server.
type AppState struct {
	App           string             `json:"app"`
	Server        string             `json:"server"`
	Domain        string             `json:"domain"`
	Type          string             `json:"type,omitempty"`
	Ingress       string             `json:"ingress,omitempty"`
	CurrentHash   string             `json:"current_hash"`
	PreviousHash  string             `json:"previous_hash"`
	CurrentPort   int                `json:"port"`
	CurrentPorts  []int              `json:"current_ports,omitempty"`
	PreviousPorts []int              `json:"previous_ports,omitempty"`
	Status        string             `json:"status"` // "running", "stopped", "unknown"
	DeployedAt    time.Time          `json:"deployed_at"`
	Containers    []ContainerInfo    `json:"containers"`
	Processes     []ProcessInfo      `json:"processes,omitempty"`
	Locked        bool               `json:"locked"`
	Maintenance   bool               `json:"maintenance"`
	ObservedAt    time.Time          `json:"observed_at,omitempty"`
	Errors        []ObservationError `json:"errors"`
	Source        string             `json:"source,omitempty"`
}

// ObservationError is a partial machine-read failure scoped to one resource.
type ObservationError struct {
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

// ProcessInfo summarizes one process type reported by the CLI machine API.
type ProcessInfo struct {
	Name       string   `json:"name"`
	Replicas   int      `json:"replicas"`
	Running    int      `json:"running"`
	Containers []string `json:"containers"`
}

// MarshalJSON also emits a "name" field mirroring App. The frontend keys and
// filters the app list on `.name`; without this the deployments list renders
// empty (undefined keys crash Alpine's x-for). Emitting both keeps the API
// backward-compatible while the UI expects `name`.
func (a AppState) MarshalJSON() ([]byte, error) {
	type alias AppState
	return json.Marshal(struct {
		alias
		Name string `json:"name"`
	}{alias(a), a.App})
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
		// Ignore directory names that aren't valid app names — defense in depth
		// so a non-conforming name from `ls` can never reach a shell command.
		if !validAppName(name) {
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
	raw, err := c.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", shellQuote(statePath)))
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
		Containers:   []ContainerInfo{},
	}

	// Get state file mtime as deployed_at.
	ts, err := c.Run(ctx, fmt.Sprintf("stat -c %%Y %s 2>/dev/null", shellQuote(statePath)))
	if err == nil && ts != "" {
		if unix, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64); err == nil {
			st.DeployedAt = time.Unix(unix, 0).UTC()
		}
	}

	// Read the app's web containers and operational flags in one SSH round trip.
	// Accessories are exposed separately and intentionally excluded here.
	live, liveErr := c.Run(ctx, fmt.Sprintf(
		"docker ps -a --filter %s --format '{{.ID}}|{{.Names}}|{{.Image}}|{{.State}}|{{.Status}}' 2>/dev/null; "+
			"echo '@@LOCK@@'; test -f %s && echo true || echo false; "+
			"echo '@@MAINT@@'; test -f %s && echo true || echo false",
		shellQuote("name="+appName+"-web-"),
		shellQuote(fmt.Sprintf("%s/%s/.lock/info", deploymentsDir, appName)),
		shellQuote(fmt.Sprintf("%s/%s/.maintenance-block", deploymentsDir, appName)),
	))
	if liveErr == nil {
		applyLiveState(st, live)
	}

	// Check live status. A container deploy records a port; a static-file
	// deploy has port 0 and is served by Caddy directly, with no container to
	// inspect. Stopped container apps keep their recorded port, so port==0 with
	// a hash reliably means "static", not "container that lost its port".
	if st.CurrentHash != "" {
		if st.CurrentPort == 0 {
			// Static site: no container exists by design, so a container check
			// would always read "stopped". A deployed static site is live via
			// Caddy — report it running rather than falsely down.
			st.Status = "running"
		} else if liveErr == nil {
			st.Status = "stopped"
			for _, container := range st.Containers {
				if container.State == "running" {
					st.Status = "running"
					break
				}
			}
		}
	}

	return st, nil
}

func applyLiveState(st *AppState, live string) {
	section := "containers"
	for _, line := range strings.Split(live, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line {
		case "@@LOCK@@":
			section = "lock"
			continue
		case "@@MAINT@@":
			section = "maintenance"
			continue
		}
		switch section {
		case "containers":
			p := strings.SplitN(line, "|", 5)
			if len(p) == 5 {
				st.Containers = append(st.Containers, ContainerInfo{ID: p[0], Name: p[1], Image: p[2], State: p[3], Status: p[4]})
			}
		case "lock":
			st.Locked = line == "true"
		case "maintenance":
			st.Maintenance = line == "true"
		}
	}
}

// ServerStatus is host-level status for the Servers detail page.
type ServerStatus struct {
	Name        string             `json:"name"`
	Host        string             `json:"host"`
	Uptime      string             `json:"uptime"`
	CPULoad     string             `json:"cpu_load"`
	MemUsed     string             `json:"mem_used"`
	MemTotal    string             `json:"mem_total"`
	MemPercent  string             `json:"mem_percent"`
	DiskUsed    string             `json:"disk_used"`
	DiskTotal   string             `json:"disk_total"`
	DiskPercent string             `json:"disk_percent"`
	Containers  []ContainerInfo    `json:"containers"`
	ObservedAt  time.Time          `json:"observed_at,omitempty"`
	Errors      []ObservationError `json:"errors"`
	Partial     bool               `json:"partial"`
	Stale       bool               `json:"stale"`
	Source      string             `json:"source,omitempty"`
}

// ContainerInfo is one container row on the server-detail page.
type ContainerInfo struct {
	ID        string `json:"ID"`
	Name      string `json:"Name"`
	Image     string `json:"Image"`
	State     string `json:"State"`
	Status    string `json:"Status"`
	CreatedAt string `json:"CreatedAt,omitempty"`
	Process   string `json:"Process,omitempty"`
	Version   string `json:"Version,omitempty"`
}

// GetServerStatus SSHes to a server and gathers host-level status (uptime,
// load, memory, disk, running containers). The dash CLI's `status` command
// reports app state, not host metrics, so the Servers detail page gathers them
// directly here.
func GetServerStatus(ctx context.Context, srv ServerConn) (*ServerStatus, error) {
	c, err := uissh.Connect(ctx, srv.Host, srv.User, srv.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", srv.Name, err)
	}
	defer c.Close()

	st := &ServerStatus{Name: srv.Name, Host: srv.Host, Containers: []ContainerInfo{}}

	// One round-trip, delimited sections.
	out, err := c.Run(ctx, "uptime; echo '@@MEM@@'; free -m; echo '@@DISK@@'; df -h /; "+
		"echo '@@DOCKER@@'; docker ps -a --format '{{.ID}}|{{.Names}}|{{.Image}}|{{.State}}|{{.Status}}' 2>/dev/null")
	if err != nil {
		return nil, fmt.Errorf("gathering status for %s: %w", srv.Name, err)
	}

	section := "uptime"
	for _, line := range strings.Split(out, "\n") {
		switch strings.TrimSpace(line) {
		case "@@MEM@@":
			section = "mem"
			continue
		case "@@DISK@@":
			section = "disk"
			continue
		case "@@DOCKER@@":
			section = "docker"
			continue
		}
		switch section {
		case "uptime":
			if line == "" {
				continue
			}
			// " ... up 12 days,  3:22,  2 users,  load average: 0.1, 0.2, 0.3"
			if i := strings.Index(line, "load average:"); i >= 0 {
				st.CPULoad = strings.TrimSpace(line[i+len("load average:"):])
			}
			if i := strings.Index(line, " up "); i >= 0 {
				rest := line[i+4:]
				if j := strings.Index(rest, " user"); j >= 0 {
					// Trim back to the last comma before "N users".
					if k := strings.LastIndex(rest[:j], ","); k >= 0 {
						rest = rest[:k]
					}
				}
				st.Uptime = strings.TrimSpace(rest)
			}
		case "mem":
			// "Mem:  7976  3200  ..." (total used, in MB)
			f := strings.Fields(line)
			if len(f) >= 3 && strings.HasPrefix(f[0], "Mem") {
				total, _ := strconv.Atoi(f[1])
				used, _ := strconv.Atoi(f[2])
				st.MemTotal = fmtGiB(total)
				st.MemUsed = fmtGiB(used)
				if total > 0 {
					st.MemPercent = fmt.Sprintf("%d%%", used*100/total)
				}
			}
		case "disk":
			// "/dev/sda1  40G  12G  26G  32%  /"
			f := strings.Fields(line)
			if len(f) >= 5 && strings.HasSuffix(f[4], "%") {
				st.DiskTotal = f[1]
				st.DiskUsed = f[2]
				st.DiskPercent = f[4]
			}
		case "docker":
			if strings.TrimSpace(line) == "" {
				continue
			}
			p := strings.SplitN(line, "|", 5)
			if len(p) == 5 {
				st.Containers = append(st.Containers, ContainerInfo{
					ID: p[0], Name: p[1], Image: p[2], State: p[3], Status: p[4],
				})
			}
		}
	}
	return st, nil
}

// fmtGiB renders a megabyte count as a one-decimal GiB string.
func fmtGiB(mb int) string {
	return fmt.Sprintf("%.1fG", float64(mb)/1024)
}

// StopApp stops all containers for an app on a server.
// Deprecated: mutations must go through the CLI operation manager. Retained
// temporarily for source compatibility; read fallbacks do not call it.
func StopApp(ctx context.Context, srv ServerConn, appName string) error {
	return runDockerOp(ctx, srv, appName, "stop")
}

// StartApp starts all stopped containers for an app on a server.
// Deprecated: mutations must go through the CLI operation manager.
func StartApp(ctx context.Context, srv ServerConn, appName string) error {
	return runDockerOp(ctx, srv, appName, "start")
}

// RestartApp restarts all containers for an app on a server.
// Deprecated: mutations must go through the CLI operation manager.
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
func StreamLogs(ctx context.Context, srv ServerConn, appName, process string, lines int, w io.Writer) error {
	c, err := uissh.Connect(ctx, srv.Host, srv.User, srv.KeyPath)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", srv.Name, err)
	}
	defer c.Close()

	if process == "" {
		process = "web"
	}
	if lines < 1 {
		lines = 200
	}

	// Get the current container for the selected process.
	container, err := c.Run(ctx, fmt.Sprintf(
		"docker ps --filter %s --format '{{.Names}}' | head -1 2>/dev/null",
		shellQuote("name="+appName+"-"+process+"-"),
	))
	if err != nil || strings.TrimSpace(container) == "" {
		return fmt.Errorf("no running container found for app %q", appName)
	}

	return c.Stream(ctx, fmt.Sprintf("docker logs -f --tail=%d %s", lines, shellQuote(strings.TrimSpace(container))), w)
}
