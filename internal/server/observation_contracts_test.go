package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/remote"
)

// X02 S6 contracts corpus generator for the observation envelope — the
// dash-owned artifact in teploy-cli's corpus (MANIFEST: "PRODUCER =
// teploy-dash"). Mirrors teploy-cli's contracts_golden_test discipline:
// fixtures are generated from the REAL dash constructors
// (collectFleetObservations' building blocks) and fail on drift against the
// committed corpus. Set TEPLOY_UPDATE_CONTRACTS=1 to rewrite after a
// deliberate contract change; commit the corpus diff WITH the code change
// and bump contracts/MANIFEST.md.
//
// The corpus lives in the sibling teploy-cli checkout (the workspace and
// CI check both repos out side by side); override with TEPLOY_CONTRACTS_DIR.
// On a bare clone without the sibling the cross-repo pin cannot run and the
// test says so — the envelope's invariants are separately pinned by the
// non-skipping fleet tests in this package.

func observationContractsDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("TEPLOY_CONTRACTS_DIR"); dir != "" {
		return dir
	}
	candidate := "../../../teploy-cli/contracts"
	if _, err := os.Stat(candidate); err != nil {
		t.Skipf("contracts corpus not found at %s (set TEPLOY_CONTRACTS_DIR or check out teploy-cli beside teploy-dash)", candidate)
	}
	return candidate
}

func writeObservationFixture(t *testing.T, dir, path string, v any) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	full := filepath.Join(dir, "fixtures", path)
	if os.Getenv("TEPLOY_UPDATE_CONTRACTS") == "1" {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
		return
	}
	want, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("fixture %s missing (run with TEPLOY_UPDATE_CONTRACTS=1 to seed): %v", path, err)
	}
	if !bytes.Equal(bytes.TrimRight(want, "\n"), bytes.TrimRight(buf.Bytes(), "\n")) {
		t.Errorf("fixture %s drifted from the committed corpus - regenerate deliberately (TEPLOY_UPDATE_CONTRACTS=1), commit the diff WITH the code change, and bump contracts/MANIFEST.md", path)
	}
}

// envelopeFixtures builds the §2.4 acceptance set through the production
// constructors: fresh, stale, unknown (unreachable, never observed), and
// the critical unreachable-with-last-known merge.
func envelopeFixtures(now time.Time) (fresh, stale, unknown, unreachableLastKnown canonicalObservation) {
	up := remote.AppState{App: "web", Server: "prod", Domain: "web.example.com", Status: "running", Source: "cli"}

	// Rule 1 + 5: a successful probe yields a fresh envelope.
	good := buildServerObservation(nil, remote.ServerConn{Name: "prod", ID: "srv-aaaaaaaaaaaaaaaa", Host: "10.0.0.1"}, []remote.AppState{up}, nil, now)
	fresh = canonicalObservationFrom(buildFleetResponse([]ServerObservation{good}, now).Servers[0])

	// Rule 2: last success older than the declared threshold reads stale,
	// never fresh.
	staleEnv := good
	staleEnv.LastSuccessAt = now.Add(-3 * time.Minute)
	staleEnv.CollectedAt = now
	stale = canonicalObservationFrom(buildFleetResponse([]ServerObservation{staleEnv}, now).Servers[0])

	// Rule 4: an unreachable host that has NEVER answered — unknown, error
	// is data, the envelope still exists (rule 1).
	never := buildServerObservation(nil, remote.ServerConn{Name: "edge", ID: "srv-bbbbbbbbbbbbbbbb", Host: "10.0.0.2"}, nil, fmt.Errorf("dial tcp 10.0.0.2:22: connect: connection refused"), now)
	never.Freshness = ""
	unknownEnv := buildFleetObservationForResponse(never, now)
	unknown = canonicalObservationFrom(unknownEnv)

	// Rule 3: an unreachable host whose PREVIOUS envelope carries the
	// last-known payload — merged by id, not name. The previous success is
	// past the freshness threshold, so the merged envelope reads stale:
	// freshness is last-success age, never the probe's own outcome.
	older := good
	older.LastSuccessAt = now.Add(-3 * time.Minute)
	prevByID := map[string]ServerObservation{"srv-aaaaaaaaaaaaaaaa": older}
	failed := buildServerObservation(prevByID, remote.ServerConn{Name: "prod", ID: "srv-aaaaaaaaaaaaaaaa", Host: "10.0.0.1"}, nil, fmt.Errorf("ssh: connect to host 10.0.0.1 port 22: timed out"), now)
	unreachableLastKnown = canonicalObservationFrom(buildFleetObservationForResponse(failed, now))
	return
}

// buildFleetObservationForResponse fills the response-time fields the
// production path fills in buildFleetResponse.
func buildFleetObservationForResponse(env ServerObservation, now time.Time) ServerObservation {
	env.Freshness = freshnessAt(env.LastSuccessAt, now)
	return env
}

func TestContractsObservationEnvelopeGolden(t *testing.T) {
	dir := observationContractsDir(t)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	fresh, stale, unknown, unreachableLastKnown := envelopeFixtures(now)

	writeObservationFixture(t, dir, "observation-envelope/valid/fresh.json", fresh)
	writeObservationFixture(t, dir, "observation-envelope/valid/stale.json", stale)
	writeObservationFixture(t, dir, "observation-envelope/valid/unknown-unreachable.json", unknown)
	writeObservationFixture(t, dir, "observation-envelope/valid/unreachable-last-known.json", unreachableLastKnown)

	// Structural self-check mirroring the corpus schema (2020-12): the
	// required keys and the tri-state enum hold on every emitted fixture.
	for name, c := range map[string]canonicalObservation{
		"fresh": fresh, "stale": stale, "unknown": unknown, "unreachable-last-known": unreachableLastKnown,
	} {
		if c.Resource.Type != "server" || c.Resource.ID == "" {
			t.Errorf("%s: resource kind/id missing: %+v", name, c.Resource)
		}
		if c.CollectedAt.IsZero() {
			t.Errorf("%s: collected_at required", name)
		}
		switch c.Freshness {
		case "fresh", "stale", "unknown":
		default:
			t.Errorf("%s: freshness %q outside the tri-state enum", name, c.Freshness)
		}
	}
	if fresh.Freshness != "fresh" {
		t.Errorf("fresh fixture freshness = %q", fresh.Freshness)
	}
	if stale.Freshness != "stale" {
		t.Errorf("stale fixture freshness = %q", stale.Freshness)
	}
	if unknown.Freshness != "unknown" || unknown.Error == "" {
		t.Errorf("unknown fixture = %+v", unknown)
	}
	// Rule 3's acceptance: the last-known payload survived the failed probe.
	if unreachableLastKnown.Error == "" || unreachableLastKnown.LastKnown.Apps == nil || len(unreachableLastKnown.LastKnown.Apps) != 1 {
		t.Errorf("unreachable-last-known fixture lost the merge: %+v", unreachableLastKnown)
	}
	if unreachableLastKnown.Freshness != "stale" {
		t.Errorf("last-known merge must read stale (last success in the past), got %q", unreachableLastKnown.Freshness)
	}
}
