// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes an exclusive lock on the data directory, so one process writes at
// a time. A server holds it for its whole life and an offline command takes it
// for the length of the command. See ADR-0018.
//
// The lock is advisory and released by the kernel when the process exits, so a
// crash does not leave a stale lock behind.
func Lock(dataDir string) (func() error, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "nuno.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another Nuno process holds %s: stop it before running this command", path)
	}
	return func() error {
		defer file.Close()
		return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	}, nil
}
