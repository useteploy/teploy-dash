package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/remote"
	"github.com/useteploy/teploy-dash/internal/store"
)

var domainsNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

type rowOpt func(*domainCheckRow)

func domainRow(server, app, host string, opts ...rowOpt) domainCheckRow {
	r := domainCheckRow{Server: server, App: app, Host: host, Ingress: "caddy"}
	r.DNS.Status = "ok"
	r.DNS.Answers = []string{"192.0.2.10"}
	r.TLS.Status = "ok"
	r.Route.Kind = "managed"
	r.Route.Owner = app
	r.Route.UpstreamResolves = "yes"
	for _, o := range opts {
		o(&r)
	}
	return r
}

func withDNS(status string, answers ...string) rowOpt {
	return func(r *domainCheckRow) { r.DNS.Status, r.DNS.Answers = status, answers }
}

func withObtainFailing(since time.Time) rowOpt {
	return func(r *domainCheckRow) {
		n := 40
		r.TLS.Status, r.TLS.Failures, r.TLS.FailingSince = "failing", &n, &since
	}
}

func withRoute(kind string) rowOpt {
	return func(r *domainCheckRow) { r.Route.Kind = kind }
}

func withDeadUpstream(dial string) rowOpt {
	return func(r *domainCheckRow) {
		r.Route.UpstreamResolves = "no"
		r.Route.Upstreams = append(r.Route.Upstreams, struct {
			Dial     string `json:"dial"`
			Resolves string `json:"resolves"`
		}{dial, "no"})
	}
}

func snapshotOf(now time.Time, rows ...domainCheckRow) *domainSnapshot {
	snap := &domainSnapshot{Servers: map[string]*domainServerRecord{}, Adopted: map[string]bool{}}
	for _, row := range rows {
		rec := snap.Servers[row.Server]
		if rec == nil {
			rec = &domainServerRecord{Server: row.Server, State: "ok", LastAttemptAt: now, LastSuccessAt: now, Caddy: domainCaddy{Caddyfile: "present"}}
			snap.Servers[row.Server] = rec
		}
		rec.Domains = append(rec.Domains, row)
	}
	return snap
}

func rowFor(t *testing.T, rows []domainRowView, host string) domainRowView {
	t.Helper()
	for _, r := range rows {
		if r.Host == host {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", host, rows)
	return domainRowView{}
}

func TestDomainRows_NameTheFirstFailingLayer(t *testing.T) {
	now := domainsNow
	expires := now.Add(5 * 24 * time.Hour)
	far := now.Add(60 * 24 * time.Hour)
	snap := snapshotOf(now,
		domainRow("edge", "a", "healthy.dev"),
		domainRow("edge", "b", "nodns.dev", withDNS("nxdomain")),
		domainRow("edge", "c", "moved.dev", withDNS("wrong_target", "198.51.100.9")),
		domainRow("edge", "d", "proxied.dev", withDNS("proxied", "104.16.0.1")),
		domainRow("edge", "e", "stale.dev", withRoute("stale_managed"), withObtainFailing(now.Add(-14*24*time.Hour))),
		domainRow("edge", "f", "dead.dev", withDeadUpstream("f-web-old:3000")),
		domainRow("edge", "g", "cert.dev", withObtainFailing(now.Add(-14*24*time.Hour))),
		domainRow("edge", "h", "newfail.dev", withObtainFailing(now.Add(-2*time.Hour))),
		domainRow("edge", "i", "expiring.dev"),
		domainRow("edge", "j", "down.dev"),
		domainRow("edge", "k", "label.dev", func(r *domainCheckRow) {
			r.Skip, r.Ingress, r.Route.Kind, r.DNS.Status = "host_label", "host", "none", "skipped"
		}),
	)
	monitors := map[string]domainMonitorEvidence{
		"expiring.dev": {monitor: store.Monitor{ID: "m1"}, known: true, result: store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: now, CertNotAfter: &expires}},
		"down.dev":     {monitor: store.Monitor{ID: "m2"}, known: true, result: store.CheckResult{Status: "down", Message: "unexpected status 502", CheckedAt: now, CertNotAfter: &far}},
		"healthy.dev":  {monitor: store.Monitor{ID: "m3"}, known: true, result: store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: now, CertNotAfter: &far}},
	}
	rows := buildDomainRows(snap, monitors, now)

	cases := []struct {
		host, overall, first, evidence string
	}{
		{"healthy.dev", layerOK, "", ""},
		{"nodns.dev", layerFail, "dns", "no DNS record"},
		{"moved.dev", layerFail, "dns", "DNS points at 198.51.100.9, not edge"},
		{"proxied.dev", layerOK, "", ""},
		{"stale.dev", layerFail, "route", "stale route: e is no longer routed by Caddy"},
		{"dead.dev", layerFail, "route", "route points at a container that no longer exists (f-web-old:3000)"},
		{"cert.dev", layerFail, "cert", "certificate failing for 14 days"},
		{"newfail.dev", layerWarn, "cert", "certificate failing for 2 hours"},
		{"expiring.dev", layerWarn, "cert", "certificate expires in 5 days"},
		{"down.dev", layerFail, "http", "unexpected status 502"},
		{"label.dev", layerNA, "", "label only: ingress: host"},
	}
	for _, c := range cases {
		r := rowFor(t, rows, c.host)
		if r.Overall != c.overall || r.FirstFailing != c.first || !strings.Contains(r.Evidence, c.evidence) {
			t.Errorf("%s: overall=%s first=%q evidence=%q; want %s %q %q", c.host, r.Overall, r.FirstFailing, r.Evidence, c.overall, c.first, c.evidence)
		}
	}
	if r := rowFor(t, rows, "healthy.dev"); r.AgeSeconds == nil || *r.AgeSeconds != 0 || r.CheckedAt == nil {
		t.Errorf("the row must carry its evidence age: %+v", r)
	}
}

