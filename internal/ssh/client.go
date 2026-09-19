package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	// stopCancellation detaches the connection-lifetime cancellation watcher
	// (F007): the watcher must live as long as the CLIENT owns the
	// connection, not just until Connect returns, or a cancellation landing
	// after setup leaves a session that can no longer be unblocked.
	stopCancellation func()
	closeOnce        sync.Once
}

// maxRunOutput bounds what Client.Run captures per stream (F009): a broken
// or compromised remote that streams forever must not grow dashboard memory
// without bound. Structured state reads are kilobytes; these ceilings are
// orders of magnitude above legitimate output.
const (
	maxRunStdout = 4 << 20
	maxRunStderr = 1 << 20
)

// ErrOutputLimit reports that a remote command produced more output than the
// capture budget allows; the truncated payload is not a valid machine
// response and must not be parsed as one.
var ErrOutputLimit = errors.New("remote output exceeded the capture limit")

// limitedWriter captures up to limit bytes and records overflow.
type limitedWriter struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.limit {
		w.overflow = true
		room := w.limit - w.buf.Len()
		if room > 0 {
			w.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

// NormalizeAddress validates a configured SSH endpoint and returns it in
// host:port form (port 22 defaulted). DNS names, IPv4, raw IPv6, and
// bracketed IPv6 with or without an explicit port are accepted; anything
// else is an error. One shared normalization serves dialing, reachability
// probing, identity, and logging so they can never disagree (F012).
func NormalizeAddress(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "/\\\t\r\n @") {
		return "", errors.New("invalid SSH address")
	}
	if ip, err := netip.ParseAddr(raw); err == nil {
		return net.JoinHostPort(ip.String(), "22"), nil
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		ip, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]"))
		if err != nil {
			return "", errors.New("invalid IPv6 address")
		}
		return net.JoinHostPort(ip.String(), "22"), nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		if strings.ContainsAny(raw, ":[]") {
			return "", errors.New("invalid host:port")
		}
		host, port = raw, "22"
	}
	n, err := strconv.Atoi(port)
	if host == "" || err != nil || n < 1 || n > 65535 {
		return "", errors.New("invalid SSH endpoint")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(n)), nil
}

// Connect establishes an SSH connection to the given host.
// keyPath is optional — falls back to ~/.ssh/id_ed25519 and ~/.ssh/id_rsa.
func Connect(ctx context.Context, host, user, keyPath string) (*Client, error) {
	if user == "" {
		user = "root"
	}
	// F012: normalize once (bare IPv6, bracketed IPv6, explicit port,
	// hostname) — the reachability probe and the dialer previously derived
	// the address independently and disagreed for host:port endpoints.
	addr, err := NormalizeAddress(host)
	if err != nil {
		return nil, err
	}

	signers, encryptedFound, err := loadSigners(keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading SSH keys: %w", err)
	}

	var auth []ssh.AuthMethod
	// ssh-agent is the correct way to use encrypted keys non-interactively in
	// a daemon: when SSH_AUTH_SOCK is set, offer the agent's keys. The agent
	// connection is owned by this function and closed once the handshake is
	// done — previously every attempt leaked the unix socket (A18). The agent
	// socket dial is deadline-bounded so a wedged agent can't stall the
	// connect phase outside every other bound (A29), and F008 bounds the
	// agent's Signers/sign reads too: the dial timeout alone did not cover a
	// hung agent that accepted the connection and then never answered.
	agentMethod, agentConn, hasAgent := agentAuthMethod()
	if hasAgent {
		auth = append(auth, agentMethod)
		defer agentConn.Close()
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

	callback, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: auth,
		// Verify host keys (trust-on-first-use against a fresh file) by
		// default so a changed key on an untrusted network is detected,
		// instead of the old accept-anything. Set TEPLOY_DASH_SSH_INSECURE=1
		// to fall back to accept-all (logged).
		HostKeyCallback: callback,
	}

	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", addr, err)
	}

	// A29: ctx cancellation closes the owned connection for EVERY phase
	// after the dial (handshake, session creation, command run) — a deadline
	// on the conn alone only bounded the handshake, and a context canceled
	// mid-handshake could leave the socket (and the caller) hanging until
	// that deadline. F007: the watcher stays attached for the CLIENT's whole
	// owned lifetime (released in Close), not just until Connect returns —
	// cancellation must keep covering NewSession and streaming, which run
	// after setup with every deadline cleared.
	stop := closeOnCancel(ctx, conn)

	// Bound the handshake with a deadline (or the ctx deadline, whichever is
	// sooner). Cleared once connected so long-lived sessions aren't affected.
	hsDeadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(hsDeadline) {
		hsDeadline = d
	}
	conn.SetDeadline(hsDeadline)
	if hasAgent {
		// F008: the handshake deadline also bounds the AGENT socket —
		// Signers/sign reads during authentication ride this deadline, so a
		// hung agent cannot stall the connect phase past its bound.
		_ = agentConn.SetDeadline(hsDeadline)
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		stop()
		conn.Close()
		return nil, fmt.Errorf("SSH handshake with %s: %w", addr, err)
	}
	conn.SetDeadline(time.Time{}) // clear handshake deadline
	if hasAgent {
		_ = agentConn.SetDeadline(time.Time{})
	}

	return &Client{
		client:           ssh.NewClient(c, chans, reqs),
		host:             addr,
		stopCancellation: stop,
	}, nil
}

