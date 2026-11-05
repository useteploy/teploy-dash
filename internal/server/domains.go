package server

// domains.go — domain health (plan P4, Slice 5). One row per domain joining
// four layers, naming the first that fails with plain-language evidence:
//
//	DNS, route, certificate obtain: `teploy domains check --json`, per server,
//	  every 15 minutes (the CLI on this host does the work over SSH).
//	Certificate expiry, HTTP: the monitors' own checks, every 60 seconds.
//
// Failures become incidents with source "domain" through the same tracker as
// monitors. One overlap is designed out: while a domain's HTTP monitor has an
// open incident, a domain rule for that host does not open a second one. The
// monitor's alert owns the outage and the Domains page names the cause; a domain
// rule that outlives the monitor's recovery opens then. A rule already open is
// left alone. A certificate that is expiring is a different problem from the
// outage, so only an already-expired one counts as the same outage.
//
// Evidence that could not be collected reads "unknown", with its age; it is
// neither a failure nor a pass, so it opens and closes nothing.
//
// State is one file-store record: <data>/domain-health.json (plan principle 9).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/durable"
	"github.com/useteploy/teploy-dash/internal/incident"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/remote"
	"github.com/useteploy/teploy-dash/internal/store"
)

const (
	domainSweepInterval    = 15 * time.Minute
	domainServerTimeout    = 2 * time.Minute
	domainSweepConcurrency = 3
	// domainStaleAfter: evidence older than two missed sweeps reads unknown.
	domainStaleAfter = 2*domainSweepInterval + domainServerTimeout
	// domainMonitorStaleAfter is how old a monitor's result may be and still
	// count (monitors check every minute by default).
	domainMonitorStaleAfter = 30 * time.Minute

	domainCertWarn      = 14 * 24 * time.Hour
	domainCertCritical  = 3 * 24 * time.Hour
	domainObtainFailing = 24 * time.Hour

	// domainsMinCLI is the release that brings `domains check`: a CLI that does
	// not advertise domains-check-v1 reads as unsupported, whatever its version.
	domainsMinCLI = "v0.1.51"

	domainAdoptMonitors = "\x00monitors"
)

// Layer states.
const (
	layerOK      = "ok"
	layerWarn    = "warn"
	layerFail    = "fail"
	layerUnknown = "unknown"
	layerNA      = "na"
)

// ── the CLI's envelope (contracts/schema/domains-check-envelope.schema.json) ─

type domainCheckEnvelope struct {
	MachineInterface int               `json:"machine_interface"`
	CheckedAt        time.Time         `json:"checked_at"`
	Resolvers        []string          `json:"resolvers"`
	Servers          []domainCheckHost `json:"servers"`
	Domains          []domainCheckRow  `json:"domains"`
}

type domainCheckHost struct {
	Server             string        `json:"server"`
	Host               string        `json:"host"`
	State              string        `json:"state"`
	CheckedAt          time.Time     `json:"checked_at"`
	Error              string        `json:"error,omitempty"`
	Addresses          []string      `json:"addresses"`
	PublicAddressKnown bool          `json:"public_address_known"`
	Caddy              domainCaddy   `json:"caddy"`
	Issues             []domainIssue `json:"issues"`
}

type domainCaddy struct {
	Container          string `json:"container"`
	Caddyfile          string `json:"caddyfile"`
	Logs               string `json:"logs"`
	LogCoverageSeconds int64  `json:"log_coverage_seconds"`
}

