package manifest

import (
	"strings"
	"testing"
)

// D07 authority indicators: EnvKeys answers the env names a manifest's
// root `env:` mapping declares (the CLI's teploy.yml schema — a flat string
// map at the app level), or nil when it cannot stand behind any answer.
func TestEnvKeys(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		want     []string
	}{
		{"flat map", "app: web\nimage: example/web:1\nenv:\n  RAILS_ENV: production\n  LOG_LEVEL: info\n", []string{"LOG_LEVEL", "RAILS_ENV"}},
		{"no env key", "app: web\nimage: example/web:1\n", nil},
		{"empty env mapping", "app: web\nenv: {}\n", []string{}},
		{"non-mapping env (unknown schema shape)", "app: web\nenv: not-a-map\n", nil},
		{"list-shaped env (unknown schema shape)", "app: web\nenv:\n  - A=1\n", nil},
		{"unparseable yaml", "\t{{{ not yaml", nil},
		{"non-mapping root", "- just\n- a\n- list\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EnvKeys([]byte(tc.manifest))
			if tc.want == nil && got != nil {
				t.Fatalf("EnvKeys = %v, want nil", got)
			}
			if tc.want != nil && strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("EnvKeys = %v, want %v", got, tc.want)
			}
		})
	}
}

// A YAML alias for the env mapping resolves to its keys.
func TestEnvKeysAlias(t *testing.T) {
	m := "app: web\n_base: &base\n  RAILS_ENV: production\nenv: *base\n"
	got := EnvKeys([]byte(m))
	if strings.Join(got, ",") != "RAILS_ENV" {
		t.Fatalf("EnvKeys = %v, want [RAILS_ENV]", got)
	}
}

// The keys a manifest declares must match what Validate accepts — a
// manifest that passes validation parses for the authority surface too
// (same document grammar).
func TestEnvKeysOnValidManifest(t *testing.T) {
	content := "app: web\nimage: example/web:1\nenv:\n  A: one\n  B: two\n"
	if err := Validate([]byte(content), "web"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := EnvKeys([]byte(content)); strings.Join(got, ",") != "A,B" {
		t.Fatalf("EnvKeys = %v, want [A B]", got)
	}
}
