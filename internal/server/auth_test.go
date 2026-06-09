package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBasicAuth_RejectsMissingCreds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := newAuthGate("admin", "secret", "").wrap(mux)

	req := httptest.NewRequest("GET", "/api/apps", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate header on 401")
	}
}

func TestBasicAuth_RejectsWrongCreds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := newAuthGate("admin", "secret", "").wrap(mux)

	req := httptest.NewRequest("GET", "/api/apps", nil)
	req.SetBasicAuth("admin", "wrong")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong password, got %d", w.Code)
	}
}

func TestBasicAuth_AcceptsCorrectCreds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := newAuthGate("admin", "secret", "").wrap(mux)

	req := httptest.NewRequest("GET", "/api/apps", nil)
	req.SetBasicAuth("admin", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 with correct creds, got %d", w.Code)
	}
}

func TestBasicAuth_HealthExempt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := newAuthGate("admin", "secret", "").wrap(mux)

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 on /api/health without auth (liveness probe), got %d", w.Code)
	}
}

func authedReq(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.SetBasicAuth("admin", "secret")
	return req
}

// A cross-origin state-changing request (browser CSRF) must be blocked even
// when authenticated.
func TestAuthGate_BlocksCrossOriginMutation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/monitors", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := newAuthGate("admin", "secret", "").wrap(mux)

	req := authedReq("POST", "http://dash.local/api/monitors")
	req.Host = "dash.local"
	req.Header.Set("Origin", "http://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST: expected 403, got %d", w.Code)
	}
}

// Same-origin and non-browser (no Origin) mutations pass.
func TestAuthGate_AllowsSameOriginAndNonBrowser(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/monitors", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := newAuthGate("admin", "secret", "").wrap(mux)

	same := authedReq("POST", "http://dash.local/api/monitors")
	same.Host = "dash.local"
	same.Header.Set("Origin", "http://dash.local")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, same)
	if w.Code != 200 {
		t.Errorf("same-origin POST: expected 200, got %d", w.Code)
	}

	noOrigin := authedReq("POST", "http://dash.local/api/monitors")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, noOrigin)
	if w2.Code != 200 {
		t.Errorf("non-browser POST (no Origin): expected 200, got %d", w2.Code)
	}
}

// After enough failed attempts from one IP, further attempts are rate-limited.
func TestAuthGate_RateLimitsFailedAuth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := newAuthGate("admin", "secret", "").wrap(mux)

	for i := 0; i < authMaxFails; i++ {
		req := httptest.NewRequest("GET", "/api/apps", nil)
		req.RemoteAddr = "10.0.0.9:1234"
		req.SetBasicAuth("admin", "wrong")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	// Next attempt (even with correct creds) is locked out.
	req := httptest.NewRequest("GET", "/api/apps", nil)
	req.RemoteAddr = "10.0.0.9:1234"
	req.SetBasicAuth("admin", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after %d failures, got %d", authMaxFails, w.Code)
	}
}

// clientIP must use X-Forwarded-For only when the direct peer is a configured
// trusted proxy — otherwise XFF could be spoofed to evade the per-IP backoff.
func TestAuthGate_TrustedProxyXFF(t *testing.T) {
	_, n, _ := net.ParseCIDR("10.0.0.0/8")
	g := &authGate{trustedProxies: []*net.IPNet{n}}

	fromProxy := httptest.NewRequest("GET", "/", nil)
	fromProxy.RemoteAddr = "10.1.2.3:5000"
	fromProxy.Header.Set("X-Forwarded-For", "203.0.113.9, 10.1.2.3")
	if got := g.clientIP(fromProxy); got != "203.0.113.9" {
		t.Errorf("trusted proxy: expected forwarded client 203.0.113.9, got %q", got)
	}

	untrusted := httptest.NewRequest("GET", "/", nil)
	untrusted.RemoteAddr = "8.8.8.8:5000"
	untrusted.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := g.clientIP(untrusted); got != "8.8.8.8" {
		t.Errorf("untrusted peer: must ignore XFF, got %q", got)
	}
}