type domainIssue struct {
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

type domainCheckRow struct {
	Server  string `json:"server"`
	App     string `json:"app"`
	Host    string `json:"host"`
	Ingress string `json:"ingress"`
	Skip    string `json:"skip,omitempty"`
	DNS     struct {
		Status  string   `json:"status"`
		Answers []string `json:"answers"`
		Detail  string   `json:"detail,omitempty"`
	} `json:"dns"`
	TLS struct {
		Status        string     `json:"status"`
		Failures      *int       `json:"failures"`
		FailingSince  *time.Time `json:"failing_since,omitempty"`
		LastFailureAt *time.Time `json:"last_failure_at,omitempty"`
		LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
		LastError     string     `json:"last_error,omitempty"`
	} `json:"tls"`
	Route struct {
		Kind      string `json:"kind"`
		Owner     string `json:"owner,omitempty"`
		TLS       string `json:"tls,omitempty"`
		Upstreams []struct {
			Dial     string `json:"dial"`
			Resolves string `json:"resolves"`
		} `json:"upstreams"`
		UpstreamResolves string `json:"upstream_resolves"`
	} `json:"route"`
}

// ── persisted state ──────────────────────────────────────────────────────────

// domainServerRecord is one server's newest result. A failed attempt keeps the
// last rows, with their age, and says it failed.
type domainServerRecord struct {
	Server             string           `json:"server"`
	Host               string           `json:"host,omitempty"`
	State              string           `json:"state"`
	Error              string           `json:"error,omitempty"`
	Issues             []domainIssue    `json:"issues,omitempty"`
	Addresses          []string         `json:"addresses,omitempty"`
	PublicAddressKnown bool             `json:"public_address_known,omitempty"`
	Caddy              domainCaddy      `json:"caddy"`
	LastAttemptAt      time.Time        `json:"last_attempt_at"`
	LastSuccessAt      time.Time        `json:"last_success_at,omitempty"`
	Domains            []domainCheckRow `json:"domains"`
}

type domainSnapshot struct {
	Servers map[string]*domainServerRecord `json:"servers"`
	// Adopted names the sources whose first results have been taken in: what was
	// already broken then became one summary, not an alert per domain.
	Adopted     map[string]bool `json:"adopted"`
	LastSweepAt time.Time       `json:"last_sweep_at,omitempty"`
	Unsupported bool            `json:"unsupported,omitempty"`
	SweepError  string          `json:"sweep_error,omitempty"`
}

type domainHealthState struct {
	path string
	mu   sync.Mutex
	snap domainSnapshot
	// ruleKeys are the rules the last evaluation saw, with the servers evidencing
	// each, so a rule that stops applying is retired even when it was only being
	// held, and only once those servers have been read.
	ruleKeys domainRuleKeys
}

func (s *Server) initDomainHealth(dataDir string) {
	d := &s.domains
	d.snap = domainSnapshot{Servers: map[string]*domainServerRecord{}, Adopted: map[string]bool{}}
	d.ruleKeys = domainRuleKeys{}
	if dataDir == "" {
		return
	}
	d.path = filepath.Join(dataDir, "domain-health.json")
	raw, err := os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var snap domainSnapshot
	if err == nil {
		err = json.Unmarshal(raw, &snap)
	}
	if err != nil {
		// Unreadable state must not stop the watching: set aside and start over.
		if aside := setAside(d.path, err); aside != nil {
			log.Printf("[domains] %s is unreadable (%v) and could not be set aside: %v", d.path, err, aside)
			d.path = ""
		}
		return
	}
	if snap.Servers == nil {
		snap.Servers = map[string]*domainServerRecord{}
	}
	if snap.Adopted == nil {
		snap.Adopted = map[string]bool{}
	}
	d.snap = snap
}

func (d *domainHealthState) saveLocked() {
	if d.path == "" {
		return
	}
	raw, err := json.Marshal(d.snap)
	if err == nil {
		err = durable.Replace(d.path, raw, 0600)
	}
	if err != nil {
		log.Printf("[domains] persist failed (state in memory until the next sweep): %v", err)
	}
}

// ── the sweep ────────────────────────────────────────────────────────────────

// RunDomainHealth sweeps now if the last sweep is older than the interval, then
// on the interval, until ctx ends. One loop, so sweeps cannot pile up.
func (s *Server) RunDomainHealth(ctx context.Context) {
	wait := time.Duration(0)
	s.domains.mu.Lock()
	if last := s.domains.snap.LastSweepAt; !last.IsZero() {
		if remaining := domainSweepInterval - time.Since(last); remaining > 0 {
			wait = remaining
		}
	}
	s.domains.mu.Unlock()
	if wait == 0 {
		// Let the monitors take their first readings before the first join.
		wait = 90 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.sweepDomainHealth(ctx)
		timer.Reset(domainSweepInterval)
	}
}

func (s *Server) sweepDomainHealth(ctx context.Context) {
	supported := s.cliInstalled() && s.capabilities(ctx).Features.DomainsCheck
	var sweepErr string
	if supported {
		if err := s.checkDomainsOnServers(ctx); err != nil {
			sweepErr = sanitizeDomainError(err.Error())
			log.Printf("[domains] %s", sweepErr)
		}
	}
	s.domains.mu.Lock()
	s.domains.snap.LastSweepAt = time.Now().UTC()
	s.domains.snap.Unsupported = !supported
	s.domains.snap.SweepError = sweepErr
	s.domains.saveLocked()
	s.domains.mu.Unlock()
	if ctx.Err() == nil {
		s.evaluateDomainIncidents(time.Now().UTC(), supported)
	}
}

// checkDomainsOnServers runs the CLI per server under bounded concurrency with
// a per-server deadline. One bad server costs its own budget.
func (s *Server) checkDomainsOnServers(ctx context.Context) error {
	servers, err := s.resolveServers(ctx)
	if err != nil {
		return fmt.Errorf("server discovery failed: %w", err)
	}
	sem := make(chan struct{}, domainSweepConcurrency)
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv remote.ServerConn) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			checkCtx, cancel := context.WithTimeout(ctx, domainServerTimeout)
			defer cancel()
			env, err := s.runDomainsCheck(checkCtx, srv.Name)
			if ctx.Err() != nil {
				return // shutting down: not evidence about the server
			}
			s.recordDomainCheck(srv, env, err, time.Now().UTC())
		}(srv)
	}
	wg.Wait()

	names := make(map[string]bool, len(servers))
	for _, srv := range servers {
		names[srv.Name] = true
	}
	s.domains.mu.Lock()
	for name := range s.domains.snap.Servers {
		if !names[name] {
			delete(s.domains.snap.Servers, name)
		}
	}
	s.domains.mu.Unlock()
	return nil
}

func (s *Server) runDomainsCheck(ctx context.Context, server string) (*domainCheckEnvelope, error) {
	args := []string{"domains", "check", "--server", server, "--json"}
	result, err := s.runCLI(ctx, args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, commandFailure(args, result)
	}
	// The central decode refuses an envelope from a newer machine interface.
	if _, err := cli.ParseJSON(result.Stdout); err != nil {
		return nil, err
	}
	var env domainCheckEnvelope
	if err := json.Unmarshal([]byte(result.Stdout), &env); err != nil {
		return nil, fmt.Errorf("decoding the domains check: %w", err)
	}
	return &env, nil
}

