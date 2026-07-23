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
	select {
	case command := <-commands:
		want := []string{"restart", "--host", "192.0.2.10", "--app", "blog", "--user", "deploy"}
		if !reflect.DeepEqual(command.Args, want) {
			t.Fatalf("command args=%v want=%v", command.Args, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("lifecycle operation did not execute")
	}
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
		case "version":
			return &cli.Result{Stdout: "teploy v0.2.0\n"}, nil
		case "app list --help", "server status --help":
			return &cli.Result{Stdout: "usage"}, nil
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
	for _, command := range []string{"version", "app list --help", "server status --help"} {
		if calls[command] != 1 {
			t.Fatalf("%s called %d times, want cached single probe", command, calls[command])
		}
	}
}
