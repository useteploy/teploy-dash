package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/remote"
)

func TestMachineAppListMapsV2State(t *testing.T) {
	observedAt := "2026-07-22T12:00:00Z"
	runner := func(_ context.Context, args ...string) (*cli.Result, error) {
		want := []string{"app", "list", "--host", "192.0.2.10", "--json", "--user", "deploy"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %v, want %v", args, want)
		}
		return &cli.Result{Stdout: `{
          "host":"192.0.2.10",
          "observed_at":"` + observedAt + `",
          "errors":[],
          "apps":[{
            "app":"blog","domain":"blog.example.com","type":"container","ingress":"external",
            "current_release":{"version":"v2","ports":[49153]},
            "previous_release":{"version":"v1","ports":[49152]},
            "containers":[{"id":"abc","name":"blog-web-v2","image":"example/blog:v2","state":"running","status":"Up","created_at":"today","process":"web","version":"v2"}],
            "processes":[{"name":"web","replicas":1,"running":1,"containers":["blog-web-v2"]}],
            "lock":{"type":"manual","user":"alice","message":"deploy freeze","ts":"2026-07-22T11:00:00Z"},
            "maintenance":true,"observed_at":"` + observedAt + `","errors":[]
          }]
        }`}, nil
	}
	s := &Server{runCLI: runner, remoteListApps: func(context.Context, remote.ServerConn) ([]remote.AppState, error) {
		t.Fatal("canonical response must not use SSH fallback")
		return nil, nil
	}}

	apps, err := s.readMachineApps(context.Background(), remote.ServerConn{Name: "prod", Host: "192.0.2.10", User: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 {
		t.Fatalf("apps = %#v", apps)
	}
	app := apps[0]
	if app.App != "blog" || app.Server != "prod" || app.CurrentHash != "v2" || app.PreviousHash != "v1" || app.CurrentPort != 49153 {
		t.Fatalf("state identity not mapped: %#v", app)
	}
	if app.Type != "container" || app.Ingress != "external" || app.Status != "running" || !app.Locked || !app.Maintenance || app.Source != "cli" {
		t.Fatalf("observed state not mapped: %#v", app)
	}
	if len(app.Containers) != 1 || app.Containers[0].ID != "abc" || app.Containers[0].Process != "web" {
		t.Fatalf("containers not mapped: %#v", app.Containers)
	}
	if len(app.Processes) != 1 || app.Processes[0].Running != 1 {
		t.Fatalf("processes not mapped: %#v", app.Processes)
	}
}

func TestMachineAppListFallsBackOnlyForOldCLI(t *testing.T) {
	var fallbackCalls atomic.Int32
	s := &Server{
		runCLI: func(context.Context, ...string) (*cli.Result, error) {
			return &cli.Result{ExitCode: 1, Stderr: `unknown command "list" for "teploy app"`}, nil
		},
		remoteListApps: func(_ context.Context, srv remote.ServerConn) ([]remote.AppState, error) {
			fallbackCalls.Add(1)
			return []remote.AppState{{App: "legacy", Server: srv.Name}}, nil
		},
	}
	apps, err := s.readMachineApps(context.Background(), remote.ServerConn{Name: "prod", Host: "prod.example"})
	if err != nil {
		t.Fatal(err)
	}
	if fallbackCalls.Load() != 1 || len(apps) != 1 || apps[0].App != "legacy" {
		t.Fatalf("fallback calls=%d apps=%#v", fallbackCalls.Load(), apps)
	}
}

func TestMachineAppListMalformedJSONDoesNotFallback(t *testing.T) {
	var fallbackCalls atomic.Int32
	s := &Server{
		runCLI: func(context.Context, ...string) (*cli.Result, error) {
			return &cli.Result{Stdout: `{"host":"prod","apps":[`}, nil
		},
		remoteListApps: func(context.Context, remote.ServerConn) ([]remote.AppState, error) {
			fallbackCalls.Add(1)
			return nil, nil
		},
	}
	_, err := s.readMachineApps(context.Background(), remote.ServerConn{Name: "prod", Host: "prod.example"})
	if err == nil || !strings.Contains(err.Error(), "decoding teploy app list") {
		t.Fatalf("error = %v", err)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("malformed canonical JSON triggered %d fallback calls", fallbackCalls.Load())
	}
}

func TestServerStatusAndProxyMachineRoutes(t *testing.T) {
	observedAt := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	statusJSON := `{
      "server":"prod","host":"192.0.2.10","uptime":{"seconds":3600},
      "load":{"one":0.1,"five":0.2,"fifteen":0.3},
      "memory":{"total_bytes":1073741824,"used_bytes":536870912,"available_bytes":536870912},
      "disks":[{"filesystem":"/dev/vda1","mountpoint":"/","total_bytes":10737418240,"used_bytes":2684354560,"available_bytes":8053063680,"used_percent":"25%"}],
      "docker":{"installed":true,"version":"29","containers":[{"id":"caddy-id","name":"caddy","image":"caddy:2","state":"running","status":"Up","created_at":"today","process":"","version":""}],"images":[]},
      "caddy":{"available":true,"routes":[{"server":"srv0","id":"blog-route","hosts":["blog.example.com"],"handlers":["reverse_proxy"],"upstreams":["blog-web-v2:3000"],"status_code":""}]},
      "observed_at":"` + observedAt + `","errors":[{"scope":"docker.images","message":"permission denied"}]
    }`
	runner := func(_ context.Context, args ...string) (*cli.Result, error) {
		switch strings.Join(args, " ") {
		case "server list --json":
			return &cli.Result{Stdout: `{"prod":{"host":"192.0.2.10","user":"deploy"}}`}, nil
		case "server status prod --json":
			return &cli.Result{Stdout: statusJSON}, nil
		default:
			return nil, errors.New("unexpected command: " + strings.Join(args, " "))
		}
	}
	s := New(Config{DataDir: t.TempDir(), NoAuth: true, CLIInstalled: func() bool { return true }, CLIRunner: runner})

	statusResponse := httptest.NewRecorder()
	s.handleServerDetail(statusResponse, httptest.NewRequest(http.MethodGet, "/api/servers/prod/status", nil))
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	var statusEnvelope struct {
		Data remote.ServerStatus `json:"data"`
	}
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &statusEnvelope); err != nil {
		t.Fatal(err)
	}
	if statusEnvelope.Data.Name != "prod" || statusEnvelope.Data.MemPercent != "50%" || statusEnvelope.Data.DiskPercent != "25%" {
		t.Fatalf("status mapping = %#v", statusEnvelope.Data)
	}
	if !statusEnvelope.Data.Partial || statusEnvelope.Data.Stale || len(statusEnvelope.Data.Errors) != 1 {
		t.Fatalf("partial/stale fields = %#v", statusEnvelope.Data)
	}

	proxyResponse := httptest.NewRecorder()
	s.handleServerDetail(proxyResponse, httptest.NewRequest(http.MethodGet, "/api/servers/prod/proxy", nil))
	if proxyResponse.Code != http.StatusOK {
		t.Fatalf("proxy code=%d body=%s", proxyResponse.Code, proxyResponse.Body.String())
	}
	var proxyEnvelope struct {
		Data proxyStatus `json:"data"`
	}
	if err := json.Unmarshal(proxyResponse.Body.Bytes(), &proxyEnvelope); err != nil {
		t.Fatal(err)
	}
	proxy := proxyEnvelope.Data
	if !proxy.Running || len(proxy.Routes) != 1 || proxy.Routes[0].ID != "blog-route" || proxy.Routes[0].Handler != "reverse_proxy" {
		t.Fatalf("proxy mapping = %#v", proxy)
	}
	if !reflect.DeepEqual(proxy.Routes[0].Domains, []string{"blog.example.com"}) || !reflect.DeepEqual(proxy.Routes[0].Upstreams, []string{"blog-web-v2:3000"}) {
		t.Fatalf("proxy route shape = %#v", proxy.Routes[0])
	}
}

func TestServerStatusFallsBackForOldCLI(t *testing.T) {
	var fallbackCalls atomic.Int32
	runner := func(_ context.Context, args ...string) (*cli.Result, error) {
		switch strings.Join(args, " ") {
		case "server list --json":
			return &cli.Result{Stdout: `{"prod":{"host":"192.0.2.10","user":"root"}}`}, nil
		case "server status prod --json":
			return &cli.Result{ExitCode: 1, Stderr: `unknown command "status" for "teploy server"`}, nil
		default:
			return nil, errors.New("unexpected command")
		}
	}
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true }, CLIRunner: runner,
		RemoteServerStatus: func(_ context.Context, srv remote.ServerConn) (*remote.ServerStatus, error) {
			fallbackCalls.Add(1)
			return &remote.ServerStatus{Name: srv.Name, Host: srv.Host, Containers: []remote.ContainerInfo{}}, nil
		},
	})
	response := httptest.NewRecorder()
	s.handleServerDetail(response, httptest.NewRequest(http.MethodGet, "/api/servers/prod/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data remote.ServerStatus `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if fallbackCalls.Load() != 1 || envelope.Data.Source != "ssh_fallback" || envelope.Data.ObservedAt.IsZero() {
		t.Fatalf("fallback calls=%d response=%#v", fallbackCalls.Load(), envelope.Data)
	}
}