// closeOnCancel arranges for c to be closed the moment ctx is done, until the
// returned stop function is called on normal completion (A29).
func closeOnCancel(ctx context.Context, c io.Closer) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-done:
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// hostKeyCallback returns a known_hosts-verifying callback (trust-on-first-use
// against a fresh file), or accept-all when TEPLOY_DASH_SSH_INSECURE=1 is set.
// Mirrors the CLI's behaviour so the two products are consistent.
//
// Fail-closed policy (A17): an existing trust store that cannot be read or
// parsed is an ERROR, never permission to trust new keys — the previous
// version fell back to accept-new whenever knownhosts.New failed (malformed
// file) or Stat failed for any reason, removing server authentication
// exactly when the trust store was broken.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	path := os.Getenv("TEPLOY_SSH_KNOWN_HOSTS")
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".ssh", "known_hosts")
		}
	}
	if os.Getenv("TEPLOY_DASH_SSH_INSECURE") == "1" {
		log.Printf("[ssh] WARNING: host-key verification disabled (TEPLOY_DASH_SSH_INSECURE=1)")
		// Accept any key, but still RECORD it to known_hosts. The bundled teploy
		// CLI (used for delegate actions like logs/env/rollback) strict-checks
		// known_hosts and has no insecure mode, so without a recorded key it
		// fails with "knownhosts: key is unknown/mismatch". Recording the key
		// the dash already accepts lets the CLI reach the same servers.
		return insecureRecordingCallback(path), nil
	}
	if path == "" {
		// Can't resolve home: no durable trust store. Fail closed rather
		// than accept keys with nowhere to persist them.
		return nil, fmt.Errorf("no usable known_hosts path (HOME unset and TEPLOY_SSH_KNOWN_HOSTS not set)")
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return acceptNewHostKeyCallback(path), nil // genuinely fresh: TOFU
		}
		return nil, fmt.Errorf("checking SSH trust store %s: %w", path, err)
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("load SSH trust store %s: %w", path, err)
	}
	return cb, nil
}

// acceptNewHostKeyCallback records an unknown host key on first connect and
// errors on a genuine mismatch (same key type, different key = possible MITM).
// An unknown key is only accepted when its trust record DURABLY persists —
// a failed append is a connection error, not silent acceptance (A17).
//
// A30: the whole verify-or-enroll decision runs under a process-wide
// enrollment mutex, and the trust file is RE-READ inside it. The old
// callback was built when the file was absent and could run after the file
// changed; its verify path also skipped straight to appending whenever the
// file couldn't be parsed, and accepted a differing key for any host that
// merely lacked a key of the same algorithm. Now: every parse/verify error
// is fatal (fail closed), an unknown host (knownhosts.KeyError with an
// EMPTY want list) is the only enrollment case, and append+sync+close
// errors are joined.
func acceptNewHostKeyCallback(knownHostsPath string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		enrollMu.Lock()
		defer enrollMu.Unlock()
		if existing, err := knownhosts.New(knownHostsPath); err == nil {
			verifyErr := existing(hostname, remote, key)
			if verifyErr == nil {
				return nil
			}
			var keyErr *knownhosts.KeyError
			if !errors.As(verifyErr, &keyErr) || len(keyErr.Want) != 0 {
				// A known host with a different key (any algorithm) is a
				// mismatch, not an enrollment.
				return verifyErr
			}
			// Want is empty: the host is genuinely unknown — enroll below.
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("load SSH trust store %s: %w", knownHostsPath, err)
		}
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0700); err != nil {
			return fmt.Errorf("recording new host key (mkdir): %w", err)
		}
		f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			return fmt.Errorf("recording new host key for %s: %w", hostname, err)
		}
		_, writeErr := fmt.Fprintln(f, line)
		syncErr := f.Sync()
		closeErr := f.Close()
		return errors.Join(writeErr, syncErr, closeErr)
	}
}