// A monitored domain no teploy server serves still gets its row: expiry and HTTP
// come from the monitor, the rest reads not applicable rather than healthy.
func TestDomainRows_MonitorOnlyDomains(t *testing.T) {
	now := domainsNow
	soon := now.Add(3 * 24 * time.Hour)
	monitors := map[string]domainMonitorEvidence{
		"elsewhere.dev": {monitor: store.Monitor{ID: "m"}, known: true, result: store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: now, CertNotAfter: &soon}},
		"fleet.dev":     {monitor: store.Monitor{ID: "f", App: "a", Server: "edge"}, known: true, result: store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: now}},
	}
	rows := buildDomainRows(snapshotOf(now), monitors, now)
	r := rowFor(t, rows, "elsewhere.dev")
	if r.DNS.State != layerNA || r.Route.State != layerNA || r.FirstFailing != "cert" {
		t.Errorf("%+v", r)
	}
	if f := rowFor(t, rows, "fleet.dev"); f.DNS.State != layerUnknown || f.Overall != layerUnknown {
		t.Errorf("a fleet domain with no check yet must read unknown: %+v", f)
	}
}

// One host on two servers (a move, a stale block): the serving row speaks for
// it, and the stale block's failing obtain is still reported.
func TestDomainRows_HostOnSeveralServers(t *testing.T) {
	now := domainsNow
	snap := snapshotOf(now,
		domainRow("old", "a", "moved.dev", withDNS("wrong_target", "198.51.100.1"), withRoute("stale_managed"), withObtainFailing(now.Add(-48*time.Hour))),
		domainRow("new", "a", "moved.dev"),
	)
	r := rowFor(t, buildDomainRows(snap, nil, now), "moved.dev")
	if r.Server != "new" || r.DNS.State != layerOK || r.Route.State != layerOK {
		t.Errorf("the serving row must speak for the domain: %+v", r)
	}
	if r.Cert.State != layerFail || !strings.Contains(r.Cert.Detail, "on old") {
		t.Errorf("the stale block's obtain failure was hidden: %+v", r.Cert)
	}
}

func TestDomainRows_MissingEvidenceReadsUnknownWithAge(t *testing.T) {
	now := domainsNow
	snap := snapshotOf(now.Add(-2*time.Hour), domainRow("edge", "a", "a.dev", withDNS("nxdomain")))
	r := rowFor(t, buildDomainRows(snap, nil, now), "a.dev")
	if r.DNS.State != layerUnknown || r.Overall != layerUnknown || !strings.Contains(r.DNS.Detail, "2 hours ago") {
		t.Errorf("stale evidence must not read as current: %+v", r)
	}

	snap.Servers["edge"].State = "unreachable"
	snap.Servers["edge"].LastSuccessAt = now.Add(-time.Minute)
	r = rowFor(t, buildDomainRows(snap, nil, now), "a.dev")
	if r.DNS.State != layerUnknown || !strings.Contains(r.DNS.Detail, "edge could not be checked") {
		t.Errorf("an unreachable server must not read as current: %+v", r)
	}
}

// ── incidents ────────────────────────────────────────────────────────────────

func domainServer(t *testing.T) (*Server, *captureSink) {
	t.Helper()
	s, sink := alertServer(t)
	s.initDomainHealth(t.TempDir())
	writeNotifications(t, `{"channels":[{"id":"w","type":"webhook","url":"https://hooks.teploy-fixture.dev/a"}]}`)
	return s, sink
}

func setSnapshot(s *Server, snap *domainSnapshot, adopted ...string) {
	s.domains.mu.Lock()
	defer s.domains.mu.Unlock()
	keep := s.domains.snap.Adopted
	s.domains.snap = *snap
	s.domains.snap.Adopted = keep
	for _, k := range adopted {
		s.domains.snap.Adopted[k] = true
	}
}

func domainEvents(sink *captureSink, kind string) []alert.Event {
	var out []alert.Event
	for _, ev := range sink.events() {
		if ev.Source == alert.SourceDomain && ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestDomainIncidents_DNSOpensOnceAfterDebounceAndRecovers(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev", withDNS("nxdomain"))), "edge", domainAdoptMonitors)

	s.evaluateDomainIncidents(now, true)
	if got := domainEvents(sink, alert.KindOpened); len(got) != 0 {
		t.Fatalf("one failed sweep must not open: %+v", got)
	}
	for i := 0; i < 3; i++ {
		s.evaluateDomainIncidents(now.Add(time.Duration(i)*domainSweepInterval), true)
	}
	opened := domainEvents(sink, alert.KindOpened)
	if len(opened) != 1 {
		t.Fatalf("opened %d times", len(opened))
	}
	ev := opened[0]
	if ev.Status != "down" || ev.Source != "domain" || ev.Severity != alert.SeverityCritical || ev.App != "a" || ev.Server != "edge" ||
		ev.Message != "no DNS record" || ev.IncidentID == "" || ev.Target != "https://a.dev/" {
		t.Errorf("event = %+v", ev)
	}

	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev")))
	s.evaluateDomainIncidents(now, true)
	s.evaluateDomainIncidents(now, true)
	recovered := domainEvents(sink, alert.KindRecovered)
	if len(recovered) != 1 || recovered[0].Status != "up" || recovered[0].IncidentID != ev.IncidentID {
		t.Fatalf("recovered = %+v", recovered)
	}
}

func TestDomainIncidents_UnknownOpensAndClosesNothing(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev", withDNS("nxdomain"))), "edge", domainAdoptMonitors)
	s.evaluateDomainIncidents(now, true)
	s.evaluateDomainIncidents(now, true)
	if n := len(domainEvents(sink, alert.KindOpened)); n != 1 {
		t.Fatalf("opened %d", n)
	}
	// The resolvers fail: nothing proves the record came back.
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev", withDNS("unknown"))))
	for i := 0; i < 3; i++ {
		s.evaluateDomainIncidents(now, true)
	}
	// The server stops answering: its rows go stale, the rule stays open.
	snap := snapshotOf(now.Add(-3*time.Hour), domainRow("edge", "a", "a.dev", withDNS("ok")))
	snap.Servers["edge"].State = "unreachable"
	setSnapshot(s, snap)
	for i := 0; i < 3; i++ {
		s.evaluateDomainIncidents(now, true)
	}
	if got := domainEvents(sink, alert.KindRecovered); len(got) != 0 {
		t.Fatalf("missing evidence closed an incident: %+v", got)
	}
	if len(s.alerts.tracker.Open()) != 1 {
		t.Fatal("the incident must stay open")
	}
}

func TestDomainIncidents_ProxiedStaleAndLabelsAreNotDNSFailures(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	setSnapshot(s, snapshotOf(now,
		domainRow("edge", "a", "proxied.dev", withDNS("proxied", "104.16.0.1")),
		domainRow("edge", "b", "stale.dev", withDNS("nxdomain"), withRoute("stale_managed")),
		domainRow("edge", "c", "label.dev", withDNS("nxdomain"), withRoute("none"), func(r *domainCheckRow) { r.Skip, r.Ingress = "host_label", "host" }),
		domainRow("edge", "", "manual.dev", withDNS("skipped"), withRoute("manual")),
	), "edge", domainAdoptMonitors)
	for i := 0; i < 3; i++ {
		s.evaluateDomainIncidents(now, true)
	}
	if got := domainEvents(sink, alert.KindOpened); len(got) != 0 {
		t.Fatalf("opened %+v", got)
	}
}

func TestDomainIncidents_ObtainFailingOver24Hours(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev", withObtainFailing(now.Add(-2*time.Hour)))), "edge", domainAdoptMonitors)
	s.evaluateDomainIncidents(now, true)
	if got := domainEvents(sink, alert.KindOpened); len(got) != 0 {
		t.Fatalf("under a day is not yet the condition: %+v", got)
	}
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev", withObtainFailing(now.Add(-14*24*time.Hour)),
		func(r *domainCheckRow) { r.TLS.LastError = "acme failed at https://user:hunter2@ca.dev/x" })))
	s.evaluateDomainIncidents(now, true)
	s.evaluateDomainIncidents(now, true)
	got := domainEvents(sink, alert.KindOpened)
	if len(got) != 1 || got[0].Severity != alert.SeverityWarning || !strings.Contains(got[0].Message, "certificate failing for 14 days") {
		t.Fatalf("opened = %+v", got)
	}
	if strings.Contains(got[0].Message, "hunter2") {
		t.Fatalf("a credential reached an alert: %s", got[0].Message)
	}
}

