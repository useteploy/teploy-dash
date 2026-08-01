package server

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// DASH-002: ListenAndServe used to build a throwaway *http.Server with no way
// to call Shutdown on it later. Verify the real lifecycle: Shutdown stops the
// listener and ListenAndServe returns nil (not http.ErrServerClosed).
func TestServer_ShutdownStopsListenAndServe(t *testing.T) {
	s := New(Config{DataDir: t.TempDir(), DeploymentsDir: t.TempDir(), NoAuth: true})

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.ListenAndServe("127.0.0.1:0")
	}()

	// Give the listener a moment to start before shutting it down.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.httpSrvMu.Lock()
		started := s.httpSrv != nil
		s.httpSrvMu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("ListenAndServe returned %v after Shutdown, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after Shutdown")
	}
}

// Shutdown before ListenAndServe has ever run (e.g. a server that failed to
// start, or a test double) must be a no-op, not a nil-pointer panic.
func TestServer_ShutdownBeforeListenIsNoop(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown before ListenAndServe: %v, want nil", err)
	}
}
