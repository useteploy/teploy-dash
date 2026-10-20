package configkeys

import (
	"strings"
	"testing"
)

func TestEnvKeys(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		want    []string
		wantErr bool
	}{
		{"flat map", "app: web\nimage: example/web:1\nenv:\n  RAILS_ENV: production\n  LOG_LEVEL: info\n", []string{"LOG_LEVEL", "RAILS_ENV"}, false},
		{"no env key", "app: web\nimage: example/web:1\n", nil, false},
		{"empty env mapping", "app: web\nenv: {}\n", []string{}, false},
		{"non-mapping env", "app: web\nenv: not-a-map\n", nil, true},
		{"list-shaped env", "app: web\nenv:\n  - A=1\n", nil, true},
		{"unparseable yaml", "\t{{{ not yaml", nil, true},
		{"non-mapping root", "- just\n- a\n- list\n", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EnvKeys([]byte(tc.config))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("EnvKeys = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("EnvKeys: %v", err)
			}
			if tc.want == nil && got != nil {
				t.Fatalf("EnvKeys = %v, want nil", got)
			}
			if tc.want != nil && strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("EnvKeys = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEnvKeysAlias(t *testing.T) {
	got, err := EnvKeys([]byte("app: web\n_base: &base\n  RAILS_ENV: production\nenv: *base\n"))
	if err != nil || strings.Join(got, ",") != "RAILS_ENV" {
		t.Fatalf("EnvKeys = %v %v, want [RAILS_ENV]", got, err)
	}
}

func TestEnvKeysLiteralAliasesMergesAndMalformed(t *testing.T) {
	for _, text := range []string{
		"app: web\nenv_literal: {RAILS_ENV: production}\n",
		"app: web\nbase: &base {RAILS_ENV: production}\nenv_literal: {<<: *base, TOKEN: auto}\nenv: {TOKEN: generate}\n",
		"base: &base {env_literal: {RAILS_ENV: production}}\n<<: *base\napp: web\n",
	} {
		keys, err := EnvKeys([]byte(text))
		if err != nil || !strings.Contains(strings.Join(keys, ","), "RAILS_ENV") {
			t.Fatalf("%s: %v %v", text, keys, err)
		}
	}
	for _, text := range []string{"app: web\nenv_literal: [x]\n", "app: web\nenv: {A: {nested: x}}\n", "app: web\nenv_literal: {A: one, A: two}\n", "app: web\n---\nenv: {}\n"} {
		if _, err := EnvKeys([]byte(text)); err == nil {
			t.Fatalf("malformed config accepted: %s", text)
		}
	}
}

func TestEnvKeysBounds(t *testing.T) {
	if _, err := EnvKeys([]byte("app: web\nenv: {A: \"" + strings.Repeat("x", maxBytes) + "\"}\n")); err == nil {
		t.Fatal("oversized config accepted")
	}
	deep := "app: web\nx: " + strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2) + "\n"
	if _, err := EnvKeys([]byte(deep)); err == nil {
		t.Fatal("overly deep config accepted")
	}
}

func TestGitSource(t *testing.T) {
	cases := []struct {
		name       string
		config     string
		repository string
		branch     string
		ok         bool
		wantErr    bool
	}{
		{"declared block", "app: web\ngit:\n  repository: git@github.com:owner/repo.git\n  branch: main\n", "git@github.com:owner/repo.git", "main", true, false},
		{"no branch", "app: web\ngit:\n  repository: https://forge.example/owner/repo.git\n", "https://forge.example/owner/repo.git", "", true, false},
		{"no git block", "app: web\nimage: example/web:1\n", "", "", false, false},
		{"empty git mapping", "app: web\ngit: {}\n", "", "", false, true},
		{"blank repository", "app: web\ngit:\n  repository: \"\"\n  branch: main\n", "", "", false, true},
		{"non-mapping git", "app: web\ngit: not-a-map\n", "", "", false, true},
		{"unparseable yaml", "\t{{{ not yaml", "", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repository, branch, ok, err := GitSource([]byte(tc.config))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("GitSource = %q %q %v, want error", repository, branch, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("GitSource: %v", err)
			}
			if repository != tc.repository || branch != tc.branch || ok != tc.ok {
				t.Fatalf("GitSource = %q %q %v, want %q %q %v", repository, branch, ok, tc.repository, tc.branch, tc.ok)
			}
		})
	}
}
