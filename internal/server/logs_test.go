package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
)

// A24/A32/A33: the log stream is SSE-only under /api/logs/. The hand-written
// WebSocket transport is deleted; these pin the endpoint's guards. (The
// happy-path stream itself opens a real SSH session and is covered by
// exercising the dashboard against a live fleet, not unit-testable here.)
func logsTestServer(t *testing.T) *Server {
	t.Helper()
	runner := func(_ context.Context, args ...string) (*cli.Result, error) {
		if strings.HasPrefix(strings.Join(args, " "), "server list") {
			// Loopback: the stream attempt fails fast (refused/handshake)
			// instead of waiting out a TEST-NET connect timeout.
			return &cli.Result{Stdout: `{"prod":{"host":"127.0.0.1","user":"deploy"}}`}, nil
		}
		return &cli.Result{}, nil
	}
	return New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true }, CLIRunner: runner,
	})
}

func TestLogsSSEGuards(t *testing.T) {
	s := logsTestServer(t)

	cases := []struct {
		name, target, origin string
		wantCode             int
	}{
		{"cross-origin blocked", "/api/logs/prod/web", "https://evil.example", http.StatusForbidden},
		{"invalid path", "/api/logs/prod", "", http.StatusBadRequest},
		{"invalid app name", "/api/logs/prod/..%2Fetc", "", http.StatusBadRequest},
		{"unknown server", "/api/logs/nosuch/web", "", http.StatusNotFound},
		{"same-origin accepted", "/api/logs/prod/web", "http://example.com", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			req.Host = "example.com"
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, req)
			if tc.wantCode != 0 && rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == 0 {
				// Accepted: SSE headers are set before the (here failing)
				// remote stream is attempted.
				if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
					t.Fatalf("content-type = %q, want text/event-stream", ct)
				}
			}
		})
	}
}

// The old WebSocket route is gone: /ws/logs/ no longer reaches a log
// stream (it falls through to the SPA fallback), and a request carrying a
// WebSocket Upgrade header against the SSE endpoint is simply an ordinary
// GET (no 101, no hijack).
func TestWebSocketRouteAndUpgradeAreGone(t *testing.T) {
	s := logsTestServer(t)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws/logs/prod/web", nil))
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("/ws/logs/ still streams (content-type %q) — route should be gone", ct)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/logs/prod/web", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-Websocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upgrade request status = %d, want 200 (SSE treats it as a plain GET)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
}

// Unauthenticated requests get a JSON 401, not the login redirect — the
// endpoint lives under /api/ now.
func TestLogsRequireSessionJSON(t *testing.T) {
	runner := func(_ context.Context, args ...string) (*cli.Result, error) {
		if strings.HasPrefix(strings.Join(args, " "), "server list") {
			return &cli.Result{Stdout: `{"prod":{"host":"127.0.0.1","user":"deploy"}}`}, nil
		}
		return &cli.Result{}, nil
	}
	s := New(Config{DataDir: t.TempDir(), AuthUser: "admin", AuthPass: "secret",
		CLIInstalled: func() bool { return true }, CLIRunner: runner})
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/logs/prod/web", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Fatalf("body = %s, want JSON unauthorized envelope", rec.Body.String())
	}
}

// R57: writer chunks are reassembled into logical lines and framed as JSON
// log events — a line split across two writes stays ONE event, and a
// carriage return inside a chunk does not corrupt the SSE framing.
func TestSSELogStreamFramesLogicalLines(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := newSSELogStream(rec, rec)

	chunks := []string{"hel", "lo\r\nworld", "\n", "tail without newline"}
	for _, chunk := range chunks {
		if _, err := stream.Write([]byte(chunk)); err != nil {
			t.Fatalf("write %q: %v", chunk, err)
		}
	}
	if err := stream.emitLine(string(stream.pending)); err != nil {
		t.Fatal(err)
	}

	body := rec.Body.String()
	want := []string{
		"event: log\ndata: {\"line\":\"hello\"}\n\n",
		"event: log\ndata: {\"line\":\"world\"}\n\n",
		"event: log\ndata: {\"line\":\"tail without newline\"}\n\n",
	}
	for _, frame := range want {
		if !strings.Contains(body, frame) {
			t.Fatalf("missing frame %q in %q", frame, body)
		}
	}
	if strings.Contains(body, "{\"line\":\"hel\"") || strings.Contains(body, "\r") {
		t.Fatalf("chunk boundaries or CR leaked into framing: %q", body)
	}

	// An oversized line is refused, not silently split.
	big := strings.Repeat("x", sseMaxLogLine+1)
	if _, err := stream.Write([]byte(big)); err == nil {
		t.Fatal("oversized line must be refused")
	}
}