func (s *Server) recordDomainCheck(srv remote.ServerConn, env *domainCheckEnvelope, err error, now time.Time) {
	d := &s.domains
	d.mu.Lock()
	defer d.mu.Unlock()
	rec := d.snap.Servers[srv.Name]
	if rec == nil {
		rec = &domainServerRecord{Server: srv.Name, Host: srv.Host, Domains: []domainCheckRow{}}
		d.snap.Servers[srv.Name] = rec
	}
	rec.LastAttemptAt = now
	if err != nil {
		rec.State, rec.Error = "unreachable", sanitizeDomainError(err.Error())
		return
	}
	var host *domainCheckHost
	for i := range env.Servers {
		if env.Servers[i].Server == srv.Name {
			host = &env.Servers[i]
		}
	}
	if host == nil {
		rec.State, rec.Error = "unreachable", "the check returned no result for this server"
		return
	}
	rec.State, rec.Error = host.State, sanitizeDomainError(host.Error)
	if host.State == "unreachable" {
		return
	}
	rec.Host = host.Host
	rec.Issues = sanitizeIssues(host.Issues)
	rec.Addresses = host.Addresses
	rec.PublicAddressKnown = host.PublicAddressKnown
	rec.Caddy = host.Caddy
	rec.LastSuccessAt = host.CheckedAt
	rec.Domains = env.Domains
	if rec.Domains == nil {
		rec.Domains = []domainCheckRow{}
	}
}

var domainURLCreds = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@"']+@`)

// sanitizeDomainError keeps stored errors free of URL credentials and control
// characters; what is stored is served to viewers and sent in alerts.
func sanitizeDomainError(s string) string {
	s = domainURLCreds.ReplaceAllString(s, "$1")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 400 {
		s = string(r[:400]) + "..."
	}
	return s
}

func sanitizeIssues(in []domainIssue) []domainIssue {
	out := make([]domainIssue, 0, len(in))
	for _, i := range in {
		out = append(out, domainIssue{Scope: sanitizeDomainError(i.Scope), Message: sanitizeDomainError(i.Message)})
	}
	return out
}

// ── the join ─────────────────────────────────────────────────────────────────

type domainLayer struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// domainRowView is one domain as the page shows it.
type domainRowView struct {
	Host         string      `json:"host"`
	App          string      `json:"app,omitempty"`
	Server       string      `json:"server,omitempty"`
	Ingress      string      `json:"ingress,omitempty"`
	LabelOnly    bool        `json:"label_only,omitempty"`
	Overall      string      `json:"overall"`
	FirstFailing string      `json:"first_failing,omitempty"`
	Evidence     string      `json:"evidence,omitempty"`
	DNS          domainLayer `json:"dns"`
	Route        domainLayer `json:"route"`
	Cert         domainLayer `json:"cert"`
	HTTP         domainLayer `json:"http"`
	// CheckedAt is when the DNS/route/obtain evidence was collected; AgeSeconds
	// is its age at response time. Empty when the domain has no such evidence.
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
	AgeSeconds    *int64     `json:"age_seconds,omitempty"`
	MonitorID     string     `json:"monitor_id,omitempty"`
	CertNotAfter  *time.Time `json:"cert_not_after,omitempty"`
	HTTPCheckedAt *time.Time `json:"http_checked_at,omitempty"`
}

// domainMonitorEvidence is what the monitors know about one host.
type domainMonitorEvidence struct {
	monitor store.Monitor
	result  store.CheckResult
	known   bool
}

func monitorHost(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// domainMonitors maps each host to its enabled HTTP monitor, preferring the one
// with the newest evidence. hostOfRule maps every such monitor's incident key to
// its host, since a host can have several. listed=false means monitors could not
// be listed.
func (s *Server) domainMonitors() (out map[string]domainMonitorEvidence, hostOfRule map[string]string, listed bool) {
	out, hostOfRule = map[string]domainMonitorEvidence{}, map[string]string{}
	if s.store == nil {
		return out, hostOfRule, false
	}
	monitors, err := s.store.ListMonitors()
	if err != nil {
		log.Printf("[domains] listing monitors: %v", err)
		return out, hostOfRule, false
	}
	for _, m := range monitors {
		if !m.Enabled || m.Type != "http" {
			continue
		}
		host := monitorHost(m.Target)
		if host == "" {
			continue
		}
		hostOfRule[monitor.RuleKey(m.ID)] = host
		ev := domainMonitorEvidence{monitor: m}
		if s.monitor != nil {
			ev.result, ev.known = s.monitor.LastResult(m.ID)
		}
		if cur, seen := out[host]; seen && cur.known && (!ev.known || !ev.result.CheckedAt.After(cur.result.CheckedAt)) {
			continue
		}
		out[host] = ev
	}
	return out, hostOfRule, true
}

// domainGroup is every (server, app) row for one host.
type domainGroup struct {
	host string
	rows []domainGroupRow
}

type domainGroupRow struct {
	row domainCheckRow
	rec *domainServerRecord
}

func (g domainGroupRow) fresh(now time.Time) bool {
	return g.rec.State != "unreachable" && !g.rec.LastSuccessAt.IsZero() && now.Sub(g.rec.LastSuccessAt) <= domainStaleAfter
}

func routeServes(kind string) bool { return kind == "managed" || kind == "manual" }

// representative picks the row that speaks for a domain: the one actually
// serving it, preferring fresh evidence and a DNS answer that agrees.
func (g domainGroup) representative(now time.Time) domainGroupRow {
	best, bestScore := g.rows[0], -1
	for _, r := range g.rows {
		score := 0
		if r.fresh(now) {
			score += 8
		}
		switch {
		case routeServes(r.row.Route.Kind):
			score += 4
		case r.row.Route.Kind == "stale_managed" || r.row.Route.Kind == "orphan_managed":
			score += 2
		}
		if r.row.DNS.Status == "ok" || r.row.DNS.Status == "proxied" {
			score++
		}
		if score > bestScore {
			best, bestScore = r, score
		}
	}
	return best
}

func domainGroups(snap *domainSnapshot) []domainGroup {
	byHost := map[string]*domainGroup{}
	names := make([]string, 0, len(snap.Servers))
	for name := range snap.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rec := snap.Servers[name]
		for _, row := range rec.Domains {
			if row.Skip == "ip_literal" || row.Skip == "invalid_name" {
				continue
			}
			g := byHost[row.Host]
			if g == nil {
				g = &domainGroup{host: row.Host}
				byHost[row.Host] = g
			}
			g.rows = append(g.rows, domainGroupRow{row: row, rec: rec})
		}
	}
	out := make([]domainGroup, 0, len(byHost))
	for _, g := range byHost {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].host < out[j].host })
	return out
}

