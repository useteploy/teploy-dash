package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPServerTimeouts(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	httpServer := s.httpServer(":3456")

	if httpServer.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want 10s", httpServer.ReadHeaderTimeout)
	}
	if httpServer.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %s, want 30s", httpServer.ReadTimeout)
	}
	if httpServer.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout = %s, want 2m", httpServer.IdleTimeout)
	}
}

func TestMutationBodyLimit(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /mutation", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("oversized request reached handler")
	})

	req := httptest.NewRequest(http.MethodPost, "/mutation", bytes.NewReader(make([]byte, maxRequestBodySize+1)))
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestWriteErrorUsesNonSuccessStatus(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, "invalid request")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
