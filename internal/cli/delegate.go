package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// cliTimeout is a generous ceiling on any delegated CLI command so a hung
// `teploy` subprocess (e.g. a stalled SSH session) can't block a dashboard
// request forever. It's well above a slow first-time deploy with a large image
// pull, so it never aborts legitimate work — it only backstops a genuine hang.
//
// It is a CEILING, not the effective deadline: a caller's context can be
// shorter and wins (the fleet refresh allows 60s). Timeout errors therefore
// report the elapsed time rather than this constant, which otherwise claimed
// every fleet timeout took 20 minutes when it had given up after one.
const cliTimeout = 20 * time.Minute

// Result holds the output of a CLI command.
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

type StreamEvent struct {
	Stream Stream
	Data   string
}

// maxStreamLine bounds one line accepted by the streaming reader (F013
// keeps the previous scanner's 1 MiB token limit).
const maxStreamLine = 1024 * 1024

// errLineTooLong marks a streamed line exceeding maxStreamLine; like the
// scanner error it replaces, it cancels the child and becomes the primary
// error (A28).
var errLineTooLong = fmt.Errorf("teploy output line exceeds %d bytes", maxStreamLine)

// lineWriter reassembles logical lines from arbitrary Write chunks and
// emits each complete line. os/exec owns the copy goroutines when it is
// installed as cmd.Stdout/Stderr, which is what lets WaitDelay engage
// (F013): waiting for externally managed pipe readers BEFORE calling
// Wait defeats the normal-exit WaitDelay, because Wait has not observed
// the exit yet.
type lineWriter struct {
	emit    func(string)
	fail    func() // cancels the child (cmd.Cancel)
	failed  error
	pending []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	consumed := 0
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		take := len(p)
		if i >= 0 {
			take = i
		}
		if len(w.pending)+take > maxStreamLine {
			w.failed = errLineTooLong
			if w.fail != nil {
				w.fail()
			}
			return consumed, w.failed
		}
		w.pending = append(w.pending, p[:take]...)
		consumed += take
		p = p[take:]
		if i < 0 {
			break
		}
		line := bytes.TrimSuffix(w.pending, []byte{'\r'})
		w.emit(string(line))
		w.pending = w.pending[:0]
		p = p[1:]
		consumed++
	}
	return consumed, nil
}

// Flush emits a final unterminated line (the scanner previously delivered
// it too). Returns the sticky failure, if any.
func (w *lineWriter) Flush() error {
	if w.failed != nil {
		return w.failed
	}
	if len(w.pending) == 0 {
		return nil
	}
	line := bytes.TrimSuffix(w.pending, []byte{'\r'})
	w.pending = nil
	w.emit(string(line))
	return nil
}

// RunStream executes an allowlisted teploy argument vector and emits stdout and
// stderr as they arrive. Cancellation terminates the subprocess process group,
// including SSH or shell descendants spawned by the CLI. stdin (when non-empty)
// is fed to the process — how secret values travel instead of the argv.
func RunStream(ctx context.Context, args []string, timeout time.Duration, onEvent func(StreamEvent)) (*Result, error) {
	return RunStreamStdin(ctx, "", args, timeout, onEvent)
}

