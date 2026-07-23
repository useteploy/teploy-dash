package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/remote"
)

const observationStaleAfter = 2 * time.Minute

type cliRunner func(context.Context, ...string) (*cli.Result, error)

type machineError struct {
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

type machineRelease struct {
	Version string `json:"version"`
	Ports   []int  `json:"ports"`
}

type machineContainer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Image     string `json:"image"`
	State     string `json:"state"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	Process   string `json:"process"`
	Version   string `json:"version"`
}

type machineProcess struct {
	Name       string   `json:"name"`
	Replicas   int      `json:"replicas"`
	Running    int      `json:"running"`
	Containers []string `json:"containers"`
}

type machineLock struct {
	Type    string `json:"type"`
	User    string `json:"user"`
	Message string `json:"message"`
	TS      string `json:"ts"`
}

type machineApp struct {
	App             string             `json:"app"`
	Domain          string             `json:"domain"`
	Type            string             `json:"type"`
	Ingress         string             `json:"ingress"`
	CurrentRelease  machineRelease     `json:"current_release"`
	PreviousRelease machineRelease     `json:"previous_release"`
	Containers      []machineContainer `json:"containers"`
	Processes       []machineProcess   `json:"processes"`
	Lock            *machineLock       `json:"lock"`
	Maintenance     bool               `json:"maintenance"`
	ObservedAt      time.Time          `json:"observed_at"`
	Errors          []machineError     `json:"errors"`
}

type machineAppList struct {
	Host       string         `json:"host"`
	Apps       []machineApp   `json:"apps"`
	ObservedAt time.Time      `json:"observed_at"`
	Errors     []machineError `json:"errors"`
}

type machineUptime struct {
	Seconds float64 `json:"seconds"`
}

type machineLoad struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

type machineMemory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

type machineDisk struct {
	Filesystem     string `json:"filesystem"`
	Mountpoint     string `json:"mountpoint"`
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	UsedPercent    string `json:"used_percent"`
}

type machineDocker struct {
	Installed  bool               `json:"installed"`
	Version    string             `json:"version"`
	Containers []machineContainer `json:"containers"`
	Images     []json.RawMessage  `json:"images"`
}

type machineCaddyRoute struct {
	Server     string   `json:"server"`
	ID         string   `json:"id"`
	Hosts      []string `json:"hosts"`
	Handlers   []string `json:"handlers"`
	Upstreams  []string `json:"upstreams"`
	StatusCode string   `json:"status_code"`
}

type machineCaddy struct {
	Available bool                `json:"available"`
	Routes    []machineCaddyRoute `json:"routes"`
}

type machineServerStatus struct {
	Server     string         `json:"server"`
	Host       string         `json:"host"`
	Uptime     machineUptime  `json:"uptime"`
	Load       machineLoad    `json:"load"`
	Memory     machineMemory  `json:"memory"`
	Disks      []machineDisk  `json:"disks"`
	Docker     machineDocker  `json:"docker"`
	Caddy      machineCaddy   `json:"caddy"`
	ObservedAt time.Time      `json:"observed_at"`
	Errors     []machineError `json:"errors"`
}

type proxyRoute struct {
	ID          string   `json:"id"`
	Domains     []string `json:"domains"`
	Upstreams   []string `json:"upstreams"`
	Handler     string   `json:"handler"`
	StatusCode  string   `json:"status_code,omitempty"`
	Maintenance bool     `json:"maintenance"`
}

type proxyStatus struct {
	Running    bool                      `json:"running"`
	Routes     []proxyRoute              `json:"routes"`
	ObservedAt time.Time                 `json:"observed_at"`
	Errors     []remote.ObservationError `json:"errors"`
	Partial    bool                      `json:"partial"`
	Stale      bool                      `json:"stale"`
	Source     string                    `json:"source"`
}

func (s *Server) readMachineApps(ctx context.Context, srv remote.ServerConn) ([]remote.AppState, error) {
	args := []string{"app", "list", "--host", srv.Host, "--json"}
	if srv.User != "" && srv.User != "root" {
		args = append(args, "--user", srv.User)
	}
	result, err := s.runCLI(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("running teploy app list for %s: %w", srv.Name, err)
	}
	if result.ExitCode != 0 {
		if cli.UnsupportedCommand(result) {
			apps, err := s.remoteListApps(ctx, srv)
			if err != nil {
				return nil, err
			}
			now := time.Now().UTC()
			for i := range apps {
				apps[i].ObservedAt = now
				apps[i].Errors = []remote.ObservationError{}
				apps[i].Source = "ssh_fallback"
				if apps[i].CurrentPorts == nil && apps[i].CurrentPort > 0 {
					apps[i].CurrentPorts = []int{apps[i].CurrentPort}
				}
			}
			return apps, nil
		}
		return nil, commandFailure(args, result)
	}

	var list machineAppList
	if err := json.Unmarshal([]byte(result.Stdout), &list); err != nil {
		return nil, fmt.Errorf("decoding teploy app list for %s: %w", srv.Name, err)
	}
	if list.Host == "" || list.ObservedAt.IsZero() || list.Apps == nil || list.Errors == nil {
		return nil, fmt.Errorf("decoding teploy app list for %s: incomplete machine response", srv.Name)
	}
	for _, partialErr := range list.Errors {
		logMachineError("fleet", srv.Name, partialErr)
	}

	apps := make([]remote.AppState, 0, len(list.Apps))
	for _, app := range list.Apps {
		if app.App == "" || app.ObservedAt.IsZero() || app.CurrentRelease.Ports == nil || app.PreviousRelease.Ports == nil || app.Containers == nil || app.Processes == nil || app.Errors == nil {
			return nil, fmt.Errorf("decoding teploy app list for %s: incomplete machine response for app %q", srv.Name, app.App)
		}
		apps = append(apps, mapMachineApp(srv.Name, list.ObservedAt, app))
	}
	return apps, nil
}

func mapMachineApp(server string, listObservedAt time.Time, app machineApp) remote.AppState {
	observedAt := app.ObservedAt
	if observedAt.IsZero() {
		observedAt = listObservedAt
	}
	state := remote.AppState{
		App:           app.App,
		Server:        server,
		Domain:        app.Domain,
		Type:          app.Type,
		Ingress:       app.Ingress,
		CurrentHash:   app.CurrentRelease.Version,
		PreviousHash:  app.PreviousRelease.Version,
		CurrentPorts:  nonNilInts(app.CurrentRelease.Ports),
		PreviousPorts: nonNilInts(app.PreviousRelease.Ports),
		Status:        "unknown",
		Containers:    make([]remote.ContainerInfo, 0, len(app.Containers)),
		Processes:     make([]remote.ProcessInfo, 0, len(app.Processes)),
		Locked:        app.Lock != nil,
		Maintenance:   app.Maintenance,
		ObservedAt:    observedAt,
		Errors:        mapMachineErrors(app.Errors),
		Source:        "cli",
	}
	if len(state.CurrentPorts) > 0 {
		state.CurrentPort = state.CurrentPorts[0]
	}
	for _, container := range app.Containers {
		state.Containers = append(state.Containers, mapMachineContainer(container))
		if container.State == "running" {
			state.Status = "running"
		}
	}
	for _, process := range app.Processes {
		state.Processes = append(state.Processes, remote.ProcessInfo{
			Name: process.Name, Replicas: process.Replicas, Running: process.Running,
			Containers: nonNilStrings(process.Containers),
		})
	}
	if state.Status == "unknown" && state.CurrentHash != "" {
		if state.Type == "static" {
			state.Status = "running"
		} else {
			state.Status = "stopped"
		}
	}
	return state
}

func (s *Server) readMachineServer(ctx context.Context, serverName string) (*machineServerStatus, bool, error) {
	args := []string{"server", "status", serverName, "--json"}
	result, err := s.runCLI(ctx, args...)
	if err != nil {
		return nil, false, fmt.Errorf("running teploy server status for %s: %w", serverName, err)
	}
	if result.ExitCode != 0 {
		if cli.UnsupportedCommand(result) {
			return nil, true, nil
		}
		return nil, false, commandFailure(args, result)
	}
	var status machineServerStatus
	if err := json.Unmarshal([]byte(result.Stdout), &status); err != nil {
		return nil, false, fmt.Errorf("decoding teploy server status for %s: %w", serverName, err)
	}
	if status.Host == "" || status.ObservedAt.IsZero() || status.Disks == nil || status.Docker.Containers == nil || status.Docker.Images == nil || status.Caddy.Routes == nil || status.Errors == nil {
		return nil, false, fmt.Errorf("decoding teploy server status for %s: incomplete machine response", serverName)
	}
	return &status, false, nil
}

func mapMachineServer(status *machineServerStatus, requestedName string, now time.Time) *remote.ServerStatus {
	name := status.Server
	if name == "" {
		name = requestedName
	}
	result := &remote.ServerStatus{
		Name:       name,
		Host:       status.Host,
		Uptime:     formatDuration(status.Uptime.Seconds),
		CPULoad:    fmt.Sprintf("%.2f, %.2f, %.2f", status.Load.One, status.Load.Five, status.Load.Fifteen),
		MemUsed:    formatBytes(status.Memory.UsedBytes),
		MemTotal:   formatBytes(status.Memory.TotalBytes),
		MemPercent: percent(status.Memory.UsedBytes, status.Memory.TotalBytes),
		Containers: make([]remote.ContainerInfo, 0, len(status.Docker.Containers)),
		ObservedAt: status.ObservedAt,
		Errors:     mapMachineErrors(status.Errors),
		Partial:    len(status.Errors) > 0,
		Stale:      observationStale(status.ObservedAt, now),
		Source:     "cli",
	}
	if disk := primaryDisk(status.Disks); disk != nil {
		result.DiskUsed = formatBytes(disk.UsedBytes)
		result.DiskTotal = formatBytes(disk.TotalBytes)
		result.DiskPercent = disk.UsedPercent
	}
	for _, container := range status.Docker.Containers {
		result.Containers = append(result.Containers, mapMachineContainer(container))
	}
	return result
}

func mapMachineProxy(status *machineServerStatus, now time.Time) proxyStatus {
	routes := make([]proxyRoute, 0, len(status.Caddy.Routes))
	for _, route := range status.Caddy.Routes {
		routes = append(routes, proxyRoute{
			ID:          route.ID,
			Domains:     nonNilStrings(route.Hosts),
			Upstreams:   nonNilStrings(route.Upstreams),
			Handler:     strings.Join(route.Handlers, ", "),
			StatusCode:  route.StatusCode,
			Maintenance: route.StatusCode == "503",
		})
	}
	return proxyStatus{
		Running:    status.Caddy.Available,
		Routes:     routes,
		ObservedAt: status.ObservedAt,
		Errors:     mapMachineErrors(status.Errors),
		Partial:    len(status.Errors) > 0,
		Stale:      observationStale(status.ObservedAt, now),
		Source:     "cli",
	}
}

func fallbackProxy(status *remote.ServerStatus, now time.Time) proxyStatus {
	running := false
	for _, container := range status.Containers {
		if container.Name == "caddy" && container.State == "running" {
			running = true
			break
		}
	}
	err := remote.ObservationError{Scope: "caddy.routes", Message: "installed teploy CLI does not support machine-readable proxy routes"}
	return proxyStatus{
		Running: running, Routes: []proxyRoute{}, ObservedAt: now.UTC(),
		Errors: []remote.ObservationError{err}, Partial: true, Stale: false, Source: "ssh_fallback",
	}
}

func mapMachineContainer(container machineContainer) remote.ContainerInfo {
	return remote.ContainerInfo{
		ID: container.ID, Name: container.Name, Image: container.Image,
		State: container.State, Status: container.Status, CreatedAt: container.CreatedAt,
		Process: container.Process, Version: container.Version,
	}
}

func mapMachineErrors(errors []machineError) []remote.ObservationError {
	result := make([]remote.ObservationError, 0, len(errors))
	for _, err := range errors {
		result = append(result, remote.ObservationError{Scope: err.Scope, Message: err.Message})
	}
	return result
}

func commandFailure(args []string, result *cli.Result) error {
	message := strings.TrimSpace(result.Stderr)
	if message == "" {
		message = strings.TrimSpace(result.Stdout)
	}
	if message == "" {
		message = fmt.Sprintf("exit code %d", result.ExitCode)
	}
	return fmt.Errorf("teploy %s failed: %s", strings.Join(args, " "), message)
}

func observationStale(observedAt, now time.Time) bool {
	return observedAt.IsZero() || now.Sub(observedAt) > observationStaleAfter
}

func primaryDisk(disks []machineDisk) *machineDisk {
	for i := range disks {
		if disks[i].Mountpoint == "/" {
			return &disks[i]
		}
	}
	if len(disks) > 0 {
		return &disks[0]
	}
	return nil
}

func formatDuration(seconds float64) string {
	if seconds <= 0 {
		return ""
	}
	duration := time.Duration(seconds * float64(time.Second)).Round(time.Second)
	return duration.String()
}

func formatBytes(value uint64) string {
	if value == 0 {
		return "0B"
	}
	const gib = uint64(1024 * 1024 * 1024)
	const mib = uint64(1024 * 1024)
	if value >= gib {
		return fmt.Sprintf("%.1fG", float64(value)/float64(gib))
	}
	return fmt.Sprintf("%.1fM", float64(value)/float64(mib))
}

func percent(used, total uint64) string {
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%d%%", int(math.Round(float64(used)*100/float64(total))))
}

func nonNilInts(values []int) []int {
	if values == nil {
		return []int{}
	}
	return values
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func logMachineError(area, server string, err machineError) {
	// Kept separate so partial canonical responses remain successful while their
	// top-level errors are still visible to operators.
	log.Printf("[%s] %s machine read partial error (%s): %s", area, server, err.Scope, err.Message)
}
