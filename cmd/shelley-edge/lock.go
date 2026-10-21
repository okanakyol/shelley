package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// fileLock serializes credential updates across shelley-edge processes. A
// refresh rotates the refresh token, so two processes refreshing the same
// credential at once would sign each other out.
type fileLock struct{ path string }

// lock blocks until no other process holds the lock.
func (l fileLock) lock() (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", l.path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", l.path, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
