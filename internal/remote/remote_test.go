package remote

import "testing"

// TestShellQuote guards the defense-in-depth escaping for the command-injection
// fix: any value interpolated into a remote shell command must be single-quoted
// with embedded quotes neutralized, so an app name can never break out of the
// docker filter argument and execute attacker commands as root on the fleet.
func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"myapp", `'myapp'`},
		{"name=myapp-", `'name=myapp-'`},
		// The injection payload: a bare quote must be escaped to '\'' so it stays
		// inside the quoted argument instead of closing it.
		{"x'; rm -rf / #", `'x'\''; rm -rf / #'`},
		{"a'b'c", `'a'\''b'\''c'`},
		{"", `''`},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestApplyLiveState(t *testing.T) {
	st := &AppState{Containers: []ContainerInfo{}}
	applyLiveState(st, "abc|myapp-web-v2|ghcr.io/acme/app:v2|running|Up 2 minutes\n@@LOCK@@\ntrue\n@@MAINT@@\ntrue\n")

	if len(st.Containers) != 1 {
		t.Fatalf("expected one container, got %d", len(st.Containers))
	}
	if st.Containers[0].Name != "myapp-web-v2" || st.Containers[0].State != "running" {
		t.Fatalf("unexpected container: %+v", st.Containers[0])
	}
	if !st.Locked || !st.Maintenance {
		t.Fatalf("expected lock and maintenance flags, got locked=%v maintenance=%v", st.Locked, st.Maintenance)
	}
}

func TestValidAppName(t *testing.T) {
	for _, name := range []string{"api", "my.app", "release-1.2"} {
		if !validAppName(name) {
			t.Errorf("expected %q to be valid", name)
		}
	}
	for _, name := range []string{"", ".", "..", "../app", "app;rm"} {
		if validAppName(name) {
			t.Errorf("expected %q to be rejected", name)
		}
	}
}
