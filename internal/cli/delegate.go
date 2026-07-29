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
// including SSH or shell descendants spawned by the CLI.
func RunStream(ctx context.Context, args []string, timeout time.Duration, onEvent func(StreamEvent)) (*Result, error) {
	if timeout <= 0 {
		timeout = cliTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "teploy", args...)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return terminateProcessGroup(cmd) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open teploy stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open teploy stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to run teploy %s: %w", strings.Join(args, " "), err)
	}

	var stdoutBuffer, stderrBuffer lockedBuffer
	var wg sync.WaitGroup
	var callbackMu sync.Mutex
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
	}
	wg.Add(2)
	go scan(StreamStdout, bufio.NewScanner(stdout), &stdoutBuffer)
	go scan(StreamStderr, bufio.NewScanner(stderr), &stderrBuffer)
	waitErr := cmd.Wait()
	wg.Wait()

	result := &Result{Stdout: stdoutBuffer.String(), Stderr: stderrBuffer.String()}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if waitErr != nil {
		if _, ok := waitErr.(*exec.ExitError); !ok {
			return result, fmt.Errorf("failed to run teploy %s: %w", strings.Join(args, " "), waitErr)
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

// Run executes a teploy CLI command and returns the result.
func Run(args ...string) (*Result, error) {
	return RunContext(context.Background(), args...)
}

// RunContext executes a teploy CLI command while honoring caller cancellation.
// Machine reads use this so a canceled HTTP/fleet request also stops its CLI
// subprocess instead of waiting for the global delegate timeout.
func RunContext(ctx context.Context, args ...string) (*Result, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "teploy", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &Result{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return result, fmt.Errorf("teploy %s timed out after %s", strings.Join(args, " "), time.Since(started).Round(time.Second))
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return result, fmt.Errorf("failed to run teploy %s: %w", strings.Join(args, " "), err)
		}
	}

	return result, nil
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
// non-zero exit as an error. Used to pass secrets (e.g. a registry password) to
// the CLI without putting them on the argv, where they'd show in the host's
// process list.
func RunWithStdin(stdin string, args ...string) (*Result, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "teploy", args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := &Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return result, fmt.Errorf("teploy %s timed out after %s", strings.Join(args, " "), time.Since(started).Round(time.Second))
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return result, fmt.Errorf("failed to run teploy %s: %w", strings.Join(args, " "), err)
		}
	}
	return result, checkExit(result, args)
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
	return result, checkExit(result, args)
}

// checkExit converts a non-zero CLI exit into an error, preferring stderr, then
// stdout, then a generic message. Split out for testability.
func checkExit(result *Result, args []string) error {
	if result.ExitCode == 0 {
		return nil
	}
	msg := strings.TrimSpace(result.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(result.Stdout)
	}
	if msg == "" {
		msg = fmt.Sprintf("teploy %s exited with code %d", strings.Join(args, " "), result.ExitCode)
	}
	return errors.New(msg)
}

// RunJSON executes a teploy CLI command with --json flag and parses output.
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
		// Return raw stdout if not JSON
		return result.Stdout, nil
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

// EnvSet sets an environment variable.
func EnvSet(server, user, app, key, value string) (*Result, error) {
	args := append([]string{"env", "set", fmt.Sprintf("%s=%s", key, value), "--host", server, "--app", app}, userArgs(user)...)
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

// Version returns the CLI version.
func Version() (string, error) {
	result, err := Run("version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}
