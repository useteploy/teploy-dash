package server

// siblings.go — Programme B item 1 (plan section 10.1): sibling product
// discovery. The CLI (`teploy server siblings <server> --json`, capability
// siblings-v1, contracts corpus rev 20) reports the candidates a server's
// containers and images claim — labels only, no probing. Dash does the
// probing: each candidate's port hints are tried for the product's discovery
// endpoint (`GET /.well-known/teploy.json`; the forgejo family answers
// `GET /api/v1/version` instead) with a short timeout on the monitor network
// guard, tailnet/private addresses included only when the owner allowed
// internal probing.
//
// Honesty rules, mirroring the domains and cleanup pages:
//
//   - A probe is ADVISORY evidence until an admin confirms: nav fills from
//     confirmed discovery docs only, and the Observe connect flow never
//     trusts a probe alone (instance_id confirmation, see observe.go).
//     Nothing is ever guessed: an empty dashboard_url or ingest_url in a
//     doc is unknown, not defaulted.
//   - state partial/unreachable is never rendered as "no siblings". A failed
//     sweep keeps the last candidates, with their age and the failure.
//   - A label whose probe answered but found no discovery doc reads
//     "found, not discoverable" (Ship ships discovery default-off) — that is
//     a positive sighting, not an error.
//   - Products the CLI does not recognise pass through verbatim
//     (unknown_labeled) and are probed like any other: a product newer than
//     the CLI stays visible.
//
// State is one dash-owned file-store record: <data>/siblings.json. The
// allow_internal choice lives there too (an admin act, audited — the A13
// rule for monitors applied to this probe).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/durable"
	"github.com/useteploy/teploy-dash/internal/monitor"
	"github.com/useteploy/teploy-dash/internal/remote"
)

// siblingsMinCLI is the release that brings `server siblings`: a CLI that
// does not advertise siblings-v1 reads as unsupported, whatever its version.
const siblingsMinCLI = "v0.1.54"

const (
	// siblingsSweepInterval: siblings change rarely (a deploy), so an hour
	// between sweeps; the page offers an on-demand rescan.
	siblingsSweepInterval    = time.Hour
	siblingsServerTimeout    = 2 * time.Minute
	siblingsSweepConcurrency = 3
	// siblingMaxAddresses caps the dial candidates per product group.
	siblingMaxAddresses = 3
	siblingDocLimit     = 64 << 10
)

// siblingProbeTimeout bounds one discovery GET (per address); a var so tests
// can shorten blackholed-dial waits.
var siblingProbeTimeout = 4 * time.Second

// Probe outcome states (stable API strings).
const (
	probeConfirmed       = "confirmed"              // a discovery doc answered
	probeNotDiscoverable = "found_not_discoverable" // answered, no doc (e.g. Ship default-off)
	probeUnreachable     = "unreachable"            // no candidate answered
	probeNotProbeable    = "not_probeable"          // only loopback binds (dash cannot reach them)
	probeBlockedInternal = "blocked_internal"       // private/tailnet target, allow_internal off
)

// ── the CLI's envelope (contracts/schema/siblings-envelope.schema.json) ──────

type siblingsEnvelope struct {
	MachineInterface int          `json:"machine_interface"`
	Server           string       `json:"server"`
	Host             string       `json:"host"`
	ObservedAt       time.Time    `json:"observed_at"`
	State            string       `json:"state"` // ok | partial | unreachable
	Error            string       `json:"error,omitempty"`
	Siblings         []siblingRow `json:"siblings"`
	UnknownLabeled   []siblingRow `json:"unknown_labeled"`
}

// siblingRow keeps product as a plain string on purpose: an unknown_labeled
// row's product is a value this dash has never heard of and must still show.
type siblingRow struct {
	Container string            `json:"container"`
	Product   string            `json:"product"`
	Version   string            `json:"version,omitempty"`
	App       string            `json:"app,omitempty"`
	State     string            `json:"state"`
	Image     string            `json:"image"`
	PortHints []siblingPortHint `json:"port_hints"`
	Source    string            `json:"source"`
}

type siblingPortHint struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Proto         string `json:"proto"`
}