func RunStreamStdin(ctx context.Context, stdin string, args []string, timeout time.Duration, onEvent func(StreamEvent)) (*Result, error) {
	if timeout <= 0 {
		timeout = cliTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "teploy", args...)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return terminateProcessGroup(cmd) }
	cmd.WaitDelay = 3 * time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdoutBuffer, stderrBuffer lockedBuffer
	var callbackMu sync.Mutex
	// Per-stream line writers installed as cmd.Stdout/Stderr: os/exec owns
	// the copy goroutines, complete lines reach the bounded capture and the
	// caller's callback in order, and a line over the budget cancels the
	// child and becomes the primary error (A28 semantics preserved).
	stdout := &lineWriter{
		emit: func(line string) {
			stdoutBuffer.WriteLine(line)
			if onEvent != nil {
				callbackMu.Lock()
				onEvent(StreamEvent{Stream: StreamStdout, Data: line})
				callbackMu.Unlock()
			}
		},
		fail: func() { _ = cmd.Cancel() },
	}
	stderr := &lineWriter{
		emit: func(line string) {
			stderrBuffer.WriteLine(line)
			if onEvent != nil {
				callbackMu.Lock()
				onEvent(StreamEvent{Stream: StreamStderr, Data: line})
				callbackMu.Unlock()
			}
		},
		fail: func() { _ = cmd.Cancel() },
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	// os/exec owns the pipe copy goroutines for writer-based output, so
	// calling Run immediately lets WaitDelay bound the post-exit drain when
	// a descendant inherits the pipes (F013).
	waitErr := cmd.Run()
	flushErr := errors.Join(stdout.Flush(), stderr.Flush())

	result := &Result{Stdout: stdoutBuffer.String(), Stderr: stderrBuffer.String()}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if stdout.failed != nil {
		return result, stdout.failed
	}
	if stderr.failed != nil {
		return result, stderr.failed
	}
	if flushErr != nil {
		return result, flushErr
	}
	if waitErr != nil {
		if _, ok := waitErr.(*exec.ExitError); !ok {
			return result, fmt.Errorf("teploy command failed: %w", waitErr)
		}
	}
	return result, nil
}

// lockedBuffer bounds captured output while the complete stream remains
// available through callbacks.
type lockedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *lockedBuffer) WriteLine(line string) {
	const maxCapturedOutput = 1024 * 1024
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data) >= maxCapturedOutput {
		return
	}
	remaining := maxCapturedOutput - len(b.data)
	chunk := []byte(line + "\n")
	if len(chunk) > remaining {
		chunk = chunk[:remaining]
	}
	b.data = append(b.data, chunk...)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// maxCapturedOutput bounds the stdout captured by a nonstreaming CLI call
// (A27): machine reads expect kilobytes of JSON, and an unbounded buffer let
// a runaway command hold the delegate's memory for the full 20-minute
// ceiling. Stderr gets a smaller budget.
const (
	maxStdoutCapture = 4 << 20
	maxStderrCapture = 1 << 20
)

// errOutputLimit marks a nonstreaming command whose output exceeded its
// capture budget. The overflow kills the child (a partial JSON payload must
// never be parsed as an authoritative answer) and the cause survives in the
// returned error instead of being replaced by a generic context error.
var errOutputLimit = errors.New("teploy output exceeded the capture limit")

// limitedCapture is a per-subprocess output buffer with a hard byte budget.
// Not concurrency-safe: one writer per stream, read after cmd.Wait joins the
// copy goroutines.
type limitedCapture struct {
	bytes.Buffer
	limit int
	stop  context.CancelFunc
	Err   error
}

func (b *limitedCapture) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.Err = errOutputLimit
		if b.stop != nil {
			b.stop()
		}
		return 0, errOutputLimit
	}
	return b.Buffer.Write(p)
}

// runBounded is the single nonstreaming execution primitive (A27): caller
// context, process-group termination, WaitDelay, bounded capture, and no
// argv in returned errors (A11).
func runBounded(ctx context.Context, stdin string, args ...string) (*Result, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "teploy", args...)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return terminateProcessGroup(cmd) }
	cmd.WaitDelay = 3 * time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	stdout := &limitedCapture{limit: maxStdoutCapture, stop: cancel}
	stderr := &limitedCapture{limit: maxStderrCapture, stop: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()
	result := &Result{Stdout: stdout.String(), Stderr: stderr.String()}
	switch {
	case stdout.Err != nil:
		return result, stdout.Err
	case stderr.Err != nil:
		return result, stderr.Err
	}
	if ctx.Err() != nil {
		// F014: an explicitly canceled request must surface its context
		// error even when the group kill produced an ExitError — the old
		// path normalized cancellation into an ordinary (nil-error) exit
		// result, and callers that only check err went on to report
		// success for work that never completed. Deadline expiration keeps
		// the elapsed-time message (no argv, A11).
		if ctx.Err() == context.DeadlineExceeded {
			return result, fmt.Errorf("teploy command timed out after %s", time.Since(started).Round(time.Second))
		}
		return result, ctx.Err()
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return result, fmt.Errorf("teploy command could not run: %w", err)
		}
	}
	return result, nil
}

// Run executes a teploy CLI command and returns the result.
func Run(args ...string) (*Result, error) {
	return RunContext(context.Background(), args...)
}