func seedMonitor(t *testing.T, s *Server, m store.Monitor, result store.CheckResult) store.Monitor {
	t.Helper()
	if s.monitor == nil {
		s.monitor = monitor.New(s.store)
		s.monitor.SetTracker(s.alerts.tracker)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.monitor.Stop(ctx)
		})
	}
	m.Enabled = true
	if m.Type == "" {
		m.Type = "http"
	}
	if m.Interval == 0 {
		m.Interval = time.Hour
	}
	if err := s.store.SaveMonitor(&m); err != nil {
		t.Fatal(err)
	}
	result.MonitorID, result.Incarnation = m.ID, m.Incarnation
	if _, err := s.store.SaveCheck(result); err != nil {
		t.Fatal(err)
	}
	s.monitor.Reload(m)
	return m
}

func TestDomainIncidents_CertExpiryFromMonitorsWorksWithoutTheCLI(t *testing.T) {
	s, sink := domainServer(t)
	now := time.Now().UTC()
	soon := now.Add(5 * 24 * time.Hour)
	seedMonitor(t, s, store.Monitor{ID: "m1", Name: "x", Target: "https://elsewhere.dev/"},
		store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: now, CertNotAfter: &soon})
	setSnapshot(s, snapshotOf(now), domainAdoptMonitors)

	s.evaluateDomainIncidents(now, false) // an older CLI: this rule does not need it
	got := domainEvents(sink, alert.KindOpened)
	if len(got) != 1 || got[0].Severity != alert.SeverityWarning || !strings.Contains(got[0].Message, "expires in 5 days") {
		t.Fatalf("opened = %+v", got)
	}

	// Renewed: a new reading closes it.
	far := now.Add(80 * 24 * time.Hour)
	seedMonitor(t, s, store.Monitor{ID: "m1", Name: "x", Target: "https://elsewhere.dev/"},
		store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: now.Add(time.Second), CertNotAfter: &far})
	s.evaluateDomainIncidents(now.Add(time.Second), false)
	if got := domainEvents(sink, alert.KindRecovered); len(got) != 1 {
		t.Fatalf("recovered = %+v", got)
	}

	// The monitor is removed: its open rule retires rather than dangling.
	soon = now.Add(2 * 24 * time.Hour)
	seedMonitor(t, s, store.Monitor{ID: "m1", Name: "x", Target: "https://elsewhere.dev/"},
		store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: now.Add(2 * time.Second), CertNotAfter: &soon})
	s.evaluateDomainIncidents(now.Add(2*time.Second), false)
	opened := domainEvents(sink, alert.KindOpened)
	if last := opened[len(opened)-1]; last.Severity != alert.SeverityCritical {
		t.Errorf("under three days is critical: %+v", last)
	}
	if err := s.store.DeleteMonitor("m1"); err != nil {
		t.Fatal(err)
	}
	s.evaluateDomainIncidents(now.Add(3*time.Second), false)
	rec := domainEvents(sink, alert.KindRecovered)
	if last := rec[len(rec)-1]; last.Reason != alert.ReasonRetired {
		t.Fatalf("removal must retire: %+v", last)
	}
}

// Design under test: while the HTTP monitor holds the outage's incident, a
// domain rule for that host opens nothing; once the monitor recovers and the
// cause remains, it opens then. Already-open rules are left alone.
func TestDomainIncidents_NoSecondIncidentWhileTheMonitorIsDown(t *testing.T) {
	s, sink := domainServer(t)
	now := time.Now().UTC()
	m := seedMonitor(t, s, store.Monitor{ID: "m1", Name: "a.dev", Target: "https://a.dev/", App: "a", Server: "edge"},
		store.CheckResult{Status: "down", Message: "no such host", CheckedAt: now})
	rule := monitor.RuleFor(m)
	for i := 0; i < rule.Policy.FailAfter; i++ {
		s.alerts.tracker.Observe(rule, false, now, "no such host")
	}
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev", withDNS("nxdomain"))), "edge", domainAdoptMonitors)
	for i := 0; i < 4; i++ {
		s.evaluateDomainIncidents(now, true)
	}
	if got := domainEvents(sink, alert.KindOpened); len(got) != 0 {
		t.Fatalf("a second incident for one outage: %+v", got)
	}
	// The row still names the cause.
	r := rowFor(t, buildDomainRows(&s.domains.snap, nil, now), "a.dev")
	if r.FirstFailing != "dns" {
		t.Errorf("row = %+v", r)
	}

	for i := 0; i < rule.Policy.RecoverAfter; i++ {
		s.alerts.tracker.Observe(rule, true, now, "recovered")
	}
	s.evaluateDomainIncidents(now, true)
	s.evaluateDomainIncidents(now, true)
	if got := domainEvents(sink, alert.KindOpened); len(got) != 1 {
		t.Fatalf("the cause outlived the monitor's incident and should open now: %+v", got)
	}
}