// decodeSiblingsEnvelope runs the envelope through the central MI gate and
// the completeness rules the page relies on.
func decodeSiblingsEnvelope(raw string) (*siblingsEnvelope, error) {
	if _, err := cli.ParseJSON(raw); err != nil {
		return nil, err
	}
	var env siblingsEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, fmt.Errorf("decoding the siblings report: %w", err)
	}
	switch env.State {
	case "ok", "partial", "unreachable":
	default:
		return nil, fmt.Errorf("decoding the siblings report: unknown state %q", env.State)
	}
	if env.Server == "" || env.ObservedAt.IsZero() || env.Siblings == nil || env.UnknownLabeled == nil {
		return nil, fmt.Errorf("decoding the siblings report: incomplete machine response")
	}
	for _, s := range append(append([]siblingRow{}, env.Siblings...), env.UnknownLabeled...) {
		if s.Container == "" || s.Product == "" {
			return nil, fmt.Errorf("decoding the siblings report: incomplete machine response for sibling %q", s.Container)
		}
	}
	return &env, nil
}

// ── discovery docs ───────────────────────────────────────────────────────────

// siblingDiscoveryDoc is the unauthenticated `GET /.well-known/teploy.json`
// answer every Teploy product serves on its dashboard listener. Empty URLs
// are unknowns, never defaulted (service-tokens.md: "Treat an empty URL as
// unknown and ask").
type siblingDiscoveryDoc struct {
	Product      string   `json:"product"`
	Version      string   `json:"version"`
	InstanceID   string   `json:"instance_id"`
	DashboardURL string   `json:"dashboard_url"`
	IngestURL    string   `json:"ingest_url"`
	Capabilities []string `json:"capabilities"`
}

// Discovery endpoints by family: the teploy family serves
// /.well-known/teploy.json; the forgejo family has no discovery doc, and its
// plain /api/v1/version is the sighting instead.
const (
	teployDiscoveryPath = "/.well-known/teploy.json"
	forgejoVersionPath  = "/api/v1/version"
)

func isForgejoFamily(product string) bool {
	switch strings.ToLower(strings.TrimSpace(product)) {
	case "forgejo", "gitea", "gogs":
		return true
	}
	return false
}

// ── persisted state ──────────────────────────────────────────────────────────

// siblingProbe is dash's verdict about one (server, product) group.
type siblingProbe struct {
	Server         string               `json:"server"`
	Product        string               `json:"product"` // the label's claim
	Containers     []string             `json:"containers"`
	App            string               `json:"app,omitempty"`
	LabelVersion   string               `json:"label_version,omitempty"`
	ContainerState string               `json:"container_state,omitempty"`
	Addresses      []string             `json:"addresses,omitempty"`
	State          string               `json:"state"`
	BaseURL        string               `json:"base_url,omitempty"`
	Doc            *siblingDiscoveryDoc `json:"doc,omitempty"`
	// Version is the confirmed product version (the doc's, or the forgejo
	// API's); LabelVersion stays the container-label hint.
	Version  string    `json:"version,omitempty"`
	Error    string    `json:"error,omitempty"`
	ProbedAt time.Time `json:"probed_at"`
}

// siblingsServerRecord is one server's newest report plus dash's probes. A
// failed sweep keeps the previous rows and probes, with their age.
type siblingsServerRecord struct {
	Server         string          `json:"server"`
	Host           string          `json:"host,omitempty"`
	State          string          `json:"state"`
	Error          string          `json:"error,omitempty"`
	Siblings       []siblingRow    `json:"siblings"`
	UnknownLabeled []siblingRow    `json:"unknown_labeled"`
	Probes         []*siblingProbe `json:"probes"`
	LastAttemptAt  time.Time       `json:"last_attempt_at"`
	LastSuccessAt  time.Time       `json:"last_success_at,omitempty"`
}

type siblingsSnapshot struct {
	Owner         string                           `json:"owner"`
	Servers       map[string]*siblingsServerRecord `json:"servers"`
	AllowInternal bool                             `json:"allow_internal,omitempty"`
	LastSweepAt   time.Time                        `json:"last_sweep_at,omitempty"`
	Unsupported   bool                             `json:"unsupported,omitempty"`
	SweepError    string                           `json:"sweep_error,omitempty"`
}

const siblingsOwner = "teploy-dash"

type siblingsState struct {
	path string
	mu   sync.Mutex
	snap siblingsSnapshot
	// sweepInFlight guards the on-demand rescan (TryLock: one at a time).
	sweepInFlight sync.Mutex
}