// RunContext executes a teploy CLI command while honoring caller cancellation.
// Machine reads use this so a canceled HTTP/fleet request also stops its CLI
// subprocess instead of waiting for the global delegate timeout.
func RunContext(ctx context.Context, args ...string) (*Result, error) {
	return runBounded(ctx, "", args...)
}

// UnsupportedCommand reports whether a completed CLI invocation failed because
// the installed teploy predates the requested command or JSON flag. Callers may
// use compatibility reads only for this case; command failures and malformed
// successful output must remain visible.
func UnsupportedCommand(result *Result) bool {
	if result == nil || result.ExitCode == 0 {
		return false
	}
	output := strings.ToLower(result.Stderr + "\n" + result.Stdout)
	for _, marker := range []string{
		"unknown command",
		"unknown flag: --json",
		"flag provided but not defined: -json",
		"no help topic",
	} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// RunWithStdin runs a teploy CLI command, feeding stdin to it, and treats a
// non-zero exit as an error. Used to pass secrets (e.g. a registry password,
// an env value, a kv value) to the CLI without putting them on the argv,
// where they'd show in the host's process list.
func RunWithStdin(ctx context.Context, stdin string, args ...string) (*Result, error) {
	result, err := runBounded(ctx, stdin, args...)
	if err != nil {
		return result, err
	}
	return result, checkExit(result)
}

// RunChecked runs a teploy CLI command and treats a non-zero exit code as an
// error (wrapping the CLI's stderr), in addition to exec failures. Use this for
// mutating commands: plain Run returns a nil error on non-zero exit, so a
// failed action would otherwise flow through a handler's success path and be
// reported to the UI as success. With RunChecked the failure rides the normal
// `if err != nil` path every handler already has.
func RunChecked(args ...string) (*Result, error) {
	result, err := Run(args...)
	if err != nil {
		return result, err
	}
	return result, checkExit(result)
}

// CheckExit is checkExit for callers that ran the CLI through an injected
// runner (Config.CLIRunner) rather than RunChecked, so they can apply the same
// non-zero-exit-is-an-error rule without giving up testability.
func CheckExit(result *Result) error {
	return checkExit(result)
}

// checkExit converts a non-zero CLI exit into an error, preferring stderr, then
// stdout, then a generic message. Split out for testability. The argv is
// deliberately absent: argument vectors can carry secret values (A11).
func checkExit(result *Result) error {
	if result.ExitCode == 0 {
		return nil
	}
	msg := strings.TrimSpace(result.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(result.Stdout)
	}
	if msg == "" {
		return fmt.Errorf("teploy exited with code %d", result.ExitCode)
	}
	return errors.New(msg)
}

// RunJSON executes a teploy CLI command with --json flag and parses output.
// Non-JSON output on a zero exit is an error, not a passthrough string: the
// caller expects a machine payload and silently returning the raw text made
// "unknown/error" indistinguishable from a real answer (A30).
func RunJSON(args ...string) (interface{}, error) {
	args = append(args, "--json")
	result, err := Run(args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("command failed: %s", result.Stderr)
	}

	var data interface{}
	if err := json.Unmarshal([]byte(result.Stdout), &data); err != nil {
		return nil, fmt.Errorf("teploy returned non-JSON output: %w", err)
	}
	return data, nil
}

// userArgs returns ["--user", user] when user is non-empty, else nil. The CLI
// defaults to root when --user is absent, so this lets delegate calls target
// non-root fleets while staying a no-op for root servers.
func userArgs(user string) []string {
	if user == "" {
		return nil
	}
	return []string{"--user", user}
}

// Deploy triggers a deploy via the CLI.
// Uses the ad-hoc deploy path (--app flag) so no teploy.yml is required.
func Deploy(server, user, app, image, domain string, port int) (*Result, error) {
	args := []string{"deploy", server, "--app", app}
	args = append(args, userArgs(user)...)
	if image != "" {
		args = append(args, "--image", image)
	}
	if domain != "" {
		args = append(args, "--domain", domain)
	}
	if port > 0 {
		args = append(args, "--port", fmt.Sprintf("%d", port))
	}
	return RunChecked(args...)
}

// Rollback triggers a rollback via the CLI.
func Rollback(server, user, app string) (*Result, error) {
	args := append([]string{"rollback", "--host", server, "--app", app}, userArgs(user)...)
	return RunChecked(args...)
}

// AppAction runs an app lifecycle action (start, stop, restart, lock, unlock).
func AppAction(server, user, app, action string) (*Result, error) {
	args := append([]string{action, "--host", server, "--app", app}, userArgs(user)...)
	return RunChecked(args...)
}

// Logs returns recent logs.
func Logs(server, user, app string, lines int) (*Result, error) {
	args := append([]string{"logs", "--host", server, "--app", app, "--tail", fmt.Sprintf("%d", lines)}, userArgs(user)...)
	return Run(args...)
}

// Status returns the current status.
func Status(server, user, app string) (interface{}, error) {
	args := append([]string{"status", "--host", server, "--app", app}, userArgs(user)...)
	return RunJSON(args...)
}

// EnvList returns environment variables.
func EnvList(server, user, app string) (interface{}, error) {
	args := append([]string{"env", "list", "--host", server, "--app", app, "--reveal"}, userArgs(user)...)
	return RunJSON(args...)
}

// EnvSet sets an environment variable. When the installed CLI supports the
// secret-stdin contract (`env set KEY --stdin`, UPSTREAM-1), the value
// travels on the process's stdin instead of the argv, where it would be
// visible to every local process (A11); an older CLI keeps the legacy argv
// path so an upgrade boundary degrades instead of breaking. F015: a probe
// that cannot establish an answer fails closed rather than silently
// selecting the argv transport.
func EnvSet(ctx context.Context, server, user, app, key, value string) (*Result, error) {
	supported, err := EnvStdinSupport()
	if err != nil {
		return nil, err
	}
	if supported {
		args := []string{"env", "set", key, "--stdin", "--host", server, "--app", app}
		args = append(args, userArgs(user)...)
		return RunWithStdin(ctx, value, args...)
	}
	args := []string{"env", "set", fmt.Sprintf("%s=%s", key, value), "--host", server, "--app", app}
	args = append(args, userArgs(user)...)
	return RunChecked(args...)
}

// EnvUnset removes an environment variable.
func EnvUnset(server, user, app, key string) (*Result, error) {
	args := append([]string{"env", "unset", key, "--host", server, "--app", app}, userArgs(user)...)
	return RunChecked(args...)
}

// AccessoryVerifyBackup runs `teploy accessory verify-backup` against a
// server's accessory. Uses plain Run (NOT RunChecked): the CLI exits non-zero
// when verification fails but still prints the structured JSON result on
// stdout — the caller parses that and treats ok=false as a result, not an
// operational error.
func AccessoryVerifyBackup(server, user, app, accessory, bucket, region string) (*Result, error) {
	args := []string{"accessory", "verify-backup", accessory,
		"--app", app, "--host", server,
		"--bucket", bucket, "--region", region, "--json"}
	args = append(args, userArgs(user)...)
	return Run(args...)
}

// ServerList returns configured servers.
func ServerList() (*Result, error) {
	return Run("server", "list", "--json")
}

// ServerAdd adds a server.
func ServerAdd(name, host, user, role string) (*Result, error) {
	args := []string{"server", "add", name, host}
	if user != "" {
		args = append(args, "--user", user)
	}
	if role != "" {
		args = append(args, "--role", role)
	}
	return RunChecked(args...)
}

// ServerRemove removes a server.
func ServerRemove(name string) (*Result, error) {
	return RunChecked("server", "remove", name)
}

// Typed server-registry errors surfaced by `server rename`/`server update`
// (UPSTREAM-2). The CLI wraps its own config.ErrServerExists /
// config.ErrServerNotFound, so dash classifies by the stable message text —
// the same single-sourcing approach kvNotSet uses.
var (
	ErrServerExists   = errors.New("server already exists")
	ErrServerNotFound = errors.New("server not found")
)

func classifyServerError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "server already exists"):
		return fmt.Errorf("%w: %w", ErrServerExists, err)
	case strings.Contains(msg, "server not found"):
		return fmt.Errorf("%w: %w", ErrServerNotFound, err)
	}
	return err
}

