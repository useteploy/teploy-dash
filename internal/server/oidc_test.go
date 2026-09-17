package server

import (
	"testing"
	"time"
)

func newTestOIDC() *oidcAuth {
	return &oidcAuth{
		usernameClaim: "preferred_username",
		roleClaim:     "teploy_role",
		groupsClaim:   "groups",
		adminGroup:    "teploy-admins",
		editorGroup:   "teploy-editors",
		viewerGroup:   "teploy-viewers",
		defaultRole:   RoleViewer,
		flows:         make(map[string]*oidcFlow),
	}
}

func TestResolveRoleDirectClaimWins(t *testing.T) {
	o := newTestOIDC()
	// A direct, recognized role claim takes precedence over groups.
	got := o.resolveRole(map[string]any{
		"teploy_role": "editor",
		"groups":      []any{"teploy-admins"},
	})
	if got != RoleEditor {
		t.Fatalf("direct claim: got %q, want editor", got)
	}
}

func TestResolveRoleUnknownClaimFallsThroughToGroups(t *testing.T) {
	o := newTestOIDC()
	got := o.resolveRole(map[string]any{
		"teploy_role": "superuser", // not a canonical role
		"groups":      []any{"teploy-editors"},
	})
	if got != RoleEditor {
		t.Fatalf("fallthrough to groups: got %q, want editor", got)
	}
}

func TestResolveRoleGroupPrecedence(t *testing.T) {
	o := newTestOIDC()
	// admin > editor > viewer regardless of order in the claim.
	got := o.resolveRole(map[string]any{
		"groups": []any{"teploy-viewers", "teploy-editors", "teploy-admins"},
	})
	if got != RoleAdmin {
		t.Fatalf("group precedence: got %q, want admin", got)
	}
}

func TestResolveRoleDefaultWhenNothingMatches(t *testing.T) {
	o := newTestOIDC()
	got := o.resolveRole(map[string]any{
		"groups": []any{"some-unrelated-group"},
	})
	if got != RoleViewer {
		t.Fatalf("default role: got %q, want viewer", got)
	}
}

func TestResolveRoleEmptyConfiguredGroupNeverMatches(t *testing.T) {
	o := newTestOIDC()
	o.adminGroup = "" // not configured
	// An empty group value in the token must not match an unconfigured admin group.
	got := o.resolveRole(map[string]any{
		"groups": []any{""},
	})
	if got != RoleViewer {
		t.Fatalf("empty group must not escalate: got %q, want viewer", got)
	}
}

func TestResolveUsernamePriority(t *testing.T) {
	o := newTestOIDC()
	if got := o.resolveUsername(map[string]any{"preferred_username": "jane", "email": "j@x", "sub": "abc"}); got != "jane" {
		t.Fatalf("preferred_username: got %q", got)
	}
	if got := o.resolveUsername(map[string]any{"email": "j@x", "sub": "abc"}); got != "j@x" {
		t.Fatalf("email fallback: got %q", got)
	}
	if got := o.resolveUsername(map[string]any{"sub": "abc"}); got != "abc" {
		t.Fatalf("sub fallback: got %q", got)
	}
	if got := o.resolveUsername(map[string]any{}); got != "" {
		t.Fatalf("no claim: got %q, want empty", got)
	}
}

func TestClaimStrings(t *testing.T) {
	if got := claimStrings([]any{"a", "b", 3, "c"}); len(got) != 3 {
		t.Fatalf("[]any of mixed types: got %v", got)
	}
	if got := claimStrings("solo"); len(got) != 1 || got[0] != "solo" {
		t.Fatalf("string: got %v", got)
	}
	if got := claimStrings([]string{"x", "y"}); len(got) != 2 {
		t.Fatalf("[]string: got %v", got)
	}
	if got := claimStrings(nil); got != nil {
		t.Fatalf("nil: got %v", got)
	}
}

func TestKnownRole(t *testing.T) {
	for _, in := range []string{"admin", "ADMIN", " editor ", "viewer"} {
		if _, ok := knownRole(in); !ok {
			t.Fatalf("expected %q to be a known role", in)
		}
	}
	if _, ok := knownRole("root"); ok {
		t.Fatal("root must not be a known role")
	}
}

