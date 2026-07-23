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