func isLabelOnly(skip string) bool { return skip == "external_label" || skip == "host_label" }

// humanSpan says how long, in the largest unit that reads naturally.
func humanSpan(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return strconv.Itoa(n) + " " + unit + "s"
	}
	switch {
	case d >= 48*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d >= 2*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d >= 2*time.Minute:
		return plural(int(d.Minutes()), "minute")
	default:
		return plural(int(d.Seconds()), "second")
	}
}

func dnsLayer(g domainGroup, rep domainGroupRow, now time.Time) domainLayer {
	if !rep.fresh(now) {
		return domainLayer{layerUnknown, staleEvidence(rep.rec, now)}
	}
	// One host on several servers: any instance DNS points at answers for it.
	for _, r := range g.rows {
		if r.fresh(now) && (r.row.DNS.Status == "ok" || r.row.DNS.Status == "proxied") {
			rep = r
			break
		}
	}
	switch rep.row.DNS.Status {
	case "ok":
		return domainLayer{layerOK, "points at " + rep.row.Server}
	case "proxied":
		return domainLayer{layerOK, "behind Cloudflare (the origin cannot be read from DNS)"}
	case "nxdomain":
		return domainLayer{layerFail, "no DNS record"}
	case "wrong_target":
		// DNS may point at a server that could not be read this round.
		for _, r := range g.rows {
			if !r.fresh(now) {
				return domainLayer{layerUnknown, "DNS may point at " + r.row.Server + ", which could not be checked"}
			}
		}
		answer := "another address"
		if len(rep.row.DNS.Answers) > 0 {
			answer = rep.row.DNS.Answers[0]
		}
		return domainLayer{layerFail, fmt.Sprintf("DNS points at %s, not %s", answer, rep.row.Server)}
	case "skipped":
		return domainLayer{layerNA, sanitizeDomainError(rep.row.DNS.Detail)}
	}
	detail := sanitizeDomainError(rep.row.DNS.Detail)
	if detail == "" {
		detail = "the public resolvers did not answer"
	}
	return domainLayer{layerUnknown, detail}
}

func staleEvidence(rec *domainServerRecord, now time.Time) string {
	if rec.LastSuccessAt.IsZero() {
		return rec.Server + " could not be checked"
	}
	age := "last checked " + humanSpan(now.Sub(rec.LastSuccessAt)) + " ago"
	if rec.State == "unreachable" {
		return rec.Server + " could not be checked (" + age + ")"
	}
	return age
}

func routeLayer(rep domainGroupRow, now time.Time) domainLayer {
	if !rep.fresh(now) {
		return domainLayer{layerUnknown, staleEvidence(rep.rec, now)}
	}
	r := rep.row.Route
	switch r.Kind {
	case "stale_managed":
		return domainLayer{layerFail, fmt.Sprintf("stale route: %s is no longer routed by Caddy, but its block is still in the Caddyfile on %s", r.Owner, rep.row.Server)}
	case "orphan_managed":
		return domainLayer{layerFail, fmt.Sprintf("route left behind by %s, which is not deployed on %s", r.Owner, rep.row.Server)}
	case "none":
		return domainLayer{layerFail, "no route on " + rep.row.Server}
	case "unknown":
		return domainLayer{layerUnknown, "the route could not be read on " + rep.row.Server}
	}
	var dead []string
	for _, u := range r.Upstreams {
		if u.Resolves == "no" {
			dead = append(dead, sanitizeDomainError(u.Dial))
		}
	}
	switch r.UpstreamResolves {
	case "no":
		return domainLayer{layerFail, "route points at a container that no longer exists (" + strings.Join(dead, ", ") + ")"}
	case "partial":
		return domainLayer{layerWarn, "part of the route points at a container that no longer exists (" + strings.Join(dead, ", ") + ")"}
	case "unknown":
		return domainLayer{layerUnknown, "could not check the route's upstream"}
	}
	if r.Kind == "manual" {
		return domainLayer{layerOK, "hand-written Caddy block on " + rep.row.Server}
	}
	return domainLayer{layerOK, "routed by " + r.Owner}
}

