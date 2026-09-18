//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireInstanceLock takes an exclusive non-blocking flock on
// <dataDir>/.instance.lock (A48). The returned release function drops the
// lock on process exit; a crash releases it via the kernel.
func acquireInstanceLock(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data directory %s: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, ".instance.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another teploy-dash instance owns the data directory %s (remove %s only after stopping the other instance)", dataDir, path)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
