package database

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// AcquireInstanceLock acquires an exclusive, advisory file lock at lockPath.
// serve holds this lock for its entire lifetime; db:reset must acquire it
// exclusively before performing any destructive operation, and fails fast
// with a clear error if another instance is already running.
func AcquireInstanceLock(lockPath string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create lock file parent dir: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another yolorouter-ce instance appears to be running (lock held on %s)", lockPath)
	}

	unlock := func() error {
		defer func() { _ = f.Close() }()
		return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	return unlock, nil
}
