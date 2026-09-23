package operation

import (
	"context"
	"errors"
	"testing"
	"time"
)

// D02 idempotency namespacing (A12/A13/R20): the idempotency key is scoped
// to the PRINCIPAL that submitted it. Two principals using the same key are
// two independent operations — one must never receive the other's operation.
// The same principal keeps today's dedupe/conflict semantics.

func TestIdempotencyKeyNamespacedByPrincipal(t *testing.T) {
	manager := newTestManager(t, t.TempDir(), 100, func(context.Context, Command, func(Stream, string)) (int, error) {
		return 0, nil
	})
	alice := &Actor{Kind: "local", Subject: "alice", Label: "alice"}
	bob := &Actor{Kind: "local", Subject: "bob", Label: "bob"}
	req := deployRequest("web", "example/web:1")

	first, replayed, err := manager.Enqueue(req, "shared-key", alice)
	if err != nil || replayed {
		t.Fatalf("alice's enqueue: replayed=%v err=%v", replayed, err)
	}
	// DIFFERENT principal, same key, same request: an independent operation,
	// never a replay of alice's.
	second, replayed, err := manager.Enqueue(req, "shared-key", bob)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || second.ID == first.ID {
		t.Fatalf("different principals sharing a key must be independent operations: first=%s second=%s replayed=%v", first.ID, second.ID, replayed)
	}
	// SAME principal, same key, same request: dedupe as before.
	again, replayed, err := manager.Enqueue(req, "shared-key", alice)
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("same principal replay: first=%s again=%s replayed=%v err=%v", first.ID, again.ID, replayed, err)
	}
	// SAME principal, same key, different request: conflict as before.
	if _, _, err := manager.Enqueue(deployRequest("web", "example/web:2"), "shared-key", alice); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same-principal conflicting request error = %v, want ErrIdempotencyConflict", err)
	}
	waitForStatus(t, manager, first.ID, StatusSucceeded)
	waitForStatus(t, manager, second.ID, StatusSucceeded)
}

// The namespace must survive a restart: the principal is persisted alongside
// the key on the operation record, and dedupe resolves from records on the
// restarted manager within the idempotency window.
func TestIdempotencyNamespacingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	executor := func(context.Context, Command, func(Stream, string)) (int, error) {
		return 0, nil
	}
	alice := &Actor{Kind: "local", Subject: "alice", Label: "alice"}
	manager := newTestManager(t, dir, 100, executor)
	admitted, _, err := manager.Enqueue(deployRequest("web", "example/web:1"), "restart-key", alice)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, manager, admitted.ID, StatusSucceeded)

	reopened := newTestManager(t, dir, 100, executor)
	replayed, replay, err := reopened.Enqueue(deployRequest("web", "example/web:1"), "restart-key", alice)
	if err != nil || !replay || replayed.ID != admitted.ID {
		t.Fatalf("same-principal replay after restart: admitted=%s replayed=%s replay=%v err=%v", admitted.ID, replayed.ID, replay, err)
	}
	other, replay, err := reopened.Enqueue(deployRequest("web", "example/web:1"), "restart-key", &Actor{Kind: "local", Subject: "bob", Label: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if replay || other.ID == admitted.ID {
		t.Fatalf("a different principal's key after restart must be a NEW operation: admitted=%s other=%s replay=%v", admitted.ID, other.ID, replay)
	}
	waitForStatus(t, reopened, other.ID, StatusSucceeded)
}

// --no-auth installs (and manager-internal callers) have no session actor;
// they share one explicit local principal, so their keys dedupe against
// EACH OTHER but never against an authenticated principal's keys.
func TestIdempotencyNoAuthScopeIsItsOwnPrincipal(t *testing.T) {
	manager := newTestManager(t, t.TempDir(), 100, func(context.Context, Command, func(Stream, string)) (int, error) {
		return 0, nil
	})
	req := deployRequest("web", "example/web:1")
	anonymous, replayed, err := manager.Enqueue(req, "noauth-key", nil)
	if err != nil || replayed {
		t.Fatalf("nil-actor enqueue: replayed=%v err=%v", replayed, err)
	}
	replay, replayed, err := manager.Enqueue(req, "noauth-key", nil)
	if err != nil || !replayed || replay.ID != anonymous.ID {
		t.Fatalf("nil-actor replay: anonymous=%s replay=%s replayed=%v err=%v", anonymous.ID, replay.ID, replayed, err)
	}
	authed, replayed, err := manager.Enqueue(req, "noauth-key", &Actor{Kind: "local", Subject: "alice", Label: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if replayed || authed.ID == anonymous.ID {
		t.Fatalf("an authenticated principal must not share the nil-actor namespace: anonymous=%s authed=%s replayed=%v", anonymous.ID, authed.ID, replayed)
	}
	waitForStatus(t, manager, anonymous.ID, StatusSucceeded)
	waitForStatus(t, manager, authed.ID, StatusSucceeded)
	if got := IdempotencyScope(nil); got != NoAuthScope {
		t.Fatalf("IdempotencyScope(nil) = %q, want the explicit %q principal", got, NoAuthScope)
	}
	if got := IdempotencyScope(&Actor{Kind: "sso", Subject: "oidc:abc:def"}); got != "sso:oidc:abc:def" {
		t.Fatalf("IdempotencyScope(sso actor) = %q", got)
	}
}

// The idempotency window bounds how long a key is honored (measured from the
// admitted operation's creation). Within the window: replay. Past it: the key
// is reusable for new work, both live and after a restart.
func TestIdempotencyWindowExpiresKeys(t *testing.T) {
	dir := t.TempDir()
	executor := func(context.Context, Command, func(Stream, string)) (int, error) {
		return 0, nil
	}
	newManager := func() *Manager {
		t.Helper()
		manager, err := New(dir, Options{
			Resolver:          testResolver,
			Executor:          executor,
			IdempotencyWindow: 50 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	alice := &Actor{Kind: "local", Subject: "alice", Label: "alice"}
	req := deployRequest("web", "example/web:1")

	manager := newManager()
	first, replayed, err := manager.Enqueue(req, "window-key", alice)
	if err != nil || replayed {
		t.Fatalf("first enqueue: replayed=%v err=%v", replayed, err)
	}
	within, replayed, err := manager.Enqueue(req, "window-key", alice)
	if err != nil || !replayed || within.ID != first.ID {
		t.Fatalf("within-window replay: first=%s within=%s replayed=%v err=%v", first.ID, within.ID, replayed, err)
	}
	waitForStatus(t, manager, first.ID, StatusSucceeded)
	time.Sleep(120 * time.Millisecond)
	after, replayed, err := manager.Enqueue(req, "window-key", alice)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || after.ID == first.ID {
		t.Fatalf("key past the window must be reusable: first=%s after=%s replayed=%v", first.ID, after.ID, replayed)
	}
	waitForStatus(t, manager, after.ID, StatusSucceeded)

	// A restart must not resurrect a key whose operation is past the window.
	// Wait past the window anchored at AFTER's creation too — the newest
	// admission is the entry that would be restored.
	time.Sleep(120 * time.Millisecond)
	reopened := newManager()
	fresh, replayed, err := reopened.Enqueue(req, "window-key", alice)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || fresh.ID == first.ID || fresh.ID == after.ID {
		t.Fatalf("restart restore must skip expired keys: fresh=%s replayed=%v", fresh.ID, replayed)
	}
	waitForStatus(t, reopened, fresh.ID, StatusSucceeded)
}
