package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunStreamEmitsBothStreamsAndCancelsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "teploy")
	contents := "#!/bin/sh\necho ready\necho warning >&2\nsleep 30 &\nwait\n"
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	var events []StreamEvent
	var eventsMu sync.Mutex
	started := time.Now()
	result, err := RunStream(ctx, []string{"deploy"}, time.Minute, func(event StreamEvent) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
		if event.Data == "warning" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunStream error = %v, want context canceled", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("cancellation did not promptly terminate the process group")
	}
	if result == nil || !strings.Contains(result.Stdout, "ready") || !strings.Contains(result.Stderr, "warning") {
		t.Fatalf("result = %+v", result)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	seen := map[Stream]bool{events[0].Stream: true, events[1].Stream: true}
	if !seen[StreamStdout] || !seen[StreamStderr] {
		t.Fatalf("events = %+v", events)
	}
}

// A28: an oversized output line fails the command promptly with the scanner
// error as the primary cause, instead of blocking until the operation
// deadline. The child is terminated as soon as the scanner gives up, so the
// sibling pipe reader unblocks too.
func TestRunStreamOversizedLineFailsFast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "teploy")
	// 2 MiB with no newline: one line beyond the 1 MiB scanner budget,
	// written with tools every Unix has.
	contents := "#!/bin/sh\nhead -c 2097152 /dev/zero | tr '\\0' x\nsleep 30\n"
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	started := time.Now()
	result, err := RunStream(context.Background(), []string{"deploy"}, time.Minute, nil)
	if err == nil || !strings.Contains(err.Error(), "stdout") {
		t.Fatalf("err = %v, want scanner error", err)
	}
	if time.Since(started) > 15*time.Second {
		t.Fatalf("oversized line blocked for %s; the child must die at scan failure", time.Since(started))
	}
	if result == nil {
		t.Fatal("expected partial result")
	}
}

// A11: start failures and timeouts never embed the argument vector — argv can
// carry secret values.
func TestRunErrorsOmitArgv(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "teploy")
	// Exit non-zero with no stderr: exercises checkExit's generic branch.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := RunChecked("env", "set", "TOKEN=hunter2")
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "env set") {
		t.Fatalf("checkExit error = %v; argv leaked or wrong", err)
	}
	if !strings.Contains(err.Error(), "exited with code 7") {
		t.Fatalf("checkExit error = %v", err)
	}
}
