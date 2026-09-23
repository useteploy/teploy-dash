package source

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Push parsing mirrors teploy-cli's autodeploy.PushCommit contract (C02
// commit pinning): GitHub/Gitea/Forgejo use after; GitLab prefers
// checkout_sha; tags, deletions (all-zero after), explicit nulls, and
// malformed hashes pin NOTHING (deploy-the-tip is the CLI's path, dash only
// records authenticated commits). The event type is normalized from the
// forge's delivery headers.
func TestNormalizeEvent(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"push", "push"},
		{"Push Hook", "push"},
		{"ping", "ping"},
		{"pull_request", "pull_request"},
		{"Pull Request", "pull_request"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := NormalizeEvent(tc.raw); got != tc.want {
			t.Fatalf("NormalizeEvent(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestParsePush(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		branch string
		commit string
	}{
		{"github push", `{"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567"}`, "main", "0123456789abcdef0123456789abcdef01234567"},
		{"gitlab push checkout_sha", `{"ref":"refs/heads/main","checkout_sha":"ffffffffffffffffffffffffffffffffffffffff","after":"0123456789abcdef0123456789abcdef01234567"}`, "main", "ffffffffffffffffffffffffffffffffffffffff"},
		{"gitlab push after fallback", `{"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567","checkout_sha":null}`, "main", "0123456789abcdef0123456789abcdef01234567"},
		{"sha256 commit", `{"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`, "main", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"tag push", `{"ref":"refs/tags/v1.0.0","after":"0123456789abcdef0123456789abcdef01234567"}`, "v1.0.0", ""},
		{"branch deletion", `{"ref":"refs/heads/gone","after":"0000000000000000000000000000000000000000"}`, "gone", ""},
		{"malformed hash", `{"ref":"refs/heads/main","after":"not-a-hash"}`, "main", ""},
		{"missing after", `{"ref":"refs/heads/main"}`, "main", ""},
		{"no ref (ping/PR shapes)", `{"action":"opened","number":1}`, "", ""},
		{"not json", `nope`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			branch, commit := ParsePush([]byte(tc.body))
			if branch != tc.branch || commit != tc.commit {
				t.Fatalf("ParsePush = (%q, %q), want (%q, %q)", branch, commit, tc.branch, tc.commit)
			}
		})
	}
}

// The delivery ledger: append is durable, fold dedupes by delivery id, a
// torn tail is dropped, mid-file corruption fails loudly, and REFUSED
// deliveries (admission refused — nothing admitted) must NOT join the seen
// index so the forge's retry re-runs admission instead of being swallowed
// as a duplicate (C02's rollback rule).
func TestDeliveryLedger(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app"})
	if err != nil {
		t.Fatal(err)
	}
	first := Delivery{ID: "d-1", Event: "push", Branch: "main", Commit: "a", Disposition: "admitted", ReceivedAt: time.Now().UTC()}
	if err := store.RecordDelivery(created.ID, first); err != nil {
		t.Fatal(err)
	}
	if !store.DeliverySeen(created.ID, "d-1") {
		t.Fatal("admitted delivery must be seen")
	}
	if store.DeliverySeen(created.ID, "d-2") {
		t.Fatal("unknown delivery must not be seen")
	}
	refused := Delivery{ID: "d-2", Event: "push", Branch: "main", Commit: "b", Disposition: "refused", Reason: "target admission budget exceeded", ReceivedAt: time.Now().UTC()}
	if err := store.RecordDelivery(created.ID, refused); err != nil {
		t.Fatal(err)
	}
	if store.DeliverySeen(created.ID, "d-2") {
		t.Fatal("a refused delivery must not dedupe its retry")
	}

	// Restart: the fold restores the same index.
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.DeliverySeen(created.ID, "d-1") {
		t.Fatal("seen index must survive restart")
	}
	if reopened.DeliverySeen(created.ID, "d-2") {
		t.Fatal("refused delivery must stay retryable after restart")
	}
	recent := reopened.RecentDeliveries(created.ID, 10)
	if len(recent) != 2 || recent[0].ID != "d-2" || recent[1].ID != "d-1" {
		t.Fatalf("recent deliveries (newest first) = %#v", recent)
	}
}

func TestDeliveryLedgerTornTailDropped(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, cerr := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app"})
	if cerr != nil {
		t.Fatal(cerr)
	}
	if err := store.RecordDelivery(created.ID, Delivery{ID: "d-1", Disposition: "admitted", ReceivedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dir(created.ID), "deliveries.jsonl")
	if err := os.WriteFile(path, append(mustRead(t, path), []byte(`{"id":"d-2","disposition":"adm`)...), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.DeliverySeen(created.ID, "d-1") {
		t.Fatal("complete records must survive a torn tail")
	}
	if reopened.DeliverySeen(created.ID, "d-2") {
		t.Fatal("a torn record was never acked; it must not count as seen")
	}
}

func TestDeliveryLedgerCorruptMiddleFailsLoud(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, cerr := store.Create(CreateInput{Forge: ForgeGitHub, CloneURL: "https://github.com/team/app"})
	if cerr != nil {
		t.Fatal(cerr)
	}
	if err := store.RecordDelivery(created.ID, Delivery{ID: "d-1", Disposition: "admitted", ReceivedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dir(created.ID), "deliveries.jsonl")
	if err := os.WriteFile(path, []byte("garbage not json\n"+string(mustRead(t, path))), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("mid-file corruption must fail loudly, not silently drop history")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
