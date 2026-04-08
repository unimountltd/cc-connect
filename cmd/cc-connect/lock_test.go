package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireSingleInstanceLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	// First acquire should succeed and write our PID into the file.
	release1, err := acquireSingleInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	t.Cleanup(func() {
		if release1 != nil {
			release1()
		}
	})

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	wantPID := fmt.Sprintf("%d", os.Getpid())
	if got := strings.TrimSpace(string(data)); got != wantPID {
		t.Errorf("lock file PID = %q, want %q", got, wantPID)
	}

	// Second acquire on the same path must fail and surface the holder PID.
	release2, err := acquireSingleInstanceLock(lockPath)
	if err == nil {
		release2()
		t.Fatal("second acquire unexpectedly succeeded; expected lock contention")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error %q should mention 'already running'", err.Error())
	}
	if !strings.Contains(err.Error(), wantPID) {
		t.Errorf("error %q should include holder PID %s", err.Error(), wantPID)
	}

	// Release the first lock; a fresh acquire should now succeed.
	release1()
	release1 = nil

	release3, err := acquireSingleInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	release3()
}

func TestAcquireSingleInstanceLock_ReleaseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	release, err := acquireSingleInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}

	// Calling release multiple times must be safe — main.go does this on the
	// /restart path: an explicit release before restartProcess() plus the
	// deferred release at function exit on Windows where restartProcess
	// returns instead of replacing the image.
	release()
	release()
	release()

	// After release, the lock must be reacquirable.
	release2, err := acquireSingleInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("acquire after multi-release failed: %v", err)
	}
	release2()
}

func TestAcquireSingleInstanceLock_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	// Lock path inside a not-yet-created subdirectory.
	lockPath := filepath.Join(dir, "nested", "subdir", "test.lock")

	release, err := acquireSingleInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer release()

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
}

func TestDefaultLockPath(t *testing.T) {
	p := defaultLockPath()
	if p == "" {
		t.Fatal("defaultLockPath returned empty string")
	}
	if !strings.HasSuffix(p, ".lock") {
		t.Errorf("defaultLockPath %q should end with .lock", p)
	}
}
