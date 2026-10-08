package monitor

import (
	"context"
	"github.com/useteploy/teploy-dash/internal/store"
	"testing"
)

func TestStopSealsLateReload(t *testing.T) {
	r := New(nil)
	r.Stop(context.Background())
	r.Reload(store.Monitor{ID: "late", Enabled: true})
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stopChs) != 0 {
		t.Fatal("reload restarted a stopped runner")
	}
}