func (s *Server) initSiblings(dataDir string) {
	sb := &s.siblings
	sb.snap = siblingsSnapshot{Owner: siblingsOwner, Servers: map[string]*siblingsServerRecord{}}
	if dataDir == "" {
		return
	}
	sb.path = filepath.Join(dataDir, "siblings.json")
	raw, err := os.ReadFile(sb.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var snap siblingsSnapshot
	if err == nil {
		err = json.Unmarshal(raw, &snap)
	}
	if err != nil {
		// Unreadable state must not take the page down: set aside, start over.
		if aside := setAside(sb.path, err); aside != nil {
			log.Printf("[siblings] %s is unreadable (%v) and could not be set aside: %v", sb.path, err, aside)
			sb.path = ""
		}
		return
	}
	if snap.Servers == nil {
		snap.Servers = map[string]*siblingsServerRecord{}
	}
	snap.Owner = siblingsOwner
	sb.snap = snap
}

func (sb *siblingsState) saveLocked() {
	if sb.path == "" {
		return
	}
	raw, err := json.Marshal(sb.snap)
	if err == nil {
		err = durable.Replace(sb.path, raw, 0600)
	}
	if err != nil {
		log.Printf("[siblings] persist failed (state in memory until the next sweep): %v", err)
	}
}

// ── the sweep ────────────────────────────────────────────────────────────────

// RunSiblings sweeps now (shortly after start, letting the fleet cache warm)
// and then every interval until ctx ends. One loop, so sweeps cannot pile up.
func (s *Server) RunSiblings(ctx context.Context) {
	wait := time.Duration(0)
	s.siblings.mu.Lock()
	if last := s.siblings.snap.LastSweepAt; !last.IsZero() {
		if remaining := siblingsSweepInterval - time.Since(last); remaining > 0 {
			wait = remaining
		}
	}
	s.siblings.mu.Unlock()
	if wait == 0 {
		wait = 30 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.sweepSiblings(ctx)
		timer.Reset(siblingsSweepInterval)
	}
}

func (s *Server) sweepSiblings(ctx context.Context) {
	supported := s.cliInstalled() && s.capabilities(ctx).Features.Siblings
	var sweepErr string
	if supported {
		if err := s.collectSiblings(ctx); err != nil {
			sweepErr = sanitizeDomainError(err.Error())
			log.Printf("[siblings] %s", sweepErr)
		}
	}
	s.siblings.mu.Lock()
	s.siblings.snap.LastSweepAt = time.Now().UTC()
	s.siblings.snap.Unsupported = !supported
	s.siblings.snap.SweepError = sweepErr
	s.siblings.saveLocked()
	s.siblings.mu.Unlock()
}

// collectSiblings reads the envelope per server under bounded concurrency
// and probes what came back. One bad server costs its own budget.
func (s *Server) collectSiblings(ctx context.Context) error {
	servers, err := s.resolveServers(ctx)
	if err != nil {
		return fmt.Errorf("server discovery failed: %w", err)
	}
	allowInternal := s.siblingsAllowInternal()
	client := siblingHTTPClient(allowInternal)
	sem := make(chan struct{}, siblingsSweepConcurrency)
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
			checkCtx, cancel := context.WithTimeout(ctx, siblingsServerTimeout)
			defer cancel()
			env, err := s.runSiblingsCheck(checkCtx, srv.Name)
			if ctx.Err() != nil {
				return // shutting down: not evidence about the server
			}
			s.recordSiblings(checkCtx, srv, env, err, client, time.Now().UTC())
		}(srv)
	}
	wg.Wait()

	names := make(map[string]bool, len(servers))
	for _, srv := range servers {
		names[srv.Name] = true
	}
	s.siblings.mu.Lock()
	for name := range s.siblings.snap.Servers {
		if !names[name] {
			delete(s.siblings.snap.Servers, name)
		}
	}
	s.siblings.mu.Unlock()
	return nil
}

func (s *Server) runSiblingsCheck(ctx context.Context, server string) (*siblingsEnvelope, error) {
	args := []string{"server", "siblings", server, "--json"}
	result, err := s.runCLI(ctx, args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, commandFailure(args, result)
	}
	return decodeSiblingsEnvelope(result.Stdout)
}

