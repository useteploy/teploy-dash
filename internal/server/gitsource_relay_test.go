package server

// gitsource_relay_test.go — the D4 webhook relay: an authenticated,
// verified push for a repository an app builds from wakes `trigger poll`
// on that app's server and records the result in the delivery ledger.
// Uses the same webhook signature fixtures as sources_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/source"
)

// relayTestServer wires a no-auth server with the git-source capability
// tokens and a push authority that always confirms; reply answers every
// other CLI call (config get, trigger poll, version handshake).
type relayServer struct {
	*Server
	runner *configRunner
}

func newRelayServer(t *testing.T, reply func(args []string, stdin string) (*cli.Result, error)) *relayServer {
	t.Helper()
	runner := &configRunner{tokens: gitSourceTokens, reply: reply}
	server := newConfigServer(t, runner)
	if server.routingInitErr != nil {
		t.Fatalf("routing init: %v", server.routingInitErr)
	}
	server.sourcePushAuthority = func(context.Context, *source.Source, string, string) error { return nil }
	return &relayServer{Server: server, runner: runner}
}

// configGetYAML answers `config get --json` for one app with a Teploy-home
// config declaring the git block.
func configGetYAML(app, repository, branch string) string {
	content := "app: " + app + "\nimage: example/" + app + ":1\ngit:\n  repository: " + repository + "\n  branch: " + branch + "\n"
	encoded, _ := json.Marshal(content)
	return `{"machine_interface":2,"host":"192.0.2.10","app":"` + app + `","home":"teploy","commit":"` + strings.Repeat("a", 40) + `","content":` + string(encoded) + `}`
}

// awaitRelay polls the ledger until the relay goroutine records the
// delivery (or the deadline passes).
func awaitRelay(t *testing.T, s *relayServer, id, deliveryID string) []source.Delivery {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, delivery := range s.sources.RecentDeliveries(id, 10) {
			if delivery.ID == deliveryID {
				return s.sources.RecentDeliveries(id, 10)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("relay never recorded the delivery")
	return nil
}

func TestWebhookRelayPollsConnectedApp(t *testing.T) {
	var pollArgs []string
	s := newRelayServer(t, func(args []string, stdin string) (*cli.Result, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "config get"):
			return &cli.Result{Stdout: configGetYAML("web", "git@github.com:owner/repo.git", "main")}, nil
		case strings.HasPrefix(joined, "trigger poll"):
			pollArgs = args
			return &cli.Result{Stdout: `{"machine_interface":2,"app":"web","ref":"refs/heads/main","head":"` + strings.Repeat("ab", 20) + `","admitted":true,"reason":"deployed","deployed_release":"r-20261019-abc123"}`}, nil
		}
		return &cli.Result{Stdout: "{}"}, nil
	})
	id, secret := createSource(t, s.Server, "github", "git@github.com:owner/repo.git", "main")
	if err := s.routing.Link("prod", "web", "https://github.com/owner/repo"); err != nil {
		t.Fatal(err)
	}

	response := pushDelivery(t, s.Server, id, secret, "d-relay-1", strings.Repeat("ab", 20))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"relayed"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	awaitRelay(t, s, id, "d-relay-1")
	if pollArgs == nil || !strings.Contains(strings.Join(pollArgs, " "), "trigger poll --now --json --hint "+strings.Repeat("ab", 20)) ||
		!strings.Contains(strings.Join(pollArgs, " "), "--actor dash:webhook-relay") {
		t.Fatalf("poll argv = %v", pollArgs)
	}
	for _, delivery := range awaitRelay(t, s, id, "d-relay-1") {
		if delivery.ID != "d-relay-1" {
			continue
		}
		if delivery.Disposition != source.DispositionAdmitted || !strings.Contains(delivery.Reason, "deployed r-20261019-abc123") {
			t.Fatalf("delivery = %+v", delivery)
		}
	}

	// The same delivery id retried while the ledger record already exists
	// answers duplicate without re-polling.
	pollArgs = nil
	if again := pushDelivery(t, s.Server, id, secret, "d-relay-1", strings.Repeat("ab", 20)); again.Code != http.StatusOK ||
		!strings.Contains(again.Body.String(), `"duplicate"`) {
		t.Fatalf("replayed delivery status=%d body=%s", again.Code, again.Body.String())
	}
	if pollArgs != nil {
		t.Fatal("replayed delivery re-polled")
	}
}

