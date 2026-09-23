package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/outbox"
	"github.com/useteploy/teploy-dash/internal/store"
)

// failingSender fails every delivery with a fixed message.
type failingSender struct{ err string }

func (f *failingSender) SendSync(alert.Event) error { return errors.New(f.err) }

// D08: the monitor detail response exposes the last alert-delivery state —
// status, attempts, and the exact last failure — so a dead-lettered alert is
// visible on the monitor it belonged to, not buried in logs.
func TestMonitorDetailExposesDeliveryStatus(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	m := store.Monitor{ID: "m1", Name: "M1", Type: "http", Target: "https://example.com", Interval: 60 * time.Second, Enabled: true}
	if err := st.SaveMonitor(&m); err != nil {
		t.Fatal(err)
	}

	// A dead-lettered delivery for m1: 2 failed attempts, visible error.
	failing := &failingSender{err: "webhook unreachable"}
	ob, err := outbox.New(t.TempDir(), outbox.Options{
		ConfigFn:     func() (alert.Config, error) { return alert.Config{WebhookURL: "http://configured.example/"}, nil },
		Sender:       failing,
		PollInterval: 2 * time.Millisecond,
		MaxAttempts:  2,
		BackoffBase:  time.Millisecond,
		BackoffMax:   time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Stop(context.Background())
	ob.Send(alert.Event{MonitorID: "m1", MonitorName: "M1", Status: "down", OccurredAt: time.Now()})
	ob.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d := ob.LatestForMonitor("m1"); d != nil && d.Status == outbox.StatusDeadLettered {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	s := &Server{store: st, outbox: ob}
	w := httptest.NewRecorder()
	s.handleMonitor(w, httptest.NewRequest("GET", "/api/monitors/m1", nil))
	if w.Code != 200 {
		t.Fatalf("GET monitor: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"delivery"`, `"dead_lettered"`, `"webhook unreachable"`, `"attempts":2`} {
		if !strings.Contains(body, want) {
			t.Fatalf("delivery status missing %s in %s", want, body)
		}
	}
}

// The list endpoint carries the same per-monitor delivery state.
func TestMonitorListExposesDeliveryStatus(t *testing.T) {
	st := store.NewFileStore(t.TempDir())
	m := store.Monitor{ID: "m1", Name: "M1", Type: "http", Target: "https://example.com", Interval: 60 * time.Second, Enabled: true}
	if err := st.SaveMonitor(&m); err != nil {
		t.Fatal(err)
	}
	ob, err := outbox.New(t.TempDir(), outbox.Options{
		ConfigFn: func() (alert.Config, error) { return alert.Config{WebhookURL: "http://configured.example/"}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Stop(context.Background())
	// Not started (no worker): the record stays pending — still visible.
	ob.Send(alert.Event{MonitorID: "m1", Status: "down", OccurredAt: time.Now()})

	s := &Server{store: st, outbox: ob}
	w := httptest.NewRecorder()
	s.handleMonitors(w, httptest.NewRequest("GET", "/api/monitors", nil))
	if w.Code != 200 {
		t.Fatalf("list monitors: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"delivery"`) || !strings.Contains(w.Body.String(), `"pending"`) {
		t.Fatalf("pending delivery not visible in list: %s", w.Body.String())
	}
}