// recordSiblings merges one server's outcome into the cache, then probes the
// candidates a successful read produced (the probes need the read's port
// hints; a failed read keeps the previous rows and probes with their age).
// The probes run outside the state lock; the merge below re-checks the
// record is still this sweep's before writing.
func (s *Server) recordSiblings(ctx context.Context, srv remote.ServerConn, env *siblingsEnvelope, err error, client *http.Client, now time.Time) {
	sb := &s.siblings
	sb.mu.Lock()
	rec := sb.snap.Servers[srv.Name]
	if rec == nil {
		rec = &siblingsServerRecord{Server: srv.Name, Siblings: []siblingRow{}, UnknownLabeled: []siblingRow{}, Probes: []*siblingProbe{}}
		sb.snap.Servers[srv.Name] = rec
	}
	rec.Host = srv.Host
	rec.LastAttemptAt = now
	if err != nil {
		rec.State, rec.Error = "unreachable", sanitizeDomainError(err.Error())
		sb.saveLocked()
		sb.mu.Unlock()
		return
	}
	rec.State, rec.Error = env.State, sanitizeDomainError(env.Error)
	rec.Siblings = env.Siblings
	rec.UnknownLabeled = env.UnknownLabeled
	if rec.Siblings == nil {
		rec.Siblings = []siblingRow{}
	}
	if rec.UnknownLabeled == nil {
		rec.UnknownLabeled = []siblingRow{}
	}
	rec.LastSuccessAt = env.ObservedAt
	rows := append(append([]siblingRow{}, rec.Siblings...), rec.UnknownLabeled...)
	probeable := env.State != "unreachable"
	sb.mu.Unlock()

	probes := probeSiblingGroups(ctx, rows, srv.Host, client, s.siblingsAllowInternal(), now)
	for _, p := range probes {
		if p.Server == "" {
			p.Server = srv.Name
		}
	}

	sb.mu.Lock()
	if cur := sb.snap.Servers[srv.Name]; cur == rec {
		if probeable {
			rec.Probes = probes
		}
		sb.saveLocked()
	}
	sb.mu.Unlock()
}

func (s *Server) siblingsAllowInternal() bool {
	s.siblings.mu.Lock()
	defer s.siblings.mu.Unlock()
	return s.siblings.snap.AllowInternal
}

// ── probing ──────────────────────────────────────────────────────────────────

func siblingHTTPClient(allowInternal bool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:     monitor.PolicyDialContext(allowInternal),
			IdleConnTimeout: 30 * time.Second,
		},
		// A discovery doc answers at the bind; a redirect is an answer too,
		// but not one to follow (the doc would come from somewhere else).
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       siblingProbeTimeout,
	}
}

// siblingGroup is one product's rows on one server (a multi-container app
// yields several rows the consumer groups — the envelope's own rule).
type siblingGroup struct {
	product    string
	containers []string
	app        string
	version    string
	state      string
	hints      []siblingPortHint
}

// groupSiblingRows folds rows into product groups, keeping each group's
// container rows, the teploy app name and the union of port hints, ordered
// deterministically.
func groupSiblingRows(rows []siblingRow) []siblingGroup {
	byProduct := map[string]*siblingGroup{}
	var order []string
	for _, r := range rows {
		g := byProduct[r.Product]
		if g == nil {
			g = &siblingGroup{product: r.Product}
			byProduct[r.Product] = g
			order = append(order, r.Product)
		}
		g.containers = append(g.containers, r.Container)
		if r.App != "" {
			g.app = r.App
		}
		if r.Version != "" && g.version == "" {
			g.version = r.Version
		}
		if r.State == "running" || g.state == "" {
			g.state = r.State
		}
		g.hints = append(g.hints, r.PortHints...)
	}
	sort.Strings(order)
	groups := make([]siblingGroup, 0, len(order))
	for _, p := range order {
		groups = append(groups, *byProduct[p])
	}
	return groups
}

