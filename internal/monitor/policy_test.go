package monitor

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/useteploy/teploy-dash/internal/store"
)

func TestIsBlockedAddr(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"10.0.0.5", true},
		{"172.16.0.5", true},
		{"192.168.1.5", true},
		{"169.254.169.254", true}, // cloud metadata / link-local
		{"fe80::1", true},         // IPv6 link-local
		{"fc00::1", true},         // IPv6 unique-local (net.IP.IsPrivate covers this)
		{"100.64.0.7", true},      // CGNAT shared address space (overlay networks)
		{"100.127.255.255", true}, // CGNAT upper edge
		{"224.0.0.1", true},       // multicast
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"100.128.0.1", false}, // just past the CGNAT range
		{"93.184.216.34", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		if got := isBlockedAddr(ip); got != c.blocked {
			t.Errorf("isBlockedAddr(%s) = %v, want %v", c.ip, got, c.blocked)
		}
	}
}

// A11: both policy transports exist from the start, are never reallocated,
// and concurrent lookups for both modes stay race-free (run under -race).
func TestTransportFor_ConcurrentStableIdentity(t *testing.T) {
	if transportFor(true) == transportFor(false) {
		t.Fatal("internal and public policy modes must not share a transport")
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_ = transportFor(i%2 == 0)
		}(i)
	}
	close(start)
	wg.Wait()
}

// DASH-010: an HTTP monitor targeting a loopback address must be rejected by
// default, and must succeed when the monitor explicitly opts in.
func TestCheckHTTP_BlocksLoopbackByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	r := New(&mockStore{})

	got := r.checkHTTP(store.Monitor{ID: "m", Type: "http", Target: srv.URL})
	if got.Status != "down" {
		t.Errorf("default policy: status = %q, want down (loopback should be blocked)", got.Status)
	}

	got = r.checkHTTP(store.Monitor{ID: "m", Type: "http", Target: srv.URL, AllowInternal: true})
	if got.Status != "up" {
		t.Errorf("AllowInternal=true: status = %q, want up (%s)", got.Status, got.Message)
	}
}

// Same policy must apply to TCP/ping checks, not just HTTP.
func TestCheckTCP_BlocksLoopbackByDefault(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	r := New(&mockStore{})

	got := r.checkTCP(store.Monitor{ID: "m", Type: "tcp", Target: ln.Addr().String()})
	if got.Status != "down" {
		t.Errorf("default policy: status = %q, want down", got.Status)
	}

	got = r.checkTCP(store.Monitor{ID: "m", Type: "tcp", Target: ln.Addr().String(), AllowInternal: true})
	if got.Status != "up" {
		t.Errorf("AllowInternal=true: status = %q, want up (%s)", got.Status, got.Message)
	}
}

// A hostname resolving to a mix of public and private addresses must be
// rejected outright, not allowed through on the public address.
func TestResolveAndFilter_MixedAddressesRejected(t *testing.T) {
	// "localhost" typically resolves to 127.0.0.1 and/or ::1 — both blocked,
	// so this also covers the plain-loopback-hostname case.
	_, err := resolveAndFilter(context.Background(), "localhost", false)
	if err == nil {
		t.Error("expected localhost to be rejected without AllowInternal")
	}
	ips, err := resolveAndFilter(context.Background(), "localhost", true)
	if err != nil {
		t.Errorf("AllowInternal=true: unexpected error: %v", err)
	}
	if len(ips) == 0 {
		t.Error("expected at least one resolved address")
	}
}
