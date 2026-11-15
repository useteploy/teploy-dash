package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// D-08: with no teploy binary on PATH every invocation answers the one
// ErrNotInstalled, carrying the install remediation — never the raw
// `exec: "teploy": executable file not found` error.
func TestMissingBinaryIsErrNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if IsInstalled() {
		t.Fatal("IsInstalled with an empty PATH")
	}
	runs := map[string]func() error{
		"RunContext": func() error { _, err := RunContext(context.Background(), "version"); return err },
		"RunWithStdin": func() error {
			_, err := RunWithStdin(context.Background(), "secret", "registry", "login")
			return err
		},
		"RunChecked": func() error { _, err := RunChecked("version"); return err },
		"RunStream": func() error {
			_, err := RunStream(context.Background(), []string{"version"}, time.Minute, nil)
			return err
		},
	}
	for name, run := range runs {
		err := run()
		if !errors.Is(err, ErrNotInstalled) {
			t.Errorf("%s: err = %v, want ErrNotInstalled", name, err)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, "executable file not found") || !strings.Contains(msg, InstallURL) {
			t.Errorf("%s: message %q should name the install URL and not the raw exec error", name, msg)
		}
	}
}
