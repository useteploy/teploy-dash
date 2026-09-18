package cli

import (
	"bufio"
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open teploy stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open teploy stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("teploy command could not start: %w", err)
	}

	var stdoutBuffer, stderrBuffer lockedBuffer
	var wg sync.WaitGroup
	var callbackMu sync.Mutex
	var scanErr error
	var scanErrMu sync.Mutex
	scan := func(stream Stream, scanner *bufio.Scanner, output *lockedBuffer) {
		defer wg.Done()
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			output.WriteLine(line)
			if onEvent != nil {
				callbackMu.Lock()
				onEvent(StreamEvent{Stream: stream, Data: line})
				callbackMu.Unlock()
			}
		}
		if err := scanner.Err(); err != nil {
			scanErrMu.Lock()
			if scanErr == nil {
				scanErr = fmt.Errorf("reading teploy %s output: %w", stream, err)
			}
			scanErrMu.Unlock()
			_ = cmd.Cancel()
		}
	}
	wg.Add(2)
	go scan(StreamStdout, bufio.NewScanner(stdout), &stdoutBuffer)
	go scan(StreamStderr, bufio.NewScanner(stderr), &stderrBuffer)
	wg.Wait()
	waitErr := cmd.Wait()

	result := &Result{Stdout: stdoutBuffer.String(), Stderr: stderrBuffer.String()}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if scanErr != nil {
		return result, scanErr
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
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			// No argv in the message — timeouts used to concatenate the full
			// argument vector, which can carry secret values (A11).
			return result, fmt.Errorf("teploy command timed out after %s", time.Since(started).Round(time.Second))
		}
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
// visible to every local process (A11); the older CLI keeps the legacy argv
// path so an upgrade boundary degrades instead of breaking.
func EnvSet(ctx context.Context, server, user, app, key, value string) (*Result, error) {
	if EnvStdinSupported() {
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

// IsInstalled checks if the teploy CLI binary is available.
func IsInstalled() bool {
	_, err := exec.LookPath("teploy")
	return err == nil
}

// ── Secret-stdin capability probes (UPSTREAM-1 adoption) ──────────────────
//
// The CLI's stdin contract (`env set KEY --stdin`, `kv set KEY --stdin`,
// `template install --var-stdin`) removed secrets from the argv. Dash shells
// out to whatever teploy is on PATH, so each call site probes once per
// process whether the installed CLI knows the flag and falls back to the
// legacy argv path when it doesn't — a hard requirement on the new flag
// would break every install at the CLI upgrade boundary.

// probeFlag runs `teploy <args...> --help` and reports whether the flag
// appears in its usage text.
func probeFlag(flag string, args ...string) bool {
	if !IsInstalled() {
		return false
	}
	result, err := Run(append(append([]string{}, args...), "--help")...)
	if err != nil || result.ExitCode != 0 {
		return false
	}
	return strings.Contains(result.Stdout, flag) || strings.Contains(result.Stderr, flag)
}

var (
	envStdinSupported    = sync.OnceValue(func() bool { return probeFlag("--stdin", "env", "set") })
	kvStdinSupportedOnce = sync.OnceValue(func() bool { return probeFlag("--stdin", "kv", "set") })
	varStdinSupported    = sync.OnceValue(func() bool { return probeFlag("--var-stdin", "template", "install") })
)

// EnvStdinSupported reports whether `teploy env set --stdin` is available.
func EnvStdinSupported() bool { return envStdinSupported() }

// KVStdinSupported reports whether `teploy kv set --stdin` is available.
func KVStdinSupported() bool { return kvStdinSupportedOnce() }

// VarStdinSupported reports whether `teploy template install --var-stdin` is
// available.
func VarStdinSupported() bool { return varStdinSupported() }

// Version returns the CLI version.
func Version() (string, error) {
	result, err := Run("version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}
