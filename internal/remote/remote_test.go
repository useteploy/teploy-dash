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
