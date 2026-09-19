// Package durable provides crash-durable whole-file replacement for
// application-owned state files (credentials, tokens). A fixed-name .tmp
// write + rename acknowledges changes before they are durable and lets two
// concurrent savers clobber each other's temporary file (audit F004); this
// helper writes a unique private temporary file, syncs it, renames it over
// the target, and syncs the parent directory so the rename itself survives
// a crash.
package durable

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Replace atomically replaces path with data. The caller's in-memory state
// should only be published as durable once this returns nil.
func Replace(path string, data []byte, mode os.FileMode) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return fmt.Errorf("create parent: %w", err)
	}
	dir, err := os.Open(parent)
	if err != nil {
		return fmt.Errorf("open parent: %w", err)
	}
	defer dir.Close()

	tmp, err := os.CreateTemp(parent, ".replace-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if n, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary file: %w", err)
	}
	closed = true
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temporary file: %w", err)
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync parent after rename: %w", err)
	}
	return nil
}