func TestLegacyLifecycleActionEnqueuesOperation(t *testing.T) {
	commands := make(chan operation.Command, 1)
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: "192.0.2.10", User: "deploy"}, nil
		},
		OperationExecutor: func(_ context.Context, command operation.Command, _ func(operation.Stream, string)) (int, error) {
			commands <- command
			return 0, nil
		},
	})
	response := httptest.NewRecorder()
	s.handleAppPost(response, httptest.NewRequest(http.MethodPost, "/api/apps/prod/blog/restart", nil), "prod", "blog", "restart")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data operation.Operation `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	select {
	case command := <-commands:
		want := []string{"restart", "--host", "192.0.2.10", "--app", "blog", "--user", "deploy"}
		if !reflect.DeepEqual(command.Args, want) {
			t.Fatalf("command args=%v want=%v", command.Args, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("lifecycle operation did not execute")
	}
	// Wait for the operation to reach a terminal status before returning.
	// Manager.execute writes the closing status event AFTER the executor
	// returns, so without this the test's t.TempDir cleanup races that write
	// and fails with "operations/events: directory not empty" — ~3 runs in 15
	// on this test alone, and it reproduces on a pristine tree. Manager has no
	// Close, so polling to terminal is the available join point; it is the
	// same wait operations_test.go:144-154 already does.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err := s.operations.Get(envelope.Data.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Terminal() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
}

func TestCapabilitiesProbesAndCachesCLIContract(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	runner := func(_ context.Context, args ...string) (*cli.Result, error) {
		command := strings.Join(args, " ")
		mu.Lock()
		calls[command]++
		mu.Unlock()
		switch command {
		case "version --json":
			return &cli.Result{Stdout: `{"version":"teploy v0.2.0","machine_interface":1,"capabilities":["app-list-machine","server-status-machine"]}`}, nil
		default:
			return nil, errors.New("unexpected command: " + command)
		}
	}
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true }, CLIRunner: runner,
	})
	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		s.handleCapabilities(response, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var envelope struct {
			Data capabilities `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if !envelope.Data.CLI.Installed || envelope.Data.CLI.Version != "v0.2.0" || !envelope.Data.Features.AppListJSON || !envelope.Data.Features.ServerStatusJSON || !envelope.Data.Features.Operations {
			t.Fatalf("capabilities = %#v", envelope.Data)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["version --json"] != 1 {
		t.Fatalf("version --json called %d times, want cached single probe", calls["version --json"])
	}
	// The token set answered; the --help probes must not have run at all.
	for _, command := range []string{"app list --help", "server status --help"} {
		if calls[command] != 0 {
			t.Fatalf("%s ran despite the capability tokens answering", command)
		}
	}
}

