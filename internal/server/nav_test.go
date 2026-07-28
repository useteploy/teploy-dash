package server

import (
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/remote"
)

func navServer(t *testing.T, apps []remote.AppState) *Server {
	t.Helper()
	s := New(Config{DataDir: t.TempDir(), NoAuth: true})
	s.fleet.set(apps)
	return s
}

func navURL(nav map[string]interface{}, key string) string {
	for _, a := range nav["apps"].([]map[string]string) {
		if a["key"] == key {
			return a["url"]
		}
	}
	return "<absent>"
}

func TestNavDiscoversSiblingByDomain(t *testing.T) {
	s := navServer(t, []remote.AppState{
		{App: "observe", Server: "infra", Domain: "observe.acme.com, www.observe.acme.com", CurrentPort: 3000, Status: "running"},
	})
	if got := navURL(s.teployNav("dash"), "observe"); got != "https://observe.acme.com" {
		t.Fatalf("observe url = %q, want the real domain", got)
	}
}

// teploy's own sample configs ship observe.example.com; linking there would
// send the operator nowhere, so a placeholder must fall through to host:port.
func TestNavIgnoresPlaceholderDomain(t *testing.T) {
	fleet := []remote.AppState{
		{App: "observe", Server: "infra", Domain: "observe.example.com", CurrentPort: 3000, Status: "running"},
	}
	hostOf := func(string) string { return "10.0.0.9" }
	if got := discoverSiblingURL("observe", fleet, hostOf); got != "http://10.0.0.9:3000" {
		t.Fatalf("observe url = %q, want host:port fallback", got)
	}
}

func TestNavEnvOverridesDiscovery(t *testing.T) {
	t.Setenv("TEPLOY_NAV_OBSERVE_URL", "https://explicit.example.net")
	s := navServer(t, []remote.AppState{
		{App: "observe", Server: "infra", Domain: "observe.acme.com", CurrentPort: 3000, Status: "running"},
	})
	if got := navURL(s.teployNav("dash"), "observe"); got != "https://explicit.example.net" {
		t.Fatalf("observe url = %q, want the explicit env value to win", got)
	}
}

func TestNavSkipsStoppedAndUnrelatedApps(t *testing.T) {
	s := navServer(t, []remote.AppState{
		{App: "observe", Server: "infra", Domain: "observe.acme.com", CurrentPort: 3000, Status: "stopped"},
		{App: "shipping-api", Server: "infra", Domain: "shipping.acme.com", CurrentPort: 8080, Status: "running"},
	})
	if got := navURL(s.teployNav("dash"), "observe"); got != "<absent>" {
		t.Errorf("stopped sibling should not appear, got %q", got)
	}
	if got := navURL(s.teployNav("dash"), "ship"); got != "<absent>" {
		t.Errorf("shipping-api is not Teploy Ship, got %q", got)
	}
}

func TestNavCurrentProductHasNoURL(t *testing.T) {
	s := navServer(t, nil)
	if got := navURL(s.teployNav("dash"), "dash"); got != "" {
		t.Fatalf("current product url = %q, want empty", got)
	}
}

// Sibling discovery must not blink out when the fleet cache passes its TTL —
// a sibling's address is a stable fact, and the TTL exists to keep app status
// fresh, which nav never reads.
func TestNavSurvivesStaleFleetCache(t *testing.T) {
	s := New(Config{DataDir: t.TempDir(), NoAuth: true})
	s.fleet.set([]remote.AppState{
		{App: "observe", Server: "infra", Domain: "observe.acme.com", CurrentPort: 3000, Status: "running"},
	})
	// Expire the cache the way time would.
	s.fleet.builtAt = time.Now().Add(-24 * time.Hour)
	if _, fresh := s.fleet.get(); fresh {
		t.Fatal("precondition: cache should read as expired")
	}
	if got := navURL(s.teployNav("dash"), "observe"); got != "https://observe.acme.com" {
		t.Fatalf("observe url = %q, want it to survive an expired cache", got)
	}
}