// siblingDialTargets derives probe addresses from a group's port hints:
// concrete routable host binds first, then the server's own address for
// wildcard binds. Loopback-only binds are dash-unreachable (they live on the
// server) and say so. blockedAll marks targets the monitor network guard
// would refuse without allow_internal (it rejects a host when ANY resolved
// address is blocked, so the classification does too).
func siblingDialTargets(ctx context.Context, hints []siblingPortHint, serverHost string) (targets []string, blockedAll bool, why string) {
	seen := map[string]bool{}
	add := func(target string) {
		if !seen[target] {
			seen[target] = true
			targets = append(targets, target)
		}
	}
	wildcardPorts := map[int]bool{}
	for _, h := range hints {
		if h.Proto != "" && h.Proto != "tcp" || h.HostPort <= 0 {
			continue
		}
		switch h.HostIP {
		case "", "0.0.0.0", "::":
			// Every interface: probe the server's own address at this port.
			if serverHost != "" {
				wildcardPorts[h.HostPort] = true
			}
		case "127.0.0.1", "::1":
			// Loopback binds are reachable only on the server itself; the
			// concrete/wildcard cases below are the probeable evidence.
		default:
			add(net.JoinHostPort(h.HostIP, strconv.Itoa(h.HostPort)))
		}
	}
	// Concrete binds speak for the product; the server's own address joins
	// only when a wildcard bind exists nothing concrete covers.
	for port := range wildcardPorts {
		add(net.JoinHostPort(serverHost, strconv.Itoa(port)))
	}
	if len(targets) == 0 {
		if len(wildcardPorts) > 0 {
			return nil, false, "only wildcard binds and the server's address is unknown"
		}
		return nil, false, "loopback-only bind on the server, not reachable from dash"
	}
	if len(targets) > siblingMaxAddresses {
		targets = targets[:siblingMaxAddresses]
	}
	blocked := 0
	for _, t := range targets {
		host, _, err := net.SplitHostPort(t)
		if err != nil {
			continue
		}
		if _, anyBlocked, err := resolveSiblingHost(ctx, host); err == nil && anyBlocked {
			blocked++
		}
	}
	return targets, blocked == len(targets), ""
}

// resolveSiblingHost resolves a literal or hostname to IPs (bounded by the
// probe budget). It never dials; anyBlocked mirrors the guard's reject-on-
// first-blocked-address rule.
func resolveSiblingHost(ctx context.Context, host string) ([]net.IP, bool, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, monitor.IsBlockedAddr(ip), nil
	}
	rctx, cancel := context.WithTimeout(ctx, siblingProbeTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(rctx, host)
	if err != nil {
		return nil, false, err
	}
	ips := make([]net.IP, 0, len(addrs))
	anyBlocked := false
	for _, a := range addrs {
		ips = append(ips, a.IP)
		if monitor.IsBlockedAddr(a.IP) {
			anyBlocked = true
		}
	}
	return ips, anyBlocked, nil
}

// probeSiblingGroups probes each product group's addresses. The first
// address that answers wins: a discovery doc confirms, any other HTTP answer
// is found-not-discoverable, no answer at all is unreachable. A target the
// guard would refuse only reads blocked when allowInternal is off — with the
// owner's consent the probe dials through the guard.
func probeSiblingGroups(ctx context.Context, rows []siblingRow, serverHost string, client *http.Client, allowInternal bool, now time.Time) []*siblingProbe {
	groups := groupSiblingRows(rows)
	probes := make([]*siblingProbe, 0, len(groups))
	for _, g := range groups {
		p := &siblingProbe{
			Product:        g.product,
			Containers:     g.containers,
			App:            g.app,
			LabelVersion:   g.version,
			ContainerState: g.state,
			ProbedAt:       now,
		}
		targets, blockedAll, why := siblingDialTargets(ctx, g.hints, serverHost)
		p.Addresses = targets
		switch {
		case blockedAll && !allowInternal:
			p.State = probeBlockedInternal
			p.Error = "target is on a private/tailnet address — enable \"allow internal\" for sibling probing"
		case len(targets) == 0:
			p.State = probeNotProbeable
			p.Error = why
		default:
			probeSiblingTargets(ctx, p, g.product, targets, client)
		}
		probes = append(probes, p)
	}
	sort.Slice(probes, func(i, j int) bool {
		if probes[i].Product != probes[j].Product {
			return probes[i].Product < probes[j].Product
		}
		return probes[i].Server < probes[j].Server
	})
	return probes
}

