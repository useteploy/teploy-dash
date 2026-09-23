package source

import (
	"strings"
	"testing"
)

func lower(s string) string { return strings.ToLower(s) }

// D04 identity normalization matrix: credentials stripped, scp/ssh forms
// canonicalized, .git and trailing slashes dropped, host case-folded with
// the display form preserving path case, and the IDENTITY KEY case-folded
// so URL-casing variants of one repository collide (they are one source).
func TestNormalizeCloneURLMatrix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain https", "https://github.com/tyler/akiroo", "https://github.com/tyler/akiroo"},
		{"git suffix", "https://github.com/tyler/akiroo.git", "https://github.com/tyler/akiroo"},
		{"trailing slash", "https://github.com/tyler/akiroo/", "https://github.com/tyler/akiroo"},
		{"git suffix and slash", "https://github.com/tyler/akiroo.git/", "https://github.com/tyler/akiroo"},
		{"embedded credentials", "https://token@github.com/tyler/akiroo.git", "https://github.com/tyler/akiroo"},
		{"user and password", "https://user:pass@forge.example/team/app", "https://forge.example/team/app"},
		{"scp form", "git@github.com:tyler/akiroo.git", "https://github.com/tyler/akiroo"},
		{"scp form no git suffix", "git@forge.example:team/app", "https://forge.example/team/app"},
		{"ssh scheme", "ssh://git@github.com/tyler/akiroo.git", "https://github.com/tyler/akiroo"},
		{"ssh scheme no user", "ssh://github.com/tyler/akiroo", "https://github.com/tyler/akiroo"},
		{"host case folded", "https://GitHub.Com/Tyler/Akiroo", "https://github.com/Tyler/Akiroo"},
		{"default https port dropped", "https://github.com:443/tyler/akiroo", "https://github.com/tyler/akiroo"},
		{"default ssh port dropped", "ssh://git@github.com:22/tyler/akiroo.git", "https://github.com/tyler/akiroo"},
		{"non-default port kept", "https://forge.example:3000/team/app", "https://forge.example:3000/team/app"},
		{"nested groups preserved", "https://gitlab.example/group/subgroup/app.git", "https://gitlab.example/group/subgroup/app"},
		{"http kept as distinct identity", "http://forge.example/team/app", "http://forge.example/team/app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeCloneURL(tc.in)
			if err != nil {
				t.Fatalf("NormalizeCloneURL(%q) error = %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeCloneURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			key, err := IdentityKey(ForgeGitHub, tc.in)
			if err != nil {
				t.Fatalf("IdentityKey error = %v", err)
			}
			if want := "github\x00" + lower(tc.want); key != want {
				t.Fatalf("IdentityKey(%q) = %q, want %q", tc.in, key, want)
			}
		})
	}
}

func TestNormalizeCloneURLRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"not a url",
		"git://github.com/tyler/akiroo",
		"ftp://github.com/tyler/akiroo",
		"https://github.com/tyler/akiroo?token=x",
		"https://github.com/tyler/akiroo#frag",
		"https://github.com/tyler/akiroo\ncrlf",
		"/local/path",
		"../relative",
	} {
		if got, err := NormalizeCloneURL(in); err == nil {
			t.Fatalf("NormalizeCloneURL(%q) = %q, want error", in, got)
		}
	}
}

// The F03 lesson (teploy-ship docs/AUDIT_2026-09-21.md): a bare owner/repo
// lookup returned the row of a DIFFERENT forge. The identity key here is
// forge kind + canonical URL, so same-named repositories on two forges are
// two independent identities.
func TestIdentityKeySeparatesForges(t *testing.T) {
	githubKey, err := IdentityKey(ForgeGitHub, "https://github.com/team/app.git")
	if err != nil {
		t.Fatal(err)
	}
	forgejoKey, err := IdentityKey(ForgeForgejo, "https://forge.example/team/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if githubKey == forgejoKey {
		t.Fatalf("same owner/repo on two forges must not collide: %q", githubKey)
	}
	// Different forge KINDS over the same URL are distinct identities too.
	forgejoSameHost, err := IdentityKey(ForgeForgejo, "https://github.com/team/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if githubKey == forgejoSameHost {
		t.Fatal("forge kind must participate in the identity key")
	}
}

// URL casing variants of one repository are ONE identity (the forges match
// owner/repo case-insensitively and redirect); the display URL keeps the
// operator's spelling.
func TestIdentityKeyFoldsURLCase(t *testing.T) {
	a, err := IdentityKey(ForgeGitHub, "https://github.com/Tyler/Akiroo.git")
	if err != nil {
		t.Fatal(err)
	}
	b, err := IdentityKey(ForgeGitHub, "https://GITHUB.COM/tyler/akiroo")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("URL casing variants must be one identity: %q vs %q", a, b)
	}
}

// DeriveID matches the fleet envelope convention: stable, deterministic,
// derived from the full identity key.
func TestDeriveID(t *testing.T) {
	a, err := DeriveID(ForgeGitHub, "https://github.com/team/app")
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeriveID(ForgeGitHub, "https://github.com/team/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("normalized variants must derive one ID: %q vs %q", a, b)
	}
	c, err := DeriveID(ForgeForgejo, "https://forge.example/team/app")
	if err != nil {
		t.Fatal(err)
	}
	if a == c || len(a) != len("src-0000000000000000") {
		t.Fatalf("IDs must be distinct and well-formed: %q vs %q", a, c)
	}
}

// The canonical URL join between sources and git-managed manifests ignores
// forge kind (the URL host already qualifies the origin — F03's defect was
// bare owner/repo lookups, not URL lookups) and folds case.
func TestCanonicalURLJoinsAcrossCase(t *testing.T) {
	a, err := CanonicalURL("https://github.com/Tyler/Akiroo.git")
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalURL("git@github.com:tyler/akiroo")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("canonical join key must fold case and transport: %q vs %q", a, b)
	}
}