func TestSanitizeNext(t *testing.T) {
	cases := map[string]string{
		"/apps":               "/apps",
		"/apps?x=1":           "/apps?x=1",
		"//evil.com":          "/",
		"https://evil.com":    "/",
		"":                    "/",
		"javascript:alert(1)": "/",
	}
	for in, want := range cases {
		if got := sanitizeNext(in); got != want {
			t.Fatalf("sanitizeNext(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestParseOIDCScopes(t *testing.T) {
	if got := parseOIDCScopes(""); len(got) != 3 || got[0] != "openid" {
		t.Fatalf("default scopes: got %v", got)
	}
	got := parseOIDCScopes("email, groups profile")
	// openid is always prepended; no duplicates.
	if got[0] != "openid" {
		t.Fatalf("openid must lead: got %v", got)
	}
	seen := map[string]int{}
	for _, s := range got {
		seen[s]++
	}
	for s, n := range seen {
		if n != 1 {
			t.Fatalf("scope %q appeared %d times: %v", s, n, got)
		}
	}
}

func TestFlowStoreOneTimeUse(t *testing.T) {
	o := newTestOIDC()
	o.storeFlow("state1", &oidcFlow{nonce: "n", verifier: "v", next: "/", exp: time.Now().Add(time.Minute)})
	if _, ok := o.takeFlow("state1"); !ok {
		t.Fatal("first take should succeed")
	}
	if _, ok := o.takeFlow("state1"); ok {
		t.Fatal("second take must fail (one-time use)")
	}
}

func TestFlowStoreRejectsExpired(t *testing.T) {
	o := newTestOIDC()
	o.storeFlow("stale", &oidcFlow{nonce: "n", exp: time.Now().Add(-time.Second)})
	if _, ok := o.takeFlow("stale"); ok {
		t.Fatal("expired flow must not be usable")
	}
}

func TestNewOIDCAuthDisabledWithoutIssuer(t *testing.T) {
	t.Setenv("TEPLOY_DASH_OIDC_ISSUER", "")
	t.Setenv("TEPLOY_DASH_OIDC_CLIENT_ID", "abc")
	if newOIDCAuth() != nil {
		t.Fatal("OIDC must be disabled without an issuer")
	}
}

func TestNewOIDCAuthDefaults(t *testing.T) {
	t.Setenv("TEPLOY_DASH_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("TEPLOY_DASH_OIDC_CLIENT_ID", "dash")
	t.Setenv("TEPLOY_DASH_OIDC_CLIENT_SECRET", "shh")
	t.Setenv("TEPLOY_DASH_OIDC_DEFAULT_ROLE", "")
	o := newOIDCAuth()
	if o == nil {
		t.Fatal("OIDC should be enabled")
	}
	if o.roleClaim != "teploy_role" {
		t.Fatalf("default role claim: got %q", o.roleClaim)
	}
	if o.defaultRole != RoleViewer {
		t.Fatalf("default role must be viewer (least privilege): got %q", o.defaultRole)
	}
	if o.label != "Single sign-on" {
		t.Fatalf("default label: got %q", o.label)
	}
}

// A06: backslash and control-character redirect targets must be rejected —
// browsers normalize '/\host/path' to an external origin despite the leading
// single slash.
func TestSanitizeNextRejectsBackslashExternalTargets(t *testing.T) {
	for _, bad := range []string{
		"/\\evil.example/path",
		"//evil.example",
		"https://evil.example",
		"/%5cevil.example",
		"/\n/evil",
		"bad",
	} {
		if got := sanitizeNext(bad); got != "/" {
			t.Errorf("sanitizeNext(%q) = %q, want /", bad, got)
		}
	}
	if got := sanitizeNext("/apps/x?tab=logs#end"); got != "/apps/x?tab=logs#end" {
		t.Errorf("legitimate path rewritten: %q", got)
	}
}

// A08: an allowlist decision keyed on the email claim requires a verified
// email; an unverified allowed-domain address must not pass.
func TestOIDCAllowedRequiresVerifiedEmail(t *testing.T) {
	o := &oidcAuth{
		allowedEmails:  map[string]bool{"ops@example.com": true},
		allowedDomains: []string{"example.com"},
	}
	if !o.allowed(map[string]any{"email": "ops@example.com", "email_verified": true}) {
		t.Error("verified allowed email should pass")
	}
	if o.allowed(map[string]any{"email": "attacker@example.com", "email_verified": false}) {
		t.Error("unverified allowed-domain email passed the allowlist")
	}
	if o.allowed(map[string]any{"email": "ops@example.com"}) {
		t.Error("missing email_verified claim passed the allowlist")
	}
	// No allowlist configured: every IdP-authenticated identity passes.
	open := &oidcAuth{}
	if !open.allowed(map[string]any{"email": "anyone@anywhere"}) {
		t.Error("open instance rejected an authenticated identity")
	}
}
