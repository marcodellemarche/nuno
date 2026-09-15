// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"fmt"
	"time"
)

// Provider is a service that can hold user data and enforce a per-user quota.
// Adding one means implementing this interface and registering it in the
// composition root, with no change to core (FR-25).
type Provider interface {
	Type() string
	Capabilities() Capabilities
	Health(ctx context.Context) HealthResult

	ListAccounts(ctx context.Context) ([]Account, error)
	GetAccount(ctx context.Context, externalID string) (Account, error)
	NormalizeQuota(q Quota) Quota

	SetQuota(ctx context.Context, externalID string, q Quota) error
}

// Capabilities is what the adapter can do. The core never guesses and never
// calls an optional API without checking here first (FR-22, FR-23).
type Capabilities struct {
	CanReadUsers       bool
	CanReadUsage       bool
	CanSetUserQuota    bool
	CanSetDefaultQuota bool // reserved for FR-24 (Later); false for Immich, which has no such concept
	HasGroups          bool // reserved; group membership comes from the identity source
	MatchKeys          []MatchKey
}

// MatchKey is a field an account can be matched on. It is never the naked
// account id: Nextcloud auto-registers OIDC and LDAP users with a UUID. See
// ADR-0013.
type MatchKey string

const (
	// MatchSubject is the OIDC subject. When two providers report the same
	// one they hold the same person's account, exactly and without
	// heuristics. See ADR-0021.
	MatchSubject  MatchKey = "subject"
	MatchEmail    MatchKey = "email"
	MatchUsername MatchKey = "username"
)

func (k MatchKey) Valid() bool {
	switch k {
	case MatchSubject, MatchEmail, MatchUsername:
		return true
	}
	return false
}

// Account is one account as the provider reports it.
type Account struct {
	ExternalID string // the provider's stable key, and the only thing writes address
	Subject    string // OIDC subject when the provider exposes one
	Username   string // may be empty, may be a UUID, never assumed meaningful
	Email      string // may be empty: a real captured account has none
	Enabled    bool
	Deleted    bool  // Immich soft-deletes; a deleted account is never linked or written
	Quota      Quota // the CONFIGURED quota, not the effective one
	Used       Quota // Unknown or Bytes only; never Unlimited
	ObservedAt time.Time

	// NeverUsed marks an account that exists and holds nothing, as opposed to
	// one whose usage could not be read. Its usage is known and zero and it is
	// fully writable, which is what lets a person's quota be right before they
	// first open the service. See ADR-0024.
	NeverUsed bool
}

// Writable reports whether this account may be written to at all, before any
// policy or guardrail is considered (FR-8a).
func (a Account) Writable() bool { return a.Enabled && !a.Deleted }

// MatchValue returns what this account offers for a match key.
func (a Account) MatchValue(k MatchKey) string {
	switch k {
	case MatchSubject:
		return a.Subject
	case MatchEmail:
		return a.Email
	case MatchUsername:
		return a.Username
	}
	return ""
}

// HealthResult is the answer to a read-only question. Whether the credential
// can write is not part of it: that takes a write, and it runs later, from the
// probe. See ADR-0025.
type HealthResult struct {
	Reachable   bool
	Version     string
	InSupported bool
	Err         error
}

// WriteAccess is the outcome of the write probe. Unproven is the honest
// default and the value on a fresh install; it is not the same as No.
type WriteAccess uint8

const (
	WriteUnproven WriteAccess = iota
	WriteYes
	WriteNo
)

func (w WriteAccess) String() string {
	switch w {
	case WriteUnproven:
		return "unproven"
	case WriteYes:
		return "yes"
	case WriteNo:
		return "no"
	}
	return fmt.Sprintf("WriteAccess(%d)", uint8(w))
}

// CompetingWriter is another component that writes the same quotas. Nuno
// refuses to apply against an instance where one is present, naming the
// remedy, while reading continues (FR-75).
type CompetingWriter struct {
	// Setting names it, for example oidc_login_default_quota.
	Setting string
	// Value is what it is set to, empty when the setting exists but is blank.
	Value string
	// Detail says what it does behind Nuno's back.
	Detail string
	// Remedy is the exact command that fixes it, which is what doctor prints
	// and what `doctor --fix` emits as a script (ADR-0027).
	Remedy []string
}

// WriterDetector is an optional capability. An adapter implements it when the
// service exposes enough to answer the question; the caller type-asserts for
// it rather than every provider carrying a method it cannot honour.
//
// It reports only what the API actually exposes. A setting a service keeps out
// of reach is diagnosed by the script doctor emits, never guessed at, and
// never claimed as a startup check (ADR-0027).
type WriterDetector interface {
	CompetingWriters(ctx context.Context) ([]CompetingWriter, error)

	// UndetectableWriters are the ones this adapter cannot see, with the
	// command that would show them. They are reported as unknown rather than
	// as absent.
	UndetectableWriters() []CompetingWriter
}