func TestDomainIncidents_FirstResultsBecomeOneSummary(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	setSnapshot(s, snapshotOf(now,
		domainRow("edge", "a", "a.dev", withDNS("nxdomain")),
		domainRow("edge", "b", "b.dev", withDNS("wrong_target", "198.51.100.1")),
		domainRow("edge", "c", "c.dev", withObtainFailing(now.Add(-5*24*time.Hour))),
		domainRow("edge", "d", "ok.dev"),
	))
	s.evaluateDomainIncidents(now, true)

	var human, summaries int
	for _, ev := range sink.events() {
		if ev.Kind == alert.KindSummary {
			summaries++
		}
		if ev.Kind != alert.KindSummary && ev.Source == alert.SourceDomain && ev.Audience != alert.AudienceMachine {
			human++
		}
		if ev.Kind == alert.KindSummary && (!strings.HasPrefix(ev.Message, "First domain health results.") || ev.Source != alert.SourceDomain) {
			t.Errorf("summary wording: %+v", ev)
		}
	}
	if summaries != 1 || human != 0 {
		t.Fatalf("summaries=%d per-domain human alerts=%d: %+v", summaries, human, sink.events())
	}
	open := s.alerts.tracker.Open()
	if len(open) != 3 {
		t.Fatalf("open = %d", len(open))
	}
	// Adopted, so its start is when the failure began, not when Dash first saw it.
	if !open[0].StartedAt.Equal(now.Add(-5 * 24 * time.Hour)) {
		t.Errorf("oldest incident started at %v", open[0].StartedAt)
	}
	// The source is adopted: a later failure is an ordinary open, and restarting
	// does not adopt again.
	before := len(sink.events())
	s.evaluateDomainIncidents(now, true)
	if len(sink.events()) != before {
		t.Fatalf("a repeat sweep sent %+v", sink.events()[before:])
	}
	if !s.domains.snap.Adopted["edge"] {
		t.Fatal("adoption must be recorded")
	}
	raw, err := os.ReadFile(s.domains.path)
	if err != nil || !strings.Contains(string(raw), `"edge":true`) {
		t.Fatalf("adoption must persist: %s %v", raw, err)
	}
}

func TestDomainIncidents_ServerUnreachableNeverAdopted(t *testing.T) {
	s, _ := domainServer(t)
	now := domainsNow
	snap := snapshotOf(now, domainRow("edge", "a", "a.dev"))
	snap.Servers["edge"].State = "unreachable"
	setSnapshot(s, snap)
	s.evaluateDomainIncidents(now, true)
	if s.domains.snap.Adopted["edge"] {
		t.Fatal("a server that never reported has adopted nothing")
	}
}

func emptyServer(snap *domainSnapshot, name, state string, now time.Time) {
	snap.Servers[name] = &domainServerRecord{Server: name, State: state, LastAttemptAt: now, LastSuccessAt: now, Caddy: domainCaddy{Caddyfile: "present"}}
}

func TestDomainIncidents_RetireOnRemovalPerServer(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	setSnapshot(s, snapshotOf(now,
		domainRow("edge", "a", "a.dev", withDNS("nxdomain")),
		domainRow("other", "b", "b.dev"),
	), "edge", "other", domainAdoptMonitors)
	s.evaluateDomainIncidents(now, true)
	s.evaluateDomainIncidents(now, true)
	if n := len(domainEvents(sink, alert.KindOpened)); n != 1 {
		t.Fatalf("opened %d", n)
	}
	// a.dev is gone from edge, whose read did not complete: wait.
	partial := snapshotOf(now, domainRow("other", "b", "b.dev"))
	emptyServer(partial, "edge", "partial", now)
	setSnapshot(s, partial)
	s.evaluateDomainIncidents(now, true)
	if len(s.alerts.tracker.Open()) != 1 {
		t.Fatal("retired on a partly read server")
	}
	// An older CLI proves nothing either.
	gone := snapshotOf(now, domainRow("other", "b", "b.dev"))
	emptyServer(gone, "edge", "ok", now)
	setSnapshot(s, gone)
	s.evaluateDomainIncidents(now, false)
	if len(s.alerts.tracker.Open()) != 1 {
		t.Fatal("retired on an unsupported CLI")
	}
	// Edge was read and no longer has it. A long-dead server elsewhere is not
	// edge's evidence, and holds nothing up.
	emptyServer(gone, "dead", "unreachable", now.Add(-48*time.Hour))
	setSnapshot(s, gone)
	s.evaluateDomainIncidents(now, true)
	rec := domainEvents(sink, alert.KindRecovered)
	if len(rec) != 1 || rec[0].Reason != alert.ReasonRetired || len(s.alerts.tracker.Open()) != 0 {
		t.Fatalf("recovered = %+v, open = %+v", rec, s.alerts.tracker.Open())
	}
}

