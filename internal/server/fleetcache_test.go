package server

import (
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/remote"
)

func TestFleetCacheServesStaleAfterExpiry(t *testing.T) {
	fc := &fleetCache{ttl: 60 * time.Second}
	fc.publish(0, []remote.AppState{{App: "web"}})
	fc.mu.Lock()
	fc.builtAt = time.Now().Add(-time.Hour) // age it out
	fc.mu.Unlock()

	if _, fresh := fc.get(); fresh {
		t.Fatal("get() should report a miss once past the TTL")
	}
	if len(fc.snapshot()) != 1 {
		t.Fatal("snapshot() must still return the last known fleet")
	}
}

// invalidate() drops the cached snapshot after a mutation so the next read
// re-fetches; it must not destroy the last known fleet, or the switcher and
// the stale-serve path go blank right after every deploy.
func TestFleetCacheInvalidationKeepsLastGood(t *testing.T) {
	fc := &fleetCache{ttl: 60 * time.Second}
	fc.publish(0, []remote.AppState{{App: "web"}})
	fc.invalidate()

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
			if _, ok := fc.beginRefresh(); ok {
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
	if _, ok := fc.beginRefresh(); !ok {
		t.Fatal("refresh latch never released")
	}
}

// R50: a refresh that started BEFORE a mutation must not publish its stale
// snapshot as freshly built afterwards — the generation token drops it.
func TestFleetCacheStaleRefreshCannotPublishAfterInvalidation(t *testing.T) {
	fc := &fleetCache{ttl: 60 * time.Second}
	fc.publish(0, []remote.AppState{{App: "old"}})

	generation, ok := fc.beginRefresh()
	if !ok {
		t.Fatal("refresh should be claimable")
	}
	// A mutation lands while the refresh is in flight.
	fc.invalidate()
	if fc.publish(generation, []remote.AppState{{App: "stale-sweep"}}) {
		t.Fatal("a sweep predating the invalidation must not publish")
	}
	if _, fresh := fc.get(); fresh {
		t.Fatal("cache must stay a miss after the dropped sweep")
	}
	if got := fc.snapshot(); len(got) != 1 || got[0].App != "old" {
		t.Fatalf("last-known state must be preserved, got %+v", got)
	}

	// A refresh started AFTER the invalidation publishes normally.
	fc.endRefresh()
	generation, ok = fc.beginRefresh()
	if !ok {
		t.Fatal("refresh should be reclaimable")
	}
	if !fc.publish(generation, []remote.AppState{{App: "new"}}) {
		t.Fatal("current-generation sweep must publish")
	}
	if got, _ := fc.get(); len(got) != 1 || got[0].App != "new" {
		t.Fatalf("expected the new snapshot, got %+v", got)
	}
}
