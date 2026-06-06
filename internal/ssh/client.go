package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	// dialTimeout bounds the TCP connect.
	dialTimeout = 10 * time.Second
	// handshakeTimeout bounds the SSH handshake — ssh.NewClientConn does not
	// honor ctx and has no built-in timeout, so without this a host that
	// accepts TCP but stalls the handshake would hang the caller (and, for
	// the dashboard, the whole fleet refresh) indefinitely.
	handshakeTimeout = 15 * time.Second
)

// Client is a minimal SSH client for teploy-dash remote operations.
// Read-only and simple ops only — no file uploads, no deploy logic.
type Client struct {
	client *ssh.Client
	host   string
}

// Connect establishes an SSH connection to the given host.
// keyPath is optional — falls back to ~/.ssh/id_ed25519 and ~/.ssh/id_rsa.
func Connect(ctx context.Context, host, user, keyPath string) (*Client, error) {
	if user == "" {
		user = "root"
	}
	if !strings.Contains(host, ":") {
		host = host + ":22"
	}

	signers, encryptedFound, err := loadSigners(keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading SSH keys: %w", err)
	}

	var auth []ssh.AuthMethod
	// ssh-agent is the correct way to use encrypted keys non-interactively in a
	// daemon: when SSH_AUTH_SOCK is set, offer the agent's keys.
	if m, ok := agentAuthMethod(); ok {
		auth = append(auth, m)
	}
	if len(signers) > 0 {
		auth = append(auth, ssh.PublicKeys(signers...))
	}
	if len(auth) == 0 {
		if encryptedFound {
			return nil, fmt.Errorf("found a passphrase-protected SSH key but no agent; start ssh-agent (SSH_AUTH_SOCK) or use an unencrypted key / TEPLOY_SSH_KEY")
		}
		return nil, fmt.Errorf("no SSH keys found; set TEPLOY_SSH_KEY or place a key at ~/.ssh/id_ed25519")
	}

	cfg := &ssh.ClientConfig{
		User: user,
		Auth: auth,
		// Verify host keys (trust-on-first-use) by default so a changed key on
		// an untrusted network is detected, instead of the old accept-anything.
		// Set TEPLOY_DASH_SSH_INSECURE=1 to fall back to accept-all (logged).
		HostKeyCallback: hostKeyCallback(),
	}

	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", host, err)
	}

	// Bound the handshake with a deadline (or the ctx deadline, whichever is
	// sooner). Cleared once connected so long-lived sessions aren't affected.
	hsDeadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(hsDeadline) {
		hsDeadline = d
	}
	conn.SetDeadline(hsDeadline)

	c, chans, reqs, err := ssh.NewClientConn(conn, host, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SSH handshake with %s: %w", host, err)
	}
	conn.SetDeadline(time.Time{}) // clear handshake deadline

	return &Client{client: ssh.NewClient(c, chans, reqs), host: host}, nil
}

// hostKeyCallback returns a known_hosts-verifying callback (trust-on-first-use),
// or accept-all when TEPLOY_DASH_SSH_INSECURE=1 is set. Mirrors the CLI's
// behaviour so the two products are consistent.
func hostKeyCallback() ssh.HostKeyCallback {
	if os.Getenv("TEPLOY_DASH_SSH_INSECURE") == "1" {
		log.Printf("[ssh] WARNING: host-key verification disabled (TEPLOY_DASH_SSH_INSECURE=1)")
		return ssh.InsecureIgnoreHostKey() //nolint:gosec — explicit opt-in
	}
	path := os.Getenv("TEPLOY_SSH_KNOWN_HOSTS")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return acceptNewHostKeyCallback("") // can't resolve home; TOFU with no file
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}
	if _, err := os.Stat(path); err != nil {
		return acceptNewHostKeyCallback(path) // record on first connect
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return acceptNewHostKeyCallback(path)
	}
	return cb
}

// acceptNewHostKeyCallback records an unknown host key on first connect and
// errors on a genuine mismatch (same key type, different key = possible MITM).
func acceptNewHostKeyCallback(knownHostsPath string) ssh.HostKeyCallback {
	var existing ssh.HostKeyCallback
	if knownHostsPath != "" {
		existing, _ = knownhosts.New(knownHostsPath)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if existing != nil {
			err := existing(hostname, remote, key)
			if err == nil {
				return nil
			}
			var keyErr *knownhosts.KeyError
			if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
				for _, want := range keyErr.Want {
					if want.Key.Type() == key.Type() {
						return err // same type, different key = real mismatch
					}
				}
			}
		}
		if knownHostsPath == "" {
			return nil // nowhere to persist; accept this session
		}
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			return nil
		}
		defer f.Close()
		fmt.Fprintln(f, line)
		return nil
	}
}

// Run executes a command and returns its combined stdout (trimmed).
func (c *Client) Run(ctx context.Context, cmd string) (string, error) {
	var buf bytes.Buffer
	if err := c.stream(ctx, cmd, &buf, io.Discard); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

// Stream executes a command and writes stdout to w line by line.
// Used for log tailing via WebSocket.
func (c *Client) Stream(ctx context.Context, cmd string, w io.Writer) error {
	return c.stream(ctx, cmd, w, io.Discard)
}

func (c *Client) stream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	sess, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("creating SSH session: %w", err)
	}
	defer sess.Close()

	sess.Stdout = stdout
	sess.Stderr = stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		sess.Signal(ssh.SIGTERM)
		sess.Close()
		return ctx.Err()
	}
}

// Close closes the underlying SSH connection.
func (c *Client) Close() {
	c.client.Close()
}

// loadSigners returns usable (unencrypted) key signers and whether any key on
// disk was passphrase-protected (so Connect can emit a clearer error pointing
// to ssh-agent rather than a misleading "no keys found").
func loadSigners(keyPath string) (signers []ssh.Signer, encryptedFound bool, err error) {
	var paths []string
	if keyPath != "" {
		paths = []string{keyPath}
	} else if env := os.Getenv("TEPLOY_SSH_KEY"); env != "" {
		paths = []string{env}
	} else {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return nil, false, herr
		}
		paths = []string{
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_rsa"),
		}
	}

	for _, p := range paths {
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		signer, perr := ssh.ParsePrivateKey(data)
		if perr != nil {
			var pp *ssh.PassphraseMissingError
			if errors.As(perr, &pp) {
				encryptedFound = true // handled via ssh-agent, not inline
			}
			continue
		}
		signers = append(signers, signer)
	}
	return signers, encryptedFound, nil
}

// agentAuthMethod returns an AuthMethod backed by ssh-agent when SSH_AUTH_SOCK
// is set, which is how a daemon uses passphrase-protected keys non-interactively.
func agentAuthMethod() (ssh.AuthMethod, bool) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, false
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, false
	}
	ag := agent.NewClient(conn)
	return ssh.PublicKeysCallback(ag.Signers), true
}
