package source

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoutingLinkUnlinkAndLookup(t *testing.T) {
	dir := t.TempDir()
	routing, err := OpenRouting(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := routing.Link("infra-home", "w6probe", "git@github.com:owner/repo.git"); err != nil {
		t.Fatal(err)
	}
	if err := routing.Link("infra-home", "w6probe2", "https://github.com/owner/repo.git"); err != nil {
		t.Fatal(err)
	}
	if err := routing.Link("infra-home", "w6probe", "https://github.com/other/repo2"); err != nil {
		t.Fatal(err)
	}
	// Re-linking the same app replaces its repository instead of stacking.
	got := routing.RoutesFor("https://github.com/OWNER/repo/")
	if len(got) != 1 || got[0].App != "w6probe2" || got[0].Server != "infra-home" {
		t.Fatalf("routes = %+v", got)
	}
	if got := routing.RoutesFor("https://github.com/other/repo2"); len(got) != 1 || got[0].App != "w6probe" {
		t.Fatalf("relinked route = %+v", got)
	}
	if got := routing.RoutesFor("https://github.com/nothing"); len(got) != 0 {
		t.Fatalf("unexpected routes %+v", got)
	}
	if err := routing.Unlink("infra-home", "w6probe2"); err != nil {
		t.Fatal(err)
	}
	if got := routing.RoutesFor("https://github.com/owner/repo"); len(got) != 0 {
		t.Fatalf("route survived unlink: %+v", got)
	}

	// The cache survives reopen, and a reopened store rejects damage.
	reopened, err := OpenRouting(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.RoutesFor("https://github.com/other/repo2"); len(got) != 1 || got[0].App != "w6probe" {
		t.Fatalf("route did not survive reopen: %+v", got)
	}
	path := filepath.Join(dir, "sources", "routing.json")
	if err := os.WriteFile(path, []byte("[{}] trailing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRouting(dir); err == nil {
		t.Fatal("damaged routing file accepted")
	}
}

func TestRoutingRejectsInvalidInput(t *testing.T) {
	routing, err := OpenRouting(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := routing.Link("infra-home", "w6probe", "not a url"); err == nil {
		t.Fatal("invalid repository accepted")
	}
	if err := routing.Link("", "w6probe", "https://github.com/owner/repo"); err == nil {
		t.Fatal("missing server accepted")
	}
	if err := routing.Link("infra-home", "", "https://github.com/owner/repo"); err == nil {
		t.Fatal("missing app accepted")
	}
}

func TestRoutingMatchesAcrossTransports(t *testing.T) {
	routing, err := OpenRouting(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The app pulls over SSH on the forge's SSH port; the source was
	// registered with the forge's HTTP clone URL on another port.
	if err := routing.Link("infra-home", "web", "ssh://git@forge.lan:2222/tyler/app.git"); err != nil {
		t.Fatal(err)
	}
	if got := routing.RoutesFor("http://forge.lan:49152/tyler/app"); len(got) != 1 || got[0].App != "web" {
		t.Fatalf("routes = %+v", got)
	}
	if got := routing.RoutesFor("http://forge.lan:49152/tyler/other"); len(got) != 0 {
		t.Fatalf("other repository woke %+v", got)
	}
}
