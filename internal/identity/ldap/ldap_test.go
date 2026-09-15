// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	goldap "github.com/go-ldap/ldap/v3"

	"github.com/marcodellemarche/nuno/internal/core"
)

// fakeConn answers searches from a script, and records what was asked, which
// is how the memberOf requirement gets asserted.
type fakeConn struct {
	groups    []*goldap.Entry
	users     []*goldap.Entry
	rootDSE   []*goldap.Entry
	err       error
	requested [][]string
	filters   []string
}

func (f *fakeConn) Search(req *goldap.SearchRequest) (*goldap.SearchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.requested = append(f.requested, req.Attributes)
	f.filters = append(f.filters, req.Filter)
	switch {
	case req.Scope == goldap.ScopeBaseObject:
		return &goldap.SearchResult{Entries: f.rootDSE}, nil
	case strings.Contains(req.Filter, "groupOfUniqueNames"):
		return &goldap.SearchResult{Entries: f.groups}, nil
	default:
		return &goldap.SearchResult{Entries: f.users}, nil
	}
}

func (f *fakeConn) Close() error { return nil }

func entry(dn string, attrs map[string][]string) *goldap.Entry {
	e := &goldap.Entry{DN: dn}
	for name, values := range attrs {
		e.Attributes = append(e.Attributes, &goldap.EntryAttribute{Name: name, Values: values})
	}
	return e
}

const baseDN = "dc=example,dc=org"

func source(t *testing.T, conn Conn) *Source {
	t.Helper()
	s, err := NewWithConn(Config{URL: "ldap://lldap:3890", BaseDN: baseDN, BindDN: "uid=nuno," + baseDN, BindPassword: "secret"}, conn)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSyncMapsUsersAndGroups(t *testing.T) {
	conn := &fakeConn{
		groups: []*goldap.Entry{
			entry("cn=photographers,ou=groups,"+baseDN, map[string][]string{
				"entryuuid": {"g-photographers"},
				"cn":        {"photographers"},
			}),
			entry("cn=staff,ou=groups,"+baseDN, map[string][]string{
				// The library compares attribute names exactly, so a
				// directory answering entryUUID must still be read.
				"entryUUID": {"g-staff"},
				"cn":        {"staff"},
			}),
		},
		users: []*goldap.Entry{
			entry("uid=alice,ou=people,"+baseDN, map[string][]string{
				"entryuuid":   {"u-alice"},
				"uid":         {"alice"},
				"mail":        {"alice@example.org"},
				"displayName": {"Alice"},
				"memberOf": {
					"cn=photographers,ou=groups," + baseDN,
					// Same DN, spelled differently: it must resolve to the
					// same group rather than being dropped.
					"CN=Staff, OU=Groups, " + strings.ToUpper(baseDN),
				},
			}),
			entry("uid=bob,ou=people,"+baseDN, map[string][]string{
				"entryuuid": {"u-bob"},
				"uid":       {"bob"},
				"mail":      {"bob@example.org"},
				"givenName": {"Bob"},
				"sn":        {"Smith"},
			}),
		},
	}

	result, err := source(t, conn).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snap := result.Snapshot
	if snap.Source != core.SourceLDAP {
		t.Errorf("source = %q", snap.Source)
	}
	if len(snap.Groups) != 2 || len(snap.Users) != 2 {
		t.Fatalf("got %d groups and %d users", len(snap.Groups), len(snap.Users))
	}

	alice := snap.Users[0]
	if alice.SourceUUID != "u-alice" || alice.UID != "alice" || alice.Email != "alice@example.org" {
		t.Errorf("alice = %+v", alice)
	}
	if alice.DisplayName != "Alice" {
		t.Errorf("display name = %q", alice.DisplayName)
	}
	if len(alice.GroupUUIDs) != 2 {
		t.Errorf("alice groups = %v, want both memberships resolved regardless of DN spelling", alice.GroupUUIDs)
	}
	if !slices.Contains(alice.GroupUUIDs, "g-staff") {
		t.Errorf("alice groups = %v, want g-staff resolved from a differently spelled DN", alice.GroupUUIDs)
	}

	bob := snap.Users[1]
	if bob.DisplayName != "Bob Smith" {
		t.Errorf("bob display name = %q, want it composed when displayName and cn are absent", bob.DisplayName)
	}
	if len(bob.GroupUUIDs) != 0 {
		t.Errorf("bob groups = %v", bob.GroupUUIDs)
	}
	for _, u := range snap.Users {
		if u.Status != core.UserActive {
			t.Errorf("%s status = %q", u.UID, u.Status)
		}
	}
}

// memberOf is absent from the wildcard attribute set, so a "*" search returns
// no group membership. It has to be requested by name.
func TestMemberOfAndUUIDAreRequestedByName(t *testing.T) {
	conn := &fakeConn{}
	if _, err := source(t, conn).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(conn.requested) != 2 {
		t.Fatalf("got %d searches, want groups then users", len(conn.requested))
	}
	userAttrs := conn.requested[1]
	for _, want := range []string{attrMemberOf, attrUUID, "uid", "mail"} {
		if !slices.Contains(userAttrs, want) {
			t.Errorf("the user search must name %q, got %v", want, userAttrs)
		}
	}
	if slices.Contains(userAttrs, "*") {
		t.Error("a wildcard attribute set would return no memberOf at all")
	}
	if !slices.Contains(conn.requested[0], attrUUID) {
		t.Errorf("the group search must name %q: a group's identity is its uuid", attrUUID)
	}
}

// An entry with no uuid cannot be keyed on anything stable, and keying on a
// mutable attribute is what loses links and overrides on a rename.
func TestEntriesWithNoUUIDAreSkippedAndReported(t *testing.T) {
	conn := &fakeConn{
		groups: []*goldap.Entry{
			entry("cn=nouuid,ou=groups,"+baseDN, map[string][]string{"cn": {"nouuid"}}),
		},
		users: []*goldap.Entry{
			entry("uid=ghost,ou=people,"+baseDN, map[string][]string{"uid": {"ghost"}}),
			entry("uid=alice,ou=people,"+baseDN, map[string][]string{
				"entryuuid": {"u-alice"}, "uid": {"alice"},
			}),
		},
	}
	result, err := source(t, conn).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Snapshot.Groups) != 0 {
		t.Errorf("groups = %+v, want the one with no uuid skipped", result.Snapshot.Groups)
	}
	if len(result.Snapshot.Users) != 1 {
		t.Errorf("users = %+v", result.Snapshot.Users)
	}
	if len(result.SkippedEntries) != 1 || !strings.Contains(result.SkippedEntries[0], "ghost") {
		t.Errorf("skipped = %v, want it reported rather than silent", result.SkippedEntries)
	}
}

