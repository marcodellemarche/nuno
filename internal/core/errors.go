// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"errors"
	"fmt"
	"time"
)

// Adapters return these so the reconciler can decide what is fatal without
// matching on strings. See ARCHITECTURE section 2.

// AuthError means the credential was rejected. On Nextcloud this is also what
// an app password gets for a write it is not allowed to confirm.
type AuthError struct {
	Provider string
	Status   int
	Message  string
	Err      error
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("%s: authentication failed (status %d): %s", e.Provider, e.Status, e.Message)
}
func (e *AuthError) Unwrap() error { return e.Err }

// UnreachableError means the service could not be contacted at all.
type UnreachableError struct {
	Provider string
	Err      error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("%s: unreachable: %v", e.Provider, e.Err)
}
func (e *UnreachableError) Unwrap() error { return e.Err }

// UnsupportedError means the instance refuses an operation by policy, for
// example Nextcloud with files/allow_unlimited_quota off. It is a capability
// answer, not a crash.
type UnsupportedError struct {
	Provider string
	Feature  string
	Detail   string
}

func (e *UnsupportedError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%s: %s is not supported by this instance", e.Provider, e.Feature)
	}
	return fmt.Sprintf("%s: %s is not supported by this instance: %s", e.Provider, e.Feature, e.Detail)
}

// ThrottledError is a rate limit. A run that hits it is incomplete, not
// failed, and resumes next cycle. See ADR-0014 and FR-38c.
type ThrottledError struct {
	Provider   string
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s: throttled, retry after %s", e.Provider, e.RetryAfter)
	}
	return fmt.Sprintf("%s: throttled", e.Provider)
}

// ProviderError is everything else the provider itself reported, including a
// response whose envelope contradicts its HTTP status.
type ProviderError struct {
	Provider string
	Op       string
	Status   int
	Message  string
	Err      error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s: %s failed (status %d): %s", e.Provider, e.Op, e.Status, e.Message)
}
func (e *ProviderError) Unwrap() error { return e.Err }

func IsAuth(err error) bool {
	var t *AuthError
	return errors.As(err, &t)
}

func IsUnreachable(err error) bool {
	var t *UnreachableError
	return errors.As(err, &t)
}

func IsUnsupported(err error) bool {
	var t *UnsupportedError
	return errors.As(err, &t)
}

// Throttled reports the retry delay when err is a rate limit.
func Throttled(err error) (time.Duration, bool) {
	var t *ThrottledError
	if errors.As(err, &t) {
		return t.RetryAfter, true
	}
	return 0, false
}
