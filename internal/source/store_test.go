package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// Two forges with the same owner/repo are TWO sources (F03): creating both
// succeeds, they carry distinct stable IDs, and each is independently
// retrievable.
func TestCreateSameRepoOnTwoForgesAreIndependent(t *testing.T) {
	store := newTestStore(t)
	github, err := store.Create(CreateInput{
		Forge:         ForgeGitHub,
		CloneURL:      "https://github.com/team/app.git",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	forgejo, err := store.Create(CreateInput{
		Forge:         ForgeForgejo,
		CloneURL:      "https://forge.example/team/app",
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatal(err)
	}
	if github.ID == forgejo.ID {
		t.Fatalf("same owner/repo on two forges must not collide: %q", github.ID)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d sources, want 2", len(list))
	}
	got, err := store.Get(forgejo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CloneURL != "https://forge.example/team/app" || got.DefaultBranch != "trunk" {
		t.Fatalf("forgejo source = %#v", got)
	}
}

// Re-registering the SAME repository (any spelling: transport, .git, case)
// is a duplicate, not a second source.
func TestCreateDuplicateIdentityRejected(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "git@github.com:tyler/akiroo.git"}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://GITHUB.COM/Tyler/Akiroo"})
	if !IsDuplicate(err) {
		t.Fatalf("casing variant must be a duplicate, got %v", err)
	}
}

// The record NEVER carries token or secret values: the credential reference
// is an opaque string, and the webhook secret lives in its own 0600 file.
func TestRecordNeverCarriesSecretValues(t *testing.T) {
	store := newTestStore(t)
	created, err := store.Create(CreateInput{
		Forge:         ForgeForgejo,
		CloneURL:      "https://forge.example/team/app",
		CredentialRef: "vault:forgero/team-app",
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(filepath.Join(store.dir(created.ID), "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), created.WebhookSecret) {
		t.Fatal("webhook secret must not appear in metadata.json")
	}
	if !strings.Contains(string(metadata), "vault:forgero/team-app") {
		t.Fatalf("credential REFERENCE should be recorded: %s", metadata)
	}
	info, err := os.Stat(filepath.Join(store.dir(created.ID), "webhook-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("webhook secret mode = %v, want 0600", info.Mode().Perm())
	}
	secret, err := store.WebhookSecret(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secret != created.WebhookSecret || secret == "" {
		t.Fatal("secret round-trip failed")
	}
}

// Display name is mutable and separate from identity; identity fields are
// not editable.
func TestUpdateDisplayNameOnly(t *testing.T) {
	store := newTestStore(t)
	created, err := store.Create(CreateInput{
		Forge: ForgeGitHub, CloneURL: "https://github.com/team/app", DisplayName: "old name",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.Update(created.ID, UpdateInput{DisplayName: ptr("new name"), CredentialRef: ptr("vault:ref")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "new name" || updated.CredentialRef != "vault:ref" {
		t.Fatalf("update = %#v", updated)
	}
	if updated.CloneURL != created.CloneURL || updated.Forge != created.Forge {
		t.Fatalf("identity must be immutable: %#v", updated)
	}
}

// A source survives restart; delivery dedupe state survives with it.
func TestStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app"})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CloneURL != created.CloneURL {
		t.Fatalf("reopened = %#v", got)
	}
}

// Revoked-permission handling: marking degraded is visible on every read,
// carries the exact reason and a timestamp, and clearing it works.
func TestDegradedStateRoundTrip(t *testing.T) {
	store := newTestStore(t)
	created, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCredentialState(created.ID, true, "forge 403: token revoked"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(created.ID)
	if !got.Degraded || got.DegradeReason != "forge 403: token revoked" || got.DegradeCheckedAt.IsZero() {
		t.Fatalf("degraded source must be visible with the exact reason: %#v", got)
	}
	if err := store.MarkCredentialState(created.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(created.ID)
	if got.Degraded || got.DegradeReason != "" {
		t.Fatalf("recovered source must be clean: %#v", got)
	}
}

// Rotation replaces the secret atomically; the old one stops verifying.
func TestRotateWebhookSecret(t *testing.T) {
	store := newTestStore(t)
	created, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app"})
	if err != nil {
		t.Fatal(err)
	}
	old := created.WebhookSecret
	rotated, err := store.RotateWebhookSecret(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated == "" || rotated == old {
		t.Fatal("rotation must return a fresh secret")
	}
	current, _ := store.WebhookSecret(created.ID)
	if current != rotated {
		t.Fatal("rotation must replace the stored secret")
	}
}

func TestDeleteRemovesEverything(t *testing.T) {
	store := newTestStore(t)
	created, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(created.ID); !IsNotFound(err) {
		t.Fatalf("get after delete = %v", err)
	}
	// Re-creating the same identity is allowed after deletion.
	if _, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app"}); err != nil {
		t.Fatalf("re-create after delete: %v", err)
	}
}

// Creation bounds: unknown forge kinds, absurd branch names, and oversized
// display names are rejected before anything touches disk.
func TestCreateValidation(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Create(CreateInput{Forge: Forge("sourcehut"), CloneURL: "https://git.sr.ht/~u/app"}); err == nil {
		t.Fatal("unknown forge kind must be rejected")
	}
	if _, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app", DefaultBranch: "main\r\ninjection"}); err == nil {
		t.Fatal("branch with control characters must be rejected")
	}
	if _, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app", DisplayName: strings.Repeat("x", 300)}); err == nil {
		t.Fatal("oversized display name must be rejected")
	}
	if _, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app", CredentialRef: strings.Repeat("c", 300)}); err == nil {
		t.Fatal("oversized credential ref must be rejected")
	}
}

func TestValidID(t *testing.T) {
	if !ValidID("src-0123456789abcdef") {
		t.Fatal("well-formed id rejected")
	}
	for _, bad := range []string{"", "src-XYZ", "../escape", "src-0123456789abcde"} {
		if ValidID(bad) {
			t.Fatalf("ValidID(%q) = true", bad)
		}
	}
}

func ptr[T any](v T) *T { return &v }
