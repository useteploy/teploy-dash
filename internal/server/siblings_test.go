package server

// siblings_test.go — Programme B item 1: the siblings envelope decode, the
// probe state machine (real httptest servers behind the monitor network
// guard), the sweep's cache honesty, and the nav fill from CONFIRMED docs
// only.

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
	"github.com/useteploy/teploy-dash/internal/remote"
)

const siblingsFixtureMixed = `{
  "machine_interface": 2,
  "server": "infra-home",
  "host": "192.0.2.10",
  "observed_at": "2026-11-05T12:00:00Z",
  "state": "ok",
  "siblings": [
    {"container":"dash-web-1a2b3c4","product":"dash","version":"v0.3.1","state":"running","image":"ghcr.io/useteploy/dash:v0.3.1",
     "port_hints":[{"host_ip":"127.0.0.1","host_port":49221,"container_port":3000,"proto":"tcp"},
                   {"host_ip":"::","host_port":49221,"container_port":3000,"proto":"tcp"}],"source":"image_label"},
    {"container":"observe-web-7cf98cae","product":"observe","version":"7cf98cae","app":"observe","state":"running","image":"ghcr.io/useteploy/observe:7cf98cae",
     "port_hints":[{"host_ip":"100.108.123.49","host_port":3000,"container_port":3000,"proto":"tcp"}],"source":"label"},
    {"container":"observe-worker-7cf98cae","product":"observe","version":"7cf98cae","app":"observe","state":"running","image":"ghcr.io/useteploy/observe:7cf98cae",
     "port_hints":[],"source":"label"},
    {"container":"forgejo","product":"forgejo","version":"11.0.1","state":"running","image":"codeberg.org/forgejo/forgejo:11",
     "port_hints":[{"host_ip":"127.0.0.1","host_port":3000,"container_port":3000,"proto":"tcp"}],"source":"oci_label"}
  ],
  "unknown_labeled": [
    {"container":"lullmail-web-9f8e7d6","product":"lullmail","version":"9f8e7d6","app":"lullmail","state":"running","image":"ghcr.io/fylun/lullmail:latest",
     "port_hints":[{"host_ip":"0.0.0.0","host_port":8080,"container_port":8080,"proto":"tcp"}],"source":"label"}
  ]
}`

func TestDecodeSiblingsEnvelopeAcceptsValidAndRefusesIncomplete(t *testing.T) {
	env, err := decodeSiblingsEnvelope(siblingsFixtureMixed)
	if err != nil {
		t.Fatalf("valid fixture must decode: %v", err)
	}
	if env.Server != "infra-home" || env.State != "ok" || len(env.Siblings) != 4 || len(env.UnknownLabeled) != 1 {
		t.Fatalf("mixed fixture decoded wrong: %+v", env)
	}
	for _, bad := range []string{
		`{"machine_interface":2,"server":"","observed_at":"2026-11-05T12:00:00Z","state":"ok","siblings":[],"unknown_labeled":[]}`,
		`{"machine_interface":2,"server":"s","observed_at":"2026-11-05T12:00:00Z","state":"sleeping","siblings":[],"unknown_labeled":[]}`,
		`{"machine_interface":2,"server":"s","observed_at":"2026-11-05T12:00:00Z","state":"ok","siblings":null,"unknown_labeled":[]}`,
		`{"machine_interface":2,"server":"s","observed_at":"2026-11-05T12:00:00Z","state":"ok","siblings":[{"container":"x","product":"","state":"running","image":"i","port_hints":[],"source":"label"}],"unknown_labeled":[]}`,
		`not json`,
	} {
		if _, err := decodeSiblingsEnvelope(bad); err == nil {
			t.Fatalf("incomplete envelope must be refused: %s", bad)
		}
	}
}

func TestGroupSiblingRowsFoldsContainersPerProduct(t *testing.T) {
	var env siblingsEnvelope
	if err := json.Unmarshal([]byte(siblingsFixtureMixed), &env); err != nil {
		t.Fatal(err)
	}
	rows := append(append([]siblingRow{}, env.Siblings...), env.UnknownLabeled...)
	groups := groupSiblingRows(rows)
	if len(groups) != 4 { // dash, observe (2 containers), forgejo, lullmail
		t.Fatalf("groups = %d, want 4: %+v", len(groups), groups)
	}
	var observe *siblingGroup
	for i, g := range groups {
		if g.product == "observe" {
			observe = &groups[i]
		}
	}
	if observe == nil || len(observe.containers) != 2 || observe.app != "observe" || observe.version != "7cf98cae" {
		t.Fatalf("observe group wrong: %+v", observe)
	}
	if len(observe.hints) != 1 {
		t.Fatalf("observe hints = %+v, want the one published bind", observe.hints)
	}
}

