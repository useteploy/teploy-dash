//go:build !unix

package main

// acquireInstanceLock is a no-op where flock is unavailable (A48).
func acquireInstanceLock(dataDir string) (func(), error) {
	return func() {}, nil
}
