package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/remote"
)

// D01 fleet observation slice. The fleet view must tell the truth: every
// configured server is present in every response, an unreachable host shows
// its error plus its last-known apps instead of vanishing, and one slow host
// delays only itself. Audit: A37/R49 (this slice), R40 (inventory envelopes).

const (
	// fleetMaxConcurrentProbes caps how many servers are probed in parallel so
	// a large fleet cannot open an unbounded SSH/CLI subprocess burst.
	fleetMaxConcurrentProbes = 8

	// fleetFreshAfter is the freshness threshold: a server reads "fresh" when
	// its last SUCCESSFUL observation is within this window of now, "stale"
	// when older, "unknown" when it has never been successfully observed.
	// Aligned with observationStaleAfter (machine.go), which governs the
	// per-resource ObservedAt fields.
	fleetFreshAfter = 2 * time.Minute

	// fleetSweepTimeout bounds one whole sweep, including server discovery
	// (R47: the deadline is applied before discovery runs).
	fleetSweepTimeout = 30 * time.Second
)

// fleetServerProbeTimeout bounds each server's probe so a slow or
// blackholing host delays only itself, not the sweep. A var (not const) so
// tests can tighten it; production never overrides it.
var fleetServerProbeTimeout = 10 * time.Second

// ServerObservation is one server's envelope in the fleet response. Apps is
// the last-known payload: the current sweep's apps on success, the previous
// envelope's apps when this sweep failed.
type ServerObservation struct {
	ID            string            `json:"id"`
	Server        string            `json:"server"`
	Host          string            `json:"host"`
	LastSuccessAt time.Time         `json:"last_success_at,omitempty"`
	CollectedAt   time.Time         `json:"collected_at"`
	Freshness     string            `json:"freshness"` // fresh|stale|unknown, filled at response time
	Error         string            `json:"error,omitempty"`
	Source        string            `json:"source,omitempty"`
	Apps          []remote.AppState `json:"apps"`
}

// serverStableID derives the envelope's internal opaque ID. Server NAMES are
// the only identity the CLI's server list exposes today, so the ID is a
// deterministic hash of the name: same name -> same ID across restarts and
// responses. Renaming a server changes its ID; a truly stable cross-rename
// identity needs the server-scoped AppRef schema migration in the CLI
// contract (audit A35) and is out of scope for this slice. The domain prefix
// keeps these IDs out of any other ID space.
func serverStableID(name string) string {
	sum := sha256.Sum256([]byte("teploy-dash/server/v1:" + name))
	return "srv-" + hex.EncodeToString(sum[:8])
}

// freshnessAt classifies a last-success timestamp against fleetFreshAfter.
func freshnessAt(lastSuccess, now time.Time) string {
	if lastSuccess.IsZero() {
		return "unknown"
	}
	if now.Sub(lastSuccess) > fleetFreshAfter {
		return "stale"
	}
	return "fresh"
}

// collectFleetObservations sweeps every configured server under bounded
// concurrency with a per-server timeout, and returns one envelope per server
// — successful or not. prev carries the previous sweep's envelopes so a
// failed probe keeps the server's last-known apps, last success time, and
// source. An error is returned only when the sweep cannot know the server
// set at all (discovery failure) or the local fallback read fails.
func (s *Server) collectFleetObservations(ctx context.Context, prev []ServerObservation) ([]ServerObservation, error) {
	// R47: the sweep deadline covers discovery too.
	ctx, cancel := context.WithTimeout(ctx, fleetSweepTimeout)
	defer cancel()

	servers, err := s.resolveServers(ctx)
	if err != nil {
		// A discovery failure must not fall through to the local-state path
		// (A34) — it would render a broken fleet as "local installation".
		return nil, fmt.Errorf("server discovery failed: %w", err)
	}

	if len(servers) == 0 {
		// Fall back to local state files when no servers configured. R49
		// (partial): a failed local-state read is an error, not an empty
		// success — the deployment list would silently lose every local app.
		localApps, err := s.state.ListApps()
		if err != nil {
			return nil, fmt.Errorf("local deployment state unavailable: %w", err)
		}
		now := time.Now().UTC()
		apps := make([]remote.AppState, 0, len(localApps))
		for _, a := range localApps {
			apps = append(apps, remote.AppState{
				App:          a.App,
				Server:       "local",
				Domain:       a.Domain,
				CurrentHash:  a.CurrentHash,
				PreviousHash: a.PreviousHash,
				Status:       a.Status,
			})
		}
		return []ServerObservation{{
			ID:            serverStableID("local"),
			Server:        "local",
			LastSuccessAt: now,
			CollectedAt:   now,
			Source:        "local",
			Apps:          apps,
		}}, nil
	}

	// Deterministic output order: resolveServers returns a map.
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })

	prevByID := make(map[string]ServerObservation, len(prev))
	for _, env := range prev {
		prevByID[env.ID] = env
	}

	envelopes := make([]ServerObservation, len(servers))
	workers := fleetMaxConcurrentProbes
	if workers > len(servers) {
		workers = len(servers)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				srv := servers[i]
				probeCtx, probeCancel := context.WithTimeout(ctx, fleetServerProbeTimeout)
				apps, err := s.readMachineApps(probeCtx, srv)
				probeCancel()
				envelopes[i] = buildServerObservation(prevByID, srv, apps, err, time.Now().UTC())
			}
		}()
	}
	for i := range servers {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	return envelopes, nil
}

