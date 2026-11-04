// Package heartbeat is Dash's outbound self-heartbeat (plan P3.7): every N
// minutes Dash GETs each configured ping URL so an external dead-man
// receiver (healthchecks.io, Uptime Kuma push, Better Stack, Cronitor, an
// Observe cron check-in, another Dash's push monitor) alerts when the pings
// STOP — the one failure Dash cannot report about itself.
package heartbeat

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
)

// DefaultInterval is the ping cadence when the config sets none.
const DefaultInterval = 5 * time.Minute

const pingTimeout = 10 * time.Second

// Result is the last ping outcome for one target.
type Result struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Status int       `json:"status,omitempty"`
	Error  string    `json:"error,omitempty"`
}

// Runner pings the configured receivers.
type Runner struct {
	configFn func() (alert.Config, error)
	// client overrides the guarded alert client (tests).
	client *http.Client

	mu      sync.Mutex
	results map[string]Result
}

// New creates a runner reading the live notifications config per round.
func New(configFn func() (alert.Config, error)) *Runner {
	return &Runner{
		configFn: configFn,
		results:  map[string]Result{},
	}
}

// Interval resolves the configured cadence (1-60 minutes, default 5).
func Interval(cfg alert.Config) time.Duration {
	m := cfg.HeartbeatIntervalMinutes
	if m <= 0 {
		return DefaultInterval
	}
	if m > 60 {
		m = 60
	}
	return time.Duration(m) * time.Minute
}

// Run pings immediately and then every interval until ctx ends. The
// interval is re-read each round, so a Settings change applies without a
// restart.
func (r *Runner) Run(ctx context.Context) {
	for {
		interval := DefaultInterval
		if cfg, err := r.configFn(); err == nil {
			interval = Interval(cfg)
			r.pingAll(ctx, cfg.Heartbeats)
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (r *Runner) pingAll(ctx context.Context, targets []alert.HeartbeatTarget) {
	for _, tgt := range targets {
		r.Ping(ctx, tgt)
	}
}

// Ping sends one heartbeat and records the outcome.
func (r *Runner) Ping(ctx context.Context, tgt alert.HeartbeatTarget) Result {
	res := Result{At: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tgt.URL, nil)
	if err == nil {
		req.Header.Set("User-Agent", "teploy-dash-heartbeat")
		// The alert client: network guard (unless the receiver opts into
		// internal addresses), 10s timeout, redirects refused — a ping that
		// is redirected elsewhere did not reach the receiver.
		client := r.client
		if client == nil {
			client = alert.ClientFor(tgt.AllowInternal)
		}
		var resp *http.Response
		resp, err = client.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
			resp.Body.Close()
			res.Status = resp.StatusCode
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				res.OK = true
			} else {
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}
	}
	if err != nil {
		// The ping URL is the credential (a healthchecks UUID, a push
		// token): never store or show it in an error.
		res.Error = alert.RedactError(err, tgt.URL).Error()
	}
	r.mu.Lock()
	r.results[tgt.ID] = res
	r.mu.Unlock()
	return res
}

// Results returns the last outcome per target id.
func (r *Runner) Results() map[string]Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Result, len(r.results))
	for k, v := range r.results {
		out[k] = v
	}
	return out
}

// Forget drops a removed target's result.
func (r *Runner) Forget(id string) {
	r.mu.Lock()
	delete(r.results, id)
	r.mu.Unlock()
}

// localAddrs lists this host's interface addresses (overridable in tests).
var localAddrs = func() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out, nil
}

var lookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, len(addrs))
	for i, a := range addrs {
		out[i] = a.IP
	}
	return out, nil
}

// SelfHostedWarning reports when a ping URL resolves to this host (loopback
// or one of its own interface addresses): a receiver on Dash's own host
// dies with Dash and can never report Dash down. "" = no warning.
func SelfHostedWarning(ctx context.Context, raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := u.Hostname()
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		ips, err = lookupIP(ctx, host)
		if err != nil {
			return ""
		}
	}
	local, _ := localAddrs()
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return fmt.Sprintf("%s resolves to %s on this host: the receiver would go down with Dash and could never report it", host, ip)
		}
		for _, l := range local {
			if l.Equal(ip) {
				return fmt.Sprintf("%s resolves to %s, an address of this host: the receiver would go down with Dash and could never report it", host, ip)
			}
		}
	}
	return ""
}