// certThresholds scale with the certificate's lifetime: a 6-day certificate is
// always under 14 days, and warning about it for its whole life is noise.
func certThresholds(notBefore, notAfter *time.Time) (warn, critical time.Duration) {
	warn, critical = domainCertWarn, domainCertCritical
	if notBefore != nil && notAfter != nil {
		if third := notAfter.Sub(*notBefore) / 3; third > 0 && third < warn {
			warn = third
		}
	}
	if half := warn / 2; half < critical {
		critical = half
	}
	return warn, critical
}

func certLayer(g domainGroup, rep *domainGroupRow, mon domainMonitorEvidence, hasMon bool, now time.Time) (domainLayer, *time.Time) {
	var notAfter *time.Time
	live := domainLayer{}
	if hasMon && mon.known && mon.result.CertNotAfter != nil && now.Sub(mon.result.CheckedAt) <= domainMonitorStaleAfter {
		notAfter = mon.result.CertNotAfter
		left := notAfter.Sub(now)
		warn, _ := certThresholds(mon.result.CertNotBefore, notAfter)
		switch {
		case left <= 0:
			live = domainLayer{layerFail, "certificate expired " + humanSpan(left) + " ago"}
		case left < warn:
			live = domainLayer{layerWarn, "certificate expires in " + humanSpan(left)}
		default:
			live = domainLayer{layerOK, "certificate valid for " + humanSpan(left)}
		}
	}
	var obtain domainLayer
	if rep != nil {
		obtain = obtainLayer(g, *rep, now)
	}
	switch {
	case live.State == layerFail:
		return live, notAfter
	case obtain.State == layerFail:
		return obtain, notAfter
	case live.State == layerWarn:
		return live, notAfter
	case obtain.State == layerWarn:
		return obtain, notAfter
	case live.State == layerOK:
		return live, notAfter
	case obtain.State != "":
		return obtain, notAfter
	}
	if hasMon {
		return domainLayer{layerUnknown, "no certificate seen yet"}, nil
	}
	return domainLayer{layerNA, ""}, nil
}

func obtainLayer(g domainGroup, rep domainGroupRow, now time.Time) domainLayer {
	if !rep.fresh(now) {
		return domainLayer{layerUnknown, staleEvidence(rep.rec, now)}
	}
	// A failing obtain anywhere is live evidence, whichever row speaks for the
	// domain: the stale block on the old server is the one burning rate limits.
	for _, r := range g.rows {
		if !r.fresh(now) || r.row.TLS.Status != "failing" {
			continue
		}
		span := "certificate failing"
		state := layerWarn
		if r.row.TLS.FailingSince != nil {
			failing := now.Sub(*r.row.TLS.FailingSince)
			span = "certificate failing for " + humanSpan(failing)
			if failing >= domainObtainFailing {
				state = layerFail
			}
		}
		if r.row.Server != rep.row.Server {
			span += " on " + r.row.Server
		}
		return domainLayer{state, span}
	}
	switch rep.row.TLS.Status {
	case "ok":
		return domainLayer{layerOK, "no certificate failures"}
	case "unknown":
		return domainLayer{layerUnknown, "Caddy's logs are too short or unreadable to say"}
	}
	return domainLayer{layerNA, ""}
}

func httpLayer(mon domainMonitorEvidence, hasMon bool, now time.Time) domainLayer {
	if !hasMon {
		return domainLayer{layerNA, "no monitor"}
	}
	if !mon.known {
		return domainLayer{layerUnknown, "not checked yet"}
	}
	if now.Sub(mon.result.CheckedAt) > domainMonitorStaleAfter {
		return domainLayer{layerUnknown, "last checked " + humanSpan(now.Sub(mon.result.CheckedAt)) + " ago"}
	}
	if mon.result.Status == "up" {
		if mon.result.StatusCode > 0 {
			return domainLayer{layerOK, "HTTP " + strconv.Itoa(mon.result.StatusCode)}
		}
		return domainLayer{layerOK, "reachable"}
	}
	msg := sanitizeDomainError(mon.result.Message)
	if msg == "" {
		msg = "the site is not answering"
	}
	return domainLayer{layerFail, msg}
}

// finishRow names the first failing layer, in the order a request meets them.
func finishRow(v *domainRowView) {
	layers := []struct {
		name string
		l    domainLayer
	}{{"dns", v.DNS}, {"route", v.Route}, {"cert", v.Cert}, {"http", v.HTTP}}
	for _, want := range []string{layerFail, layerWarn} {
		for _, l := range layers {
			if l.l.State == want {
				v.Overall, v.FirstFailing, v.Evidence = want, l.name, l.l.Detail
				return
			}
		}
	}
	for _, l := range layers {
		if l.l.State == layerUnknown {
			v.Overall, v.Evidence = layerUnknown, l.l.Detail
			return
		}
	}
	v.Overall = layerOK
}

