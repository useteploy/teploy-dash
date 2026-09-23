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

// X02 S2: the central decode fails closed on envelopes from a newer machine
// interface; MI <= max and pre-MI (field absent) envelopes pass.
func TestParseJSONMachineInterfaceGate(t *testing.T) {
	if _, err := ParseJSON(`{"machine_interface":2,"host":"h"}`); err == nil || !strings.Contains(err.Error(), "newer than this dash supports") {
		t.Fatalf("MI 2 must refuse with the upgrade remedy, got %v", err)
	}
	if v, err := ParseJSON(`{"machine_interface":1,"host":"h"}`); err != nil || v == nil {
		t.Fatalf("MI 1 must decode, got %v %v", v, err)
	}
	if v, err := ParseJSON(`{"host":"h"}`); err != nil || v == nil {
		t.Fatalf("pre-MI envelope must decode (legacy path), got %v %v", v, err)
	}
	// server list's bare map and non-object payloads are not gated here.
	if v, err := ParseJSON(`{"srv":{"host":"h"}}`); err != nil || v == nil {
		t.Fatalf("bare map must decode, got %v %v", v, err)
	}
}