func TestWebhookRelayRecordsPollRefusalsAndFailures(t *testing.T) {
	s := newRelayServer(t, func(args []string, stdin string) (*cli.Result, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "config get"):
			return &cli.Result{Stdout: configGetYAML("web", "https://github.com/owner/repo.git", "main")}, nil
		case strings.HasPrefix(joined, "trigger poll"):
			return &cli.Result{ExitCode: 1, Stderr: `{"code":"force-push","detail":"refs/heads/main was force-pushed"}`}, &cli.ExitStatusError{Code: 1}
		}
		return &cli.Result{Stdout: "{}"}, nil
	})
	id, secret := createSource(t, s.Server, "github", "https://github.com/owner/repo", "main")
	s.routing.Link("prod", "web", "https://github.com/owner/repo")
	pushDelivery(t, s.Server, id, secret, "d-refuse-1", strings.Repeat("cd", 20))
	for _, delivery := range awaitRelay(t, s, id, "d-refuse-1") {
		if delivery.ID != "d-refuse-1" {
			continue
		}
		if delivery.Disposition != source.DispositionIgnored || !strings.Contains(delivery.Reason, "force-pushed") {
			t.Fatalf("delivery = %+v", delivery)
		}
	}

	// A transport failure (exec error, no result) is recorded, never retried.
	s2 := newRelayServer(t, func(args []string, stdin string) (*cli.Result, error) {
		if strings.HasPrefix(strings.Join(args, " "), "config get") {
			return &cli.Result{Stdout: configGetYAML("web", "https://github.com/owner/repo.git", "main")}, nil
		}
		return nil, &cliExitFake{}
	})
	id2, secret2 := createSource(t, s2.Server, "github", "https://github.com/owner/repo", "main")
	s2.routing.Link("prod", "web", "https://github.com/owner/repo")
	pushDelivery(t, s2.Server, id2, secret2, "d-fail-1", strings.Repeat("ef", 20))
	for _, delivery := range awaitRelay(t, s2, id2, "d-fail-1") {
		if delivery.ID != "d-fail-1" {
			continue
		}
		if delivery.Disposition != source.DispositionIgnored || !strings.Contains(delivery.Reason, "relay failed") {
			t.Fatalf("delivery = %+v", delivery)
		}
	}
}

type cliExitFake struct{}

func (cliExitFake) Error() string { return "ssh: connection refused" }

