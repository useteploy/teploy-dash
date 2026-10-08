//go:build !unix

package source

import (
	"os"
	"os/exec"
)

func configureGitProcessGroup(cmd *exec.Cmd) {}

func terminateGitProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