// probeSiblingTargets tries each address until one answers.
func probeSiblingTargets(ctx context.Context, p *siblingProbe, product string, targets []string, client *http.Client) {
	var lastErr string
	sawHTTP := false
	for _, base := range targets {
		path := teployDiscoveryPath
		if isForgejoFamily(product) {
			path = forgejoVersionPath
		}
		url := "http://" + base + path
		rctx, cancel := context.WithTimeout(ctx, siblingProbeTimeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			lastErr = sanitizeDomainError(err.Error())
			continue
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = sanitizeDomainError(err.Error())
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, siblingDocLimit))
		resp.Body.Close()
		cancel()
		sawHTTP = true
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Sprintf("%s answered %s", base, resp.Status)
			continue
		}
		if readErr != nil {
			lastErr = sanitizeDomainError(readErr.Error())
			continue
		}
		if isForgejoFamily(product) {
			var v struct {
				Version string `json:"version"`
			}
			if err := json.Unmarshal(body, &v); err != nil || v.Version == "" {
				lastErr = base + " answered, but not with a version"
				continue
			}
			p.State, p.BaseURL, p.Version = probeConfirmed, "http://"+base, v.Version
			return
		}
		var doc siblingDiscoveryDoc
		if err := json.Unmarshal(body, &doc); err != nil || doc.Product == "" {
			lastErr = base + " answered, but not with a Teploy discovery doc"
			continue
		}
		p.State, p.BaseURL, p.Doc = probeConfirmed, "http://"+base, &doc
		p.Version = doc.Version
		if doc.Product != product {
			p.Error = fmt.Sprintf("the doc says product %q, the label said %q", doc.Product, product)
		}
		return
	}
	// No address produced a doc. An HTTP answer (any status) is a sighting
	// of the product — Ship ships discovery default-off, which reads here as
	// "found, not discoverable"; only transport failures mean unreachable.
	if sawHTTP {
		p.State, p.Error = probeNotDiscoverable, lastErr
		return
	}
	p.State = probeUnreachable
	if lastErr != "" {
		p.Error = lastErr
	}
}

// ── views ────────────────────────────────────────────────────────────────────

type siblingCandidateView struct {
	siblingProbe
	AgeSeconds *int64 `json:"age_seconds,omitempty"`
	// Recognized is false for unknown_labeled products (a product newer than
	// the CLI, passed through verbatim).
	Recognized bool `json:"recognized"`
}

type siblingsServerView struct {
	Server        string     `json:"server"`
	State         string     `json:"state"`
	Error         string     `json:"error,omitempty"`
	LastAttemptAt time.Time  `json:"last_attempt_at"`
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
	AgeSeconds    *int64     `json:"age_seconds,omitempty"`
}

func (s *Server) siblingsView(ctx context.Context, now time.Time) map[string]any {
	caps := s.capabilities(ctx)
	supported := caps.CLI.Installed && caps.Features.Siblings

	sb := &s.siblings
	sb.mu.Lock()
	defer sb.mu.Unlock()

	servers := make([]siblingsServerView, 0, len(sb.snap.Servers))
	candidates := make([]siblingCandidateView, 0)
	// A group is "recognized" when the CLI listed it under siblings (vs
	// unknown_labeled); matched by container, the stable per-row key.
	recognized := map[string]bool{}
	for _, rec := range sb.snap.Servers {
		for _, row := range rec.Siblings {
			recognized[row.Product+"\x00"+row.Container] = true
		}
	}
	for _, rec := range sb.snap.Servers {
		sv := siblingsServerView{Server: rec.Server, State: rec.State, Error: rec.Error, LastAttemptAt: rec.LastAttemptAt}
		if !rec.LastSuccessAt.IsZero() {
			at := rec.LastSuccessAt
			age := int64(now.Sub(at) / time.Second)
			sv.CheckedAt, sv.AgeSeconds = &at, &age
		}
		servers = append(servers, sv)
		for _, p := range rec.Probes {
			v := siblingCandidateView{siblingProbe: *p}
			v.Recognized = recognized[p.Product+"\x00"+firstOf(p.Containers)]
			if !p.ProbedAt.IsZero() {
				age := int64(now.Sub(p.ProbedAt) / time.Second)
				v.AgeSeconds = &age
			}
			if v.Server == "" {
				v.Server = rec.Server
			}
			candidates = append(candidates, v)
		}
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Server < servers[j].Server })
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Product != candidates[j].Product {
			return candidates[i].Product < candidates[j].Product
		}
		return candidates[i].Server < candidates[j].Server
	})

	view := map[string]any{
		"servers":         servers,
		"candidates":      candidates,
		"allow_internal":  sb.snap.AllowInternal,
		"cli_supported":   supported,
		"cli_version":     caps.CLI.Version,
		"min_cli_version": siblingsMinCLI,
	}
	if !sb.snap.LastSweepAt.IsZero() {
		view["last_sweep_at"] = sb.snap.LastSweepAt
	}
	if sb.snap.SweepError != "" {
		view["sweep_error"] = sb.snap.SweepError
	}
	if !supported {
		view["note"] = "Sibling discovery needs teploy " + siblingsMinCLI + " or later on the dashboard host."
	}
	return view
}

