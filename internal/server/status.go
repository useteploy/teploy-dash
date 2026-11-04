package server

import (
	"net/http"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/store"
)

// The public status page is an unauthenticated, customer-facing view of the
// monitors' current health. It is OFF by default (--public-status /
// TEPLOY_DASH_PUBLIC_STATUS) because it exposes uptime without a login. Even
// when on it deliberately leaks nothing sensitive: only each monitor's Name,
// current up/down state, and 24h uptime %% — never the target URL/IP, server
// name, response bodies, or any config. Disabled monitors are omitted.
//
// Alerts v2 (plan P3.9): open incidents and active maintenance/silences are
// listed by MONITOR NAME only (the name the page already shows), with their
// start/expiry times — never a target, app, server, rule source or reason.

type statusMonitor struct {
	Name          string  `json:"name"`
	Status        string  `json:"status"` // "up", "down", "unknown"
	UptimePercent float64 `json:"uptime_percent"`
	Maintenance   bool    `json:"maintenance,omitempty"`
}

type statusIncident struct {
	Name  string    `json:"name"`
	Since time.Time `json:"since"`
}

type statusMaintenance struct {
	Name  string     `json:"name"`
	Until *time.Time `json:"until,omitempty"`
}

type statusResponse struct {
	Status      string              `json:"status"` // "operational", "degraded", "down"
	UpdatedAt   time.Time           `json:"updated_at"`
	Monitors    []statusMonitor     `json:"monitors"`
	Incidents   []statusIncident    `json:"incidents"`
	Maintenance []statusMaintenance `json:"maintenance"`
}