// enrollMu serializes trust-store enrollment within this process (A30):
// without it, two concurrent first connections presenting DIFFERENT keys
// could both pass the "unknown" check and both be enrolled.
var enrollMu sync.Mutex

// insecureRecordingCallback accepts every host key (the container explicitly
// opted into insecure host-key handling on a trusted private mesh) but appends
// the key to known_hosts so the bundled teploy CLI — which strict-checks
// known_hosts and has no insecure mode — can reach the same servers. It
// re-reads the file each call so a key it already recorded isn't duplicated,
// and a changed key (server rebuild) is appended alongside the old one, which
// the CLI's knownhosts matcher accepts.
func insecureRecordingCallback(knownHostsPath string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if knownHostsPath == "" {
			return nil // nowhere to persist; accept this session
		}
		if cb, err := knownhosts.New(knownHostsPath); err == nil {
			if cb(hostname, remote, key) == nil {
				return nil // current key already recorded and matching
			}
		}
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		if f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644); err == nil {
			fmt.Fprintln(f, line)
			f.Close()
		}
		return nil
	}
}

// Run executes a command and returns its stdout (trimmed), bounded by the
// capture budget (F009). Overflow returns ErrOutputLimit — the truncated
// payload must not be parsed as a valid machine response.
func (c *Client) Run(ctx context.Context, cmd string) (string, error) {
	var stdout, stderr limitedWriter
	stdout.limit = maxRunStdout
	stderr.limit = maxRunStderr
	if err := c.stream(ctx, cmd, &stdout, &stderr); err != nil {
		return "", err
	}
	if stdout.overflow || stderr.overflow {
		return "", fmt.Errorf("%w (stdout=%d bytes, stderr=%d bytes)", ErrOutputLimit, stdout.buf.Len(), stderr.buf.Len())
	}
	return strings.TrimSpace(stdout.buf.String()), nil
}

// Stream executes a command and writes stdout to w line by line.
// Used for log tailing via SSE.
func (c *Client) Stream(ctx context.Context, cmd string, w io.Writer) error {
	return c.stream(ctx, cmd, w, io.Discard)
}

func (c *Client) stream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	// F007: register the cancellation watcher BEFORE NewSession and against
	// the underlying transport — a peer that completes the handshake but
	// never answers a session-open previously left NewSession blocking
	// outside every context bound (the connection watcher had already been
	// detached when Connect returned). Closing the transport is what
	// unblocks it; a session that does not exist cannot be signaled.
	stop := context.AfterFunc(ctx, func() { _ = c.client.Close() })
	defer stop()

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
		// A29: close FIRST — sending a remote signal can itself block on an
		// unresponsive peer, and the session close is what unblocks the
		// local run; the signal is best-effort after it.
		sess.Close()
		sess.Signal(ssh.SIGKILL) //nolint:errcheck
		return ctx.Err()
	}
}

// Close closes the underlying SSH connection and detaches the
// connection-lifetime cancellation watcher (F007).
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		if c.stopCancellation != nil {
			c.stopCancellation()
		}
		_ = c.client.Close()
	})
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
// is set, plus the agent connection's owner so the caller can close it once
// authentication is done — each Connect previously leaked the unix socket
// (A18). The concrete net.Conn (not io.Closer) lets the caller bound the
// agent's reads with deadlines (F008).
func agentAuthMethod() (ssh.AuthMethod, net.Conn, bool) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil, false
	}
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, nil, false
	}
	ag := agent.NewClient(conn)
	return ssh.PublicKeysCallback(ag.Signers), conn, true
}