func firstOf(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// ── the API ──────────────────────────────────────────────────────────────────

func (s *Server) handleSiblings(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeData(w, s.siblingsView(r.Context(), time.Now().UTC()))
}

func (s *Server) handleSiblingsAction(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/siblings/"), "/")
	switch {
	case path == "scan" && r.Method == http.MethodPost:
		s.handleSiblingsScan(w, r)
	case path == "settings" && r.Method == http.MethodPost:
		s.handleSiblingsSettings(w, r)
	default:
		writeErrorStatus(w, "siblings route not found", http.StatusNotFound)
	}
}

// handleSiblingsScan starts one sweep now (unless one is already running).
// The sweep is a quick read (docker ps + inspect over SSH per server, then
// short HTTP GETs), so it runs inline on a background context rather than as
// an operation — the same shape as the domain health sweep.
func (s *Server) handleSiblingsScan(w http.ResponseWriter, r *http.Request) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Siblings }, "Sibling discovery", siblingsMinCLI) {
		return
	}
	if !s.siblings.sweepInFlight.TryLock() {
		writeErrorStatus(w, "a sibling sweep is already running", http.StatusConflict)
		return
	}
	go func() {
		defer s.siblings.sweepInFlight.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), siblingsServerTimeout+time.Minute)
		defer cancel()
		s.sweepSiblings(ctx)
	}()
	s.auditf(r, "siblings.scan", "fleet", "sibling discovery sweep started")
	writeData(w, map[string]bool{"started": true})
}

// handleSiblingsSettings changes the probe network policy. Allowing internal
// addresses is an explicit admin choice (the A13 rule for monitors: an
// editor cannot flip it in either direction).
func (s *Server) handleSiblingsSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AllowInternal bool `json:"allow_internal"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body: "+err.Error())
		return
	}
	sb := &s.siblings
	sb.mu.Lock()
	prev := sb.snap.AllowInternal
	if prev != body.AllowInternal && !s.isEffectiveAdmin(r) {
		sb.mu.Unlock()
		writeErrorStatus(w, "changing internal-network probing requires an admin", http.StatusForbidden)
		return
	}
	sb.snap.AllowInternal = body.AllowInternal
	sb.saveLocked()
	sb.mu.Unlock()
	if prev != body.AllowInternal {
		s.auditf(r, "siblings.settings", "fleet", "allow_internal -> %v", body.AllowInternal)
	}
	writeData(w, map[string]bool{"allow_internal": body.AllowInternal})
}

// ── nav ──────────────────────────────────────────────────────────────────────

// siblingNavURL reads the confirmed discovery docs for a product and returns
// its dashboard URL (the doc's own dashboard_url — never derived, never
// guessed; an empty one means unknown and stays absent). The newest
// confirmed probe wins.
func (s *Server) siblingNavURL(product string) string {
	sb := &s.siblings
	sb.mu.Lock()
	defer sb.mu.Unlock()
	best := ""
	var bestAt time.Time
	for _, rec := range sb.snap.Servers {
		for _, p := range rec.Probes {
			if p.State != probeConfirmed || p.Doc == nil || p.Doc.Product != product {
				continue
			}
			if p.Doc.DashboardURL == "" || !p.ProbedAt.After(bestAt) {
				continue
			}
			best, bestAt = p.Doc.DashboardURL, p.ProbedAt
		}
	}
	return best
}

// siblingForgejoNav returns the base URL of the newest confirmed
// forgejo-family sighting (a plain link; it has no discovery doc).
func (s *Server) siblingForgejoNav() string {
	sb := &s.siblings
	sb.mu.Lock()
	defer sb.mu.Unlock()
	best := ""
	var bestAt time.Time
	for _, rec := range sb.snap.Servers {
		for _, p := range rec.Probes {
			if p.State != probeConfirmed || !isForgejoFamily(p.Product) || !p.ProbedAt.After(bestAt) {
				continue
			}
			best, bestAt = p.BaseURL, p.ProbedAt
		}
	}
	return best
}