// The false all-clear: a server whose Caddyfile or state read failed comes back
// partial, its rows read route unknown and TLS not_applicable, so its rule keys
// vanish. Open incidents must survive that, across more than one sweep and a
// restart of the in-memory key set.
func TestDomainIncidents_PartialServerNeverAllClears(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	setSnapshot(s, snapshotOf(now,
		domainRow("edge", "a", "a.dev", withDNS("nxdomain"), withObtainFailing(now.Add(-48*time.Hour))),
	), "edge", domainAdoptMonitors)
	s.evaluateDomainIncidents(now, true)
	s.evaluateDomainIncidents(now, true)
	if n := len(s.alerts.tracker.Open()); n != 2 {
		t.Fatalf("open = %d", n)
	}
	blind := snapshotOf(now, domainRow("edge", "a", "a.dev", withRoute("unknown"), func(r *domainCheckRow) {
		r.DNS.Status, r.TLS.Status, r.Route.UpstreamResolves = "unknown", "not_applicable", "unknown"
	}))
	blind.Servers["edge"].State = "partial"
	blind.Servers["edge"].Caddy.Caddyfile = "unreadable"
	setSnapshot(s, blind)
	for i := 0; i < 3; i++ {
		s.evaluateDomainIncidents(now, true)
	}
	if got := domainEvents(sink, alert.KindRecovered); len(got) != 0 {
		t.Fatalf("a false all-clear: %+v", got)
	}
	if n := len(s.alerts.tracker.Open()); n != 2 {
		t.Fatalf("open = %d", n)
	}
	s.domains.ruleKeys = domainRuleKeys{} // as after a restart: inc.Server still scopes it
	s.evaluateDomainIncidents(now, true)
	if got := domainEvents(sink, alert.KindRecovered); len(got) != 0 {
		t.Fatalf("a false all-clear after a restart: %+v", got)
	}
}

// Caddy recreated: logs and elapsed reset, so an open obtain incident reads
// failing for under a day. It is still failing.
func TestDomainIncidents_ResetClockIsNotARecovery(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev", withObtainFailing(now.Add(-14*24*time.Hour)))), "edge", domainAdoptMonitors)
	s.evaluateDomainIncidents(now, true)
	if n := len(domainEvents(sink, alert.KindOpened)); n != 1 {
		t.Fatalf("opened %d", n)
	}
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev", withObtainFailing(now.Add(-time.Hour)))))
	for i := 0; i < 2; i++ {
		s.evaluateDomainIncidents(now, true)
	}
	if got := domainEvents(sink, alert.KindRecovered); len(got) != 0 {
		t.Fatalf("said recovered while failing: %+v", got)
	}
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev")))
	s.evaluateDomainIncidents(now, true)
	if got := domainEvents(sink, alert.KindRecovered); len(got) != 1 {
		t.Fatalf("a real ok closes: %+v", got)
	}
}

// Host on A and B, DNS points at A, A unreachable: B's wrong_target proves
// nothing.
func TestDomainIncidents_WrongTargetWithAnUnreadInstanceIsUnknown(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	snap := snapshotOf(now,
		domainRow("a", "x", "x.dev", withRoute("managed")),
		domainRow("b", "x", "x.dev", withDNS("wrong_target", "192.0.2.1")),
	)
	snap.Servers["a"].State = "unreachable"
	snap.Servers["a"].LastSuccessAt = now.Add(-3 * time.Hour)
	setSnapshot(s, snap, "a", "b", domainAdoptMonitors)
	for i := 0; i < 3; i++ {
		s.evaluateDomainIncidents(now, true)
	}
	if got := domainEvents(sink, alert.KindOpened); len(got) != 0 {
		t.Fatalf("opened a critical: %+v", got)
	}
	r := rowFor(t, buildDomainRows(snap, nil, now), "x.dev")
	if r.DNS.State != layerUnknown {
		t.Errorf("dns = %+v", r.DNS)
	}
}

func TestDomainIncidents_PartialFirstSweepIsNotAdopted(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	partial := snapshotOf(now, domainRow("edge", "a", "a.dev"))
	partial.Servers["edge"].State = "partial"
	setSnapshot(s, partial)
	s.evaluateDomainIncidents(now, true)
	if s.domains.snap.Adopted["edge"] {
		t.Fatal("a partial sweep adopted the server")
	}
	setSnapshot(s, snapshotOf(now,
		domainRow("edge", "a", "a.dev", withDNS("nxdomain")),
		domainRow("edge", "b", "b.dev", withDNS("nxdomain")),
	))
	s.evaluateDomainIncidents(now, true)
	summaries := 0
	for _, ev := range sink.events() {
		if ev.Kind == alert.KindSummary {
			summaries++
		} else if ev.Audience != alert.AudienceMachine {
			t.Errorf("an individual human alert: %+v", ev)
		}
	}
	if summaries != 1 {
		t.Fatalf("summaries = %d", summaries)
	}
}

func TestDomainIncidents_WildcardObtainAlerts(t *testing.T) {
	s, sink := domainServer(t)
	now := domainsNow
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "*.foo.dev", withDNS("skipped"), withObtainFailing(now.Add(-3*24*time.Hour)),
		func(r *domainCheckRow) { r.Skip = "wildcard" })), "edge", domainAdoptMonitors)
	s.evaluateDomainIncidents(now, true)
	got := domainEvents(sink, alert.KindOpened)
	if len(got) != 1 || !strings.Contains(got[0].Message, "failing for 3 days") || got[0].Target != "*.foo.dev" {
		t.Fatalf("opened = %+v", got)
	}
	r := rowFor(t, buildDomainRows(&s.domains.snap, nil, now), "*.foo.dev")
	if r.FirstFailing != "cert" {
		t.Errorf("row = %+v", r)
	}
}

// A long-open monitor incident must not hide an expiring certificate, and every
// monitor for the host counts as the one holding the outage.
func TestDomainIncidents_MonitorOverlapScope(t *testing.T) {
	s, sink := domainServer(t)
	now := time.Now().UTC()
	soon := now.Add(5 * 24 * time.Hour)
	older := seedMonitor(t, s, store.Monitor{ID: "down", Name: "a", Target: "https://a.dev/health"},
		store.CheckResult{Status: "down", Message: "503", CheckedAt: now.Add(-time.Minute), CertNotAfter: &soon})
	seedMonitor(t, s, store.Monitor{ID: "newer", Name: "b", Target: "https://a.dev/"},
		store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: now, CertNotAfter: &soon})
	rule := monitor.RuleFor(older)
	for i := 0; i < rule.Policy.FailAfter; i++ {
		s.alerts.tracker.Observe(rule, false, now, "503")
	}
	setSnapshot(s, snapshotOf(now, domainRow("edge", "x", "a.dev", withDNS("nxdomain"))), "edge", domainAdoptMonitors)
	s.evaluateDomainIncidents(now, true)
	s.evaluateDomainIncidents(now, true)
	var names []string
	for _, ev := range domainEvents(sink, alert.KindOpened) {
		names = append(names, ev.MonitorName)
	}
	if len(names) != 1 || !strings.Contains(names[0], "certificate expiry") {
		t.Fatalf("want only the expiry alert (DNS is the monitor's outage, on either monitor): %v", names)
	}

	// Already expired: that is the outage.
	s2, sink2 := domainServer(t)
	gone := now.Add(-time.Hour)
	m := seedMonitor(t, s2, store.Monitor{ID: "m", Name: "m", Target: "https://b.dev/"},
		store.CheckResult{Status: "down", Message: "certificate has expired", CheckedAt: now, CertNotAfter: &gone})
	r2 := monitor.RuleFor(m)
	for i := 0; i < r2.Policy.FailAfter; i++ {
		s2.alerts.tracker.Observe(r2, false, now, "certificate has expired")
	}
	setSnapshot(s2, snapshotOf(now), domainAdoptMonitors)
	s2.evaluateDomainIncidents(now, true)
	if got := domainEvents(sink2, alert.KindOpened); len(got) != 0 {
		t.Fatalf("a second incident for an expired certificate: %+v", got)
	}
}

