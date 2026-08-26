package server

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// authedReq creates a request carrying a live session cookie issued by g.
func authedReq(g *authGate, method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	token := g.newSession("admin", RoleAdmin)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	return req
}

func loginBody(password string) *bytes.Buffer {
	b, _ := json.Marshal(map[string]string{"password": password})
	return bytes.NewBuffer(b)
}

func TestAuth_RejectsMissingSession(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := newAuthGate("admin", "secret", "").wrap(mux)

	req := httptest.NewRequest("GET", "/api/apps", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without session, got %d", w.Code)
	}
}

func TestAuth_RejectsWrongPassword(t *testing.T) {
	g := newAuthGate("admin", "secret", "")
	req := httptest.NewRequest("POST", "/api/login", loginBody("wrong"))
	w := httptest.NewRecorder()
	g.handleLogin(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong password, got %d", w.Code)
	}
}

func TestAuth_AcceptsCorrectPassword(t *testing.T) {
	g := newAuthGate("admin", "secret", "")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := g.wrap(mux)

	// Login to get a session cookie.
	loginReq := httptest.NewRequest("POST", "/api/login", loginBody("secret"))
	lw := httptest.NewRecorder()
	g.handleLogin(lw, loginReq)
	if lw.Code != 200 {
		t.Fatalf("login failed: %d", lw.Code)
	}
	var cookie *http.Cookie
	for _, c := range lw.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie after login")
	}
	if cookie.Secure {
		t.Fatal("local HTTP login cookie must not be Secure")
	}
	if got := lw.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("login Cache-Control = %q, want no-store", got)
	}

	// Use the session cookie.
	req := httptest.NewRequest("GET", "/api/apps", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200 with valid session, got %d", w.Code)
	}
}

func TestAuth_SecureCookieForHTTPSAndTrustedForwardedProto(t *testing.T) {
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	g := newAuthGate("admin", "secret", "")
	g.trustedProxies = []*net.IPNet{trusted}

	tests := []struct {
		name       string
		target     string
		remoteAddr string
		proto      string
		wantSecure bool
	}{
		{name: "direct HTTPS", target: "https://dash.local/api/login", remoteAddr: "203.0.113.1:1234", wantSecure: true},
		{name: "trusted HTTPS proxy", target: "http://dash.local/api/login", remoteAddr: "10.1.2.3:1234", proto: "https", wantSecure: true},
		{name: "untrusted forwarded proto", target: "http://dash.local/api/login", remoteAddr: "203.0.113.1:1234", proto: "https", wantSecure: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.target, loginBody("secret"))
			req.RemoteAddr = tt.remoteAddr
			if tt.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			w := httptest.NewRecorder()
			g.handleLogin(w, req)

			cookies := w.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("got %d cookies, want 1", len(cookies))
			}
			if cookies[0].Secure != tt.wantSecure {
				t.Errorf("Secure = %v, want %v", cookies[0].Secure, tt.wantSecure)
			}
		})
	}
}

func TestAuth_HealthExempt(t *testing.T) {
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

// A cross-origin state-changing request (browser CSRF) must be blocked even
// when authenticated.
func TestAuthGate_BlocksCrossOriginMutation(t *testing.T) {
	g := newAuthGate("admin", "secret", "")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/monitors", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := g.wrap(mux)

	req := authedReq(g, "POST", "http://dash.local/api/monitors")
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
	g := newAuthGate("admin", "secret", "")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/monitors", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := g.wrap(mux)

	same := authedReq(g, "POST", "http://dash.local/api/monitors")
	same.Host = "dash.local"
	same.Header.Set("Origin", "http://dash.local")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, same)
	if w.Code != 200 {
		t.Errorf("same-origin POST: expected 200, got %d", w.Code)
	}

	noOrigin := authedReq(g, "POST", "http://dash.local/api/monitors")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, noOrigin)
	if w2.Code != 200 {
		t.Errorf("non-browser POST (no Origin): expected 200, got %d", w2.Code)
	}
}

// After enough failed login attempts from one IP, further attempts are rate-limited.
func TestAuthGate_RateLimitsFailedAuth(t *testing.T) {
	g := newAuthGate("admin", "secret", "")

	for i := 0; i < authMaxFails; i++ {
		req := httptest.NewRequest("POST", "/api/login", loginBody("wrong"))
		req.RemoteAddr = "10.0.0.9:1234"
		w := httptest.NewRecorder()
		g.handleLogin(w, req)
	}
	// Next attempt is locked out even with correct password.
	req := httptest.NewRequest("POST", "/api/login", loginBody("secret"))
	req.RemoteAddr = "10.0.0.9:1234"
	w := httptest.NewRecorder()
	g.handleLogin(w, req)
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

	// A client may PREPEND a forged address; the proxy appends the address it
	// actually saw. The rightmost non-trusted entry wins, so the forgery can
	// neither mint fresh rate-limit keys nor pin a lockout on a victim.
	spoofed := httptest.NewRequest("GET", "/", nil)
	spoofed.RemoteAddr = "10.1.2.3:5000"
	spoofed.Header.Set("X-Forwarded-For", "198.51.100.77, 203.0.113.9, 10.1.2.3")
	if got := g.clientIP(spoofed); got != "203.0.113.9" {
		t.Errorf("spoofed prefix: expected 203.0.113.9, got %q", got)
	}

	// A chain made only of trusted proxies leaves the peer as the answer.
	allTrusted := httptest.NewRequest("GET", "/", nil)
	allTrusted.RemoteAddr = "10.1.2.3:5000"
	allTrusted.Header.Set("X-Forwarded-For", "10.9.9.9, 10.1.2.3")
	if got := g.clientIP(allTrusted); got != "10.1.2.3" {
		t.Errorf("all-trusted chain: expected 10.1.2.3, got %q", got)
	}

	untrusted := httptest.NewRequest("GET", "/", nil)
	untrusted.RemoteAddr = "8.8.8.8:5000"
	untrusted.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := g.clientIP(untrusted); got != "8.8.8.8" {
		t.Errorf("untrusted peer: must ignore XFF, got %q", got)
	}
}