// handleStatusAPI serves the public status JSON. Returns 404 when the public
// status page is disabled so the endpoint is indistinguishable from absent.
//
// Freshness policy (A38): a monitor's latest result only counts as its
// current state when it was observed within roughly two check intervals plus
// the timeout — anything older (or no result at all) is "unknown", and
// unknown is never aggregated into "operational". Missing evidence is not
// evidence of health.
func (s *Server) handleStatusAPI(w http.ResponseWriter, r *http.Request) {
	if !s.config.PublicStatus {
		http.NotFound(w, r)
		return
	}

	monitors, err := s.store.ListMonitors()
	if err != nil {
		jsonError(w, "failed to load status", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	since := now.Add(-24 * time.Hour)
	out := make([]statusMonitor, 0, len(monitors))
	maintenance := []statusMaintenance{}
	enabledNames := map[string]string{}
	upCount, downCount, unknownCount := 0, 0, 0

	for _, m := range monitors {
		if !m.Enabled {
			continue
		}

		status := "unknown"
		// Effective schedule mirrors the runner's floors so hand-written
		// zero-value configs get a sane freshness window.
		interval := m.Interval
		if interval < 10*time.Second {
			interval = 10 * time.Second
		}
		timeout := m.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		freshnessLimit := 2*interval + timeout
		if checks, err := s.store.GetChecks(m.ID, since, 1); err == nil && len(checks) > 0 {
			latest := checks[0]
			fresh := !latest.CheckedAt.After(now.Add(time.Minute)) &&
				now.Sub(latest.CheckedAt) <= freshnessLimit
			switch {
			case !fresh:
				// stale or implausibly future-dated — no current evidence
			case latest.Status == "up":
				status = "up"
				upCount++
			case latest.Status == "down", latest.Status == "timeout":
				status = "down"
				downCount++
			}
		}
		if status == "unknown" {
			unknownCount++
		}

		var uptime float64
		if stats, err := s.store.GetStats(m.ID, since); err == nil && stats != nil {
			uptime = stats.UptimePercent
		}

		row := statusMonitor{Name: m.Name, Status: status, UptimePercent: uptime}
		if mt, ok := s.statusMaintenanceFor(m, now); ok {
			row.Maintenance = true
			maintenance = append(maintenance, mt)
		}
		out = append(out, row)
		enabledNames[m.ID] = m.Name
	}
	incidents := []statusIncident{}
	if s.alerts.tracker != nil {
		for _, inc := range s.alerts.tracker.Open() {
			if inc.Source != alert.SourceMonitor && inc.Source != alert.SourceHeartbeat {
				continue // restore tests, operations, channels are internal
			}
			if name, ok := enabledNames[inc.RuleID]; ok {
				incidents = append(incidents, statusIncident{Name: name, Since: inc.StartedAt})
			}
		}
	}
	if s.alerts.silences != nil {
		for _, sl := range s.alerts.silences.Active(now) {
			if sl.Scope == silenceAll {
				until := sl.ExpiresAt
				maintenance = append([]statusMaintenance{{Name: "All services", Until: &until}}, maintenance...)
				break
			}
		}
	}

	overall := "unknown"
	switch {
	case len(out) == 0:
		// nothing monitored — unknown, not a green all-clear
	case downCount > 0 && downCount+unknownCount == len(out) && upCount == 0 && unknownCount == 0:
		overall = "down"
	case downCount > 0:
		overall = "degraded"
	case unknownCount > 0:
		overall = "unknown"
	default:
		overall = "operational"
	}

	writeJSON(w, statusResponse{
		Status:      overall,
		UpdatedAt:   now.UTC(),
		Monitors:    out,
		Incidents:   incidents,
		Maintenance: maintenance,
	})
}

// statusMaintenanceFor reports a monitor under maintenance: its app's
// maintenance flag (fleet cache) or a silence covering it (its expiry is
// the "until").
func (s *Server) statusMaintenanceFor(m store.Monitor, now time.Time) (statusMaintenance, bool) {
	if s.alerts.silences != nil {
		for _, sl := range s.alerts.silences.Active(now) {
			if sl.Scope != silenceAll && sl.covers(m.ID, m.App) {
				until := sl.ExpiresAt
				return statusMaintenance{Name: m.Name, Until: &until}, true
			}
		}
	}
	if m.App != "" && s.fleet != nil {
		for _, app := range s.fleet.snapshot() {
			if app.App == m.App && (m.Server == "" || app.Server == m.Server) && app.Maintenance {
				return statusMaintenance{Name: m.Name}, true
			}
		}
	}
	return statusMaintenance{}, false
}

// handleStatusPage serves the self-contained public status HTML. Returns 404
// when disabled. The page fetches /api/status and refreshes on an interval; it
// has no external dependencies (inline CSS/JS) so it works behind any proxy.
func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	if !s.config.PublicStatus {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(statusPageHTML))
}

const statusPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Status</title>
<style>
  :root {
    --bg: #0b0d10; --card: #15181d; --line: #262b33;
    --fg: #e6e9ef; --muted: #8b93a1;
    --up: #3fb950; --down: #f85149; --unknown: #8b93a1;
  }
  @media (prefers-color-scheme: light) {
    :root { --bg: #f6f7f9; --card: #fff; --line: #e3e6ea; --fg: #1c1f24; --muted: #666e7b; }
  }
  * { box-sizing: border-box; }
  body { margin: 0; background: var(--bg); color: var(--fg);
    font: 15px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
  .wrap { max-width: 720px; margin: 0 auto; padding: 48px 20px; }
  h1 { font-size: 20px; margin: 0 0 4px; }
  .overall { display: flex; align-items: center; gap: 10px;
    padding: 18px 20px; border: 1px solid var(--line); border-radius: 10px;
    background: var(--card); margin: 20px 0 24px; font-weight: 600; }
  .banner-operational { color: var(--up); }
  .banner-degraded, .banner-down { color: var(--down); }
  .banner-unknown { color: var(--unknown); }
  .list { border: 1px solid var(--line); border-radius: 10px; overflow: hidden; background: var(--card); }
  .row { display: flex; align-items: center; justify-content: space-between;
    padding: 14px 20px; border-top: 1px solid var(--line); }
  .row:first-child { border-top: none; }
  .name { display: flex; align-items: center; gap: 10px; }
  .dot { width: 10px; height: 10px; border-radius: 50%; flex: none; }
  .dot.up { background: var(--up); } .dot.down { background: var(--down); } .dot.unknown { background: var(--unknown); }
  .uptime { color: var(--muted); font-variant-numeric: tabular-nums; font-size: 13px; }
  .foot { color: var(--muted); font-size: 12px; margin-top: 20px; text-align: center; }
  .empty { padding: 24px 20px; color: var(--muted); text-align: center; }
  .notice { border: 1px solid var(--line); border-radius: 10px; background: var(--card); padding: 14px 20px; margin: 0 0 16px; }
  .notice h2 { font-size: 14px; margin: 0 0 6px; }
  .notice ul { margin: 0; padding-left: 18px; color: var(--muted); font-size: 13px; }
  .notice.incidents h2 { color: var(--down); }
  .maint { color: var(--muted); font-size: 12px; margin-left: 6px; }
</style>
</head>
<body>
<div class="wrap">
  <h1>Service status</h1>
  <div id="overall" class="overall"><span>Loading…</span></div>
  <div id="notices"></div>
  <div id="list" class="list"><div class="empty">Loading…</div></div>
  <div class="foot" id="foot"></div>
</div>
<script>
  var LABELS = { operational: "All systems operational", degraded: "Partial outage", down: "Major outage", unknown: "Status unknown" };
  function pct(n) { return (Math.round(n * 100) / 100).toFixed(2) + "%"; }
  function render(d) {
    var overall = document.getElementById("overall");
    overall.className = "overall banner-" + (LABELS[d.status] ? d.status : "unknown");
    overall.innerHTML = "<span>" + (LABELS[d.status] || d.status) + "</span>";
    var list = document.getElementById("list");
    if (!d.monitors || d.monitors.length === 0) {
      list.innerHTML = '<div class="empty">No services are being monitored.</div>';
    } else {
      list.innerHTML = d.monitors.map(function (m) {
        return '<div class="row"><div class="name"><span class="dot ' + m.status + '"></span>' +
          escapeHtml(m.name) + (m.maintenance ? '<span class="maint">maintenance</span>' : '') +
          '</div><div class="uptime">' + pct(m.uptime_percent) + ' uptime (24h)</div></div>';
      }).join("");
    }
    var notices = "";
    if (d.incidents && d.incidents.length) {
      notices += '<div class="notice incidents"><h2>Open incidents</h2><ul>' + d.incidents.map(function (i) {
        return "<li>" + escapeHtml(i.name) + " - since " + escapeHtml(new Date(i.since).toLocaleString()) + "</li>";
      }).join("") + "</ul></div>";
    }
    if (d.maintenance && d.maintenance.length) {
      notices += '<div class="notice"><h2>Maintenance</h2><ul>' + d.maintenance.map(function (m) {
        return "<li>" + escapeHtml(m.name) + (m.until ? " - until " + escapeHtml(new Date(m.until).toLocaleString()) : "") + "</li>";
      }).join("") + "</ul></div>";
    }
    document.getElementById("notices").innerHTML = notices;
    document.getElementById("foot").textContent = "Updated " + new Date(d.updated_at).toLocaleString();
  }
  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function load() {
    fetch("/api/status", { cache: "no-store" })
      .then(function (r) {
        if (!r.ok) { throw new Error("status unavailable"); }
        return r.json();
      })
      .then(render)
      .catch(function () {
        // A failed refresh must not leave the previous (possibly green)
        // state standing as if it were current.
        var banner = document.getElementById("overall");
        banner.className = "overall banner-unknown";
        banner.textContent = "Status unavailable — the last result is not current.";
      });
  }
  load();
  setInterval(load, 30000);
</script>
</body>
</html>`