// The store refuses a membership it cannot resolve, so a memberOf naming a
// group outside the search is dropped here and reported.
func TestUnresolvableMembershipIsDroppedAndReported(t *testing.T) {
	conn := &fakeConn{
		users: []*goldap.Entry{
			entry("uid=alice,ou=people,"+baseDN, map[string][]string{
				"entryuuid": {"u-alice"}, "uid": {"alice"},
				"memberOf": {"cn=lldap_admin,ou=groups," + baseDN},
			}),
		},
	}
	result, err := source(t, conn).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Snapshot.Users[0].GroupUUIDs) != 0 {
		t.Errorf("groups = %v, want the unresolvable one dropped", result.Snapshot.Users[0].GroupUUIDs)
	}
	if len(result.UnresolvedMemberships) != 1 {
		t.Errorf("unknown memberOf = %v, want it reported", result.UnresolvedMemberships)
	}
}

// LLDAP puts uid= in the RDN and other layouts use cn=. Both have to work.
func TestUIDFallsBackToTheRDN(t *testing.T) {
	for _, dn := range []string{"uid=carol,ou=people," + baseDN, "cn=carol,ou=people," + baseDN} {
		conn := &fakeConn{
			users: []*goldap.Entry{entry(dn, map[string][]string{"entryuuid": {"u-carol"}})},
		}
		result, err := source(t, conn).Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := result.Snapshot.Users[0].UID; got != "carol" {
			t.Errorf("%s: uid = %q, want carol", dn, got)
		}
	}
}

func TestVersionFromTheRootDSE(t *testing.T) {
	conn := &fakeConn{
		rootDSE: []*goldap.Entry{entry("", map[string][]string{
			"vendorName":    {"LLDAP"},
			"vendorVersion": {"lldap_0.6.3"},
		})},
	}
	version, err := source(t, conn).Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "LLDAP lldap_0.6.3" {
		t.Errorf("version = %q", version)
	}
}

func TestVersionIsEmptyRatherThanWrongWhenTheDirectoryIsSilent(t *testing.T) {
	version, err := source(t, &fakeConn{}).Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "" {
		t.Errorf("version = %q, want empty", version)
	}
}

func TestSearchFailuresPropagate(t *testing.T) {
	conn := &fakeConn{err: errors.New("size limit exceeded")}
	if _, err := source(t, conn).Sync(context.Background()); err == nil {
		t.Fatal("a failed search must not look like an empty directory")
	}
}

func TestNewValidatesTheURL(t *testing.T) {
	cases := []Config{
		{BaseDN: baseDN},
		{URL: "ldap://x", BaseDN: ""},
		// StartTLS is not implemented by LLDAP, so a bare host would have no
		// way to become secure and is refused outright.
		{URL: "lldap:3890", BaseDN: baseDN},
		{URL: "https://lldap:3890", BaseDN: baseDN},
	}
	for _, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) must fail", cfg)
		}
	}
	if _, err := New(Config{URL: "ldaps://lldap:6360", BaseDN: baseDN}); err != nil {
		t.Errorf("ldaps must be accepted: %v", err)
	}
}

func TestDefaultsAreLLDAPShaped(t *testing.T) {
	s, err := New(Config{URL: "ldap://lldap:3890", BaseDN: baseDN})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.UserFilter != DefaultUserFilter || s.cfg.GroupFilter != DefaultGroupFilter {
		t.Errorf("filters = %q, %q", s.cfg.UserFilter, s.cfg.GroupFilter)
	}
	if s.cfg.Timeout != DefaultTimeout {
		t.Errorf("timeout = %s", s.cfg.Timeout)
	}
}

func TestBindPasswordIsNotInAnError(t *testing.T) {
	// The password is a core.Secret, so even an error that embeds the config
	// cannot print it.
	cfg := Config{URL: "ldap://lldap:3890", BaseDN: baseDN, BindPassword: "hunter2"}
	if strings.Contains(strings.ToLower(errorsString(cfg)), "hunter2") {
		t.Error("the bind password leaked")
	}
}

func errorsString(cfg Config) string {
	return strings.Join([]string{
		core.RedactURL(cfg.URL),
		cfg.BindPassword.String(),
	}, " ")
}
