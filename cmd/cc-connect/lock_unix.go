//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// acquireSingleInstanceLock obtains an exclusive non-blocking lock on lockPath
// to ensure that only one cc-connect daemon process runs at a time on this
// machine (per user). The lock is held for the lifetime of the process; the
// kernel releases it automatically when the process exits, even on crash.
//
// On success it returns a release function that closes the underlying file.
// On contention it returns an error whose message includes the PID of the
// existing lock holder (read from the lock file contents).
func acquireSingleInstanceLock(lockPath string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Lock is held by another process. Read the file to surface the holder PID.
		holderPID := readLockHolderPID(f)
		_ = f.Close()
		if holderPID > 0 {
			return nil, fmt.Errorf("another cc-connect instance is already running (PID: %d, lock: %s)", holderPID, lockPath)
		}
		return nil, fmt.Errorf("another cc-connect instance is already running (lock: %s)", lockPath)
	}

	// We hold the lock. Truncate and write our PID for diagnostics.
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("write lock file: %w", err)
	}

	release = func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return release, nil
}

// readLockHolderPID reads the PID written by the current lock holder. The
// caller has already failed to acquire the lock, so the file's contents are
// the holder's PID (or empty if the holder has not yet written it).
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
	return filepath.Join(os.TempDir(), fmt.Sprintf("cc-connect-%d.lock", os.Getuid()))
}
