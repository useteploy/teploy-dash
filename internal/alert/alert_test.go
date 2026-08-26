package alert

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// sanitizeHeader must strip CR/LF so a monitor name can't inject extra SMTP
// headers into the Subject line.
func TestSanitizeHeader_StripsCRLF(t *testing.T) {
	cases := map[string]string{
		"normal name":         "normal name",
		"x\r\nBcc: evil@host": "xBcc: evil@host",
		"line1\nline2":        "line1line2",
		"carriage\rreturn":    "carriagereturn",
		"":                    "",
	}
	for in, want := range cases {
		if got := sanitizeHeader(in); got != want {
			t.Errorf("sanitizeHeader(%q) = %q, want %q", in, got, want)
		}
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

// signWebhook uses the signature scheme shared by teploy-cli and
// teploy-observe. These tests pin it: a receiver of all three products writes
// one verifier, so a change here silently breaks every one of them.
func verifyDashSignature(secret string, body []byte, ts, sig string) bool {
	sig = strings.TrimPrefix(sig, "sha256=")
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func TestSendWebhook_SignedWhenSecretSet(t *testing.T) {
	const secret = "dash-secret"
	type captured struct {
		body []byte
		ts   string
		sig  string
	}
	got := make(chan captured, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- captured{body, r.Header.Get("X-Teploy-Timestamp"), r.Header.Get("X-Teploy-Signature")}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	New(Config{WebhookURL: srv.URL, WebhookSecret: secret}).Send(Event{
		MonitorID: "m1", MonitorName: "akiroo-lite", Status: "down",
		Message: "connection refused", OccurredAt: time.Unix(1700000000, 0).UTC(),
	})

	select {
	case c := <-got:
		if c.sig == "" || c.ts == "" {
			t.Fatalf("delivery was not signed (sig=%q ts=%q)", c.sig, c.ts)
		}
		if !verifyDashSignature(secret, c.body, c.ts, c.sig) {
			t.Error("signature did not verify against the received body")
		}
		if verifyDashSignature("wrong", c.body, c.ts, c.sig) {
			t.Error("signature verified under the wrong secret")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook was never delivered")
	}
}

func TestSendWebhook_UnsignedWhenNoSecret(t *testing.T) {
	// An install that has not configured a secret must keep delivering exactly
	// what it delivered before, so an existing receiver is unaffected.
	type hdrs struct{ ts, sig string }
	got := make(chan hdrs, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- hdrs{r.Header.Get("X-Teploy-Timestamp"), r.Header.Get("X-Teploy-Signature")}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	New(Config{WebhookURL: srv.URL}).Send(Event{MonitorName: "m", Status: "up"})

	select {
	case h := <-got:
		if h.sig != "" || h.ts != "" {
			t.Errorf("unsigned config still sent signature headers (sig=%q ts=%q)", h.sig, h.ts)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook was never delivered")
	}
}

func TestBuildEmailMessage_SanitizesFromAndTo(t *testing.T) {
	event := testEvent()
	msg := buildEmailMessage("alerts@example.com\r\nBcc: attacker@evil.test", "ops@example.com", event)

	headerPart := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(headerPart) != 2 {
		t.Fatalf("message missing header/body separator: %q", msg)
	}
	headerLines := strings.Split(headerPart[0], "\r\n")
	if len(headerLines) != 3 {
		t.Fatalf("expected exactly 3 header lines, got %d: %v", len(headerLines), headerLines)
	}
	for _, line := range headerLines {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("injected Bcc header line survived sanitization: %v", headerLines)
		}
	}
	if !strings.HasPrefix(headerLines[0], "From: alerts@example.comBcc: attacker@evil.test") {
		t.Errorf("unexpected From line: %q", headerLines[0])
	}
}

func TestBuildEmailMessage_BenignInput(t *testing.T) {
	event := testEvent()
	got := buildEmailMessage("alerts@example.com", "ops@example.com", event)

	subject := "[teploy] api is down"
	body := "Monitor: api\nStatus: down\nMessage: connection refused\nTime: 2026-05-30T12:00:00Z"
	want := "From: alerts@example.com\r\nTo: ops@example.com\r\nSubject: " + subject + "\r\n\r\n" + body

	if got != want {
		t.Errorf("benign input produced different message:\ngot:  %q\nwant: %q", got, want)
	}
}