// ServerRename atomically renames a server entry, preserving every field of
// the original record (host, user, role, tags, vpn_ip) in one commit — the
// remove+add emulation lost metadata and could be interrupted between the
// two writes (A38 / UPSTREAM-2 adoption).
func ServerRename(oldName, newName string) (*Result, error) {
	result, err := RunChecked("server", "rename", oldName, newName)
	if err != nil {
		return result, classifyServerError(err)
	}
	return result, nil
}

// ServerUpdate atomically updates the fields named by the non-empty
// arguments; empty arguments leave that field untouched. Each write is one
// atomic file replacement (A38 / UPSTREAM-2 adoption).
func ServerUpdate(name, host, user, role string) (*Result, error) {
	args := []string{"server", "update", name}
	if host != "" {
		args = append(args, "--host", host)
	}
	if user != "" {
		args = append(args, "--user", user)
	}
	if role != "" {
		args = append(args, "--role", role)
	}
	if len(args) == 3 {
		return nil, fmt.Errorf("nothing to update")
	}
	result, err := RunChecked(args...)
	if err != nil {
		return result, classifyServerError(err)
	}
	return result, nil
}

// IsInstalled checks if the teploy CLI binary is available.
func IsInstalled() bool {
	_, err := exec.LookPath("teploy")
	return err == nil
}

