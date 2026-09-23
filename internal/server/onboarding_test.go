package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
)

// preflightRunner answers the CLI delegate boundary for an onboarding
// preflight: server discovery, the local CLI capability surface (version +
// --help probes), and one `server status <name> --json` machine read per
// known server. Status entries absent from the map fail like an unreachable
// host (non-zero exit with the SSH error in stderr).
func preflightRunner(serversJSON string, status map[string]*cli.Result) func(context.Context, ...string) (*cli.Result, error) {
	return func(_ context.Context, args ...string) (*cli.Result, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "server list --json":
			return &cli.Result{Stdout: serversJSON}, nil
		case joined == "version":
			return &cli.Result{Stdout: "teploy v0.1.35\n"}, nil
		case joined == "app list --help" || joined == "server status --help":
			return &cli.Result{Stdout: "Usage:\n  --json   machine-readable output\n"}, nil
		case strings.HasPrefix(joined, "server status ") && strings.HasSuffix(joined, " --json"):
			name := strings.TrimSuffix(strings.TrimPrefix(joined, "server status "), " --json")
			if result, ok := status[name]; ok {
				return result, nil
			}
			return &cli.Result{ExitCode: 1, Stderr: "ssh: connect to host port 22: connection refused"}, nil
		default:
			return &cli.Result{ExitCode: 1, Stderr: "unexpected command: " + joined}, nil
		}
	}
}

// preflightMachineStatus builds one complete `server status --json` payload
// (the shape readMachineServer requires: non-nil disks/containers/images/
// routes/errors, non-zero observed_at).
func preflightMachineStatus(server, host string, dockerInstalled, caddyUp bool, availGib uint64, errs string) string {
	docker := "true"
	if !dockerInstalled {
		docker = "false"
	}
	caddy := "true"
	if !caddyUp {
		caddy = "false"
	}
	total := availGib + 50
	return fmt.Sprintf(`{"server":%q,"host":%q,
		"uptime":{"seconds":3600},"load":{"one":0.1,"five":0.2,"fifteen":0.3},
		"memory":{"total_bytes":8000000000,"used_bytes":4000000000,"available_bytes":4000000000},
		"disks":[{"filesystem":"/dev/sda1","mountpoint":"/","total_bytes":%d,"used_bytes":%d,"available_bytes":%d,"used_percent":"20%%"}],
		"docker":{"installed":%s,"version":"27.0.3","containers":[],"images":[]},
		"caddy":{"available":%s,"routes":[]},
		"observed_at":%q,"errors":[%s]}`,
		server, host,
		total*1024*1024*1024, 50*1024*1024*1024, availGib*1024*1024*1024,
		docker, caddy,
		time.Now().UTC().Format(time.RFC3339Nano), errs)
}

// preflightEnvelopeExt mirrors the served preflight envelope for decoding.
type preflightCheckExt struct {
	Name        string `json:"name"`
	Result      string `json:"result"`
	Severity    string `json:"severity"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation"`
}

type preflightEnvelopeExt struct {
	ID          string              `json:"id"`
	Server      string              `json:"server"`
	Host        string              `json:"host"`
	Known       bool                `json:"known"`
	Ready       bool                `json:"ready"`
	CollectedAt time.Time           `json:"collected_at"`
	Error       string              `json:"error"`
	Source      string              `json:"source"`
	Checks      []preflightCheckExt `json:"checks"`
}

type onboardingEntryExt struct {
	Server    string                `json:"server"`
	Gated     bool                  `json:"gated"`
	Reason    string                `json:"reason"`
	Preflight *preflightEnvelopeExt `json:"preflight"`
}

func preflightChecksByName(env preflightEnvelopeExt) map[string]preflightCheckExt {
	byName := map[string]preflightCheckExt{}
	for _, c := range env.Checks {
		byName[c.Name] = c
	}
	return byName
}

func getPreflight(t *testing.T, s *Server, target string) (int, preflightEnvelopeExt) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("preflight status=%d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data preflightEnvelopeExt `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return rec.Code, envelope.Data
}