func TestWebhookRelayReverifiesAndUnlinksStaleRoutes(t *testing.T) {
	home := func(app string) string {
		// The app moved to a different repository (or none): folder home.
		return `{"machine_interface":2,"host":"192.0.2.10","app":"` + app + `","home":"folder"}`
	}
	s := newRelayServer(t, func(args []string, stdin string) (*cli.Result, error) {
		return &cli.Result{Stdout: home("web")}, nil
	})
	id, secret := createSource(t, s.Server, "github", "https://github.com/owner/repo", "main")
	s.routing.Link("prod", "web", "https://github.com/owner/repo")
	pushDelivery(t, s.Server, id, secret, "d-stale-1", strings.Repeat("12", 20))
	awaitRelay(t, s, id, "d-stale-1")
	if routes := s.routing.RoutesFor("https://github.com/owner/repo"); len(routes) != 0 {
		t.Fatalf("stale route survived: %+v", routes)
	}
	if deliveries := s.sources.RecentDeliveries(id, 10); len(deliveries) != 1 || deliveries[0].Reason != noAppConnected {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	// A branch mismatch keeps the route but relays nothing.
	yaml := func(app, branch string) string {
		return configGetYAML(app, "https://github.com/owner/repo.git", branch)
	}
	s2 := newRelayServer(t, func(args []string, stdin string) (*cli.Result, error) {
		return &cli.Result{Stdout: yaml("web", "develop")}, nil
	})
	id2, secret2 := createSource(t, s2.Server, "github", "https://github.com/owner/repo", "")
	s2.routing.Link("prod", "web", "https://github.com/owner/repo")
	pushDelivery(t, s2.Server, id2, secret2, "d-branch-1", strings.Repeat("34", 20))
	awaitRelay(t, s2, id2, "d-branch-1")
	if routes := s2.routing.RoutesFor("https://github.com/owner/repo"); len(routes) != 1 {
		t.Fatalf("branch mismatch unlinked the route: %+v", routes)
	}
	if deliveries := s2.sources.RecentDeliveries(id2, 10); len(deliveries) != 1 || deliveries[0].Reason != noAppConnected {
		t.Fatalf("deliveries = %+v", deliveries)
	}
}

func TestWebhookRelayHiddenWithoutCapabilityOrRoute(t *testing.T) {
	// Older CLI: the relay is inert and the delivery keeps today's
	// ignore reason.
	runner := &configRunner{tokens: []string{"app-list-machine", "config-home-v1"}}
	s := newConfigServer(t, runner)
	s.sourcePushAuthority = func(context.Context, *source.Source, string, string) error { return nil }
	if err := s.routing.Link("prod", "web", "https://github.com/owner/repo"); err != nil {
		t.Fatal(err)
	}
	id, secret := createSource(t, s, "github", "https://github.com/owner/repo", "main")
	response := pushDelivery(t, s, id, secret, "d-old-1", strings.Repeat("56", 20))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), noAppConnected) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if deliveries := s.sources.RecentDeliveries(id, 10); len(deliveries) != 1 || deliveries[0].Disposition != source.DispositionIgnored {
		t.Fatalf("deliveries = %+v", deliveries)
	}

	// Capable CLI but no route: same answer, no CLI app call beyond the
	// push-head authority.
	calls := 0
	s2 := newRelayServer(t, func(args []string, stdin string) (*cli.Result, error) {
		calls++
		return &cli.Result{Stdout: "{}"}, nil
	})
	id2, secret2 := createSource(t, s2.Server, "github", "https://github.com/owner/repo", "main")
	response = pushDelivery(t, s2.Server, id2, secret2, "d-noroute-1", strings.Repeat("78", 20))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), noAppConnected) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, delivery := range s2.sources.RecentDeliveries(id2, 10) {
		if delivery.ID == "d-noroute-1" && (delivery.Disposition != source.DispositionIgnored || delivery.Reason != noAppConnected) {
			t.Fatalf("delivery = %+v", delivery)
		}
	}
}

func TestWebhookRelayDedupesInFlightDelivery(t *testing.T) {
	block := make(chan struct{})
	s := newRelayServer(t, func(args []string, stdin string) (*cli.Result, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "config get"):
			return &cli.Result{Stdout: configGetYAML("web", "https://github.com/owner/repo.git", "main")}, nil
		case strings.HasPrefix(joined, "trigger poll"):
			<-block
			return &cli.Result{Stdout: `{"machine_interface":2,"app":"web","ref":"refs/heads/main","admitted":false,"reason":"up-to-date"}`}, nil
		}
		return &cli.Result{Stdout: "{}"}, nil
	})
	id, secret := createSource(t, s.Server, "github", "https://github.com/owner/repo", "main")
	s.routing.Link("prod", "web", "https://github.com/owner/repo")
	first := pushDelivery(t, s.Server, id, secret, "d-race-1", strings.Repeat("9a", 20))
	if !strings.Contains(first.Body.String(), `"relayed"`) {
		t.Fatalf("first delivery body=%s", first.Body.String())
	}
	second := pushDelivery(t, s.Server, id, secret, "d-race-1", strings.Repeat("9a", 20))
	if !strings.Contains(second.Body.String(), `"duplicate"`) || !strings.Contains(second.Body.String(), "relay already running") {
		t.Fatalf("in-flight retry body=%s", second.Body.String())
	}
	close(block)
	awaitRelay(t, s, id, "d-race-1")
	deliveries := s.sources.RecentDeliveries(id, 10)
	if len(deliveries) != 1 || deliveries[0].ID != "d-race-1" || !strings.Contains(deliveries[0].Reason, "up-to-date") {
		t.Fatalf("deliveries = %+v", deliveries)
	}
}