func TestSiblingDialTargetsClassifiesBinds(t *testing.T) {
	ctx := context.Background()
	hints := func(hhs ...siblingPortHint) []siblingPortHint { return hhs }
	ph := func(ip string, port int) siblingPortHint {
		return siblingPortHint{HostIP: ip, HostPort: port, Proto: "tcp"}
	}

	// Concrete public bind: probeable directly.
	targets, blocked, why := siblingDialTargets(ctx, hints(ph("192.0.2.10", 3000)), "")
	if len(targets) != 1 || targets[0] != "192.0.2.10:3000" || blocked || why != "" {
		t.Fatalf("concrete target = %v blocked=%v why=%q", targets, blocked, why)
	}

	// Tailnet literal: the guard refuses without allow_internal.
	targets, blocked, _ = siblingDialTargets(ctx, hints(ph("100.108.123.49", 3000)), "")
	if len(targets) != 1 || !blocked {
		t.Fatalf("tailnet target = %v blocked=%v, want blocked", targets, blocked)
	}

	// Loopback-only: dash cannot reach the server's loopback.
	targets, _, why = siblingDialTargets(ctx, hints(ph("127.0.0.1", 3000)), "")
	if len(targets) != 0 || why == "" {
		t.Fatalf("loopback-only = %v why=%q, want not probeable with a reason", targets, why)
	}

	// Wildcard bind: the server's own address joins at the hinted port.
	targets, blocked, _ = siblingDialTargets(ctx, hints(ph("0.0.0.0", 8080)), "203.0.113.7")
	if len(targets) != 1 || targets[0] != "203.0.113.7:8080" || blocked {
		t.Fatalf("wildcard target = %v blocked=%v", targets, blocked)
	}

	// UDP hints are not HTTP candidates.
	udp := siblingPortHint{HostIP: "192.0.2.10", HostPort: 53, Proto: "udp"}
	targets, _, _ = siblingDialTargets(ctx, hints(udp), "")
	if len(targets) != 0 {
		t.Fatalf("udp target = %v, want none", targets)
	}
}

