package heartbeat

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
)

func TestRunPingsEveryTargetAndRecordsResults(t *testing.T) {
	var okHits, badHits atomic.Int32
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("User-Agent") != "teploy-dash-heartbeat" {
			t.Errorf("ping = %s %q", r.Method, r.Header.Get("User-Agent"))
		}
		okHits.Add(1)
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer bad.Close()
	cfg := alert.Config{Heartbeats: []alert.HeartbeatTarget{{ID: "hc", URL: ok.URL + "/ping/uuid"}, {ID: "gone", URL: bad.URL}}}
	r := New(func() (alert.Config, error) { return cfg, nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(r.Results()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	res := r.Results()
	if !res["hc"].OK || res["hc"].Status != 200 || okHits.Load() != 1 {
		t.Fatalf("ok target: %+v hits=%d", res["hc"], okHits.Load())
	}
	if res["gone"].OK || !strings.Contains(res["gone"].Error, "404") || badHits.Load() != 1 {
		t.Fatalf("bad target: %+v", res["gone"])
	}
}

func TestPingTimesOut(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer slow.Close()
	defer close(block)
	r := New(nil)
	r.client.Timeout = 50 * time.Millisecond
	res := r.Ping(context.Background(), alert.HeartbeatTarget{ID: "s", URL: slow.URL})
	if res.OK || res.Error == "" {
		t.Fatalf("slow receiver = %+v", res)
	}
}

func TestInterval(t *testing.T) {
	for in, want := range map[int]time.Duration{0: 5 * time.Minute, 1: time.Minute, 15: 15 * time.Minute, 600: time.Hour} {
		if got := Interval(alert.Config{HeartbeatIntervalMinutes: in}); got != want {
			t.Errorf("%d -> %s, want %s", in, got, want)
		}
	}
}

func TestSelfHostedWarning(t *testing.T) {
	origLocal, origLookup := localAddrs, lookupIP
	defer func() { localAddrs, lookupIP = origLocal, origLookup }()
	localAddrs = func() ([]net.IP, error) { return []net.IP{net.ParseIP("100.108.1.5")}, nil }
	lookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		switch host {
		case "observe.home":
			return []net.IP{net.ParseIP("100.108.1.5")}, nil
		default:
			return []net.IP{net.ParseIP("203.0.113.9")}, nil
		}
	}
	if w := SelfHostedWarning(context.Background(), "https://observe.home/api/cron/x"); !strings.Contains(w, "address of this host") {
		t.Fatalf("own interface address not warned: %q", w)
	}
	if w := SelfHostedWarning(context.Background(), "http://127.0.0.1:3001/ping"); !strings.Contains(w, "on this host") {
		t.Fatalf("loopback not warned: %q", w)
	}
	if w := SelfHostedWarning(context.Background(), "https://hc-ping.com/uuid"); w != "" {
		t.Fatalf("external receiver warned: %q", w)
	}
}
