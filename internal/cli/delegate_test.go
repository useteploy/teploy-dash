package cli

import (
	"strings"
	"testing"
)

func TestUserArgs(t *testing.T) {
	if got := userArgs(""); got != nil {
		t.Errorf("userArgs(\"\") = %v, want nil (no --user for root default)", got)
	}
	got := userArgs("tyler")
	if len(got) != 2 || got[0] != "--user" || got[1] != "tyler" {
		t.Errorf("userArgs(\"tyler\") = %v, want [--user tyler]", got)
	}
}

// userArgs must be appendable without mutating a shared base slice. Building two
// command arg lists from the same prefix should not let one corrupt the other.
func TestUserArgs_AppendSafe(t *testing.T) {
	base := []string{"status", "--host", "h", "--app", "a"}
	a := append(append([]string{}, base...), userArgs("alice")...)
	b := append(append([]string{}, base...), userArgs("bob")...)

	if strings.Join(a, " ") == strings.Join(b, " ") {
		t.Fatal("expected distinct arg lists for different users")
	}
	if !strings.Contains(strings.Join(a, " "), "--user alice") {
		t.Errorf("a missing alice: %v", a)
	}
	if !strings.Contains(strings.Join(b, " "), "--user bob") {
		t.Errorf("b missing bob: %v", b)
	}
}