func TestCertThresholds(t *testing.T) {
	at := func(d time.Duration) *time.Time { v := domainsNow.Add(d); return &v }
	for _, c := range []struct {
		name           string
		lifetime       time.Duration
		warn, critical time.Duration
	}{
		{"90 days", 90 * 24 * time.Hour, 14 * 24 * time.Hour, 3 * 24 * time.Hour},
		{"6 days", 6 * 24 * time.Hour, 2 * 24 * time.Hour, 24 * time.Hour},
		{"12 hours", 12 * time.Hour, 4 * time.Hour, 2 * time.Hour},
	} {
		warn, critical := certThresholds(at(0), at(c.lifetime))
		if warn != c.warn || critical != c.critical {
			t.Errorf("%s: warn=%v critical=%v", c.name, warn, critical)
		}
	}
	if warn, critical := certThresholds(nil, at(time.Hour)); warn != domainCertWarn || critical != domainCertCritical {
		t.Errorf("no start: warn=%v critical=%v", warn, critical)
	}
}

func TestDomainIncidents_ShortLivedCertDoesNotWarnForever(t *testing.T) {
	s, sink := domainServer(t)
	now := time.Now().UTC()
	start, end := now.Add(-time.Hour), now.Add(6*24*time.Hour)
	seedMonitor(t, s, store.Monitor{ID: "m", Name: "m", Target: "https://short.dev/"},
		store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: now, CertNotBefore: &start, CertNotAfter: &end})
	setSnapshot(s, snapshotOf(now), domainAdoptMonitors)
	s.evaluateDomainIncidents(now, false)
	if got := domainEvents(sink, alert.KindOpened); len(got) != 0 {
		t.Fatalf("a fresh 6-day certificate alerted: %+v", got)
	}
}

func TestDomainIncidents_ExpiryEscalationReAlerts(t *testing.T) {
	s, sink := domainServer(t)
	now := time.Now().UTC()
	soon := now.Add(5 * 24 * time.Hour)
	seedMonitor(t, s, store.Monitor{ID: "m", Name: "m", Target: "https://e.dev/"},
		store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: now, CertNotAfter: &soon})
	setSnapshot(s, snapshotOf(now), domainAdoptMonitors)
	s.evaluateDomainIncidents(now, false)
	later := now.Add(time.Second)
	sooner := now.Add(2 * 24 * time.Hour)
	seedMonitor(t, s, store.Monitor{ID: "m", Name: "m", Target: "https://e.dev/"},
		store.CheckResult{Status: "up", StatusCode: 200, CheckedAt: later, CertNotAfter: &sooner})
	s.evaluateDomainIncidents(later, false)
	s.evaluateDomainIncidents(later, false)
	repeats := domainEvents(sink, alert.KindRepeat)
	if len(repeats) != 1 || repeats[0].Severity != alert.SeverityCritical {
		t.Fatalf("repeats = %+v", repeats)
	}
}

