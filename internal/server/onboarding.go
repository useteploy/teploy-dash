package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/remote"
	sshclient "github.com/useteploy/teploy-dash/internal/ssh"
)

// D03 onboarding preflight. Programme demand: BEFORE asking for application
// details, show host connection/capability readiness. One envelope per
// probed server in the D01 observation-envelope style — visible with its
// error, never dropped — where each check carries result + detail + severity
// + an actionable remediation string (the recovery-from-failures first
// rung). Flow gating is additive: /api/deploy keeps its direct contract for
// API consumers; the onboarding entry surfaces the preflight state the UI
// gates the create form on.

const (
	// preflightSSHTimeout bounds the SSH reachability dial (the same
	// tcpReachable path /api/servers uses for its online flag).
	preflightSSHTimeoutDefault = 3 * time.Second

	// preflightProbeTimeout bounds the whole probe (machine read included)
	// so one blackholing host delays only its own preflight.
	preflightProbeTimeout = 20 * time.Second

	// Disk headroom thresholds for the deploy target's root filesystem.
	preflightDiskBlockingBytes = 1 << 30 // < 1 GiB free: deploys will fail.
	preflightDiskWarningBytes  = 5 << 30 // < 5 GiB free: tight for images.

	// preflightMinCLIVersion is advisory: below it the machine interface may
	// be missing and preflight detail degrades to the SSH fallback.
	preflightMinCLIVersion = "0.1.35"
)

// preflightSSHTimeout is a var so tests can tighten the dial; production
// never overrides it.
var preflightSSHTimeout = preflightSSHTimeoutDefault