// buildServerObservation merges one probe result with the server's previous
// envelope: success replaces the payload and advances the last-success time;
// failure keeps the last-known payload and surfaces the error.
func buildServerObservation(prevByID map[string]ServerObservation, srv remote.ServerConn, apps []remote.AppState, err error, collectedAt time.Time) ServerObservation {
	env := ServerObservation{
		ID:          serverStableID(srv.Name),
		Server:      srv.Name,
		Host:        srv.Host,
		CollectedAt: collectedAt,
	}
	if err != nil {
		env.Error = err.Error()
		if prev, ok := prevByID[env.ID]; ok {
			env.Apps = nonNilApps(prev.Apps)
			env.LastSuccessAt = prev.LastSuccessAt
			env.Source = prev.Source
		} else {
			env.Apps = []remote.AppState{}
		}
		return env
	}
	env.LastSuccessAt = collectedAt
	env.Apps = nonNilApps(apps)
	if len(env.Apps) > 0 {
		env.Source = env.Apps[0].Source
	}
	return env
}

// fleetResponseView fills the response-time fields (freshness) and the sweep
// collection timestamp for /api/fleet.
type fleetResponseView struct {
	CollectedAt time.Time           `json:"collected_at"`
	Servers     []ServerObservation `json:"servers"`
}

func buildFleetResponse(envelopes []ServerObservation, now time.Time) fleetResponseView {
	servers := make([]ServerObservation, 0, len(envelopes))
	var collected time.Time
	for _, env := range envelopes {
		env.Freshness = freshnessAt(env.LastSuccessAt, now)
		if env.CollectedAt.After(collected) {
			collected = env.CollectedAt
		}
		servers = append(servers, env)
	}
	return fleetResponseView{CollectedAt: collected, Servers: servers}
}

// flattenFleetApps projects the envelopes onto the flat app list the existing
// /api/apps consumers expect: only this sweep's successful observations
// contribute, preserving the endpoint's current contract.
func flattenFleetApps(envelopes []ServerObservation) []remote.AppState {
	var apps []remote.AppState
	for _, env := range envelopes {
		if env.Error == "" {
			apps = append(apps, env.Apps...)
		}
	}
	return apps
}

// fleetAllFailed reports whether every server errored in this sweep.
func fleetAllFailed(envelopes []ServerObservation) bool {
	if len(envelopes) == 0 {
		return false
	}
	for _, env := range envelopes {
		if env.Error == "" {
			return false
		}
	}
	return true
}

// collectFleetApps preserves the historical flat-apps contract on top of the
// envelope sweep, including the all-servers-failed error.
func (s *Server) collectFleetApps(ctx context.Context) ([]remote.AppState, error) {
	envelopes, err := s.collectFleetObservations(ctx, s.fleet.snapshotObservations())
	if err != nil {
		return nil, err
	}
	return fleetAppsOrError(envelopes)
}

// fleetAppsOrError projects a sweep onto the flat app-list contract: current
// sweep's successful apps, with the historical all-servers-failed error.
func fleetAppsOrError(envelopes []ServerObservation) ([]remote.AppState, error) {
	apps := flattenFleetApps(envelopes)
	if len(apps) == 0 && fleetAllFailed(envelopes) {
		return nil, fmt.Errorf("fleet app collection failed for all %d server(s)", len(envelopes))
	}
	return apps, nil
}

func nonNilApps(apps []remote.AppState) []remote.AppState {
	if apps == nil {
		return []remote.AppState{}
	}
	return apps
}
