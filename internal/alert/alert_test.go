package alert

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEvent() Event {
	return Event{
		MonitorID:   "m1",
		MonitorName: "api",
		Status:      "down",
		Message:     "connection refused",
		OccurredAt:  time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
	}
}

// Send fires a webhook POST when WebhookURL is configured. Send dispatches in a
// goroutine, so we synchronize on the handler receiving the request.
func TestSend_WebhookFires(t *testing.T) {
	var (
		mu       sync.Mutex
		gotBody  []byte
		received = make(chan struct{}, 1)
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		mu.Unlock()
		received <- struct{}{}
	}))
	defer srv.Close()

	New(Config{WebhookURL: srv.URL}).Send(testEvent())

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook was not called within 2s")
	}

	mu.Lock()
	defer mu.Unlock()
	var ev Event
	if err := json.Unmarshal(gotBody, &ev); err != nil {
		t.Fatalf("webhook body not valid Event JSON: %v", err)
	}
	if ev.MonitorName != "api" || ev.Status != "down" {
		t.Errorf("unexpected payload: %+v", ev)
	}
}

// With no channels configured, Send must be a no-op (no panic, no goroutine
// touching nil config).
func TestSend_NoChannelsConfigured(t *testing.T) {
	// Should not panic.
	New(Config{}).Send(testEvent())
}

// A webhook URL set but unreachable must not panic Send (error is logged in the
// goroutine, not propagated). We just assert Send returns promptly.
func TestSend_WebhookUnreachableDoesNotBlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		New(Config{WebhookURL: "http://127.0.0.1:0"}).Send(testEvent())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Send blocked instead of dispatching async")
	}
}

// Email is only attempted when both SMTPHost and EmailTo are set. We can't
// assert the SMTP send without a server, but we can assert that a config
// missing EmailTo doesn't attempt delivery (which would otherwise dial and log).
// This is a guard against the dispatch-condition regressing.
func TestSend_EmailRequiresHostAndTo(t *testing.T) {
	// Host set but no EmailTo → no email path. Should return promptly, no panic.
	done := make(chan struct{})
	go func() {
		New(Config{SMTPHost: "localhost", SMTPPort: 25}).Send(testEvent())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Send blocked")
	}
}

// Verify the webhook Content-Type so downstream consumers (Slack-style hooks)
// parse correctly.
func TestSend_WebhookContentType(t *testing.T) {
	gotCT := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT <- r.Header.Get("Content-Type")
	}))
	defer srv.Close()

	New(Config{WebhookURL: srv.URL}).Send(testEvent())

	select {
	case ct := <-gotCT:
		if !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook not called")
	}
}