// buildDomainRows joins the CLI's rows with the monitors' evidence.
func buildDomainRows(snap *domainSnapshot, monitors map[string]domainMonitorEvidence, now time.Time) []domainRowView {
	groups := domainGroups(snap)
	seen := make(map[string]bool, len(groups))
	rows := make([]domainRowView, 0, len(groups))
	for _, g := range groups {
		seen[g.host] = true
		rep := g.representative(now)
		mon, hasMon := monitors[g.host]
		v := domainRowView{Host: g.host, App: rep.row.App, Server: rep.row.Server, Ingress: rep.row.Ingress}
		if v.App == "" {
			v.App = rep.row.Route.Owner
		}
		if hasMon {
			v.MonitorID = mon.monitor.ID
		}
		if !rep.rec.LastSuccessAt.IsZero() {
			at := rep.rec.LastSuccessAt
			age := int64(now.Sub(at) / time.Second)
			v.CheckedAt, v.AgeSeconds = &at, &age
		}
		if isLabelOnly(rep.row.Skip) && !hasMon {
			v.LabelOnly = true
			why := "ingress: " + rep.row.Ingress + ", and nothing fronts it"
			v.DNS, v.Route, v.Cert, v.HTTP = domainLayer{layerNA, why}, domainLayer{layerNA, why}, domainLayer{layerNA, ""}, domainLayer{layerNA, ""}
			v.Overall, v.Evidence = layerNA, "label only: "+why
			rows = append(rows, v)
			continue
		}
		v.DNS = dnsLayer(g, rep, now)
		if isLabelOnly(rep.row.Skip) {
			v.Route = domainLayer{layerNA, "ingress: " + rep.row.Ingress + ", no Caddy route"}
		} else {
			v.Route = routeLayer(rep, now)
		}
		v.Cert, v.CertNotAfter = certLayer(g, &rep, mon, hasMon, now)
		v.HTTP = httpLayer(mon, hasMon, now)
		if hasMon && mon.known {
			at := mon.result.CheckedAt
			v.HTTPCheckedAt = &at
		}
		finishRow(&v)
		rows = append(rows, v)
	}
	// Monitored domains no teploy server serves (or none has reported yet).
	for host, mon := range monitors {
		if seen[host] {
			continue
		}
		v := domainRowView{Host: host, App: mon.monitor.App, Server: mon.monitor.Server, MonitorID: mon.monitor.ID}
		na := domainLayer{layerNA, "not on a Teploy server"}
		if mon.monitor.Server != "" {
			na = domainLayer{layerUnknown, "not checked yet on " + mon.monitor.Server}
		}
		v.DNS, v.Route = na, na
		v.Cert, v.CertNotAfter = certLayer(domainGroup{}, nil, mon, true, now)
		v.HTTP = httpLayer(mon, true, now)
		if mon.known {
			at := mon.result.CheckedAt
			v.HTTPCheckedAt = &at
		}
		finishRow(&v)
		rows = append(rows, v)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Host < rows[j].Host })
	return rows
}

// ── the API ──────────────────────────────────────────────────────────────────

type domainServerView struct {
	Server        string        `json:"server"`
	State         string        `json:"state"`
	CheckedAt     *time.Time    `json:"checked_at,omitempty"`
	AgeSeconds    *int64        `json:"age_seconds,omitempty"`
	LastAttemptAt time.Time     `json:"last_attempt_at"`
	Error         string        `json:"error,omitempty"`
	Issues        []domainIssue `json:"issues,omitempty"`
	Caddy         domainCaddy   `json:"caddy"`
}

func (s *Server) domainsView(ctx context.Context, now time.Time) map[string]any {
	caps := s.capabilities(ctx)
	supported := caps.CLI.Installed && caps.Features.DomainsCheck
	monitors, _, _ := s.domainMonitors()

	d := &s.domains
	d.mu.Lock()
	defer d.mu.Unlock()
	rows := buildDomainRows(&d.snap, monitors, now)
	servers := make([]domainServerView, 0, len(d.snap.Servers))
	for _, rec := range d.snap.Servers {
		sv := domainServerView{Server: rec.Server, State: rec.State, LastAttemptAt: rec.LastAttemptAt,
			Error: rec.Error, Issues: rec.Issues, Caddy: rec.Caddy}
		if !rec.LastSuccessAt.IsZero() {
			at := rec.LastSuccessAt
			age := int64(now.Sub(at) / time.Second)
			sv.CheckedAt, sv.AgeSeconds = &at, &age
		}
		servers = append(servers, sv)
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Server < servers[j].Server })

	view := map[string]any{
		"rows":                   rows,
		"servers":                servers,
		"cli_supported":          supported,
		"cli_version":            caps.CLI.Version,
		"min_cli_version":        domainsMinCLI,
		"sweep_interval_seconds": int(domainSweepInterval / time.Second),
	}
	if !d.snap.LastSweepAt.IsZero() {
		view["last_sweep_at"] = d.snap.LastSweepAt
	}
	if d.snap.SweepError != "" {
		view["sweep_error"] = d.snap.SweepError
	}
	if !supported {
		view["note"] = "DNS, route and certificate-obtain checks need teploy " + domainsMinCLI + " or later on the dashboard host. Certificate expiry and HTTP still come from the monitors."
	}
	return view
}

func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeData(w, s.domainsView(r.Context(), time.Now().UTC()))
}

// ── incidents ────────────────────────────────────────────────────────────────

func domainRuleKey(cause, host string) string { return "domain:" + cause + ":" + host }

func domainRule(cause, host, app, server, severity string, policy incident.Policy) incident.Rule {
	label := map[string]string{"dns": "DNS", "cert-expiry": "certificate expiry", "cert-obtain": "certificate"}[cause]
	target := "https://" + host + "/"
	if strings.HasPrefix(host, "*.") {
		target = host
	}
	return incident.Rule{Key: domainRuleKey(cause, host), Source: alert.SourceDomain, ID: "domain-" + cause + "-" + host,
		Name: host + " " + label, App: app, Server: server, Target: target, Severity: severity,
		Path: "/domains", Policy: policy}
}

