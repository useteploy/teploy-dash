package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Result holds the output of a CLI command.
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Run executes a teploy CLI command and returns the result.
func Run(args ...string) (*Result, error) {
	cmd := exec.Command("teploy", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &Result{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return result, fmt.Errorf("failed to run teploy %s: %w", strings.Join(args, " "), err)
		}
	}

	return result, nil
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

// Deploy triggers a deploy via the CLI.
// Uses the ad-hoc deploy path (--app flag) so no teploy.yml is required.
func Deploy(server, app, image, domain string, port int) (*Result, error) {
	args := []string{"deploy", server, "--app", app}
	if image != "" {
		args = append(args, "--image", image)
	}
	if domain != "" {
		args = append(args, "--domain", domain)
	}
	if port > 0 {
		args = append(args, "--port", fmt.Sprintf("%d", port))
	}
	return Run(args...)
}

// Rollback triggers a rollback via the CLI.
func Rollback(server, app string) (*Result, error) {
	return Run("rollback", "--host", server)
}

// AppAction runs an app lifecycle action (start, stop, restart, lock, unlock).
func AppAction(server, app, action string) (*Result, error) {
	return Run(action, "--host", server)
}

// Logs returns recent logs.
func Logs(server, app string, lines int) (*Result, error) {
	return Run("logs", "--host", server, "--lines", fmt.Sprintf("%d", lines))
}

// Status returns the current status.
func Status(server, app string) (interface{}, error) {
	return RunJSON("status", "--host", server)
}

// EnvList returns environment variables.
func EnvList(server, app string) (interface{}, error) {
	return RunJSON("env", "list", "--host", server, "--reveal")
}

// EnvSet sets an environment variable.
func EnvSet(server, app, key, value string) (*Result, error) {
	return Run("env", "set", fmt.Sprintf("%s=%s", key, value), "--host", server)
}

// EnvUnset removes an environment variable.
func EnvUnset(server, app, key string) (*Result, error) {
	return Run("env", "unset", key, "--host", server)
}

// ServerList returns configured servers.
func ServerList() (*Result, error) {
	return Run("server", "list", "--json")
}

// ServerAdd adds a server.
func ServerAdd(name, host string) (*Result, error) {
	return Run("server", "add", name, host)
}

// ServerRemove removes a server.
func ServerRemove(name string) (*Result, error) {
	return Run("server", "remove", name)
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
