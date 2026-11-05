package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/store"
)

// Fleet monitors: every public domain a deployed app serves gets an uptime
// monitor without anyone creating one. A route that silently stops reaching
// its app (a tunnel pointed at a port the app no longer publishes) otherwise
// fails unseen until someone happens to load the page.
//
// The reconciler only creates and retires monitors it owns (App set). It never
// overwrites one that exists, so a person can disable a fleet monitor and it
// stays disabled; deleting one only brings it back on the next sweep.

const (
	fleetMonitorSweep    = 5 * time.Minute
	fleetMonitorInterval = 60 * time.Second
	fleetMonitorTimeout  = 10 * time.Second
)

func fleetMonitorID(host string) string {
	sum := sha256.Sum256([]byte("teploy-dash/fleet-monitor/v1:" + host))
	return "fleet-" + hex.EncodeToString(sum[:8])
}

// fleetMonitorHosts lists the probe-able hosts of one app's domain field:
// public names only — no placeholders, IP literals, wildcards or host:port.
func fleetMonitorHosts(domain string) []string {
	var hosts []string
	for _, d := range strings.Split(domain, ",") {
		h := strings.ToLower(strings.TrimSpace(d))
		if h == "" || isPlaceholderDomain(h) || isPrivateSuffix(h) || net.ParseIP(h) != nil ||
			strings.ContainsAny(h, "*:/ ") || !strings.Contains(h, ".") {
			continue
		}
		hosts = append(hosts, h)
	}
	return hosts
}

// isPrivateSuffix reports names that only resolve inside a network (the
// ICANN-reserved .internal, home/LAN suffixes, tailnet MagicDNS): a public
// probe can never reach them, so a monitor would only ever read down.
func isPrivateSuffix(h string) bool {
	for _, suffix := range []string{".internal", ".lan", ".home.arpa", ".ts.net", ".intranet", ".corp"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// frontedHostsFunc says which hosts on a server have a Caddy route naming them;
// known=false means nobody can say yet.
type frontedHostsFunc func(server string) (fronted map[string]bool, known bool)

// desiredFleetMonitors derives the monitor set from one fleet sweep. complete
// reports whether every server was observed successfully: only a complete
// sweep may retire a monitor, so an unreachable server never costs its apps
// their monitoring.
//
// A host-ingress domain is monitored only when something fronts it. Otherwise
// it is a label on a raw published port and https://<domain>/ answers nothing.
// Where the routes are not known, undetermined keeps an existing monitor without
// creating one: missing evidence neither adds nor retires.
func desiredFleetMonitors(envelopes []ServerObservation, frontedHosts frontedHostsFunc) (want map[string]store.Monitor, undetermined map[string]bool, complete bool) {
	want = make(map[string]store.Monitor)
	undetermined = make(map[string]bool)
	complete = len(envelopes) > 0
	for _, env := range envelopes {
		if env.Error != "" {
			complete = false
			continue
		}
		apps := append(env.Apps[:0:0], env.Apps...)
		sort.Slice(apps, func(i, j int) bool { return apps[i].App < apps[j].App })
		for _, app := range apps {
			// ingress external: teploy publishes no route, so the domain is
			// a label that may have no DNS at all.
			if app.Ingress == "external" {
				continue
			}
			var fronted map[string]bool
			frontedKnown := true
			if app.Ingress == "host" {
				if frontedHosts == nil {
					frontedKnown = false
				} else {
					fronted, frontedKnown = frontedHosts(env.Server)
				}
			}
			for _, h := range fleetMonitorHosts(app.Domain) {
				if app.Server == "" {
					app.Server = env.Server
				}
				id := fleetMonitorID(h)
				if app.Ingress == "host" {
					if !frontedKnown {
						undetermined[id] = true
						continue
					}
					if !fronted[h] {
						continue
					}
				}
				if _, ok := want[id]; ok {
					continue
				}
				want[id] = store.Monitor{
					ID:        id,
					Name:      h,
					Type:      "http",
					Target:    "https://" + h + "/",
					Interval:  fleetMonitorInterval,
					Timeout:   fleetMonitorTimeout,
					Enabled:   true,
					Reachable: true,
					App:       app.App,
					Server:    app.Server,
				}
			}
		}
	}
	return want, undetermined, complete
}

// reconcileFleetMonitors creates missing fleet monitors and, after a complete
// sweep, retires fleet monitors whose domain no deployed app serves any more.
func (s *Server) reconcileFleetMonitors(envelopes []ServerObservation) (added, removed int, err error) {
	if s.store == nil {
		return 0, 0, nil
	}
	want, undetermined, complete := desiredFleetMonitors(envelopes, s.domainFrontedHosts)
	existing, err := s.store.ListMonitors()
	if err != nil {
		return 0, 0, err
	}
	have := make(map[string]bool, len(existing))
	for _, m := range existing {
		have[m.ID] = true
		if m.App == "" {
			continue
		}
		if w, ok := want[m.ID]; ok {
			// Alerts v2: the owning server is derived state the reconciler
			// owns (suppression and the Event v2 "server" field read it);
			// backfill it without touching anything a person may have
			// edited.
			if w.Server != "" && (m.Server != w.Server || m.App != w.App) {
				m.Server, m.App = w.Server, w.App
				if err := s.store.SaveMonitor(&m); err != nil {
					return added, removed, err
				}
				if s.monitor != nil {
					s.monitor.Reload(m)
				}
			}
			continue
		}
		if !complete || undetermined[m.ID] {
			continue
		}
		if s.monitor != nil {
			s.monitor.Remove(m.ID)
		}
		if err := s.store.DeleteMonitor(m.ID); err != nil {
			return added, removed, err
		}
		removed++
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if have[id] {
			continue
		}
		m := want[id]
		if err := validateMonitor(&m); err != nil {
			log.Printf("[fleet-monitors] skipping %s: %v", m.Name, err)
			continue
		}
		if err := s.store.SaveMonitor(&m); err != nil {
			return added, removed, err
		}
		if s.monitor != nil {
			s.monitor.Reload(m)
		}
		added++
	}
	return added, removed, nil
}

// RunFleetMonitors reconciles fleet monitors now and then every sweep period
// until ctx is cancelled. A no-op when the CLI (and so the fleet) is absent.
func (s *Server) RunFleetMonitors(ctx context.Context) {
	if !s.cliInstalled() {
		return
	}
	run := func() {
		sweepCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		envelopes, err := s.coldFleet(sweepCtx)
		if err != nil {
			log.Printf("[fleet-monitors] fleet unavailable: %v", err)
			return
		}
		added, removed, err := s.reconcileFleetMonitors(envelopes)
		if err != nil {
			log.Printf("[fleet-monitors] reconcile failed: %v", err)
			return
		}
		if added > 0 || removed > 0 {
			log.Printf("[fleet-monitors] added %d, retired %d", added, removed)
		}
	}
	run()
	t := time.NewTicker(fleetMonitorSweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
