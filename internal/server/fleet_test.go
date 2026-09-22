package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
)

// fleetRunner answers the CLI delegate boundary for a fleet sweep: server
// discovery plus one app-list probe per configured host. Hosts not in the
// appLists map fail with a connection error, the way an unreachable server
// fails in production.
func fleetRunner(serversJSON string, appLists map[string]*cli.Result) func(context.Context, ...string) (*cli.Result, error) {
	return func(_ context.Context, args ...string) (*cli.Result, error) {
		switch strings.Join(args, " ") {
		case "server list --json":
			return &cli.Result{Stdout: serversJSON}, nil
		default:
			if len(args) >= 4 && args[0] == "app" && args[1] == "list" && args[2] == "--host" {
				if result, ok := appLists[args[3]]; ok {
					return result, nil
				}
			}
			return nil, context.Canceled
		}
	}
}

// fleetAppListJSON builds one complete machine app-list response (the shape
// readMachineApps requires: non-nil errors/apps/containers/processes, non-zero
// observed_at).
func fleetAppListJSON(host string, appNames ...string) string {
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	var apps []string
	for _, name := range appNames {
		apps = append(apps, `{"app":"`+name+`","observed_at":"`+observedAt+`",
			"current_release":{"version":"v1","ports":[3000]},
			"previous_release":{"version":"v0","ports":[]},
			"containers":[],"processes":[],"errors":[]}`)
	}
	return `{"host":"` + host + `","observed_at":"` + observedAt + `","errors":[],"apps":[` + strings.Join(apps, ",") + `]}`
}

func fleetFailedAppList(host string) *cli.Result {
	return &cli.Result{ExitCode: 1, Stderr: "dial tcp " + host + ":22: i/o timeout"}
}