// Debounce. DNS can blip, so two sweeps; the certificate rules are already
// conditions over days, so one reading of them is the alert.
var (
	domainDNSPolicy  = incident.Policy{FailAfter: 2, RecoverAfter: 2}
	domainCertPolicy = incident.Policy{FailAfter: 1, RecoverAfter: 1}
)

type domainObservation struct {
	rule     incident.Rule
	ok       bool
	message  string
	host     string
	adoptKey string
	// sameOutage marks a failure that is the outage an open HTTP monitor incident
	// already reports, so it opens nothing of its own.
	sameOutage bool
	startedAt  time.Time
}

// domainRuleKeys maps a rule key to the servers whose rows evidence it.
type domainRuleKeys map[string]map[string]bool

func (k domainRuleKeys) add(key, server string) {
	if k[key] == nil {
		k[key] = map[string]bool{}
	}
	if server != "" {
		k[key][server] = true
	}
}

// domainObservations derives what this sweep says about each rule. Unknown says
// nothing. keys are the rules that still apply, which the caller uses to retire
// the rest.
func domainObservations(snap *domainSnapshot, monitors map[string]domainMonitorEvidence, monitorsListed bool, supported bool, now time.Time) (obs []domainObservation, keys domainRuleKeys) {
	keys = domainRuleKeys{}
	for _, g := range domainGroups(snap) {
		if !supported {
			break
		}
		rep := g.representative(now)
		app := rep.row.App
		if app == "" {
			app = rep.row.Route.Owner
		}

		// DNS: only for a route teploy or a person actually serves here. A stale
		// block's name has nothing to resolve to; its row names the route.
		if routeServes(rep.row.Route.Kind) && !isLabelOnly(rep.row.Skip) && rep.row.DNS.Status != "skipped" {
			key := domainRuleKey("dns", g.host)
			for _, r := range g.rows {
				keys.add(key, r.row.Server)
			}
			layer := dnsLayer(g, rep, now)
			switch layer.State {
			case layerOK:
				obs = append(obs, domainObservation{rule: domainRule("dns", g.host, app, rep.row.Server, alert.SeverityCritical, domainDNSPolicy), ok: true, message: layer.Detail, host: g.host, adoptKey: rep.row.Server})
			case layerFail:
				at := rep.rec.LastSuccessAt
				obs = append(obs, domainObservation{rule: domainRule("dns", g.host, app, rep.row.Server, alert.SeverityCritical, domainDNSPolicy), message: layer.Detail, host: g.host, adoptKey: rep.row.Server, sameOutage: true, startedAt: at})
			}
		}

		// Certificate obtain: any row Caddy would obtain for.
		for _, r := range g.rows {
			if r.row.TLS.Status == "not_applicable" || r.row.TLS.Status == "" {
				continue
			}
			keys.add(domainRuleKey("cert-obtain", g.host), r.row.Server)
		}
		if keys[domainRuleKey("cert-obtain", g.host)] != nil {
			obtainRow, obtainApp := rep, app
			for _, r := range g.rows {
				if r.fresh(now) && r.row.TLS.Status == "failing" {
					obtainRow = r
					obtainApp = r.row.App
					if obtainApp == "" {
						obtainApp = r.row.Route.Owner
					}
					break
				}
			}
			layer := obtainLayer(g, obtainRow, now)
			rule := domainRule("cert-obtain", g.host, obtainApp, obtainRow.row.Server, alert.SeverityWarning, domainCertPolicy)
			switch layer.State {
			case layerOK:
				obs = append(obs, domainObservation{rule: rule, ok: true, message: "certificate obtain is no longer failing", host: g.host, adoptKey: obtainRow.row.Server})
			case layerWarn:
				// Failing, for under a day as far as the logs reach: not yet this
				// rule's condition to open, and a recreated Caddy's reset clock is
				// not a recovery either. Only a real ok closes.
			case layerFail:
				message := layer.Detail
				if routeLayerStale := obtainRow.row.Route.Kind == "stale_managed"; routeLayerStale {
					message += "; it comes from a stale route, which the next deploy of " + obtainApp + " removes"
				}
				if e := sanitizeDomainError(obtainRow.row.TLS.LastError); e != "" {
					message += ": " + e
				}
				started := now
				if obtainRow.row.TLS.FailingSince != nil {
					started = *obtainRow.row.TLS.FailingSince
				}
				obs = append(obs, domainObservation{rule: rule, message: message, host: g.host, adoptKey: obtainRow.row.Server, sameOutage: true, startedAt: started})
			}
		}
	}

	// Certificate expiry: from the monitors, so it covers every monitored domain,
	// including ones not on any teploy server, whatever the CLI can do.
	if monitorsListed {
		for host, mon := range monitors {
			if !strings.HasPrefix(strings.ToLower(mon.monitor.Target), "https://") {
				continue
			}
			keys.add(domainRuleKey("cert-expiry", host), "")
			if !mon.known || mon.result.CertNotAfter == nil || now.Sub(mon.result.CheckedAt) > domainMonitorStaleAfter {
				continue
			}
			left := mon.result.CertNotAfter.Sub(now)
			warnAt, criticalAt := certThresholds(mon.result.CertNotBefore, mon.result.CertNotAfter)
			severity := alert.SeverityWarning
			if left < criticalAt {
				severity = alert.SeverityCritical
			}
			rule := domainRule("cert-expiry", host, mon.monitor.App, mon.monitor.Server, severity, domainCertPolicy)
			if left >= warnAt {
				obs = append(obs, domainObservation{rule: rule, ok: true, message: "certificate valid for " + humanSpan(left), host: host, adoptKey: domainAdoptMonitors})
				continue
			}
			message := "certificate expires in " + humanSpan(left) + " (" + mon.result.CertNotAfter.UTC().Format("2006-01-02") + ")"
			if left <= 0 {
				message = "certificate expired " + humanSpan(left) + " ago (" + mon.result.CertNotAfter.UTC().Format("2006-01-02") + ")"
			}
			obs = append(obs, domainObservation{rule: rule, message: message, host: host, adoptKey: domainAdoptMonitors, sameOutage: left <= 0, startedAt: mon.result.CheckedAt})
		}
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].rule.Key < obs[j].rule.Key })
	return obs, keys
}

