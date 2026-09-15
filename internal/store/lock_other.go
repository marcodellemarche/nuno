// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix

package store

// Lock is a no-op where flock is unavailable. Nuno ships as a Linux container,
// so this exists to keep the package building elsewhere rather than to provide
// the guarantee: on such a platform, do not run a command and a server at once.
func Lock(dataDir string) (func() error, error) {
	return func() error { return nil }, nil
}
