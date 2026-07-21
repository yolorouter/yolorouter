package database

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireInstanceLockExclusivity(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "yolorouter.lock")

	unlock1, err := AcquireInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}

	_, err = AcquireInstanceLock(lockPath)
	if err == nil {
		t.Fatalf("second acquire should fail while first holder is still active")
	}

	if err := unlock1(); err != nil {
		t.Fatalf("unlock failed: %v", err)
	}

	unlock2, err := AcquireInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("acquire after unlock should succeed: %v", err)
	}
	_ = unlock2()
}

func TestAcquireInstanceLockCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "nested", "yolorouter.lock")

	unlock, err := AcquireInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("acquire should create parent dir and succeed: %v", err)
	}
	defer func() { _ = unlock() }()

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should exist: %v", err)
	}
}
