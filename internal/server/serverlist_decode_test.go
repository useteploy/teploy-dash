package server

// X02 S2 tail: the server-list decode paths (resolveServers, the
// /api/config/servers GET) ride cli.DecodeServerList, which carries both
// wire eras — the MI-2 envelope and the legacy bare map. These tests pin
// the fleet path against the NEW shape and the UI contract staying the
// OLD shape, while the existing fleet/machine/onboarding stubs (bare-map
// server list) keep exercising the legacy branch.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/remote"
)

const serverListEnvelopeJSON = `{
  "machine_interface": 2,
  "servers": [
    {"name": "prod", "id": "srv-0123456789abcdef", "host": "192.0.2.10", "user": "deploy", "role": "app"},
    {"name": "staging", "host": "192.0.2.20"}
  ],
  "observed_at": "2026-09-23T12:00:00Z"
}`

func TestResolveServersDecodesMI2Envelope(t *testing.T) {
	s := New(Config{
		DataDir:      t.TempDir(),
		NoAuth:       true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			return &cli.Result{Stdout: serverListEnvelopeJSON}, nil
		},
	})
	servers, err := s.resolveServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("servers = %#v", servers)
	}
	byName := map[string]remote.ServerConn{}
	for _, srv := range servers {
		byName[srv.Name] = srv
	}
	prod := byName["prod"]
	if prod.ID != "srv-0123456789abcdef" || prod.Host != "192.0.2.10" || prod.User != "deploy" {
		t.Fatalf("prod not mapped from the envelope: %#v", prod)
	}
	staging := byName["staging"]
	if staging.ID != "" || staging.User != "root" {
		t.Fatalf("id-less entry must keep empty ID and default root user: %#v", staging)
	}
}

func TestConfigServersGETNormalizesEnvelopeToBareMap(t *testing.T) {
	s := New(Config{
		DataDir:      t.TempDir(),
		NoAuth:       true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			return &cli.Result{Stdout: serverListEnvelopeJSON}, nil
		},
	})
	response := httptest.NewRecorder()
	s.handleConfigServers(response, httptest.NewRequest(http.MethodGet, "/api/config/servers", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data map[string]map[string]string `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data) != 2 {
		t.Fatalf("data = %#v", envelope.Data)
	}
	prod := envelope.Data["prod"]
	if prod["host"] != "192.0.2.10" || prod["user"] != "deploy" || prod["role"] != "app" || prod["id"] != "srv-0123456789abcdef" {
		t.Fatalf("prod entry = %#v", prod)
	}
	if strings.HasPrefix(response.Body.String(), `{"data":{"machine_interface"`) {
		t.Fatalf("the CLI envelope leaked to the frontend: %s", response.Body.String())
	}
}

func TestResolveServersRefusesNewerServerListInterface(t *testing.T) {
	s := New(Config{
		DataDir:      t.TempDir(),
		NoAuth:       true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			return &cli.Result{Stdout: `{"machine_interface": 3, "servers": [], "observed_at": "2026-09-23T12:00:00Z"}`}, nil
		},
	})
	_, err := s.resolveServers(context.Background())
	if err == nil || !strings.Contains(err.Error(), "upgrade teploy-dash") {
		t.Fatalf("discovery must fail closed on a newer interface with the remedy, got %v", err)
	}
}