// Reachable + healthy: every check passes, the envelope is ready, and the
// specifics (CLI version, disk headroom) are carried in the details.
func TestOnboardingPreflightHealthyServer(t *testing.T) {
	sshListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind loopback listener: %v", err)
	}
	defer sshListener.Close()
	go func() {
		for {
			conn, err := sshListener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	host := sshListener.Addr().String()

	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: preflightRunner(
			fmt.Sprintf(`{"alpha":{"host":%q}}`, host),
			map[string]*cli.Result{
				"alpha": {Stdout: preflightMachineStatus("alpha", host, true, true, 40, "")},
			},
		),
	})

	code, env := getPreflight(t, s, "/api/onboarding/preflight?server=alpha")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if !env.Known || !env.Ready || env.Error != "" || env.Host != host {
		t.Fatalf("healthy envelope = %#v", env)
	}
	if env.ID != serverStableID("alpha") {
		t.Fatalf("envelope ID must use the stable derivation, got %q", env.ID)
	}
	if env.CollectedAt.IsZero() {
		t.Fatal("envelope needs a collection timestamp")
	}
	byName := preflightChecksByName(env)
	for _, required := range []string{"teploy_cli", "machine_interface", "ssh", "host_read", "docker", "disk", "caddy"} {
		if _, ok := byName[required]; !ok {
			t.Fatalf("healthy preflight must carry check %q: %#v", required, env.Checks)
		}
	}
	if c := byName["teploy_cli"]; c.Result != "pass" || c.Severity != "blocking" || !strings.Contains(c.Detail, "v0.1.35") {
		t.Fatalf("teploy_cli check = %#v", c)
	}
	if c := byName["ssh"]; c.Result != "pass" {
		t.Fatalf("ssh check = %#v", c)
	}
	if c := byName["docker"]; c.Result != "pass" || !strings.Contains(c.Detail, "27.0.3") {
		t.Fatalf("docker check = %#v", c)
	}
	if c := byName["disk"]; c.Result != "pass" || !strings.Contains(c.Detail, "40") {
		t.Fatalf("disk check must carry headroom detail: %#v", c)
	}
	if c := byName["caddy"]; c.Result != "pass" {
		t.Fatalf("caddy check = %#v", c)
	}
}

// Reachable but degraded (docker daemon down, surfaced through the machine
// contract's scoped errors): the docker check fails BLOCKING with the exact
// error and a remediation hint; the envelope stays present and not ready.
func TestOnboardingPreflightDockerDownIsBlocking(t *testing.T) {
	sshListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind loopback listener: %v", err)
	}
	defer sshListener.Close()
	go func() {
		for {
			conn, err := sshListener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	host := sshListener.Addr().String()
	degraded := preflightMachineStatus("alpha", host, true, true, 40,
		`{"scope":"docker","message":"cannot connect to the Docker daemon at unix:///var/run/docker.sock"}`)

	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: preflightRunner(
			fmt.Sprintf(`{"alpha":{"host":%q}}`, host),
			map[string]*cli.Result{"alpha": {Stdout: degraded}},
		),
	})

	_, env := getPreflight(t, s, "/api/onboarding/preflight?server=alpha")
	if env.Ready {
		t.Fatal("docker down must make the envelope not ready")
	}
	byName := preflightChecksByName(env)
	docker := byName["docker"]
	if docker.Result != "fail" || docker.Severity != "blocking" {
		t.Fatalf("docker check = %#v", docker)
	}
	if !strings.Contains(docker.Detail, "Docker daemon") && !strings.Contains(docker.Detail, "docker.sock") {
		t.Fatalf("docker check must carry the exact error: %#v", docker)
	}
	if docker.Remediation == "" {
		t.Fatal("a failed check must carry an actionable remediation hint")
	}
	ssh := byName["ssh"]
	if ssh.Result != "pass" {
		t.Fatalf("host is reachable; ssh check = %#v", ssh)
	}
}

// Unreachable host: the ssh and host_read checks fail blocking with the
// exact transport error, the server-specific checks are unknown, and the
// envelope is PRESENT (never dropped) with ready=false.
func TestOnboardingPreflightUnreachableServerNotDropped(t *testing.T) {
	restore := preflightSSHTimeout
	preflightSSHTimeout = 100 * time.Millisecond
	t.Cleanup(func() { preflightSSHTimeout = restore })

	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: preflightRunner(
			`{"beta":{"host":"203.0.113.7"}}`,
			map[string]*cli.Result{
				"beta": {ExitCode: 1, Stderr: "ssh: connect to host 203.0.113.7 port 22: operation timed out"},
			},
		),
	})

	_, env := getPreflight(t, s, "/api/onboarding/preflight?server=beta")
	if env.Error != "" && env.Ready {
		t.Fatalf("unreachable server must not be ready: %#v", env)
	}
	if env.Ready {
		t.Fatal("unreachable server must not be ready")
	}
	byName := preflightChecksByName(env)
	if c := byName["ssh"]; c.Result != "fail" || c.Severity != "blocking" || c.Remediation == "" {
		t.Fatalf("ssh check = %#v", c)
	}
	if c := byName["host_read"]; c.Result != "fail" || c.Severity != "blocking" {
		t.Fatalf("host_read check = %#v", c)
	}
	if !strings.Contains(byName["host_read"].Detail, "timed out") {
		t.Fatalf("host_read must carry the exact CLI error: %#v", byName["host_read"])
	}
	for _, name := range []string{"docker", "disk", "caddy"} {
		if c := byName[name]; c.Result != "unknown" {
			t.Fatalf("%s check must be unknown when the host cannot be read: %#v", name, c)
		}
	}
}