func TestProbeSiblingGroupsStates(t *testing.T) {
	old := siblingProbeTimeout
	siblingProbeTimeout = 2 * time.Second
	t.Cleanup(func() { siblingProbeTimeout = old })

	// All httptest servers bind loopback: the probing client runs with
	// allowInternal on (the admin's choice this surface exposes), and the
	// loopback targets reach the servers through the guard.
	confirmed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == teployDiscoveryPath {
			w.Write([]byte(`{"product":"observe","version":"0.2.0","instance_id":"9f2c0c0e","dashboard_url":"https://observe.example.org","ingest_url":"https://ingest.example.org","capabilities":["service_tokens"]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer confirmed.Close()
	notDiscoverable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notDiscoverable.Close()
	forgejo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == forgejoVersionPath {
			w.Write([]byte(`{"version":"11.0.1"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer forgejo.Close()

	tcpHint := func(addr string) siblingPortHint {
		h, p, _ := net.SplitHostPort(addr)
		var pp int
		fmt.Sscan(p, &pp)
		return siblingPortHint{HostIP: h, HostPort: pp, Proto: "tcp"}
	}
	// httptest binds loopback: present the binds as wildcards so the server
	// host (127.0.0.1) is the dial target — the allow_internal probe path.
	wildHint := func(addr string) siblingPortHint {
		h := tcpHint(addr)
		return siblingPortHint{HostIP: "0.0.0.0", HostPort: h.HostPort, Proto: "tcp"}
	}

	client := siblingHTTPClient(true)
	rows := []siblingRow{
		{Container: "observe-web", Product: "observe", State: "running", PortHints: []siblingPortHint{wildHint(confirmed.Listener.Addr().String())}},
		{Container: "ship-web", Product: "ship", State: "running", PortHints: []siblingPortHint{wildHint(notDiscoverable.Listener.Addr().String())}},
		{Container: "forgejo", Product: "forgejo", State: "running", PortHints: []siblingPortHint{wildHint(forgejo.Listener.Addr().String())}},
		{Container: "dead", Product: "dash", State: "running", PortHints: []siblingPortHint{wildHint("127.0.0.1:1")}},
		{Container: "lo", Product: "lullmail", State: "running", PortHints: []siblingPortHint{{HostIP: "127.0.0.1", HostPort: 8080, Proto: "tcp"}}},
	}
	probes := probeSiblingGroups(context.Background(), rows, "127.0.0.1", client, true, time.Now().UTC())
	byProduct := map[string]*siblingProbe{}
	for i, p := range probes {
		byProduct[p.Product] = probes[i]
	}
	if p := byProduct["observe"]; p.State != probeConfirmed || p.Doc == nil || p.Doc.InstanceID != "9f2c0c0e" || p.Version != "0.2.0" || p.BaseURL != confirmed.URL {
		t.Fatalf("observe probe = %+v, want confirmed with the doc", p)
	}
	if p := byProduct["ship"]; p.State != probeNotDiscoverable {
		t.Fatalf("ship probe = %+v, want found_not_discoverable (discovery default-off)", p)
	}
	if p := byProduct["forgejo"]; p.State != probeConfirmed || p.Version != "11.0.1" || p.Doc != nil {
		t.Fatalf("forgejo probe = %+v, want confirmed via /api/v1/version with no doc", p)
	}
	if p := byProduct["dash"]; p.State != probeUnreachable {
		t.Fatalf("dead probe = %+v, want unreachable", p)
	}
	if p := byProduct["lullmail"]; p.State != probeNotProbeable {
		t.Fatalf("loopback probe = %+v, want not_probeable", p)
	}
}

// The same candidates behind the guard WITHOUT allow_internal: the tailnet
// target is classified blocked before any dial, with the remedy in the error.
func TestProbeSiblingGroupsBlocksInternalWithoutConsent(t *testing.T) {
	rows := []siblingRow{
		{Container: "observe-web", Product: "observe", State: "running", PortHints: []siblingPortHint{{HostIP: "100.108.123.49", HostPort: 3000, Proto: "tcp"}}},
	}
	probes := probeSiblingGroups(context.Background(), rows, "", siblingHTTPClient(false), false, time.Now().UTC())
	if len(probes) != 1 || probes[0].State != probeBlockedInternal {
		t.Fatalf("probe = %+v, want blocked_internal", probes)
	}
	if !strings.Contains(probes[0].Error, "allow internal") {
		t.Fatalf("blocked error must name the remedy: %q", probes[0].Error)
	}
}

// ── the sweep and the API ─────────────────────────────────────────────────────

type siblingsTestServer struct {
	*Server
	runner *configRunner
}

func newSiblingsTestServer(t *testing.T, envelope string) *siblingsTestServer {
	t.Helper()
	runner := &configRunner{tokens: []string{"siblings-v1", "server-audit-v1"}}
	runner.reply = func(args []string, _ string) (*cli.Result, error) {
		if strings.HasPrefix(strings.Join(args, " "), "server siblings ") {
			return &cli.Result{Stdout: envelope}, nil
		}
		return &cli.Result{Stdout: "{}"}, nil
	}
	s := New(Config{
		DataDir:      t.TempDir(),
		NoAuth:       true,
		CLIInstalled: func() bool { return true },
		CLIRunner:    runner.run,
		CLIIdentity:  func() string { return "test-cli" },
	})
	return &siblingsTestServer{Server: s, runner: runner}
}

func TestSiblingsSweepCachesAndProbes(t *testing.T) {
	old := siblingProbeTimeout
	siblingProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { siblingProbeTimeout = old })
	s := newSiblingsTestServer(t, siblingsFixtureMixed)
	s.collectSiblings(context.Background())

	s.siblings.mu.Lock()
	rec := s.siblings.snap.Servers["prod"]
	s.siblings.mu.Unlock()
	if rec == nil || rec.State != "ok" {
		t.Fatalf("record = %+v, want state ok", rec)
	}
	if len(rec.Siblings) != 4 || len(rec.UnknownLabeled) != 1 {
		t.Fatalf("rows = %d/%d, want 4 siblings + 1 unknown", len(rec.Siblings), len(rec.UnknownLabeled))
	}
	// Four products across the two lists (observe's two containers group);
	// each gets exactly one probe.
	if len(rec.Probes) != 4 {
		t.Fatalf("probes = %d, want 4", len(rec.Probes))
	}
	byProduct := map[string]string{}
	for _, p := range rec.Probes {
		byProduct[p.Product] = p.State
		if p.Server != "prod" {
			t.Fatalf("probe %s server = %q, want prod", p.Product, p.Server)
		}
	}
	// Honest outcomes for the fixture's addresses (allow_internal off):
	// observe on the tailnet → blocked; dash on TEST-NET (wildcard fallback
	// via server host 192.0.2.10) → unreachable; forgejo loopback-only →
	// not probeable; lullmail wildcard on the server host → unreachable.
	if byProduct["observe"] != probeBlockedInternal {
		t.Fatalf("observe = %s, want blocked_internal", byProduct["observe"])
	}
	if byProduct["forgejo"] != probeNotProbeable {
		t.Fatalf("forgejo = %s, want not_probeable", byProduct["forgejo"])
	}
	if byProduct["dash"] != probeUnreachable || byProduct["lullmail"] != probeUnreachable {
		t.Fatalf("dash=%s lullmail=%s, want unreachable (TEST-NET addresses answer nothing here)", byProduct["dash"], byProduct["lullmail"])
	}
}

// A failed sweep keeps the previous candidates instead of clearing the page.
func TestSiblingsSweepFailureKeepsLastReport(t *testing.T) {
	old := siblingProbeTimeout
	siblingProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { siblingProbeTimeout = old })
	s := newSiblingsTestServer(t, siblingsFixtureMixed)
	s.collectSiblings(context.Background())

	s.runner.reply = func(args []string, _ string) (*cli.Result, error) {
		return &cli.Result{ExitCode: 255, Stderr: "dial tcp: connection refused"}, nil
	}
	// resolveServers uses `server list` (handled by the runner core), so the
	// sweep reaches the failing siblings call.
	s.collectSiblings(context.Background())

	s.siblings.mu.Lock()
	defer s.siblings.mu.Unlock()
	rec := s.siblings.snap.Servers["prod"]
	if rec == nil {
		t.Fatal("the failed sweep must keep the server's record")
	}
	if rec.State != "unreachable" || !strings.Contains(rec.Error, "connection refused") {
		t.Fatalf("record = %+v, want unreachable with the sanitized error", rec)
	}
	if len(rec.Siblings) != 4 || len(rec.Probes) != 4 {
		t.Fatalf("failed sweep cleared the page: %d rows / %d probes", len(rec.Siblings), len(rec.Probes))
	}
}

func TestSiblingsViewIsHonest(t *testing.T) {
	old := siblingProbeTimeout
	siblingProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { siblingProbeTimeout = old })
	s := newSiblingsTestServer(t, siblingsFixtureMixed)
	s.collectSiblings(context.Background())

	view := s.siblingsView(context.Background(), time.Now().UTC())
	if view["cli_supported"] != true {
		t.Fatalf("cli_supported = %v, want true (siblings-v1 advertised)", view["cli_supported"])
	}
	cands := view["candidates"].([]siblingCandidateView)
	if len(cands) != 4 {
		t.Fatalf("candidates = %d, want 4", len(cands))
	}
	recognizedLullmail := false
	for _, c := range cands {
		if c.Product == "lullmail" {
			recognizedLullmail = c.Recognized
		}
	}
	if recognizedLullmail {
		t.Fatal("lullmail comes from unknown_labeled and must render as unrecognized, not vanish")
	}
}

func TestSiblingsSettingsRequiresAdminToFlip(t *testing.T) {
	s := newSiblingsTestServer(t, siblingsFixtureMixed)

	// NoAuth mode treats the operator as the admin: the flip works and
	// persists.
	body := strings.NewReader(`{"allow_internal":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/siblings/settings", body)
	rec := httptest.NewRecorder()
	s.handleSiblingsSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-auth flip = %d: %s", rec.Code, rec.Body.String())
	}
	if !s.siblingsAllowInternal() {
		t.Fatal("allow_internal must persist")
	}
}

func TestSiblingsScanRefusesConcurrentSweeps(t *testing.T) {
	s := newSiblingsTestServer(t, siblingsFixtureMixed)
	if !s.siblings.sweepInFlight.TryLock() {
		t.Fatal("precondition: lock free")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/siblings/scan", nil)
	rec := httptest.NewRecorder()
	s.handleSiblingsScan(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second scan = %d, want 409", rec.Code)
	}
	s.siblings.sweepInFlight.Unlock()
}

// ── nav ──────────────────────────────────────────────────────────────────────

func publishSiblingsProbe(s *Server, server string, probes ...*siblingProbe) {
	s.siblings.mu.Lock()
	defer s.siblings.mu.Unlock()
	rec := s.siblings.snap.Servers[server]
	if rec == nil {
		rec = &siblingsServerRecord{Server: server, Siblings: []siblingRow{}, UnknownLabeled: []siblingRow{}, Probes: []*siblingProbe{}}
		s.siblings.snap.Servers[server] = rec
	}
	rec.Probes = append(rec.Probes, probes...)
}

func TestNavFillsFromConfirmedDiscoveryDoc(t *testing.T) {
	s := New(Config{DataDir: t.TempDir(), NoAuth: true})
	publishFleetApps(s, []remote.AppState{
		{App: "observe", Server: "infra", Domain: "observe.acme.com", CurrentPort: 3000, Status: "running"},
	})
	publishSiblingsProbe(s, "infra", &siblingProbe{
		Server: "infra", Product: "observe", State: probeConfirmed,
		Doc: &siblingDiscoveryDoc{Product: "observe", DashboardURL: "https://observe.doc-win.org"}, ProbedAt: time.Now(),
	})
	if got := navURL(s.teployNav("dash"), "observe"); got != "https://observe.doc-win.org" {
		t.Fatalf("observe url = %q, want the confirmed doc's dashboard_url", got)
	}
}

// A doc with an empty dashboard_url is an unknown, never a guessed link: the
// nav falls through to the fleet-name derivation instead.
func TestNavIgnoresEmptyDashboardURL(t *testing.T) {
	s := New(Config{DataDir: t.TempDir(), NoAuth: true})
	publishFleetApps(s, []remote.AppState{
		{App: "observe", Server: "infra", Domain: "observe.acme.com", CurrentPort: 3000, Status: "running"},
	})
	publishSiblingsProbe(s, "infra", &siblingProbe{
		Server: "infra", Product: "observe", State: probeConfirmed,
		Doc: &siblingDiscoveryDoc{Product: "observe"}, ProbedAt: time.Now(),
	})
	if got := navURL(s.teployNav("dash"), "observe"); got != "https://observe.acme.com" {
		t.Fatalf("observe url = %q, want the fleet fallback (doc had no dashboard_url)", got)
	}
}

// Ship ships discovery default-off: a found-not-discoverable probe fills
// nothing; an env URL still wins over everything.
func TestNavSkipsUnconfirmedProbes(t *testing.T) {
	s := New(Config{DataDir: t.TempDir(), NoAuth: true})
	publishSiblingsProbe(s, "infra",
		&siblingProbe{Server: "infra", Product: "ship", State: probeNotDiscoverable, ProbedAt: time.Now()},
		&siblingProbe{Server: "infra", Product: "observe", State: probeUnreachable, ProbedAt: time.Now()},
	)
	if got := navURL(s.teployNav("dash"), "ship"); got != "<absent>" {
		t.Fatalf("ship url = %q, want absent (no confirmed doc)", got)
	}
	if got := navURL(s.teployNav("dash"), "observe"); got != "<absent>" {
		t.Fatalf("observe url = %q, want absent (probe unreachable)", got)
	}
}

func TestNavAppendsConfirmedForgejoLink(t *testing.T) {
	s := New(Config{DataDir: t.TempDir(), NoAuth: true})
	publishSiblingsProbe(s, "infra", &siblingProbe{
		Server: "infra", Product: "forgejo", State: probeConfirmed, BaseURL: "http://203.0.113.9:3000", Version: "11.0.1", ProbedAt: time.Now(),
	})
	if got := navURL(s.teployNav("dash"), "forgejo"); got != "http://203.0.113.9:3000" {
		t.Fatalf("forgejo url = %q, want the confirmed base URL as a plain link", got)
	}
}