func TestDomainIncidents_SilenceHolds(t *testing.T) {
	s, sink := domainServer(t)
	now := time.Now().UTC()
	if err := s.alerts.silences.Add(Silence{ID: "sil", Scope: silenceApp, Target: "a", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	setSnapshot(s, snapshotOf(now, domainRow("edge", "a", "a.dev", withDNS("nxdomain"))), "edge", domainAdoptMonitors)
	s.evaluateDomainIncidents(now, true)
	s.evaluateDomainIncidents(now, true)
	if got := domainEvents(sink, alert.KindOpened); len(got) != 0 {
		t.Fatalf("a silence must hold: %+v", got)
	}
	if len(s.alerts.tracker.HeldRules()) != 1 {
		t.Fatalf("held = %+v", s.alerts.tracker.HeldRules())
	}
	// Adoption honors it too.
	s2, sink2 := domainServer(t)
	if err := s2.alerts.silences.Add(Silence{ID: "sil", Scope: silenceAll, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	setSnapshot(s2, snapshotOf(now, domainRow("edge", "a", "a.dev", withDNS("nxdomain"))))
	s2.evaluateDomainIncidents(now, true)
	if len(s2.alerts.tracker.Open()) != 0 || len(domainEvents(sink2, alert.KindOpened)) != 0 {
		t.Fatalf("adoption ignored a silence: %+v", s2.alerts.tracker.Open())
	}
}

// ── the sweep, the gate, the API ─────────────────────────────────────────────

type domainCLI struct {
	mu        sync.Mutex
	version   string
	tokens    []string
	calls     int
	inFlight  int
	maxFlight int
	check     func(server string) (*cli.Result, error)
}

func (c *domainCLI) run(ctx context.Context, args ...string) (*cli.Result, error) {
	switch {
	case len(args) >= 2 && args[0] == "version":
		raw, _ := json.Marshal(map[string]any{"version": c.version, "machine_interface": 2, "capabilities": c.tokens})
		return &cli.Result{Stdout: string(raw)}, nil
	case len(args) >= 2 && args[0] == "server" && args[1] == "list":
		return &cli.Result{Stdout: `{"machine_interface":2,"servers":[{"name":"edge","host":"192.0.2.10"},{"name":"dead","host":"192.0.2.11"},{"name":"half","host":"192.0.2.12"}],"observed_at":"2026-11-04T12:00:00Z"}`}, nil
	case len(args) >= 4 && args[0] == "domains":
		c.mu.Lock()
		c.calls++
		c.inFlight++
		if c.inFlight > c.maxFlight {
			c.maxFlight = c.inFlight
		}
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			c.inFlight--
			c.mu.Unlock()
		}()
		return c.check(args[3])
	}
	return &cli.Result{ExitCode: 1, Stderr: "unknown"}, nil
}

func envelopeFor(t *testing.T, server, state string, rows ...domainCheckRow) *cli.Result {
	t.Helper()
	if rows == nil {
		rows = []domainCheckRow{}
	}
	raw, err := json.Marshal(domainCheckEnvelope{MachineInterface: 2, CheckedAt: time.Now().UTC(),
		Servers: []domainCheckHost{{Server: server, Host: "192.0.2.10", State: state, CheckedAt: time.Now().UTC(), Caddy: domainCaddy{Caddyfile: "present", Logs: "ok"}}}, Domains: rows})
	if err != nil {
		t.Fatal(err)
	}
	return &cli.Result{Stdout: string(raw)}
}

func domainSweepServer(t *testing.T, fake *domainCLI) *Server {
	t.Helper()
	withTempGroupsFile(t)
	writeNotifications(t, `{}`)
	return New(Config{DataDir: t.TempDir(), NoAuth: true, CLIInstalled: func() bool { return true }, CLIRunner: fake.run})
}

// v0.1.50 advertises no domains-check-v1: the page says so plainly, the CLI is
// never asked, and the monitor-derived layers keep working.
func TestDomainsGate_OlderCLIIsANoteNotAnError(t *testing.T) {
	fake := &domainCLI{version: "v0.1.50", tokens: []string{"app-list-machine", "server-status-machine"},
		check: func(string) (*cli.Result, error) {
			t.Error("an unsupported CLI must not be asked")
			return &cli.Result{}, nil
		}}
	s := domainSweepServer(t, fake)
	s.sweepDomainHealth(context.Background())
	if fake.calls != 0 {
		t.Fatalf("calls = %d", fake.calls)
	}
	view := s.domainsView(context.Background(), time.Now().UTC())
	if view["cli_supported"] != false || !strings.Contains(view["note"].(string), "v0.1.51") {
		t.Fatalf("view = %+v", view)
	}
	if s.domainFronted("a.dev") {
		t.Error("an older CLI cannot say what fronts a host")
	}
}

func TestDomainsSweep_PerServerOutcomes(t *testing.T) {
	var fake *domainCLI
	fake = &domainCLI{version: "v0.1.51", tokens: []string{"domains-check-v1"}, check: func(server string) (*cli.Result, error) {
		switch server {
		case "edge":
			return envelopeFor(t, "edge", "ok", domainRow("edge", "a", "a.dev")), nil
		case "half":
			r := envelopeFor(t, "half", "partial", domainRow("half", "b", "b.dev"))
			return r, nil
		}
		return nil, errors.New("exit 1: dial https://user:hunter2@box: refused")
	}}
	s := domainSweepServer(t, fake)
	s.sweepDomainHealth(context.Background())

	view := s.domainsView(context.Background(), time.Now().UTC())
	servers := view["servers"].([]domainServerView)
	if len(servers) != 3 {
		t.Fatalf("every server must be listed: %+v", servers)
	}
	states := map[string]domainServerView{}
	for _, sv := range servers {
		states[sv.Server] = sv
	}
	if states["edge"].State != "ok" || states["half"].State != "partial" || states["dead"].State != "unreachable" {
		t.Fatalf("states = %+v", states)
	}
	if strings.Contains(states["dead"].Error, "hunter2") {
		t.Fatalf("a credential was stored: %s", states["dead"].Error)
	}
	if states["dead"].CheckedAt != nil {
		t.Errorf("a server that never answered has no check time: %+v", states["dead"])
	}
	if fake.maxFlight > domainSweepConcurrency {
		t.Errorf("concurrency %d over the bound", fake.maxFlight)
	}

	// A later failure keeps the last rows, marked by the server's state.
	fake.check = func(string) (*cli.Result, error) { return nil, errors.New("timeout") }
	s.sweepDomainHealth(context.Background())
	rows := s.domainsView(context.Background(), time.Now().UTC())["rows"].([]domainRowView)
	r := rowFor(t, rows, "a.dev")
	if r.Overall != layerUnknown || !strings.Contains(r.DNS.Detail, "edge could not be checked") {
		t.Errorf("row = %+v", r)
	}

	// The state survives a restart.
	path := s.domains.path
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0600 || strings.Contains(string(raw), "hunter2") {
		t.Errorf("mode %v, content leak=%v", fi.Mode().Perm(), strings.Contains(string(raw), "hunter2"))
	}
	reborn := &Server{}
	reborn.initDomainHealth(filepath.Dir(path))
	if rec := reborn.domains.snap.Servers["edge"]; rec == nil || len(rec.Domains) != 1 {
		t.Fatalf("reloaded = %+v", reborn.domains.snap)
	}
}

func TestDomainsSweep_ANewerMachineInterfaceIsRefused(t *testing.T) {
	fake := &domainCLI{version: "v0.1.51", tokens: []string{"domains-check-v1"}, check: func(string) (*cli.Result, error) {
		return &cli.Result{Stdout: `{"machine_interface":99,"servers":[],"domains":[]}`}, nil
	}}
	s := domainSweepServer(t, fake)
	s.sweepDomainHealth(context.Background())
	if rec := s.domains.snap.Servers["edge"]; rec == nil || rec.State != "unreachable" {
		t.Fatalf("rec = %+v", rec)
	}
}

func TestDomainHealth_CorruptStateIsSetAside(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "domain-health.json"), []byte("{nope"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.initDomainHealth(dir)
	if s.domains.snap.Servers == nil || len(s.domains.snap.Servers) != 0 {
		t.Fatalf("snap = %+v", s.domains.snap)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "domain-health.json.corrupt-*"))
	if len(matches) != 1 {
		t.Fatalf("not set aside: %v", matches)
	}
}

func TestDomainsRoute(t *testing.T) {
	if got := requiredCapabilities(http.MethodGet, "/api/domains"); len(got) != 1 || got[0] != caps.ViewMetadata {
		t.Errorf("GET /api/domains = %v", got)
	}
	if got := requiredCapabilities(http.MethodPost, "/api/domains"); len(got) != 1 || got[0] != caps.ExecuteMutate {
		t.Errorf("POST /api/domains = %v", got)
	}
	fake := &domainCLI{version: "v0.1.51", tokens: []string{"domains-check-v1"}, check: func(string) (*cli.Result, error) { return nil, errors.New("x") }}
	s := domainSweepServer(t, fake)
	code, body := call(t, s.handleDomains, "GET", "/api/domains", "")
	if code != 200 || body["data"].(map[string]any)["cli_supported"] != true {
		t.Fatalf("%d %v", code, body)
	}
	if code, _ := call(t, s.handleDomains, "POST", "/api/domains", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", code)
	}
}

// ── fleet monitors ───────────────────────────────────────────────────────────

func TestFleetMonitors_HostIngressNeedsAFront(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	s := &Server{store: st}
	s.initDomainHealth("")
	now := time.Now().UTC()
	host := func(r *domainCheckRow) { r.Ingress = "host" }
	label := func(r *domainCheckRow) { r.Ingress, r.Skip = "host", "host_label" }
	setSnapshot(s, snapshotOf(now,
		domainRow("a", "manual", "manual.dev", withRoute("manual"), host),
		domainRow("a", "tunnelled", "tunnelled.dev", withRoute("none"), withDNS("proxied", "104.16.0.1"), label),
		domainRow("a", "elsewhere", "elsewhere.dev", withRoute("none"), withDNS("nxdomain"), label),
		domainRow("b", "other-box", "elsewhere.dev", withRoute("managed"), withDNS("nxdomain")),
		domainRow("a", "bare", "bare.dev", withRoute("none"), withDNS("nxdomain"), label),
		domainRow("a", "stale", "stale.dev", withRoute("stale_managed"), withDNS("nxdomain"), host),
	))
	sweep := []ServerObservation{fleetEnv("a", "",
		remote.AppState{App: "manual", Domain: "manual.dev", Ingress: "host"},
		remote.AppState{App: "tunnelled", Domain: "tunnelled.dev", Ingress: "host"},
		remote.AppState{App: "elsewhere", Domain: "elsewhere.dev", Ingress: "host"},
		remote.AppState{App: "bare", Domain: "bare.dev", Ingress: "host"},
		remote.AppState{App: "stale", Domain: "stale.dev", Ingress: "host"},
		remote.AppState{App: "fresh", Domain: "unseen.dev", Ingress: "host"},
		remote.AppState{App: "routed", Domain: "routed.dev", Ingress: "caddy"},
		remote.AppState{App: "ext", Domain: "ext.dev", Ingress: "external"},
	)}
	// A monitor an unfronted host-ingress app got under the old rule: nothing
	// proves a tunnel is not there, so it stays.
	old := store.Monitor{ID: fleetMonitorID("bare.dev"), Name: "bare.dev", Type: "http", Target: "https://bare.dev/", Enabled: true, App: "bare", Interval: time.Minute, Timeout: time.Second}
	if err := st.SaveMonitor(&old); err != nil {
		t.Fatal(err)
	}
	added, removed, err := s.reconcileFleetMonitors(sweep)
	if err != nil || added != 4 || removed != 0 {
		t.Fatalf("added=%d removed=%d err=%v", added, removed, err)
	}
	for host, want := range map[string]bool{"manual.dev": true, "tunnelled.dev": true, "elsewhere.dev": true, "routed.dev": true, "bare.dev": true,
		"stale.dev": false, "unseen.dev": false, "ext.dev": false} {
		m, _ := st.GetMonitor(fleetMonitorID(host))
		if (m != nil) != want {
			t.Errorf("%s: monitor = %+v, want present=%v", host, m, want)
		}
	}
}

// Missing evidence neither adds nor retires.
func TestFleetMonitors_UnknownRoutesKeepWhatExists(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	s := &Server{store: st}
	s.initDomainHealth("")
	s.domains.snap.Unsupported = true
	s.domains.snap.Servers["a"] = &domainServerRecord{Server: "a", State: "ok", LastSuccessAt: time.Now(), Domains: []domainCheckRow{domainRow("a", "kept", "kept.dev", withRoute("managed"))}}
	old := store.Monitor{ID: fleetMonitorID("kept.dev"), Name: "kept.dev", Type: "http", Target: "https://kept.dev/", Enabled: true, App: "kept", Interval: time.Minute, Timeout: time.Second}
	if err := st.SaveMonitor(&old); err != nil {
		t.Fatal(err)
	}
	sweep := []ServerObservation{fleetEnv("a", "",
		remote.AppState{App: "kept", Domain: "kept.dev", Ingress: "host"},
		remote.AppState{App: "fresh", Domain: "fresh.dev", Ingress: "host"},
	)}
	added, removed, err := s.reconcileFleetMonitors(sweep)
	if err != nil || added != 0 || removed != 0 {
		t.Fatalf("added=%d removed=%d err=%v", added, removed, err)
	}
	if m, _ := st.GetMonitor(old.ID); m == nil {
		t.Fatal("an existing monitor was retired on no evidence")
	}
}

func TestSanitizeDomainError(t *testing.T) {
	got := sanitizeDomainError("failed https://u:p4ss@host/x\n\x1b[31m " + strings.Repeat("a", 500))
	if strings.Contains(got, "p4ss") || strings.ContainsAny(got, "\n\x1b") || len(got) > 420 {
		t.Errorf("got %q", got)
	}
}

func TestHumanSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{
		14 * 24 * time.Hour: "14 days",
		36 * time.Hour:      "36 hours",
		3 * time.Hour:       "3 hours",
		90 * time.Minute:    "90 minutes",
		-3 * time.Hour:      "3 hours",
		time.Minute:         "60 seconds",
		time.Second:         "1 second",
	} {
		if got := humanSpan(d); got != want {
			t.Errorf("humanSpan(%v) = %q, want %q", d, got, want)
		}
	}
}
