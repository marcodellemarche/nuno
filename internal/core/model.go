// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "time"

// IdentitySource is where a user came from.
type IdentitySource string

const (
	SourceLDAP   IdentitySource = "lldap"
	SourceManual IdentitySource = "manual"
)

type UserStatus string

const (
	UserActive   UserStatus = "active"
	UserOrphaned UserStatus = "orphaned"
)

// User is a person. Identity is (Source, SourceUUID): uid and email are
// mutable attributes, so a rename in the directory is a no-op. See ADR-0013
// and FR-9a.
type User struct {
	ID          int64
	Source      IdentitySource
	SourceUUID  string
	UID         string
	Email       string
	DisplayName string
	Status      UserStatus
	GroupUUIDs  []string
}

// Group is keyed on the directory UUID too, because a group's display name is
// its DN and renaming it would otherwise move everyone to the default tier.
type Group struct {
	ID          int64
	Source      IdentitySource
	SourceUUID  string
	Name        string
	DisplayName string
}

// ProviderInstance is a configured provider, not a provider type. The MVP runs
// one per type, but keying accounts on the instance from the start is what
// avoids a migration when a second Nextcloud appears (FR-27).
type ProviderInstance struct {
	ID        int64
	Type      string
	Name      string
	MatchKey  MatchKey
	ConfigRef string // the env prefix its URL and credentials are read from
}

type LinkOrigin string

const (
	LinkMatched LinkOrigin = "matched"
	LinkManual  LinkOrigin = "manual"
)

// AccountLink holds only real links. A manual link is never auto-invalidated,
// only reported. See ADR-0013.
type AccountLink struct {
	UserID     int64
	ProviderID int64
	ExternalID string
	Origin     LinkOrigin
	LinkedAt   time.Time
}

// LinkIssueKind names a match problem. These are recomputed every observe and
// feed FR-57, rather than living in a log line.
type LinkIssueKind string

const (
	IssueUnlinked  LinkIssueKind = "unlinked"  // a person with no account on this provider
	IssueAmbiguous LinkIssueKind = "ambiguous" // more than one account matches one person
	IssueDangling  LinkIssueKind = "dangling"  // the linked account was not observed this cycle
	IssueStale     LinkIssueKind = "stale"     // a matched link whose key no longer matches
	IssueUnmanaged LinkIssueKind = "unmanaged" // an account that matches nobody (FR-3)
)

type LinkIssue struct {
	ProviderID int64
	Kind       LinkIssueKind
	UserID     *int64
	ExternalID string
	Detail     string
}

// ExternalAccount is observed state, keyed by the provider's own account. A
// null UserID is what makes an unmanaged account representable at all.
type ExternalAccount struct {
	ProviderID int64
	ExternalID string
	UserID     *int64
	Subject    string
	Username   string
	Email      string
	Enabled    bool
	Deleted    bool
	Quota      Quota
	Used       Quota
	NeverUsed  bool
	ObservedAt time.Time
	ObserveOK  bool
}