func (s *Server) evaluateDomainIncidents(now time.Time, supported bool) {
	t := s.alerts.tracker
	if t == nil {
		return
	}
	monitors, hostOfRule, listed := s.domainMonitors()

	d := &s.domains
	d.mu.Lock()
	defer d.mu.Unlock()
	obs, keys := domainObservations(&d.snap, monitors, listed, supported, now)

	open := map[string]bool{}
	monitorDown := map[string]bool{}
	for _, inc := range t.Open() {
		open[inc.Key] = true
		if host, ok := hostOfRule[inc.Key]; ok {
			monitorDown[host] = true
		}
	}

	adopted := 0
	for _, o := range obs {
		switch {
		case o.ok:
			t.Observe(o.rule, true, now, o.message)
		case o.sameOutage && monitorDown[o.host] && !open[o.rule.Key]:
			// The monitor's incident owns this outage (see the file comment).
		case !d.snap.Adopted[o.adoptKey]:
			// The first results from a source: what was already broken becomes
			// one summary, announced below.
			if t.AdoptFailing(o.rule, o.startedAt, o.message) {
				adopted++
			} else {
				t.Observe(o.rule, false, now, o.message)
			}
		default:
			t.Observe(o.rule, false, now, o.message)
		}
	}

	// Mark what has now fully reported. A server read only in part gave only part
	// of what there is to adopt, and the rest must not arrive as individual opens.
	for name, rec := range d.snap.Servers {
		if supported && rec.State == "ok" && !rec.LastSuccessAt.IsZero() {
			d.snap.Adopted[name] = true
		}
	}
	if listed {
		d.snap.Adopted[domainAdoptMonitors] = true
	}
	if adopted > 0 {
		log.Printf("[domains] first results: %d existing failure(s) adopted into one announcement", adopted)
		t.AnnounceWith(incident.Summary{Intro: "First domain health results. Already failing:", Path: "/domains", Source: alert.SourceDomain})
	}

	// Retire a rule that no longer applies only on evidence from the servers that
	// held it: a partly read server's rows lose their rule keys without anything
	// having changed. Its keys are carried instead, and a dead server elsewhere
	// holds up nothing else.
	carried := domainRuleKeys{}
	for key, servers := range keys {
		carried[key] = servers
	}
	mayRetire := func(key string, servers map[string]bool) bool {
		if strings.HasPrefix(key, "domain:cert-expiry:") {
			return listed
		}
		if !supported {
			return false
		}
		if len(servers) == 0 {
			// Nothing says whose it was: every server must have been read.
			for _, rec := range d.snap.Servers {
				if rec.State != "ok" {
					return false
				}
			}
			return true
		}
		for name := range servers {
			// A server removed from the fleet has no record, and no say.
			if rec := d.snap.Servers[name]; rec != nil && rec.State != "ok" {
				return false
			}
		}
		return true
	}
	consider := func(key string, servers map[string]bool) {
		if keys[key] != nil {
			return
		}
		if mayRetire(key, servers) {
			t.Retire(key, "domain no longer served")
			return
		}
		for name := range servers {
			carried.add(key, name)
		}
		if len(servers) == 0 {
			carried.add(key, "")
		}
	}
	for key, servers := range d.ruleKeys {
		consider(key, servers)
	}
	for _, inc := range t.Open() {
		if inc.Source != alert.SourceDomain || carried[inc.Key] != nil {
			continue
		}
		servers := d.ruleKeys[inc.Key]
		if len(servers) == 0 && inc.Server != "" {
			servers = map[string]bool{inc.Server: true}
		}
		consider(inc.Key, servers)
	}
	d.ruleKeys = carried
	d.saveLocked()
}

// ── fleet monitors ───────────────────────────────────────────────────────────

// domainFronted reports whether evidence shows something fronts a host: a live
// route naming it on any server, or public DNS that answers (a proxy, a tunnel,
// another box). False is not proof of absence: callers treat it as undetermined.
func (s *Server) domainFronted(host string) bool {
	d := &s.domains
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.snap.Unsupported {
		return false
	}
	for _, rec := range d.snap.Servers {
		for _, row := range rec.Domains {
			if row.Host != host {
				continue
			}
			if routeServes(row.Route.Kind) || row.DNS.Status == "ok" || row.DNS.Status == "proxied" {
				return true
			}
		}
	}
	return false
}
