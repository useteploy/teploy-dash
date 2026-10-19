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

func TestParseStateReadFormats(t *testing.T) {
	js, _, ok := parseStateRead("json\n1700000000\n" + `{"schema_version":2,"current_hash":"v2","current_port":49153,"domain":"a.example.com","updated_at":"2026-10-01T10:00:00Z"}`)
	if !ok || js.CurrentHash != "v2" || js.Port != 49153 || js.Domain != "a.example.com" {
		t.Fatalf("json state = %+v ok=%v", js, ok)
	}
	kv, mtime, ok := parseStateRead("kv\n1700000000\ncurrent_hash=v1\ncurrent_port=49152\n")
	if !ok || kv.CurrentHash != "v1" || mtime.Unix() != 1700000000 {
		t.Fatalf("kv state = %+v mtime=%v ok=%v", kv, mtime, ok)
	}
	for _, raw := range []string{
		"",
		"json\n1\n" + `{"schema_version":3,"tombstone":true}`,
		"json\n1\n" + `{"schema_version":9}`,
		"json\n1\nnot json",
		"kv\n1\n",
	} {
		if _, _, ok := parseStateRead(raw); ok {
			t.Errorf("parseStateRead(%q) must report no usable state", raw)
		}
	}
}
