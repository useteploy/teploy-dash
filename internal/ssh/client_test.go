package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// A17: an existing but malformed trust store must fail closed — never fall
// back to accepting new keys.
func TestHostKeyCallback_MalformedTrustStoreFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte("this is not known_hosts syntax \x00\x01"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEPLOY_SSH_KNOWN_HOSTS", path)

	if _, err := hostKeyCallback(); err == nil {
		t.Fatal("expected an error for a malformed known_hosts")
	}
}

// A17: trust-on-first-use applies only while the file is genuinely absent;
// once a key is recorded, a different key of the same type is a mismatch.
func TestAcceptNewHostKeyCallback_TOFUMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	cb := acceptNewHostKeyCallback(path)
	remote := &net.TCPAddr{IP: net.ParseIP("10.0.0.9"), Port: 22}

	first := testHostKey(t)
	if err := cb("server.test:22", remote, first); err != nil {
		t.Fatalf("first-use acceptance failed: %v", err)
	}
	// Recorded and matching now verifies clean.
	if err := cb("server.test:22", remote, first); err != nil {
		t.Fatalf("recorded key rejected: %v", err)
	}
	// A different key of the same type = possible MITM.
	other := testHostKey(t)
	if err := cb("server.test:22", remote, other); err == nil {
		t.Fatal("key mismatch accepted")
	}
}

// A17: an unknown key whose trust record cannot persist is refused, not
// silently accepted.
func TestAcceptNewHostKeyCallback_RecordFailureFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission-based failure does not trigger")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	path := filepath.Join(dir, "known_hosts")

	cb := acceptNewHostKeyCallback(path)
	err := cb("server.test:22", &net.TCPAddr{}, testHostKey(t))
	if err == nil {
		t.Fatal("unpersistable trust record was accepted")
	}
}

// A17 (regression shape): with an existing, valid trust store the strict
// knownhosts callback is used — an unknown host is rejected there too.
func TestHostKeyCallback_ExistingStoreIsStrict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	key := testHostKey(t)
	line := knownhosts.Line([]string{"server.test"}, key)
	if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEPLOY_SSH_KNOWN_HOSTS", path)

	cb, err := hostKeyCallback()
	if err != nil {
		t.Fatal(err)
	}
	remote := &net.TCPAddr{IP: net.ParseIP("10.0.0.9"), Port: 22}
	if err := cb("server.test:22", remote, key); err != nil {
		t.Fatalf("recorded key rejected: %v", err)
	}
	unknown := testHostKey(t)
	if err := cb("server.test:22", remote, unknown); err == nil {
		t.Fatal("unknown key accepted by the strict callback")
	}
	// Sanity: errors.Is plumbing works for mismatch classification.
	var keyErr *knownhosts.KeyError
	if !errors.As(cb("server.test:22", remote, unknown), &keyErr) {
		t.Fatal("mismatch is not a knownhosts.KeyError")
	}
}

// F012: one normalization for probe + dial. An explicit host:port must keep
// its port (the old probe wrapped it in another :22), and every accepted
// shape must round to a dialable host:port.
func TestNormalizeAddress(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.test", "example.test:22"},
		{"example.test:2222", "example.test:2222"},
		{"Example.Test:2222", "example.test:2222"},
		{"10.0.0.9", "10.0.0.9:22"},
		{"10.0.0.9:2222", "10.0.0.9:2222"},
		{"::1", "[::1]:22"},
		{"[::1]", "[::1]:22"},
		{"[::1]:2222", "[::1]:2222"},
	}
	for _, tc := range cases {
		got, err := NormalizeAddress(tc.in)
		if err != nil {
			t.Fatalf("NormalizeAddress(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", " ", "host test", "host/name", "a@b", "host:", ":22", "host:0", "host:70000", "host:abc", "[broken", "example.test:2222:3333"} {
		if got, err := NormalizeAddress(bad); err == nil {
			t.Fatalf("NormalizeAddress(%q) = %q, want error", bad, got)
		}
	}
}

// F009: capture respects the byte budget and reports overflow instead of
// returning a silently truncated payload.
func TestLimitedWriterOverflow(t *testing.T) {
	var w limitedWriter
	w.limit = 8
	if _, err := w.Write([]byte("12345678")); err != nil || w.overflow {
		t.Fatalf("in-budget write failed: %v overflow=%v", err, w.overflow)
	}
	if _, err := w.Write([]byte("9")); err != nil {
		t.Fatalf("over-budget write errored: %v", err)
	}
	if !w.overflow {
		t.Fatal("overflow not recorded")
	}
	if w.buf.String() != "12345678" {
		t.Fatalf("captured %q", w.buf.String())
	}
}
