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
