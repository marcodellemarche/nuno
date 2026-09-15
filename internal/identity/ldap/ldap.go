// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ldap reads users and groups from an LDAP directory, LLDAP first.
//
// Three behaviours are specific to LLDAP and are why this adapter looks the
// way it does: memberOf is absent from the wildcard attribute set so it has to
// be requested by name, StartTLS is not implemented so the transport is either
// ldaps or plaintext on a private network, and entryuuid is exposed for both
// users and groups, which is the identity everything else is keyed on
// (ADR-0013).
package ldap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	goldap "github.com/go-ldap/ldap/v3"

	"github.com/marcodellemarche/nuno/internal/core"
)

const (
	DefaultUserFilter  = "(objectClass=person)"
	DefaultGroupFilter = "(objectClass=groupOfUniqueNames)"
	DefaultTimeout     = 15 * time.Second

	// The UUID attribute is requested by name because a wildcard search does
	// not return it, the same way memberOf does not appear.
	attrUUID     = "entryuuid"
	attrMemberOf = "memberOf"
)

type Config struct {
	URL          string
	BaseDN       string
	BindDN       string
	BindPassword core.Secret
	UserFilter   string
	GroupFilter  string
	Timeout      time.Duration
}

// Conn is the part of an LDAP connection this adapter uses. Taking it as an
// interface is what lets the mapping be tested without a directory, which is
// the half that actually gets things wrong.
type Conn interface {
	Search(*goldap.SearchRequest) (*goldap.SearchResult, error)
	Close() error
}

type Source struct {
	cfg  Config
	dial func(ctx context.Context) (Conn, error)
}

