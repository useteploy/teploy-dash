package monitor

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/store"
)

// TestStart_LogsOnlyEnabledMonitorCount verifies that Start() logs the count
// of monitors it actually started (i.e. those with Enabled: true), not the
// total number of monitors returned by the store.
func TestStart_LogsOnlyEnabledMonitorCount(t *testing.T) {
	ms := &mockStore{
		monitors: []store.Monitor{
			{ID: "m1", Name: "enabled", Type: "http", Target: "http://127.0.0.1:1", Enabled: true},
			{ID: "m2", Name: "disabled-1", Type: "http", Target: "http://127.0.0.1:1", Enabled: false},
			{ID: "m3", Name: "disabled-2", Type: "http", Target: "http://127.0.0.1:1", Enabled: false},
		},
	}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	r := New(ms)
	r.Start()
	r.Stop()

	if !strings.Contains(buf.String(), "Started 1 monitors") {
		t.Errorf("expected log to contain %q, got %q", "Started 1 monitors", buf.String())
	}
}
