package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validManifest = "app: web\nimage: example/web:1\ndomain: web.example.com\n"

func TestStoreRejectsPathTraversal(t *testing.T) {
	dataDir := t.TempDir()
	store, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range [][2]string{{"../outside", "web"}, {"prod", "../../outside"}, {"/tmp", "web"}, {"prod", ".."}} {
		if _, _, err := store.Put(identity[0], identity[1], Update{Mode: ModeDashManaged, Manifest: validManifest}); err == nil {
			t.Fatalf("accepted identity %q/%q", identity[0], identity[1])
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "outside")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path traversal created outside path: %v", err)
	}
}

func TestAtomicImmutableRevisionsAndOptimisticConflict(t *testing.T) {
	dataDir := t.TempDir()
	store, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	first, created, err := store.Put("prod", "web", Update{Mode: ModeDashManaged, Manifest: validManifest})
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	wrong := strings.Repeat("0", 64)
	if _, _, err := store.Put("prod", "web", Update{Mode: ModeDashManaged, Manifest: strings.Replace(validManifest, ":1", ":2", 1), ExpectedRevision: &wrong}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting update error = %v", err)
	}
	current, err := store.Get("prod", "web")
	if err != nil || current.CurrentRevision != first.CurrentRevision || current.Manifest != validManifest {
		t.Fatalf("conflict changed current manifest: document=%+v err=%v", current, err)
	}

	expected := first.CurrentRevision
	secondContent := strings.Replace(validManifest, ":1", ":2", 1)
	second, created, err := store.Put("prod", "web", Update{Mode: ModeDashManaged, Manifest: secondContent, ExpectedRevision: &expected})
	if err != nil || created || second.CurrentRevision == first.CurrentRevision {
		t.Fatalf("update: document=%+v created=%v err=%v", second, created, err)
	}
	firstSnapshot, err := store.Export("prod", "web", first.CurrentRevision)
	if err != nil || string(firstSnapshot) != validManifest {
		t.Fatalf("immutable first snapshot changed: %q err=%v", firstSnapshot, err)
	}
	history, err := store.History("prod", "web")
	if err != nil || len(history) != 2 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "manifests", "prod", "web", "revisions"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("revision directories=%v err=%v", entries, err)
	}
}

func TestManifestSecretRejectionAndReferences(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secretCases := []string{
		"app: web\nimage: web:1\ndomain: web.example.com\nenv:\n  DB_PASSWORD: hunter2\n",
		"app: web\nimage: web:1\ndomain: web.example.com\nenv:\n  SECRET: hunter2\n",
		"app: web\nimage: web:1\ndomain: web.example.com\naudit:\n  token: ghp_verysecret\n",
		"app: web\nimage: web:1\ndomain: web.example.com\nenv:\n  VALUE: https://user:pass@example.com/db\n",
		"app: web\nimage: web:1\ndomain: web.example.com\nenv:\n  KEY: '-----BEGIN PRIVATE KEY-----abc'\n",
	}
	for _, content := range secretCases {
		if _, _, err := store.Put("prod", "web", Update{Mode: ModeDashManaged, Manifest: content}); !errors.Is(err, ErrSecrets) {
			t.Fatalf("secret manifest error = %v for %q", err, content)
		}
	}
	reference := "app: web\nimage: web:1\ndomain: web.example.com\nenv:\n  DB_PASSWORD: secret:db#password\n"
	if _, _, err := store.Put("prod", "web", Update{Mode: ModeDashManaged, Manifest: reference}); err != nil {
		t.Fatalf("secret reference rejected: %v", err)
	}
	provider := "app: web\nimage: web:1\ndomain: web.example.com\nsecret:\n  provider: openbao\n  accessory: openbao\n"
	expected := documentRevision(t, store, "prod", "web")
	if _, _, err := store.Put("prod", "web", Update{Mode: ModeDashManaged, Manifest: provider, ExpectedRevision: &expected}); err != nil {
		t.Fatalf("secret provider config rejected: %v", err)
	}
}

func documentRevision(t *testing.T, store *Store, server, app string) string {
	t.Helper()
	document, err := store.Get(server, app)
	if err != nil {
		t.Fatal(err)
	}
	return document.CurrentRevision
}

func TestRestartPersistenceAndSafeDelete(t *testing.T) {
	dataDir := t.TempDir()
	store, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := store.Put("prod", "web", Update{Mode: ModeDashManaged, Manifest: validManifest})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Get("prod", "web")
	if err != nil || loaded.CurrentRevision != document.CurrentRevision || loaded.Manifest != validManifest {
		t.Fatalf("reopened document=%+v err=%v", loaded, err)
	}
	deployedSentinel := filepath.Join(dataDir, "deployments", "web", "state.json")
	if err := os.MkdirAll(filepath.Dir(deployedSentinel), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deployedSentinel, []byte("deployed"), 0600); err != nil {
		t.Fatal(err)
	}
	expected := loaded.CurrentRevision
	if err := reopened.Delete("prod", "web", &expected); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(deployedSentinel); err != nil || string(data) != "deployed" {
		t.Fatalf("delete touched deployed resource: %q err=%v", data, err)
	}
	if _, err := reopened.Get("prod", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted manifest get error = %v", err)
	}
}