// PreflightCheck is one readiness probe. Result is pass|fail|unknown;
// Severity is blocking (a failure gates app creation) or warning (shown,
// non-blocking). Remediation is the actionable recovery hint carried on
// every non-passing check.
type PreflightCheck struct {
	Name        string `json:"name"`
	Result      string `json:"result"`
	Severity    string `json:"severity"`
	Detail      string `json:"detail,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

// PreflightEnvelope is the per-server readiness answer. D01 conventions: an
// unknown server name or a failed discovery is VISIBLE (Known=false /
// Error carries the exact reason, blocking checks read unknown) — the
// envelope is never dropped in favor of an empty success.
type PreflightEnvelope struct {
	ID          string           `json:"id"`
	Server      string           `json:"server"`
	Host        string           `json:"host,omitempty"`
	Known       bool             `json:"known"`
	Ready       bool             `json:"ready"`
	CollectedAt time.Time        `json:"collected_at"`
	Error       string           `json:"error,omitempty"`
	Source      string           `json:"source,omitempty"`
	Checks      []PreflightCheck `json:"checks"`
}

// onboardingEntryView is the create-entry answer: the preflight state for
// the selected server plus the derived gate verdict.
type onboardingEntryView struct {
	Server    string             `json:"server"`
	Gated     bool               `json:"gated"`
	Reason    string             `json:"reason,omitempty"`
	Preflight *PreflightEnvelope `json:"preflight"`
}

// buildPreflight probes one server reference (a registered name, or a
// candidate host when the name matches nothing) and returns the readiness
// envelope. Every outcome — unreachable host, unregistered name, failed
// discovery — comes back as an envelope; the probe itself never answers an
// empty success.
func (s *Server) buildPreflight(ctx context.Context, serverRef string) PreflightEnvelope {
	ctx, cancel := context.WithTimeout(ctx, preflightProbeTimeout)
	defer cancel()

	env := PreflightEnvelope{
		// Legacy name-hash until discovery says otherwise; replaced by the
		// CLI-recorded stable id below when the ref resolves to a
		// registered server (X02 §1.3).
		ID:          serverStableID(serverRef),
		Server:      serverRef,
		CollectedAt: time.Now().UTC(),
		Checks:      []PreflightCheck{},
	}

	servers, err := s.resolveServers(ctx)
	if err != nil {
		// Discovery failed: dash cannot even know the fleet. Visible error,
		// blocking checks unknown, never dropped (D01 convention).
		env.Error = fmt.Sprintf("server discovery failed: %v", err)
		env.Checks = append(env.Checks, s.preflightCLIchecks(ctx)...)
		env.Checks = append(env.Checks,
			preflightCheck("ssh", "unknown", "blocking", "server list unavailable — the fleet cannot be read", remediationSSH),
			preflightCheck("host_read", "unknown", "blocking", env.Error, "Fix the teploy CLI on the dashboard host (see the teploy_cli check), then retry."),
		)
		return env
	}

	var target *remote.ServerConn
	for i := range servers {
		if servers[i].Name == serverRef {
			target = &servers[i]
			break
		}
	}
	if target != nil {
		env.ID = serverEnvelopeID(*target)
	}
	if target == nil {
		// Candidate host: not registered, so the CLI cannot manage it yet.
		// Probe bare reachability so the operator still learns something,
		// and gate on registration.
		env.Error = fmt.Sprintf("server not found: %q is not registered — add it in Settings > Servers before deploying to it", serverRef)
		if addr, addrErr := normalizeSSHAddress(serverRef); addrErr == nil {
			env.Host = serverRef
			if tcpReachable(addr, preflightSSHTimeout) {
				env.Checks = append(env.Checks, preflightCheck("ssh", "pass", "blocking", "TCP "+addr+" accepts connections", ""))
			} else {
				env.Checks = append(env.Checks, preflightCheck("ssh", "fail", "blocking", "TCP "+addr+" unreachable", remediationSSH))
			}
		}
		env.Checks = append(env.Checks, s.preflightCLIchecks(ctx)...)
		env.Checks = append(env.Checks,
			preflightCheck("host_read", "unknown", "blocking", env.Error, "Add the server in Settings > Servers (or `teploy server add`), then retry the readiness check."),
		)
		return env
	}

	env.Known = true
	env.Host = target.Host

	// Dash-host CLI capability surface (cached, bounded — capabilities()).
	env.Checks = append(env.Checks, s.preflightCLIchecks(ctx)...)

	// SSH reachability: the existing connectivity path.
	if addr, addrErr := normalizeSSHAddress(target.Host); addrErr != nil {
		env.Checks = append(env.Checks, preflightCheck("ssh", "fail", "blocking", "invalid host address: "+addrErr.Error(), "Fix the server's host in Settings > Servers."))
	} else if tcpReachable(addr, preflightSSHTimeout) {
		env.Checks = append(env.Checks, preflightCheck("ssh", "pass", "blocking", "TCP "+addr+" accepts connections", ""))
	} else {
		env.Checks = append(env.Checks, preflightCheck("ssh", "fail", "blocking", "TCP "+addr+" unreachable (timeout "+preflightSSHTimeout.String()+")", remediationSSH))
	}

	// Host specifics via the machine contract, with the SSH executor
	// fallback when the CLI is too old for `server status --json`.
	machineStatus, unsupported, err := s.readMachineServer(ctx, target.Name)
	switch {
	case err != nil:
		env.Checks = append(env.Checks, preflightCheck("host_read", "fail", "blocking", err.Error(), remediationHostRead(target.Name)))
		env.Checks = append(env.Checks, unknownHostChecks("the host could not be read: "+err.Error())...)
	case unsupported:
		fallback, fbErr := s.remoteServerStatus(ctx, *target)
		if fbErr != nil {
			env.Checks = append(env.Checks, preflightCheck("host_read", "fail", "blocking", fbErr.Error(), remediationHostRead(target.Name)))
			env.Checks = append(env.Checks, unknownHostChecks("the host could not be read: "+fbErr.Error())...)
			break
		}
		env.Source = "ssh_fallback"
		env.Checks = append(env.Checks, preflightCheck("host_read", "pass", "blocking", "read via SSH fallback (installed teploy CLI lacks `server status --json`)", ""))
		env.Checks = append(env.Checks, s.preflightFallbackChecks(fallback)...)
	default:
		env.Source = "cli"
		env.Checks = append(env.Checks, preflightCheck("host_read", "pass", "blocking", "teploy server status --json", ""))
		env.Checks = append(env.Checks, s.preflightMachineChecks(machineStatus)...)
	}

	env.Ready = preflightReady(env.Checks)
	return env
}

// preflightReady derives the gate verdict: no blocking failure, and the
// core path (CLI present, SSH reachable, host readable) must have PASSed —
// an unknown core check gates exactly like a failure because nothing about
// the target was verifiable. Warning-tier checks never gate.
func preflightReady(checks []PreflightCheck) bool {
	for _, c := range checks {
		if c.Severity != "blocking" {
			continue
		}
		switch c.Name {
		case "teploy_cli", "ssh", "host_read":
			if c.Result != "pass" {
				return false
			}
		default:
			if c.Result == "fail" {
				return false
			}
		}
	}
	return true
}

// preflightCLIchecks projects the dash-host CLI capability surface (cached
// probe) into checks: presence + version (blocking) and the machine
// interface (warning — its absence degrades detail, it does not block).
func (s *Server) preflightCLIchecks(ctx context.Context) []PreflightCheck {
	caps := s.capabilities(ctx)
	if !caps.CLI.Installed {
		return []PreflightCheck{
			{Name: "teploy_cli", Result: "fail", Severity: "blocking",
				Detail:      "teploy CLI not found on the dashboard host",
				Remediation: "Install the teploy CLI on the dashboard host (https://teploy.dev), then retry."},
			preflightCheck("machine_interface", "unknown", "warning", "CLI not installed — machine interface unknown", remediationMachineInterface),
		}
	}
	version := caps.CLI.Version
	detail := "teploy " + version + " on the dashboard host"
	if version == "" {
		detail = "installed (version unavailable)"
	}
	cliCheck := PreflightCheck{Name: "teploy_cli", Result: "pass", Severity: "blocking", Detail: detail}
	for _, probeErr := range caps.Errors {
		if probeErr.Probe == "version" {
			cliCheck = PreflightCheck{Name: "teploy_cli", Result: "fail", Severity: "blocking",
				Detail:      "version probe failed: " + probeErr.Message,
				Remediation: "Run `teploy version` on the dashboard host and fix the reported error, then retry."}
			break
		}
	}
	var iface PreflightCheck
	if caps.Features.ServerStatusJSON {
		iface = preflightCheck("machine_interface", "pass", "warning", "server status --json supported", "")
	} else {
		iface = preflightCheck("machine_interface", "fail", "warning", "installed CLI lacks `server status --json`", remediationMachineInterface)
	}
	return []PreflightCheck{cliCheck, iface}
}

// preflightMachineChecks projects the CLI machine contract onto the
// server-specific checks. Scoped machine errors (docker, caddy) fail their
// check with the exact message — that is the "reachable but degraded"
// shape (daemon down while SSH answers).
func (s *Server) preflightMachineChecks(status *machineServerStatus) []PreflightCheck {
	var checks []PreflightCheck

	docker := PreflightCheck{Name: "docker", Result: "pass", Severity: "blocking",
		Detail: "Docker " + status.Docker.Version + " reachable"}
	if !status.Docker.Installed {
		docker.Result = "fail"
		docker.Detail = "Docker is not installed on the host"
		docker.Remediation = "Install Docker on the host (docs.docker.com/engine/install), then retry."
	} else if msg := scopedMachineError(status.Errors, "docker"); msg != "" {
		docker.Result = "fail"
		docker.Detail = msg
		docker.Remediation = "Start the Docker daemon on the host (`systemctl enable --now docker` or `service docker start`), then retry."
	}
	checks = append(checks, docker)

	if disk := primaryDisk(status.Disks); disk == nil {
		checks = append(checks, preflightCheck("disk", "unknown", "warning", "no disk information in the machine response", "Check `df -h /` on the host before deploying."))
	} else {
		avail := disk.AvailableBytes
		detail := fmt.Sprintf("%s available of %s on %s", formatBytes(avail), formatBytes(disk.TotalBytes), disk.Mountpoint)
		switch {
		case avail < preflightDiskBlockingBytes:
			checks = append(checks, preflightCheck("disk", "fail", "blocking", detail,
				"Free disk space on the host (remove unused images with `docker system prune` or old releases) before deploying."))
		case avail < preflightDiskWarningBytes:
			checks = append(checks, preflightCheck("disk", "fail", "warning", detail,
				"Disk headroom is tight; consider `docker system prune` on the host before deploying large images."))
		default:
			checks = append(checks, preflightCheck("disk", "pass", "warning", detail, ""))
		}
	}

	caddy := PreflightCheck{Name: "caddy", Result: "pass", Severity: "warning",
		Detail: fmt.Sprintf("Caddy reachable, %d route(s) configured", len(status.Caddy.Routes))}
	if !status.Caddy.Available {
		caddy.Result = "fail"
		caddy.Detail = "Caddy/proxy is not reachable on the host"
		caddy.Remediation = "Caddy is managed by teploy on the host — deploys will queue but domains will not answer until it runs; check `docker ps` for the caddy container."
	} else if msg := scopedMachineError(status.Errors, "caddy"); msg != "" {
		caddy.Result = "fail"
		caddy.Detail = msg
		caddy.Remediation = "Caddy reported a partial failure — deploys will queue but domains may not answer; inspect the caddy container on the host."
	}
	checks = append(checks, caddy)

	return checks
}

// preflightFallbackChecks projects the SSH executor fallback (remote.
// GetServerStatus — uptime/free/df/docker ps in one SSH round trip) onto
// the server-specific checks. Docker/caddy daemon state is NOT verifiable
// through this path (the fallback swallows docker errors), so those checks
// read unknown at warning tier instead of guessing.
func (s *Server) preflightFallbackChecks(status *remote.ServerStatus) []PreflightCheck {
	var checks []PreflightCheck

	docker := PreflightCheck{Name: "docker", Result: "unknown", Severity: "warning",
		Detail:      fmt.Sprintf("SSH fallback sees %d container(s); daemon health not verifiable without the machine interface", len(status.Containers)),
		Remediation: remediationMachineInterface,
	}
	checks = append(checks, docker)

	if status.DiskTotal == "" {
		checks = append(checks, preflightCheck("disk", "unknown", "warning", "SSH fallback returned no disk data", "Check `df -h /` on the host before deploying."))
	} else if availGib, ok := fallbackDiskAvailableGib(status.DiskTotal, status.DiskUsed); !ok {
		checks = append(checks, preflightCheck("disk", "unknown", "warning",
			fmt.Sprintf("SSH fallback disk figures unparseable (total %s, used %s)", status.DiskTotal, status.DiskUsed),
			"Check `df -h /` on the host before deploying."))
	} else {
		detail := fmt.Sprintf("~%d GiB available of %s on / (SSH fallback)", availGib, status.DiskTotal)
		switch {
		case availGib < 1:
			checks = append(checks, preflightCheck("disk", "fail", "blocking", detail,
				"Free disk space on the host (remove unused images with `docker system prune` or old releases) before deploying."))
		case availGib < 5:
			checks = append(checks, preflightCheck("disk", "fail", "warning", detail,
				"Disk headroom is tight; consider `docker system prune` on the host before deploying large images."))
		default:
			checks = append(checks, preflightCheck("disk", "pass", "warning", detail, ""))
		}
	}

	caddyRunning := false
	for _, container := range status.Containers {
		if container.Name == "caddy" && container.State == "running" {
			caddyRunning = true
			break
		}
	}
	if caddyRunning {
		checks = append(checks, preflightCheck("caddy", "pass", "warning", "caddy container running (SSH fallback)", ""))
	} else {
		checks = append(checks, preflightCheck("caddy", "unknown", "warning",
			"no running caddy container visible via SSH fallback",
			"Caddy is managed by teploy on the host — check `docker ps` for the caddy container if domains should answer."))
	}

	return checks
}

// unknownHostChecks fills the server-specific checks when the host could
// not be read at all: honest unknowns, not guesses.
func unknownHostChecks(reason string) []PreflightCheck {
	hint := "Fix the blocking ssh/host_read checks above, then retry."
	return []PreflightCheck{
		preflightCheck("docker", "unknown", "warning", reason, hint),
		preflightCheck("disk", "unknown", "warning", reason, hint),
		preflightCheck("caddy", "unknown", "warning", reason, hint),
	}
}

func preflightCheck(name, result, severity, detail, remediation string) PreflightCheck {
	return PreflightCheck{Name: name, Result: result, Severity: severity, Detail: detail, Remediation: remediation}
}

const remediationSSH = "Check the host address and network/VPN connectivity from the dashboard host, verify sshd is listening, and confirm the server entry in Settings > Servers."

const remediationMachineInterface = "Upgrade the teploy CLI on the dashboard host to a release with machine-readable server status (`server status --json`) for full preflight detail."

// remediationHostRead carries the host-read recovery hint, naming the
// server so the operator can reproduce the failure from a terminal.
func remediationHostRead(name string) string {
	return "The teploy CLI could not read the host over SSH. Run `teploy server status " + name + "` on the dashboard host to see the exact error; if credentials changed, re-add the server in Settings > Servers."
}

// handleOnboardingPreflight answers GET/POST /api/onboarding/preflight with
// the per-server readiness envelope. POST accepts {"server": "..."} for
// API consumers that prefer a body; the GET query form is the same probe.
func (s *Server) handleOnboardingPreflight(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// server comes from the query string below.
	case http.MethodPost:
		// Optional body; absent/empty body falls back to the query string.
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	serverRef := r.URL.Query().Get("server")
	if r.Method == http.MethodPost && r.ContentLength > 0 {
		var body struct {
			Server string `json:"server"`
		}
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		if body.Server != "" {
			serverRef = body.Server
		}
	}
	if strings.TrimSpace(serverRef) == "" {
		writeError(w, "server query parameter is required (registered name or candidate host)")
		return
	}
	writeData(w, s.buildPreflight(r.Context(), serverRef))
}

// handleOnboardingEntry is the D03 create-entry contract: the preflight
// state for the selected server plus the derived gate verdict. Without a
// selected server the entry is gated ("demand a server be selected and
// readiness shown BEFORE app-detail forms are usable"). Additive: direct
// creation paths (/api/deploy) keep working for API consumers.
func (s *Server) handleOnboardingEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	serverRef := r.URL.Query().Get("server")
	if strings.TrimSpace(serverRef) == "" {
		writeData(w, onboardingEntryView{
			Server: "",
			Gated:  true,
			Reason: "no server selected",
		})
		return
	}
	env := s.buildPreflight(r.Context(), serverRef)
	entry := onboardingEntryView{Server: serverRef, Preflight: &env}
	if !env.Ready {
		entry.Gated = true
		entry.Reason = preflightGateReason(env)
	}
	writeData(w, entry)
}

// preflightGateReason names the blocking checks (or the envelope error)
// that gated the entry, so the caller can render why without re-deriving.
func preflightGateReason(env PreflightEnvelope) string {
	if env.Error != "" {
		return env.Error
	}
	var blocking []string
	for _, c := range env.Checks {
		if c.Severity == "blocking" && c.Result != "pass" {
			blocking = append(blocking, fmt.Sprintf("%s: %s (%s)", c.Name, c.Result, c.Detail))
		}
	}
	if len(blocking) == 0 {
		return "host readiness could not be verified"
	}
	return "blocking checks failed — " + strings.Join(blocking, "; ")
}

// scopedMachineError returns the first machine-contract error whose scope
// matches the area (exact or prefix before a dot), empty when none.
func scopedMachineError(errors []machineError, area string) string {
	for _, err := range errors {
		scope := err.Scope
		if i := strings.Index(scope, "."); i >= 0 {
			scope = scope[:i]
		}
		if scope == area && err.Message != "" {
			return err.Message
		}
	}
	return ""
}

// fallbackDiskAvailableGib derives available GiB from the SSH fallback's
// human figures ("40G" total, "12G" used). ok=false when unparseable.
func fallbackDiskAvailableGib(total, used string) (uint64, bool) {
	totalGib, ok := parseHumanSizeGib(total)
	if !ok {
		return 0, false
	}
	usedGib, ok := parseHumanSizeGib(used)
	if !ok || usedGib > totalGib {
		return 0, false
	}
	return totalGib - usedGib, true
}

// parseHumanSizeGib parses the fallback's df -h figures ("40G", "512M",
// "1023M"). Fractions truncate toward zero — headroom tiering only.
func parseHumanSizeGib(v string) (uint64, bool) {
	v = strings.TrimSpace(v)
	if len(v) < 2 {
		return 0, false
	}
	unit := v[len(v)-1]
	number, err := strconv.ParseFloat(strings.TrimSpace(v[:len(v)-1]), 64)
	if err != nil || number < 0 {
		return 0, false
	}
	switch unit {
	case 'G', 'g':
		return uint64(number), true
	case 'M', 'm':
		return uint64(number / 1024), true
	case 'T', 't':
		return uint64(number * 1024), true
	default:
		return 0, false
	}
}

// normalizeSSHAddress is the shared F012 normalization (dial and probe must
// agree on the endpoint).
func normalizeSSHAddress(host string) (string, error) {
	return sshclient.NormalizeAddress(host)
}
