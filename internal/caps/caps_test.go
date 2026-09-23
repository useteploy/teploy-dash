package caps

import (
	"reflect"
	"testing"
)

func TestTokensAreStable(t *testing.T) {
	want := []string{
		"administer.credentials",
		"administer.users",
		"execute.deploy",
		"execute.mutate",
		"restore.data",
		"reveal.secrets",
		"view.logs",
		"view.metadata",
	}
	if got := All(); !reflect.DeepEqual(got, want) {
		t.Fatalf("All() = %v, want %v", got, want)
	}
}

func TestPresetRoleMatrix(t *testing.T) {
	cases := []struct {
		role string
		want []string
	}{
		{"viewer", []string{ViewMetadata}},
		{"editor", []string{ExecuteDeploy, ExecuteMutate, ViewLogs, ViewMetadata}},
		{"operator", []string{ExecuteDeploy, ExecuteMutate, ViewLogs, ViewMetadata}},
		{"admin", All()},
		{"", []string{ViewMetadata}},
		{"garbage", []string{ViewMetadata}},
	}
	for _, c := range cases {
		if got := PresetForRole(c.role).Sorted(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("PresetForRole(%q) = %v, want %v", c.role, got, c.want)
		}
	}
}

// LegacyForRole records TODAY's effective permissions per role (pre-X03):
// viewers can read secret values and logs (the documented viewer contract),
// editors additionally mutate and run restore verifications, admins hold
// everything. The legacy profile must reproduce these exactly.
func TestLegacyRoleMatrix(t *testing.T) {
	cases := []struct {
		role string
		want []string
	}{
		{"viewer", []string{RevealSecrets, ViewLogs, ViewMetadata}},
		{"editor", []string{ExecuteDeploy, ExecuteMutate, RestoreData, RevealSecrets, ViewLogs, ViewMetadata}},
		{"admin", All()},
		{"", []string{RevealSecrets, ViewLogs, ViewMetadata}},
		{"garbage", []string{RevealSecrets, ViewLogs, ViewMetadata}},
	}
	for _, c := range cases {
		if got := LegacyForRole(c.role).Sorted(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("LegacyForRole(%q) = %v, want %v", c.role, got, c.want)
		}
	}
}

func TestMCPOperatorDefault(t *testing.T) {
	if got := OperatorDefault().Sorted(); !reflect.DeepEqual(got, []string{ExecuteDeploy, ExecuteMutate, ViewLogs, ViewMetadata}) {
		t.Fatalf("OperatorDefault() = %v", got)
	}
	if got := ViewerDefault().Sorted(); !reflect.DeepEqual(got, []string{ViewMetadata}) {
		t.Fatalf("ViewerDefault() = %v", got)
	}
}

func TestValidateRejectsUnknownTokens(t *testing.T) {
	if err := Validate([]string{ViewMetadata, "execute.everything"}); err == nil {
		t.Fatal("unknown capability accepted")
	}
	if err := Validate([]string{ViewMetadata, ViewLogs}); err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
}

func TestNormalizeDropsUnknownAndSorts(t *testing.T) {
	got := Normalize([]string{"nope", ViewLogs, ViewMetadata, ViewMetadata})
	if !reflect.DeepEqual(got, []string{ViewLogs, ViewMetadata}) {
		t.Fatalf("Normalize() = %v", got)
	}
}

func TestMissingNamesEveryAbsentCapability(t *testing.T) {
	s := NewSet(ViewMetadata)
	missing := s.Missing([]string{ViewMetadata, RevealSecrets})
	if !reflect.DeepEqual(missing, []string{RevealSecrets}) {
		t.Fatalf("Missing() = %v, want [reveal.secrets]", missing)
	}
	if m := s.Missing([]string{ViewMetadata}); len(m) != 0 {
		t.Fatalf("Missing() = %v, want none", m)
	}
}
