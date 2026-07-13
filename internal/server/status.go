package server

import (
	"net/http"
	"time"
)

// The public status page is an unauthenticated, customer-facing view of the
// monitors' current health. It is OFF by default (--public-status /
// TEPLOY_DASH_PUBLIC_STATUS) because it exposes uptime without a login. Even
// when on it deliberately leaks nothing sensitive: only each monitor's Name,
// current up/down state, and 24h uptime %% — never the target URL/IP, server
// name, response bodies, or any config. Disabled monitors are omitted.

type statusMonitor struct {
	Name          string  `json:"name"`
	Status        string  `json:"status"` // "up", "down", "unknown"
	UptimePercent float64 `json:"uptime_percent"`
}

type statusResponse struct {
	Status    string          `json:"status"` // "operational", "degraded", "down"
	UpdatedAt time.Time       `json:"updated_at"`
	Monitors  []statusMonitor `json:"monitors"`
}

// handleStatusAPI serves the public status JSON. Returns 404 when the public
// status page is disabled so the endpoint is indistinguishable from absent.
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

	since := time.Now().Add(-24 * time.Hour)
	out := make([]statusMonitor, 0, len(monitors))
	upCount, downCount := 0, 0

	for _, m := range monitors {
		if !m.Enabled {
			continue
		}

		status := "unknown"
		if checks, err := s.store.GetChecks(m.ID, since, 1); err == nil && len(checks) > 0 {
			// GetChecks returns newest-first; [0] is the latest result.
			switch checks[0].Status {
			case "up":
				status = "up"
				upCount++
			case "down", "timeout":
				status = "down"
				downCount++
			}
		}

		var uptime float64
		if stats, err := s.store.GetStats(m.ID, since); err == nil && stats != nil {
			uptime = stats.UptimePercent
		}

		out = append(out, statusMonitor{Name: m.Name, Status: status, UptimePercent: uptime})
	}

	overall := "operational"
	switch {
	case len(out) > 0 && downCount > 0 && upCount == 0:
		overall = "down"
	case downCount > 0:
		overall = "degraded"
	}

	writeJSON(w, statusResponse{
		Status:    overall,
		UpdatedAt: time.Now().UTC(),
		Monitors:  out,
	})
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
</style>
</head>
<body>
<div class="wrap">
  <h1>Service status</h1>
  <div id="overall" class="overall"><span>Loading…</span></div>
  <div id="list" class="list"><div class="empty">Loading…</div></div>
  <div class="foot" id="foot"></div>
</div>
<script>
  var LABELS = { operational: "All systems operational", degraded: "Partial outage", down: "Major outage" };
  function pct(n) { return (Math.round(n * 100) / 100).toFixed(2) + "%"; }
  function render(d) {
    var overall = document.getElementById("overall");
    overall.className = "overall banner-" + d.status;
    overall.innerHTML = "<span>" + (LABELS[d.status] || d.status) + "</span>";
    var list = document.getElementById("list");
    if (!d.monitors || d.monitors.length === 0) {
      list.innerHTML = '<div class="empty">No services are being monitored.</div>';
    } else {
      list.innerHTML = d.monitors.map(function (m) {
        return '<div class="row"><div class="name"><span class="dot ' + m.status + '"></span>' +
          escapeHtml(m.name) + '</div><div class="uptime">' + pct(m.uptime_percent) + ' uptime (24h)</div></div>';
      }).join("");
    }
    document.getElementById("foot").textContent = "Updated " + new Date(d.updated_at).toLocaleString();
  }
  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function load() {
    fetch("/api/status").then(function (r) { return r.json(); }).then(render).catch(function () {});
  }
  load();
  setInterval(load, 30000);
</script>
</body>
</html>`
