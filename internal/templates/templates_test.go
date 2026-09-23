package templates

import (
	"strings"
	"testing"
)

func TestDecodeCatalogTodayShapeValidatesAsUnversioned(t *testing.T) {
	// The shape the CLI emits today (teploy-cli internal/template Info):
	// name/description/accessories/variables, no version.
	raw := `[{"name":"postgres-admin","description":"PostgreSQL 16 with Adminer","accessories":["postgres"],"variables":["domain","db_password"]}]`
	entries, err := DecodeCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	e := entries[0]
	if e.Name != "postgres-admin" || e.VersionState != VersionStateUnversioned || e.Version != "" {
		t.Fatalf("entry = %#v, want unversioned postgres-admin", e)
	}
	if len(e.Variables) != 2 || e.Variables[1] != "db_password" {
		t.Fatalf("variables = %#v", e.Variables)
	}
}

func TestDecodeCatalogD05ShapeValidates(t *testing.T) {
	raw := `[{
		"name":"postgres-admin","version":"1.4.0","architecture":"container+accessory",
		"required_secrets":["db_password"],"upgrade_notes":"pg16 major upgrades require dump/restore",
		"backup_scope":"accessory dump for db; volume copy for data","fixture_check":"templates ci validate.py",
		"description":"PostgreSQL 16 with Adminer","accessories":["postgres"],
		"variables":["domain","db_password"],
		"future_field_from_newer_cli":{"tolerated":true}
	}]`
	entries, err := DecodeCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	e := entries[0]
	if e.VersionState != VersionStateVersioned || e.Version != "1.4.0" {
		t.Fatalf("entry = %#v", e)
	}
	if e.Architecture != "container+accessory" || e.FixtureCheck != "templates ci validate.py" {
		t.Fatalf("manifest fields = %#v", e)
	}
}

func TestDecodeCatalogRejectsInvalidEntries(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"missing name", `[{"description":"x"}]`, "name is required"},
		{"non-semver version", `[{"name":"a","version":"latest"}]`, "not semver"},
		{"duplicate names", `[{"name":"a"},{"name":"a"}]`, "duplicate"},
		{
			"required secret not a declared variable",
			`[{"name":"a","variables":["domain"],"required_secrets":["db_password"]}]`,
			`required secret "db_password" is not a declared`,
		},
		{"not json", `<!doctype html>`, "decoding template catalog"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeCatalog(c.raw)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}

func TestFind(t *testing.T) {
	entries, err := DecodeCatalog(`[{"name":"a"},{"name":"b"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if Find(entries, "b") == nil || Find(entries, "c") != nil {
		t.Fatal("Find mismatch")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"0.10.0", "0.9.0", 1}, // the string-compare trap
		{"1.0.0", "1.0.1", -1},
		{"2.0.0", "10.0.0", -1}, // numeric, not lexical
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
