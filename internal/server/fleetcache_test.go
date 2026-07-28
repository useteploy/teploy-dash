package server

import (
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/remote"
)

func TestFleetCacheServesStaleAfterExpiry(t *testing.T) {
	fc := &fleetCache{ttl: 60 * time.Second}
	fc.set([]remote.AppState{{App: "web"}})
	fc.builtAt = time.Now().Add(-time.Hour) // age it out

	if _, fresh := fc.get(); fresh {
		t.Fatal("get() should report a miss once past the TTL")
	}
	if len(fc.snapshot()) != 1 {
		t.Fatal("snapshot() must still return the last known fleet")
	}
}

// set(nil) invalidates after a mutation so the next read re-fetches; it must not
// destroy the last known fleet, or the switcher and the stale-serve path go
// blank right after every deploy.
func TestFleetCacheInvalidationKeepsLastGood(t *testing.T) {
	fc := &fleetCache{ttl: 60 * time.Second}
	fc.set([]remote.AppState{{App: "web"}})
	fc.set(nil)

	if _, fresh := fc.get(); fresh {
		t.Fatal("invalidated cache should read as a miss")
	}
	if len(fc.snapshot()) != 1 {
		t.Fatal("invalidation must not discard the last known fleet")
	}
}

// A burst of stale reads must trigger exactly one refresh — otherwise every
// request SSHes the whole fleet at once.
func TestFleetCacheRefreshIsSingleFlight(t *testing.T) {
	fc := &fleetCache{ttl: time.Millisecond}
	var claimed int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if fc.beginRefresh() {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claimed != 1 {
		t.Fatalf("concurrent claims = %d, want exactly 1", claimed)
	}

	// Once the in-flight refresh finishes, the next caller may claim again.
	fc.endRefresh()
	if !fc.beginRefresh() {
		t.Fatal("refresh latch never released")
	}
}