// F072: help exiting zero WITHOUT the --json flag advertises no machine
// output — the old probe credited mere command existence.
func TestCapabilitiesProbeRequiresJSONFlag(t *testing.T) {
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			command := strings.Join(args, " ")
			switch command {
			case "version --json":
				// Pre-MI CLI: unknown flag.
				return &cli.Result{ExitCode: 1, Stderr: "unknown flag: --json"}, nil
			case "version":
				return &cli.Result{Stdout: "teploy v0.1.0\n"}, nil
			case "app list --help", "server status --help":
				return &cli.Result{Stdout: "usage (no json flag here)"}, nil
			default:
				return nil, errors.New("unexpected command: " + command)
			}
		},
	})
	response := httptest.NewRecorder()
	s.handleCapabilities(response, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	var envelope struct {
		Data capabilities `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Features.AppListJSON || envelope.Data.Features.ServerStatusJSON {
		t.Fatalf("--json-less help advertised as machine-capable: %#v", envelope.Data.Features)
	}
}

// A pre-MI CLI that ignores --json (v0.1.35 prints `teploy 0.1.35`, exit 0)
// is a legacy answer, not a malformed handshake: the version reads, no
// version-probe error is recorded, and so onboarding does not gate every
// server on a failed teploy_cli check.
func TestCapabilitiesPreMIIgnoringJSONFlagReadsLegacyVersion(t *testing.T) {
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			switch strings.Join(args, " ") {
			case "version --json":
				return &cli.Result{Stdout: "teploy 0.1.35\n"}, nil
			case "app list --help", "server status --help":
				return &cli.Result{Stdout: "  --json   machine-readable output"}, nil
			default:
				return nil, errors.New("unexpected command: " + strings.Join(args, " "))
			}
		},
	})
	response := httptest.NewRecorder()
	s.handleCapabilities(response, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	var envelope struct {
		Data capabilities `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.CLI.Version != "0.1.35" || envelope.Data.CLI.MachineInterface != 0 {
		t.Fatalf("cli = %#v", envelope.Data.CLI)
	}
	if len(envelope.Data.Errors) != 0 {
		t.Fatalf("legacy version answer recorded as probe errors: %#v", envelope.Data.Errors)
	}
	if !envelope.Data.Features.AppListJSON || !envelope.Data.Features.ServerStatusJSON {
		t.Fatalf("legacy --help probes did not run: %#v", envelope.Data.Features)
	}
}

// X02 S2 done-check: an interface newer than the one dash decodes fails
// CLOSED at the capability probe — machine features report unsupported and
// the remedy is visible at /api/capabilities — before any envelope or
// mutation rides the newer interface. Max supported is MI 2 (the
// server-list envelope era), so the refusal class is MI 3.
func TestCapabilitiesFailsClosedOnNewerMachineInterface(t *testing.T) {
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			if strings.Join(args, " ") == "version --json" {
				return &cli.Result{Stdout: `{"version":"teploy v0.3.0","machine_interface":3,"capabilities":["app-list-machine","server-status-machine"]}`}, nil
			}
			return nil, errors.New("unexpected command: " + strings.Join(args, " "))
		},
	})
	response := httptest.NewRecorder()
	s.handleCapabilities(response, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	var envelope struct {
		Data capabilities `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Features.AppListJSON || envelope.Data.Features.ServerStatusJSON {
		t.Fatalf("features advertised against a newer machine interface: %#v", envelope.Data.Features)
	}
	if len(envelope.Data.Errors) == 0 || !strings.Contains(envelope.Data.Errors[0].Message, "upgrade teploy-dash") {
		t.Fatalf("expected the upgrade remedy error, got %#v", envelope.Data.Errors)
	}
	if envelope.Data.CLI.MachineInterface != 3 {
		t.Fatalf("machine_interface not surfaced: %#v", envelope.Data.CLI)
	}
}

// X02 S2 tail: the MI-2 handshake (the server-list envelope era; token
// set unchanged by the bump) still lights the feature flags — the bump
// itself must not read as skew.
func TestCapabilitiesMI2HandshakeLightsFeatureFlags(t *testing.T) {
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			if strings.Join(args, " ") == "version --json" {
				return &cli.Result{Stdout: `{"version":"teploy v0.2.0","machine_interface":2,"capabilities":["app-list-machine","server-status-machine"]}`}, nil
			}
			return nil, errors.New("unexpected command: " + strings.Join(args, " "))
		},
	})
	response := httptest.NewRecorder()
	s.handleCapabilities(response, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	var envelope struct {
		Data capabilities `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.CLI.MachineInterface != 2 {
		t.Fatalf("machine_interface = %d, want 2: %#v", envelope.Data.CLI.MachineInterface, envelope.Data.CLI)
	}
	if !envelope.Data.Features.AppListJSON || !envelope.Data.Features.ServerStatusJSON {
		t.Fatalf("MI-2 handshake must light the machine feature flags (token set unchanged): %#v", envelope.Data.Features)
	}
	if len(envelope.Data.Errors) != 0 {
		t.Fatalf("MI-2 within supported max must not error: %#v", envelope.Data.Errors)
	}
}