// ── Secret-stdin capability probes (UPSTREAM-1 adoption) ──────────────────
//
// The CLI's stdin contract (`env set KEY --stdin`, `kv set KEY --stdin`,
// `template install --var-stdin`) removed secrets from the argv. Dash shells
// out to whatever teploy is on PATH, so each call site probes whether the
// installed CLI knows the flag and falls back to the legacy argv path when a
// VERIFIED probe says it doesn't — a hard requirement on the new flag would
// break every install at the CLI upgrade boundary.
//
// F015: only verified probe outcomes are cached. A probe that could not run
// (missing binary, timeout, non-zero help exit) is NOT cached as
// "unsupported" — a transient failure previously selected the legacy argv
// transport forever after, silently putting secret values back on the
// process list. An unverified probe now fails closed for secret-bearing
// writes: the operator retries, and no secret travels by argv.

// probeFlagVerified runs `teploy <args...> --help` and reports whether the
// flag appears in its usage text. The second return is FALSE when the probe
// itself failed (no verified answer).
func probeFlagVerified(flag string, args ...string) (supported, verified bool) {
	if !IsInstalled() {
		return false, false
	}
	result, err := Run(append(append([]string{}, args...), "--help")...)
	if err != nil || result.ExitCode != 0 {
		return false, false
	}
	return strings.Contains(result.Stdout, flag) || strings.Contains(result.Stderr, flag), true
}

// secretFlagProbe caches one flag probe's VERIFIED outcome; unverified
// attempts are retried on the next call instead of being frozen.
type secretFlagProbe struct {
	flag      string
	args      []string
	mu        sync.Mutex
	decided   bool
	supported bool
}

func (p *secretFlagProbe) check() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.decided {
		return p.supported, nil
	}
	supported, verified := probeFlagVerified(p.flag, p.args...)
	if !verified {
		return false, fmt.Errorf("cannot verify whether the installed teploy CLI supports %s (probe did not complete)", p.flag)
	}
	p.decided, p.supported = true, supported
	return supported, nil
}

var (
	envStdinProbe = &secretFlagProbe{flag: "--stdin", args: []string{"env", "set"}}
	kvStdinProbe  = &secretFlagProbe{flag: "--stdin", args: []string{"kv", "set"}}
	varStdinProbe = &secretFlagProbe{flag: "--var-stdin", args: []string{"template", "install"}}
)

// EnvStdinSupport reports whether `teploy env set --stdin` is available. A
// non-nil error means the probe could not establish an answer (F015).
func EnvStdinSupport() (bool, error) { return envStdinProbe.check() }

// KVStdinSupport reports whether `teploy kv set --stdin` is available.
func KVStdinSupport() (bool, error) { return kvStdinProbe.check() }

// VarStdinSupport reports whether `teploy template install --var-stdin` is
// available.
func VarStdinSupport() (bool, error) { return varStdinProbe.check() }

// Version returns the CLI version.
func Version() (string, error) {
	result, err := Run("version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}
