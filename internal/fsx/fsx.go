// Package fsx has the file operations that must stay correct when two
// Agentium processes share state: a cross-process lock and atomic writes.
package fsx

import (
	"os"
	"path/filepath"
	"time"
)

// WriteFile writes data to path atomically: a unique temporary file in
// the same directory is written, synced and renamed over the target, so
// readers never see half a file and concurrent writers never share a
// temporary name. A symlinked path is resolved first, so the link (e.g.
// into a dotfiles repository) stays a link.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		path = r
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// Lock takes an exclusive lock shared by all processes on lockPath
// (created if needed) and returns the function that releases it. It
// waits up to timeout; after that it proceeds without the lock rather
// than hang (the caller's own writes are still atomic).
func Lock(lockPath string, timeout time.Duration) func() {
	_ = os.MkdirAll(filepath.Dir(lockPath), 0o700)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	deadline := time.Now().Add(timeout)
	for {
		if tryLock(f) {
			return func() {
				unlock(f)
				f.Close()
			}
		}
		if time.Now().After(deadline) {
			f.Close()
			return func() {}
		}
		time.Sleep(20 * time.Millisecond)
	}
}