func New(cfg Config) (*Source, error) {
	if cfg.URL == "" {
		return nil, errors.New("NUNO_LDAP_URL is required")
	}
	if cfg.BaseDN == "" {
		return nil, errors.New("NUNO_LDAP_BASE_DN is required")
	}
	scheme, _, ok := strings.Cut(cfg.URL, "://")
	if !ok || (scheme != "ldap" && scheme != "ldaps") {
		return nil, fmt.Errorf("NUNO_LDAP_URL must start with ldap:// or ldaps://, got %q", core.RedactURL(cfg.URL))
	}
	if cfg.UserFilter == "" {
		cfg.UserFilter = DefaultUserFilter
	}
	if cfg.GroupFilter == "" {
		cfg.GroupFilter = DefaultGroupFilter
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	source := &Source{cfg: cfg}
	source.dial = source.dialReal
	return source, nil
}

// NewWithConn builds a source over an existing connection, for tests and for
// the integration harness.
func NewWithConn(cfg Config, conn Conn) (*Source, error) {
	source, err := New(cfg)
	if err != nil {
		return nil, err
	}
	source.dial = func(context.Context) (Conn, error) { return conn, nil }
	return source, nil
}

func (s *Source) dialReal(ctx context.Context) (Conn, error) {
	// StartTLS is deliberately not attempted: LLDAP does not implement it, so
	// trying would fail on the directory Nuno targets. Use ldaps, or
	// plaintext on a private network.
	conn, err := goldap.DialURL(s.cfg.URL, goldap.DialWithDialer(&net.Dialer{Timeout: s.cfg.Timeout}))
	if err != nil {
		return nil, &core.UnreachableError{Provider: "ldap", Err: err}
	}
	conn.SetTimeout(s.cfg.Timeout)

	if s.cfg.BindDN != "" {
		if err := conn.Bind(s.cfg.BindDN, s.cfg.BindPassword.Reveal()); err != nil {
			conn.Close()
			return nil, &core.AuthError{Provider: "ldap", Message: "bind failed: " + err.Error(), Err: err}
		}
	}
	return conn, nil
}

// Version reads the root DSE, which LLDAP fills with vendorName and
// vendorVersion. It is the same signal a provider's version endpoint gives,
// for free (ADR-0015).
func (s *Source) Version(ctx context.Context) (string, error) {
	conn, err := s.dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	result, err := conn.Search(goldap.NewSearchRequest(
		"", goldap.ScopeBaseObject, goldap.NeverDerefAliases, 1, int(s.cfg.Timeout.Seconds()), false,
		"(objectClass=*)", []string{"vendorName", "vendorVersion"}, nil,
	))
	if err != nil {
		return "", fmt.Errorf("read the root DSE: %w", err)
	}
	if len(result.Entries) == 0 {
		return "", nil
	}
	name := attribute(result.Entries[0], "vendorName")
	version := attribute(result.Entries[0], "vendorVersion")
	return strings.TrimSpace(name + " " + version), nil
}

// Sync reads groups and then users, and resolves membership against the
// groups the same read returned.
func (s *Source) Sync(ctx context.Context) (core.IdentityRead, error) {
	conn, err := s.dial(ctx)
	if err != nil {
		return core.IdentityRead{}, err
	}
	defer conn.Close()

	groups, byDN, err := s.readGroups(conn)
	if err != nil {
		return core.IdentityRead{}, err
	}
	users, result, err := s.readUsers(conn, byDN)
	if err != nil {
		return core.IdentityRead{}, err
	}

	result.Snapshot = core.IdentitySnapshot{
		Source: core.SourceLDAP,
		Users:  users,
		Groups: groups,
	}
	return result, nil
}

func (s *Source) readGroups(conn Conn) ([]core.Group, map[string]string, error) {
	result, err := conn.Search(s.request(s.cfg.GroupFilter, []string{attrUUID, "cn", "displayName", "description"}))
	if err != nil {
		return nil, nil, fmt.Errorf("search groups in %s: %w", s.cfg.BaseDN, err)
	}

	groups := make([]core.Group, 0, len(result.Entries))
	byDN := make(map[string]string, len(result.Entries))
	for _, entry := range result.Entries {
		uuid := attribute(entry, attrUUID)
		if uuid == "" {
			// A group with no uuid cannot be keyed, and its display name is
			// its DN, so renaming it would move everyone to the default tier.
			// Skipping it is safer than keying on something mutable.
			continue
		}
		name := attribute(entry, "cn")
		if name == "" {
			name = firstRDNValue(entry.DN)
		}
		display := attribute(entry, "displayName")
		if display == "" {
			display = name
		}
		groups = append(groups, core.Group{
			Source:      core.SourceLDAP,
			SourceUUID:  uuid,
			Name:        name,
			DisplayName: display,
		})
		byDN[normalizeDN(entry.DN)] = uuid
	}
	return groups, byDN, nil
}

func (s *Source) readUsers(conn Conn, groupsByDN map[string]string) ([]core.User, core.IdentityRead, error) {
	// memberOf has to be named: a wildcard attribute set does not include it,
	// so a "*" search returns no group membership at all.
	attrs := []string{attrUUID, "uid", "mail", "cn", "displayName", "givenName", "sn", attrMemberOf}
	result, err := conn.Search(s.request(s.cfg.UserFilter, attrs))
	if err != nil {
		return nil, core.IdentityRead{}, fmt.Errorf("search users in %s: %w", s.cfg.BaseDN, err)
	}

	var report core.IdentityRead
	unknown := map[string]bool{}
	users := make([]core.User, 0, len(result.Entries))
	for _, entry := range result.Entries {
		uuid := attribute(entry, attrUUID)
		if uuid == "" {
			report.SkippedEntries = append(report.SkippedEntries, entry.DN)
			continue
		}

		uid := attribute(entry, "uid")
		if uid == "" {
			// Both uid= and cn= appear as the RDN depending on how the
			// directory is laid out.
			uid = firstRDNValue(entry.DN)
		}
		user := core.User{
			Source:      core.SourceLDAP,
			SourceUUID:  uuid,
			UID:         uid,
			Email:       attribute(entry, "mail"),
			DisplayName: displayName(entry, uid),
			Status:      core.UserActive,
		}

		for _, dn := range attributeValues(entry, attrMemberOf) {
			groupUUID, known := groupsByDN[normalizeDN(dn)]
			if !known {
				// A membership naming a group outside this search is not an
				// error, but the store refuses a membership it cannot
				// resolve, so it is dropped here and reported.
				if !unknown[dn] {
					unknown[dn] = true
					report.UnresolvedMemberships = append(report.UnresolvedMemberships, dn)
				}
				continue
			}
			user.GroupUUIDs = append(user.GroupUUIDs, groupUUID)
		}
		users = append(users, user)
	}
	return users, report, nil
}

func (s *Source) request(filter string, attrs []string) *goldap.SearchRequest {
	return goldap.NewSearchRequest(
		s.cfg.BaseDN,
		goldap.ScopeWholeSubtree,
		goldap.NeverDerefAliases,
		0, int(s.cfg.Timeout.Seconds()), false,
		filter, attrs, nil,
	)
}

func displayName(entry *goldap.Entry, fallback string) string {
	for _, attr := range []string{"displayName", "cn"} {
		if value := attribute(entry, attr); value != "" {
			return value
		}
	}
	given := attribute(entry, "givenName")
	surname := attribute(entry, "sn")
	if name := strings.TrimSpace(given + " " + surname); name != "" {
		return name
	}
	return fallback
}

// attribute reads one value case-insensitively. LDAP attribute names are
// case-insensitive by the standard, but the library compares them exactly, so
// asking for entryuuid and being handed entryUUID would silently return
// nothing.
func attribute(entry *goldap.Entry, name string) string {
	values := attributeValues(entry, name)
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func attributeValues(entry *goldap.Entry, name string) []string {
	for _, attr := range entry.Attributes {
		if strings.EqualFold(attr.Name, name) {
			return attr.Values
		}
	}
	return nil
}

// normalizeDN makes two spellings of one DN compare equal, which is what
// matching memberOf against a group's DN needs.
func normalizeDN(dn string) string {
	parsed, err := goldap.ParseDN(dn)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(dn))
	}
	var parts []string
	for _, rdn := range parsed.RDNs {
		for _, attr := range rdn.Attributes {
			parts = append(parts, strings.ToLower(attr.Type)+"="+strings.ToLower(strings.TrimSpace(attr.Value)))
		}
	}
	return strings.Join(parts, ",")
}

func firstRDNValue(dn string) string {
	parsed, err := goldap.ParseDN(dn)
	if err != nil || len(parsed.RDNs) == 0 || len(parsed.RDNs[0].Attributes) == 0 {
		return ""
	}
	return parsed.RDNs[0].Attributes[0].Value
}
