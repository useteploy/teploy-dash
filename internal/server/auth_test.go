package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBasicAuth_RejectsMissingCreds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := basicAuthMiddleware("admin", "secret", mux)

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
	h := basicAuthMiddleware("admin", "secret", mux)

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
	h := basicAuthMiddleware("admin", "secret", mux)

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
	h := basicAuthMiddleware("admin", "secret", mux)

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 on /api/health without auth (liveness probe), got %d", w.Code)
	}
}
