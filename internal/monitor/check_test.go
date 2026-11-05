package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/store"
)

// checkHTTP must honor a configured ExpectedStatus exactly. Before the fix it
// computed expectedStatus but always judged up/down on the 2xx-3xx range, so a
// monitor expecting e.g. 401 or 201 was scored wrong.
func TestCheckHTTP_ExpectedStatusExactMatch(t *testing.T) {
	cases := []struct {
		name       string
		serverCode int
		expected   int
		wantStatus string
	}{
		{"expect 200 get 200", 200, 200, "up"},
		{"expect 201 get 201", 201, 201, "up"},
		{"expect 401 get 401", 401, 401, "up"}, // would be "down" under old 2xx/3xx rule
		{"expect 200 get 500", 500, 200, "down"},
		{"expect 201 get 200", 200, 201, "down"}, // would be "up" under old 2xx/3xx rule
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.serverCode)
			}))
			defer srv.Close()

			r := New(&mockStore{})
			got := r.checkHTTP(store.Monitor{
				ID: "m", Type: "http", Target: srv.URL, ExpectedStatus: tc.expected, AllowInternal: true,
			})
			if got.Status != tc.wantStatus {
				t.Errorf("status %d expect %d: got %q want %q (%s)",
					tc.serverCode, tc.expected, got.Status, tc.wantStatus, got.Message)
			}
		})
	}
}

// With no ExpectedStatus set, any 2xx/3xx is up and everything else is down.
func TestCheckHTTP_DefaultRange(t *testing.T) {
	cases := []struct {
		code       int
		wantStatus string
	}{
		{200, "up"}, {204, "up"}, {301, "up"}, {399, "up"},
		{400, "down"}, {404, "down"}, {500, "down"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.code)
		}))
		r := New(&mockStore{})
		got := r.checkHTTP(store.Monitor{ID: "m", Type: "http", Target: srv.URL, AllowInternal: true})
		if got.Status != tc.wantStatus {
			t.Errorf("code %d: got %q want %q", tc.code, got.Status, tc.wantStatus)
		}
		srv.Close()
	}
}

// checkHTTP must apply the monitor's own Timeout, not a hardcoded client
// timeout. A server that sleeps longer than the monitor timeout is "down".
func TestCheckHTTP_PerMonitorTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	r := New(&mockStore{})
	got := r.checkHTTP(store.Monitor{
		ID: "m", Type: "http", Target: srv.URL, Timeout: 50 * time.Millisecond, AllowInternal: true,
	})
	if got.Status != "down" {
		t.Errorf("expected timeout -> down, got %q (%s)", got.Status, got.Message)
	}
}

// Reachable: any answer below 500 is up (a root that 401s is a live site);
// a 5xx — a tunnel whose origin is gone answers 502 — is down.
func TestCheckHTTP_Reachable(t *testing.T) {
	for code, want := range map[int]string{200: "up", 401: "up", 404: "up", 502: "down", 530: "down"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		got := New(&mockStore{}).checkHTTP(store.Monitor{
			ID: "m", Type: "http", Target: srv.URL, Reachable: true, AllowInternal: true,
		})
		srv.Close()
		if got.Status != want {
			t.Errorf("status %d: got %q want %q (%s)", code, got.Status, want, got.Message)
		}
	}
}

func TestFirstHopCert(t *testing.T) {
	first, last := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	cert := func(at time.Time) *tls.ConnectionState {
		return &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{NotAfter: at}}}
	}
	redirect := &http.Response{TLS: cert(first), Request: &http.Request{}}
	final := &http.Response{TLS: cert(last), Request: &http.Request{Response: redirect}}
	got := firstHopCert(final)
	if got == nil || !got.NotAfter.Equal(first) {
		t.Fatalf("cert = %+v, want the monitored host's %v", got, first)
	}
	if got := firstHopCert(&http.Response{Request: &http.Request{}}); got != nil {
		t.Errorf("a plain HTTP response has no certificate: %v", got)
	}
	if got := firstHopCert(&http.Response{TLS: &tls.ConnectionState{}}); got != nil {
		t.Errorf("no peer certificate: %v", got)
	}
}

// A refusal on a later redirect hop is another host's certificate: recording its
// expiry would alert on the wrong domain.
func TestCheckHTTP_RefusedOnLaterHopRecordsNoExpiry(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer tlsSrv.Close()
	target := "https://localhost:" + tlsSrv.URL[strings.LastIndex(tlsSrv.URL, ":")+1:] + "/"
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	defer first.Close()
	got := New(&mockStore{}).checkHTTP(store.Monitor{ID: "m", Type: "http", Target: first.URL, AllowInternal: true})
	if got.Status != "down" {
		t.Fatalf("%+v", got)
	}
	if got.CertNotAfter != nil {
		t.Fatalf("recorded another host's expiry: %v", got.CertNotAfter)
	}
}

// A certificate the client refuses still tells when it expires: that is exactly
// when the date matters, and there is no response to read it from.
func TestCheckHTTP_RejectedCertificateStillReportsExpiry(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	r := New(&mockStore{})
	got := r.checkHTTP(store.Monitor{ID: "m", Type: "http", Target: srv.URL, AllowInternal: true})
	if got.Status != "down" {
		t.Fatalf("an untrusted certificate must read down: %+v", got)
	}
	if got.CertNotAfter == nil || !got.CertNotAfter.After(time.Now()) || got.CertNotBefore == nil {
		t.Fatalf("expiry = %v, start = %v", got.CertNotAfter, got.CertNotBefore)
	}
}

func TestCheckHTTP_PlainHTTPHasNoCertificate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	got := New(&mockStore{}).checkHTTP(store.Monitor{ID: "m", Type: "http", Target: srv.URL, AllowInternal: true})
	if got.Status != "up" || got.CertNotAfter != nil {
		t.Fatalf("%+v", got)
	}
}

func TestLastResult_CommittedChecksAndRemoval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	st := store.NewFileStore(t.TempDir())
	m := store.Monitor{ID: "m", Name: "m", Type: "http", Target: srv.URL, AllowInternal: true, Enabled: true, Interval: time.Hour, Timeout: time.Second}
	if err := st.SaveMonitor(&m); err != nil {
		t.Fatal(err)
	}
	notAfter := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := st.SaveCheck(store.CheckResult{MonitorID: "m", Incarnation: m.Incarnation, Status: "up", CheckedAt: time.Now(), CertNotAfter: &notAfter}); err != nil {
		t.Fatal(err)
	}
	r := New(st)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r.Stop(ctx)
	}()
	r.Reload(m)
	got, ok := r.LastResult("m")
	if !ok || got.CertNotAfter == nil || !got.CertNotAfter.Equal(notAfter) {
		t.Fatalf("seeded result = %+v, %v", got, ok)
	}
	r.Remove("m")
	if _, ok := r.LastResult("m"); ok {
		t.Fatal("a removed monitor keeps no result")
	}
}
