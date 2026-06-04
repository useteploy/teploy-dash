package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
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

	signers, err := loadSigners(keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading SSH keys: %w", err)
	}
	if len(signers) == 0 {
		return nil, fmt.Errorf("no SSH keys found; set TEPLOY_SSH_KEY or place a key at ~/.ssh/id_ed25519")
	}

	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		// Host keys are intentionally not verified. teploy-dash is designed to
		// run on a trusted/private network (e.g. a Tailscale mesh) where the
		// transport is already authenticated and encrypted, and the fleet's dev
		// servers are frequently re-provisioned — strict known_hosts checking
		// would refuse the changed key on every rebuild. If dash is ever pointed
		// at servers over an untrusted network, this is the seam to add opt-in
		// known_hosts verification.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec — trusted-network assumption, see comment
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

func loadSigners(keyPath string) ([]ssh.Signer, error) {
	var paths []string
	if keyPath != "" {
		paths = []string{keyPath}
	} else if env := os.Getenv("TEPLOY_SSH_KEY"); env != "" {
		paths = []string{env}
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		paths = []string{
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_rsa"),
		}
	}

	var signers []ssh.Signer
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			continue // skip passphrase-protected keys in daemon mode
		}
		signers = append(signers, signer)
	}
	return signers, nil
}