// D01 load-bearing assertion: an unreachable host STAYS VISIBLE — /api/fleet
// returns every configured server; the degraded one carries its error and
// freshness "unknown" (never observed), while healthy siblings stay fresh.
func TestFleetUnreachableServerStaysVisible(t *testing.T) {
	s := New(Config{
		DataDir:      t.TempDir(),
		NoAuth:       true,
		CLIInstalled: func() bool { return true },
		CLIRunner: fleetRunner(
			`{"alpha":{"host":"192.0.2.10"},"beta":{"host":"192.0.2.11"}}`,
			map[string]*cli.Result{
				"192.0.2.10": {Stdout: fleetAppListJSON("192.0.2.10", "site")},
				"192.0.2.11": fleetFailedAppList("192.0.2.11"),
			},
		),
	})

	appsResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(appsResponse, httptest.NewRequest(http.MethodGet, "/api/apps", nil))
	if appsResponse.Code != http.StatusOK {
		t.Fatalf("apps status=%d body=%s", appsResponse.Code, appsResponse.Body.String())
	}
	var appsEnvelope struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(appsResponse.Body.Bytes(), &appsEnvelope); err != nil {
		t.Fatal(err)
	}
	if len(appsEnvelope.Data) != 1 || appsEnvelope.Data[0]["server"] != "alpha" {
		t.Fatalf("apps = %#v", appsEnvelope.Data)
	}

	fleetResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(fleetResponse, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
	if fleetResponse.Code != http.StatusOK {
		t.Fatalf("fleet status=%d body=%s", fleetResponse.Code, fleetResponse.Body.String())
	}
	var fleet struct {
		Data struct {
			CollectedAt time.Time              `json:"collected_at"`
			Servers     []ServerObservationExt `json:"servers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(fleetResponse.Body.Bytes(), &fleet); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Data.Servers) != 2 {
		t.Fatalf("fleet must return ALL servers, got %d: %s", len(fleet.Data.Servers), fleetResponse.Body.String())
	}
	byServer := map[string]ServerObservationExt{}
	for _, obs := range fleet.Data.Servers {
		byServer[obs.Server] = obs
	}
	alpha, beta := byServer["alpha"], byServer["beta"]
	if alpha.Error != "" || alpha.Freshness != "fresh" || len(alpha.Apps) != 1 || alpha.Apps[0]["app"] != "site" {
		t.Fatalf("alpha envelope = %#v", alpha)
	}
	if beta.Error == "" || beta.Freshness != "unknown" || len(beta.Apps) != 0 {
		t.Fatalf("beta envelope = %#v", beta)
	}
	if beta.ID == "" || beta.ID == alpha.ID {
		t.Fatalf("beta envelope needs a distinct stable ID, got %q vs %q", beta.ID, alpha.ID)
	}
	if fleet.Data.CollectedAt.IsZero() {
		t.Fatal("fleet response needs a collection timestamp")
	}
}

// ServerObservationExt mirrors the served envelope for decoding in tests.
type ServerObservationExt struct {
	ID            string           `json:"id"`
	Server        string           `json:"server"`
	Host          string           `json:"host"`
	LastSuccessAt time.Time        `json:"last_success_at"`
	CollectedAt   time.Time        `json:"collected_at"`
	Freshness     string           `json:"freshness"`
	Error         string           `json:"error"`
	Source        string           `json:"source"`
	Apps          []map[string]any `json:"apps"`
}

// All healthy: every envelope fresh, no errors, apps present, and the stable
// ID is deterministic (same name -> same ID across sweeps).
func TestFleetAllHealthy(t *testing.T) {
	runner := fleetRunner(
		`{"alpha":{"host":"192.0.2.10"},"beta":{"host":"192.0.2.11"}}`,
		map[string]*cli.Result{
			"192.0.2.10": {Stdout: fleetAppListJSON("192.0.2.10", "site")},
			"192.0.2.11": {Stdout: fleetAppListJSON("192.0.2.11", "api")},
		},
	)
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true }, CLIRunner: runner,
	})

	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		s.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("fleet status=%d body=%s", response.Code, response.Body.String())
		}
		var fleet struct {
			Data struct {
				Servers []struct {
					ID            string           `json:"id"`
					Server        string           `json:"server"`
					Freshness     string           `json:"freshness"`
					Error         string           `json:"error"`
					LastSuccessAt time.Time        `json:"last_success_at"`
					Apps          []map[string]any `json:"apps"`
				} `json:"servers"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &fleet); err != nil {
			t.Fatal(err)
		}
		if len(fleet.Data.Servers) != 2 {
			t.Fatalf("servers = %d, want 2", len(fleet.Data.Servers))
		}
		for _, srv := range fleet.Data.Servers {
			if srv.Freshness != "fresh" || srv.Error != "" || len(srv.Apps) != 1 || srv.LastSuccessAt.IsZero() {
				t.Fatalf("healthy server envelope = %#v", srv)
			}
			if srv.ID != serverStableID(srv.Server) {
				t.Fatalf("ID %q is not the deterministic derivation for %q", srv.ID, srv.Server)
			}
		}
		// Second pass exercises the cached path (60s TTL, warm after sweep 1).
		s.fleet.mu.Lock()
		s.fleet.obsBuiltAt = time.Now().Add(-time.Hour)
		s.fleet.mu.Unlock()
	}
	if serverStableID("alpha") == serverStableID("beta") {
		t.Fatal("distinct names must derive distinct IDs")
	}
}

// Last-known retention: a sweep that fails for one server keeps that server's
// previous apps, last-success timestamp, and source, while successful
// siblings advance normally.
func TestFleetFailedSweepRetainsLastKnownApps(t *testing.T) {
	healthy := map[string]*cli.Result{
		"192.0.2.10": {Stdout: fleetAppListJSON("192.0.2.10", "site")},
		"192.0.2.11": {Stdout: fleetAppListJSON("192.0.2.11", "mail")},
	}
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner:    fleetRunner(`{"alpha":{"host":"192.0.2.10"},"beta":{"host":"192.0.2.11"}}`, healthy),
	})

	first, err := s.collectFleetObservations(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var betaBefore ServerObservation
	for _, env := range first {
		if env.Server == "beta" {
			betaBefore = env
		}
	}
	if len(betaBefore.Apps) != 1 || betaBefore.Apps[0].App != "mail" || betaBefore.LastSuccessAt.IsZero() {
		t.Fatalf("beta first sweep = %#v", betaBefore)
	}

	s.config.CLIRunner = fleetRunner(
		`{"alpha":{"host":"192.0.2.10"},"beta":{"host":"192.0.2.11"}}`,
		map[string]*cli.Result{
			"192.0.2.10": healthy["192.0.2.10"],
			"192.0.2.11": fleetFailedAppList("192.0.2.11"),
		},
	)
	s.runCLI = s.config.CLIRunner

	second, err := s.collectFleetObservations(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range second {
		switch env.Server {
		case "alpha":
			if env.Error != "" || len(env.Apps) != 1 || !env.LastSuccessAt.After(betaBefore.CollectedAt.Add(-time.Second)) {
				t.Fatalf("alpha second sweep = %#v", env)
			}
		case "beta":
			if env.Error == "" {
				t.Fatalf("beta second sweep must carry the probe error: %#v", env)
			}
			if len(env.Apps) != 1 || env.Apps[0].App != "mail" {
				t.Fatalf("beta must retain last-known apps: %#v", env)
			}
			if !env.LastSuccessAt.Equal(betaBefore.LastSuccessAt) {
				t.Fatalf("beta last-success must stay at the old observation: %v vs %v", env.LastSuccessAt, betaBefore.LastSuccessAt)
			}
			if env.Freshness != "" {
				t.Fatalf("freshness is filled at response time, not collection: %#v", env)
			}
		}
	}
}

// A slow host delays only itself: its probe is cut by the per-server timeout
// and the sweep completes well under timeout x 4, with the slow host's
// envelope carrying the error and the healthy sibling staying fresh.
func TestFleetSlowHostBoundedByPerServerTimeout(t *testing.T) {
	restore := fleetServerProbeTimeout
	fleetServerProbeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { fleetServerProbeTimeout = restore })

	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(ctx context.Context, args ...string) (*cli.Result, error) {
			switch strings.Join(args, " ") {
			case "server list --json":
				return &cli.Result{Stdout: `{"alpha":{"host":"192.0.2.10"},"beta":{"host":"192.0.2.11"}}`}, nil
			default:
				if args[3] == "192.0.2.11" {
					<-ctx.Done() // blackholing host: ignores the probe until cancelled
					return nil, ctx.Err()
				}
				return &cli.Result{Stdout: fleetAppListJSON("192.0.2.10", "site")}, nil
			}
		},
	})

	start := time.Now()
	envelopes, err := s.collectFleetObservations(context.Background(), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > fleetServerProbeTimeout*4 {
		t.Fatalf("sweep took %v; one slow host must not approach the fleet deadline", elapsed)
	}
	byServer := map[string]ServerObservation{}
	for _, env := range envelopes {
		byServer[env.Server] = env
	}
	if byServer["alpha"].Error != "" || len(byServer["alpha"].Apps) != 1 {
		t.Fatalf("alpha = %#v", byServer["alpha"])
	}
	if byServer["beta"].Error == "" || len(byServer["beta"].Apps) != 0 {
		t.Fatalf("beta = %#v", byServer["beta"])
	}
}

