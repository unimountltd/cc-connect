//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// acquireSingleInstanceLock obtains an exclusive non-blocking lock on lockPath
// to ensure that only one cc-connect daemon process runs at a time on this
// machine (per user). The lock is held for the lifetime of the process; the
// kernel releases it automatically when the process exits.
func acquireSingleInstanceLock(lockPath string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}

	// LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY
	const flags = windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, ol); err != nil {
		holderPID := readLockHolderPID(f)
		_ = f.Close()
		if holderPID > 0 {
			return nil, fmt.Errorf("another cc-connect instance is already running (PID: %d, lock: %s)", holderPID, lockPath)
		}
		return nil, fmt.Errorf("another cc-connect instance is already running (lock: %s)", lockPath)
	}

	if err := f.Truncate(0); err != nil {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
		_ = f.Close()
		return nil, fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
		_ = f.Close()
		return nil, fmt.Errorf("write lock file: %w", err)
	}

	release = func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
		_ = f.Close()
	}
	return release, nil
}

func readLockHolderPID(f *os.File) int {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	buf := make([]byte, 32)
	n, _ := f.Read(buf)
	if n <= 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}

// defaultLockPath returns the path of the single-instance lock file. It is
// intentionally fixed (not derived from the loaded config's data_dir) so that
// the singleton guarantee holds across all configs for the current user.
func defaultLockPath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cc-connect", "cc-connect.lock")
	}
	return filepath.Join(os.TempDir(), "cc-connect.lock")
}