// Unknown server name: the envelope is still served (visible with error,
// never dropped), known=false, and the blocking checks read unknown.
func TestOnboardingPreflightUnknownServerNameNotDropped(t *testing.T) {
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: preflightRunner(
			`{"alpha":{"host":"192.0.2.10"}}`,
			map[string]*cli.Result{"alpha": {Stdout: preflightMachineStatus("alpha", "192.0.2.10", true, true, 40, "")}},
		),
	})

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/onboarding/preflight?server=ghost", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown server must still answer an envelope, got %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data preflightEnvelopeExt `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	env := envelope.Data
	if env.Known || env.Ready || env.Error == "" || !strings.Contains(env.Error, "ghost") {
		t.Fatalf("unknown-server envelope = %#v", env)
	}
	byName := preflightChecksByName(env)
	if c := byName["host_read"]; c.Result != "unknown" || c.Severity != "blocking" || c.Remediation == "" {
		t.Fatalf("host_read check for an unregistered server = %#v", c)
	}
}

// Server discovery itself fails: the envelope carries the discovery error
// visibly instead of answering an empty success or a dropped body.
func TestOnboardingPreflightDiscoveryFailureVisible(t *testing.T) {
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			if strings.Join(args, " ") == "server list --json" {
				return nil, fmt.Errorf("teploy binary vanished")
			}
			return preflightRunner("{}", nil)(context.Background(), args...)
		},
	})

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/onboarding/preflight?server=alpha", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("discovery failure must still answer an envelope, got %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data preflightEnvelopeExt `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Error == "" || !strings.Contains(envelope.Data.Error, "server discovery failed") {
		t.Fatalf("discovery failure envelope = %#v", envelope.Data)
	}
	if envelope.Data.Ready {
		t.Fatal("a fleet whose discovery fails must not be ready")
	}
}

// POST with a JSON body answers the same envelope as the GET query form.
func TestOnboardingPreflightPostBody(t *testing.T) {
	sshListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind loopback listener: %v", err)
	}
	defer sshListener.Close()
	go func() {
		for {
			conn, err := sshListener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	host := sshListener.Addr().String()

	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: preflightRunner(
			fmt.Sprintf(`{"alpha":{"host":%q}}`, host),
			map[string]*cli.Result{"alpha": {Stdout: preflightMachineStatus("alpha", host, true, true, 40, "")}},
		),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/onboarding/preflight", strings.NewReader(`{"server":"alpha"}`))
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST preflight status=%d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data preflightEnvelopeExt `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Server != "alpha" || !envelope.Data.Ready {
		t.Fatalf("POST envelope = %#v", envelope.Data)
	}
}

// Flow gating: the create-entry API answers the preflight state for the
// selected server — healthy = not gated; a blocking failure = gated with the
// failing check named in the reason; no server = gated.
func TestOnboardingEntryGatingStates(t *testing.T) {
	sshListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind loopback listener: %v", err)
	}
	defer sshListener.Close()
	go func() {
		for {
			conn, err := sshListener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	host := sshListener.Addr().String()
	serversJSON := fmt.Sprintf(`{"alpha":{"host":%q}}`, host)

	fetchEntry := func(s *Server, target string) onboardingEntryExt {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("entry status=%d body=%s", rec.Code, rec.Body.String())
		}
		var entry struct {
			Data onboardingEntryExt `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		return entry.Data
	}

	healthy := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: preflightRunner(serversJSON, map[string]*cli.Result{
			"alpha": {Stdout: preflightMachineStatus("alpha", host, true, true, 40, "")},
		}),
	})
	if entry := fetchEntry(healthy, "/api/onboarding/entry?server=alpha"); entry.Gated || entry.Preflight == nil || !entry.Preflight.Ready {
		t.Fatalf("healthy entry must not be gated: %#v", entry)
	}

	degraded := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: preflightRunner(serversJSON, map[string]*cli.Result{
			"alpha": {Stdout: preflightMachineStatus("alpha", host, false, true, 40, "")},
		}),
	})
	entry := fetchEntry(degraded, "/api/onboarding/entry?server=alpha")
	if !entry.Gated || entry.Preflight == nil || entry.Preflight.Ready {
		t.Fatalf("docker-down entry must be gated: %#v", entry)
	}
	if entry.Reason == "" || !strings.Contains(entry.Reason, "docker") {
		t.Fatalf("gated entry must name the blocking check in its reason: %#v", entry)
	}

	if entry := fetchEntry(healthy, "/api/onboarding/entry"); !entry.Gated || entry.Preflight != nil {
		t.Fatalf("no server selected must gate the entry: %#v", entry)
	}
}
