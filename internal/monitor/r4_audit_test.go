package monitor

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/store"
)

// R33: a check whose scheduler generation no longer matches the live counter
// must not save, mutate the baseline, or alert — even when the monitor id is
// still present under a NEW generation (the reload case). The pre-fix
// runCheck read the live generation at start, so a scheduler delayed past a
// reload adopted the new generation with its old captured configuration.
func TestRunCheck_StaleGenerationDoesNothing(t *testing.T) {
	ms := &mockStore{}
	r := New(ms)
	r.SetAlerter(newNoopAlerter())

	m := store.Monitor{ID: "m1", Name: "t", Type: "http", Target: "http://127.0.0.1:1", AllowInternal: true}

	r.mu.Lock()
	r.generations[m.ID] = 6 // a reload/remove bumped the live counter past 5
	r.mu.Unlock()

	// A scheduler holding generation 5 runs while the live counter is 6.
	r.runCheck(m, 5)

	if len(ms.checks) != 0 {
		t.Fatalf("stale check must not persist, got %d checks", len(ms.checks))
	}
	r.mu.Lock()
	_, hasBaseline := r.lastStat[m.ID]
	r.mu.Unlock()
	if hasBaseline {
		t.Fatal("stale check must not mutate the transition baseline")
	}
}

// R33: the fresh generation still works end to end (saves + baseline).
func TestRunCheck_CurrentGenerationPersists(t *testing.T) {
	ms := &mockStore{}
	r := New(ms)

	m := store.Monitor{ID: "m1", Name: "t", Type: "http", Target: "http://127.0.0.1:1", AllowInternal: true}
	r.mu.Lock()
	r.generations[m.ID] = 5
	r.mu.Unlock()

	r.runCheck(m, 5)
	if len(ms.checks) != 1 {
		t.Fatalf("fresh check must persist, got %d checks", len(ms.checks))
	}
}

// R41: the policy transport must not carry hidden phase deadlines that
// override a monitor's configured total timeout.
func TestPolicyTransport_NoHiddenPhaseTimeouts(t *testing.T) {
	tr := newPolicyTransport(false)
	if tr.TLSHandshakeTimeout != 0 {
		t.Fatalf("TLSHandshakeTimeout must be 0 (context owns the deadline), got %s", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("ResponseHeaderTimeout must be 0 (context owns the deadline), got %s", tr.ResponseHeaderTimeout)
	}
}

// R41: a server that delays its response headers beyond any old fixed
// phase cap must still succeed when the configured total timeout allows it.
func TestCheckHTTP_SlowHeadersWithinConfiguredTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New(&mockStore{})
	result := r.checkHTTP(store.Monitor{
		ID: "m", Type: "http", Target: srv.URL, Timeout: 3 * time.Second, AllowInternal: true,
	})
	if result.Status != "up" {
		t.Fatalf("expected up within the total budget, got %q (%s)", result.Status, result.Message)
	}
}

// R42: a blackholing first address must not consume the whole budget — the
// second address is attempted and succeeds within the total deadline.
func TestDialAddrs_BlackholedFirstAddressFallsThrough(t *testing.T) {
	blocked := net.ParseIP("203.0.113.1")
	good := net.ParseIP("127.0.0.1")
	deadline := 400 * time.Millisecond
	calls := 0
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	conn, err := dialAddrs(ctx, "tcp", "0", []net.IP{blocked, good},
		func(ctx context.Context, network, addr string) (net.Conn, error) {
			calls++
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if net.ParseIP(hostOf(addr)).Equal(blocked) {
				<-ctx.Done() // blackhole until this attempt's budget expires
				return nil, ctx.Err()
			}
			// A real listener socket for the success case.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			defer ln.Close()
			return net.DialTimeout(network, ln.Addr().String(), deadline)
		})
	if err != nil {
		t.Fatalf("expected the second address to succeed, got %v", err)
	}
	conn.Close()
	if calls != 2 {
		t.Fatalf("expected 2 dial attempts, got %d", calls)
	}
	if elapsed := time.Since(start); elapsed > deadline {
		t.Fatalf("fallback took %s, exceeding the %s budget", elapsed, deadline)
	}
}

func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