// The concurrency cap is actually applied: 16 servers x 100ms probes with a
// cap of 8 must run in at least two waves (>= ~200ms) while staying far from
// serial execution (~1.6s).
func TestFleetConcurrencyCapBoundsParallelProbes(t *testing.T) {
	const serversN = 16
	const probeDelay = 100 * time.Millisecond
	restore := fleetServerProbeTimeout
	fleetServerProbeTimeout = 10 * time.Second
	t.Cleanup(func() { fleetServerProbeTimeout = restore })

	serversJSON := []string{"{"}
	for i := 0; i < serversN; i++ {
		if i > 0 {
			serversJSON = append(serversJSON, ",")
		}
		serversJSON = append(serversJSON, fmt.Sprintf(`"srv%02d":{"host":"192.0.2.%d"}`, i, i))
	}
	serversJSON = append(serversJSON, "}")

	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: func(_ context.Context, args ...string) (*cli.Result, error) {
			if strings.Join(args, " ") == "server list --json" {
				return &cli.Result{Stdout: strings.Join(serversJSON, "")}, nil
			}
			time.Sleep(probeDelay)
			return &cli.Result{Stdout: fleetAppListJSON(args[3])}, nil
		},
	})

	start := time.Now()
	envelopes, err := s.collectFleetObservations(context.Background(), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(envelopes) != serversN {
		t.Fatalf("envelopes = %d, want %d", len(envelopes), serversN)
	}
	if minWaves := 2*probeDelay - 10*time.Millisecond; elapsed < minWaves {
		t.Fatalf("elapsed %v: cap of %d not applied (expected >= 2 waves)", elapsed, fleetMaxConcurrentProbes)
	}
	if serialFloor := time.Duration(serversN-2) * probeDelay; elapsed > serialFloor {
		t.Fatalf("elapsed %v: probes look serial, cap/parallelism broken", elapsed)
	}
}

// The live fleet has two lullmail apps with the same name on different
// servers (2026-09-19 duplicate-key bug): both must survive as distinct
// entries in the flat list AND stay scoped under their own servers in the
// envelopes.
func TestFleetDuplicateAppNamesAcrossServersStayDistinct(t *testing.T) {
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner: fleetRunner(
			`{"ovh":{"host":"192.0.2.10"},"nas":{"host":"192.0.2.11"}}`,
			map[string]*cli.Result{
				"192.0.2.10": {Stdout: fleetAppListJSON("192.0.2.10", "lullmail")},
				"192.0.2.11": {Stdout: fleetAppListJSON("192.0.2.11", "lullmail")},
			},
		),
	})

	appsResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(appsResponse, httptest.NewRequest(http.MethodGet, "/api/apps", nil))
	var appsEnvelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(appsResponse.Body.Bytes(), &appsEnvelope); err != nil {
		t.Fatal(err)
	}
	if len(appsEnvelope.Data) != 2 {
		t.Fatalf("flat list must keep both same-named apps, got %d", len(appsEnvelope.Data))
	}
	servers := map[string]bool{}
	for _, app := range appsEnvelope.Data {
		servers[app["server"].(string)] = true
	}
	if len(servers) != 2 {
		t.Fatalf("the two lullmail entries must sit on different servers, got %v", servers)
	}

	fleetResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(fleetResponse, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
	var fleet struct {
		Data struct {
			Servers []struct {
				Server string           `json:"server"`
				Apps   []map[string]any `json:"apps"`
			} `json:"servers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(fleetResponse.Body.Bytes(), &fleet); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Data.Servers) != 2 {
		t.Fatalf("envelopes = %d, want 2", len(fleet.Data.Servers))
	}
	for _, srv := range fleet.Data.Servers {
		if len(srv.Apps) != 1 || srv.Apps[0]["app"] != "lullmail" {
			t.Fatalf("server %q envelope apps = %#v", srv.Server, srv.Apps)
		}
	}
}
