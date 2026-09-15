// SPDX-License-Identifier: AGPL-3.0-or-later

package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

// fake is a provider whose answers a test decides.
type fake struct {
	typ      string
	accounts []core.Account
	listErr  error
	health   core.HealthResult
	caps     *core.Capabilities
	writes   map[string]core.Quota
}

func (f *fake) Type() string { return f.typ }
func (f *fake) Capabilities() core.Capabilities {
	if f.caps != nil {
		return *f.caps
	}
	return core.Capabilities{CanReadUsers: true, CanReadUsage: true, CanSetUserQuota: true}
}
func (f *fake) Health(context.Context) core.HealthResult { return f.health }
func (f *fake) ListAccounts(context.Context) ([]core.Account, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.accounts, nil
}
func (f *fake) GetAccount(_ context.Context, id string) (core.Account, error) {
	for _, a := range f.accounts {
		if a.ExternalID == id {
			return a, nil
		}
	}
	return core.Account{}, errors.New("no such account")
}
func (f *fake) NormalizeQuota(q core.Quota) core.Quota { return q }
func (f *fake) SetQuota(_ context.Context, id string, q core.Quota) error {
	if f.writes == nil {
		f.writes = map[string]core.Quota{}
	}
	f.writes[id] = q
	return nil
}

type harness struct {
	db     *store.DB
	engine *Engine
	t      *testing.T
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "nuno.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(context.Background(), db, log); err != nil {
		t.Fatal(err)
	}
	return &harness{db: db, engine: New(db, log), t: t}
}

func (h *harness) instance(name, typ string, key core.MatchKey, provider core.Provider) Instance {
	h.t.Helper()
	id, err := h.db.UpsertProvider(context.Background(), core.ProviderInstance{
		Type: typ, Name: name, MatchKey: key, ConfigRef: "NUNO_PROVIDER_" + name,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	row, err := h.db.GetProviderByName(context.Background(), name)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = id
	return Instance{Row: row, Provider: provider}
}

func (h *harness) identity(users ...core.User) {
	h.t.Helper()
	if _, err := h.db.ReplaceIdentity(context.Background(), core.IdentitySnapshot{
		Source: core.SourceLDAP, Users: users,
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) userID(sourceUUID string) int64 {
	h.t.Helper()
	users, err := h.db.ListUsers(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, u := range users {
		if u.SourceUUID == sourceUUID {
			return u.ID
		}
	}
	h.t.Fatalf("no user %q", sourceUUID)
	return 0
}

func (h *harness) issues() map[core.LinkIssueKind][]core.LinkIssue {
	h.t.Helper()
	issues, err := h.db.ListLinkIssues(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	byKind := map[core.LinkIssueKind][]core.LinkIssue{}
	for _, issue := range issues {
		byKind[issue.Kind] = append(byKind[issue.Kind], issue)
	}
	return byKind
}

func account(externalID, email string) core.Account {
	return core.Account{
		ExternalID: externalID, Email: email, Username: externalID, Enabled: true,
		Quota: core.MustBytes(1073741824), Used: core.MustBytes(1024),
	}
}

// One provider failing degrades that provider and does not stop work on the
// others, and its accounts keep their last values.
func TestObserveDegradesOneProviderWithoutLosingTheOthers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.identity(core.User{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"})

	cloud := &fake{typ: "nextcloud", accounts: []core.Account{account("nc-alice", "alice@example.org")}}
	photos := &fake{typ: "immich", accounts: []core.Account{account("im-alice", "alice@example.org")}}
	instances := []Instance{
		h.instance("cloud", "nextcloud", core.MatchEmail, cloud),
		h.instance("photos", "immich", core.MatchEmail, photos),
	}

	if _, err := h.engine.Observe(ctx, instances); err != nil {
		t.Fatal(err)
	}

	// Now Immich breaks.
	photos.listErr = &core.UnreachableError{Provider: "immich", Err: errors.New("connection refused")}
	report, err := h.engine.Observe(ctx, instances)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("a failed provider must make the report not clean")
	}
	failed := report.Failed()
	if len(failed) != 1 || failed[0].Name != "photos" {
		t.Fatalf("failed = %+v", failed)
	}

	// The accounts of the failed provider are still there, from the earlier
	// cycle, which is what lets the page show them marked unavailable.
	row, err := h.db.GetProviderByName(ctx, "photos")
	if err != nil {
		t.Fatal(err)
	}
	if row.LastObserveOK {
		t.Error("the failed observe must be recorded as failed")
	}
	stored, err := h.db.ListExternalAccounts(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Errorf("got %d accounts, want the last known values kept", len(stored))
	}

	// The healthy provider still linked.
	cloudRow, _ := h.db.GetProviderByName(ctx, "cloud")
	links, _ := h.db.ListLinks(ctx)
	found := false
	for _, l := range links {
		if l.ProviderID == cloudRow.ID {
			found = true
		}
	}
	if !found {
		t.Error("the healthy provider must still have linked its accounts")
	}
}

func TestLinkMatchesReportsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.identity(
		core.User{SourceUUID: "u-alice", UID: "alice", Email: "Alice@Example.org"},
		core.User{SourceUUID: "u-bob", UID: "bob", Email: "bob@example.org"},
	)

	cloud := &fake{typ: "nextcloud", accounts: []core.Account{
		account("nc-alice", "alice@example.org"),
		// The local admin: matches nobody, and its email is empty.
		account("admin", ""),
	}}
	instances := []Instance{h.instance("cloud", "nextcloud", core.MatchEmail, cloud)}

	report, err := h.engine.Observe(ctx, instances)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("report = %+v", report)
	}
	if _, linked, _ := report.Totals(); linked != 1 {
		t.Errorf("linked = %d, want 1", linked)
	}

	byKind := h.issues()
	if len(byKind[core.IssueUnmanaged]) != 1 || byKind[core.IssueUnmanaged][0].ExternalID != "admin" {
		t.Errorf("unmanaged = %+v, want just the admin account (FR-3)", byKind[core.IssueUnmanaged])
	}
	// Bob has no account here. Nuno reports it and never provisions one.
	if len(byKind[core.IssueUnlinked]) != 1 {
		t.Errorf("unlinked = %+v, want bob", byKind[core.IssueUnlinked])
	}

	// Matching is case-insensitive on email, which is why alice matched at all.
	links, _ := h.db.ListLinks(ctx)
	if len(links) != 1 || links[0].ExternalID != "nc-alice" || links[0].Origin != core.LinkMatched {
		t.Fatalf("links = %+v", links)
	}

	// Running again changes nothing: no duplicate links, no accumulated issues.
	if _, err := h.engine.Observe(ctx, instances); err != nil {
		t.Fatal(err)
	}
	links, _ = h.db.ListLinks(ctx)
	if len(links) != 1 {
		t.Errorf("links = %d after a second cycle, want 1", len(links))
	}
	byKind = h.issues()
	if len(byKind[core.IssueUnmanaged]) != 1 || len(byKind[core.IssueUnlinked]) != 1 {
		t.Errorf("issues accumulated: %+v", byKind)
	}
}

// Ambiguity is never resolved by guessing, because writing to the wrong
// account is the worst thing Nuno can do (FR-7).
func TestAmbiguityProducesNoLink(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.identity(
		core.User{SourceUUID: "u-alice", UID: "alice", Email: "shared@example.org"},
		core.User{SourceUUID: "u-alicia", UID: "alicia", Email: "shared@example.org"},
	)
	cloud := &fake{typ: "nextcloud", accounts: []core.Account{account("nc-1", "shared@example.org")}}
	instances := []Instance{h.instance("cloud", "nextcloud", core.MatchEmail, cloud)}

	if _, err := h.engine.Observe(ctx, instances); err != nil {
		t.Fatal(err)
	}
	links, _ := h.db.ListLinks(ctx)
	if len(links) != 0 {
		t.Fatalf("links = %+v, want none: two people share the key", links)
	}
	byKind := h.issues()
	if len(byKind[core.IssueAmbiguous]) != 2 {
		t.Errorf("ambiguous = %+v, want one per person", byKind[core.IssueAmbiguous])
	}
	// The account belongs to nobody, so it is also unmanaged.
	if len(byKind[core.IssueUnmanaged]) != 1 {
		t.Errorf("unmanaged = %+v", byKind[core.IssueUnmanaged])
	}
}

// The subject join carries a link across providers, which recovers an account
// email cannot match (ADR-0021).
func TestSubjectJoinsAcrossProviders(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.identity(core.User{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"})

	const subject = "11111111-1111-4111-8111-111111111111"
	ncAlice := account(subject, "alice@example.org")
	ncAlice.Subject = subject

	// On Immich the same person has no email at all, so only the subject can
	// find her.
	imAlice := account("im-alice", "")
	imAlice.Subject = subject

	instances := []Instance{
		h.instance("cloud", "nextcloud", core.MatchEmail, &fake{typ: "nextcloud", accounts: []core.Account{ncAlice}}),
		h.instance("photos", "immich", core.MatchEmail, &fake{typ: "immich", accounts: []core.Account{imAlice}}),
	}
	if _, err := h.engine.Observe(ctx, instances); err != nil {
		t.Fatal(err)
	}

	links, _ := h.db.ListLinks(ctx)
	if len(links) != 2 {
		t.Fatalf("links = %+v, want both providers linked", links)
	}
	aliceID := h.userID("u-alice")
	for _, l := range links {
		if l.UserID != aliceID {
			t.Errorf("link %+v belongs to someone else", l)
		}
	}
	if len(h.issues()[core.IssueUnmanaged]) != 0 {
		t.Errorf("nothing should be unmanaged: %+v", h.issues())
	}
}

// The order providers are observed in must not decide the outcome.
func TestSubjectJoinDoesNotDependOnProviderOrder(t *testing.T) {
	ctx := context.Background()
	const subject = "22222222-2222-4222-8222-222222222222"

	for _, reversed := range []bool{false, true} {
		h := newHarness(t)
		h.identity(core.User{SourceUUID: "u-bob", UID: "bob", Email: "bob@example.org"})

		withEmail := account("nc-bob", "bob@example.org")
		withEmail.Subject = subject
		withoutEmail := account("im-bob", "")
		withoutEmail.Subject = subject

		instances := []Instance{
			h.instance("photos", "immich", core.MatchEmail, &fake{typ: "immich", accounts: []core.Account{withoutEmail}}),
			h.instance("cloud", "nextcloud", core.MatchEmail, &fake{typ: "nextcloud", accounts: []core.Account{withEmail}}),
		}
		if reversed {
			instances[0], instances[1] = instances[1], instances[0]
		}
		if _, err := h.engine.Observe(ctx, instances); err != nil {
			t.Fatal(err)
		}
		links, _ := h.db.ListLinks(ctx)
		if len(links) != 2 {
			t.Fatalf("reversed=%v: links = %+v, want 2", reversed, links)
		}
	}
}

func TestRevalidation(t *testing.T) {
	ctx := context.Background()

	t.Run("an account the provider stops reporting becomes dangling", func(t *testing.T) {
		h := newHarness(t)
		h.identity(core.User{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"})
		provider := &fake{typ: "nextcloud", accounts: []core.Account{account("nc-alice", "alice@example.org")}}
		instances := []Instance{h.instance("cloud", "nextcloud", core.MatchEmail, provider)}

		if _, err := h.engine.Observe(ctx, instances); err != nil {
			t.Fatal(err)
		}
		provider.accounts = nil
		if _, err := h.engine.Observe(ctx, instances); err != nil {
			t.Fatal(err)
		}

		byKind := h.issues()
		if len(byKind[core.IssueDangling]) != 1 {
			t.Fatalf("dangling = %+v, want one", byKind[core.IssueDangling])
		}
		// The link stays for history and is excluded, rather than aborting
		// the run (FR-9b).
		if links, _ := h.db.ListLinks(ctx); len(links) != 1 {
			t.Errorf("links = %+v, want the link kept and reported", links)
		}
	})

	t.Run("a matched link whose key moved becomes stale", func(t *testing.T) {
		h := newHarness(t)
		h.identity(core.User{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"})
		provider := &fake{typ: "nextcloud", accounts: []core.Account{account("nc-alice", "alice@example.org")}}
		instances := []Instance{h.instance("cloud", "nextcloud", core.MatchEmail, provider)}
		if _, err := h.engine.Observe(ctx, instances); err != nil {
			t.Fatal(err)
		}

		// The account's email changed to someone else's.
		provider.accounts = []core.Account{account("nc-alice", "someone.else@example.org")}
		if _, err := h.engine.Observe(ctx, instances); err != nil {
			t.Fatal(err)
		}
		byKind := h.issues()
		if len(byKind[core.IssueStale]) != 1 {
			t.Fatalf("stale = %+v, want one: an admin decides, Nuno does not retarget", byKind[core.IssueStale])
		}
	})

	t.Run("a manual link is reported, never auto-invalidated", func(t *testing.T) {
		h := newHarness(t)
		h.identity(core.User{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"})
		provider := &fake{typ: "nextcloud", accounts: []core.Account{account("nc-odd", "nothing@example.org")}}
		instances := []Instance{h.instance("cloud", "nextcloud", core.MatchEmail, provider)}

		// An admin linked these by hand, against the match key.
		if _, err := h.engine.Observe(ctx, instances); err != nil {
			t.Fatal(err)
		}
		if err := h.db.UpsertLink(ctx, core.AccountLink{
			UserID: h.userID("u-alice"), ProviderID: instances[0].Row.ID,
			ExternalID: "nc-odd", Origin: core.LinkManual,
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := h.engine.Observe(ctx, instances); err != nil {
			t.Fatal(err)
		}
		if len(h.issues()[core.IssueStale]) != 0 {
			t.Errorf("a manual link must not be marked stale: %+v", h.issues())
		}
		links, _ := h.db.ListLinks(ctx)
		if len(links) != 1 || links[0].Origin != core.LinkManual {
			t.Errorf("links = %+v, want the manual link intact", links)
		}
	})
}

// Disabled and soft-deleted accounts are skipped before linking and before
// writing (FR-8a).
func TestDisabledAndDeletedAccountsAreNeverLinked(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.identity(
		core.User{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"},
		core.User{SourceUUID: "u-bob", UID: "bob", Email: "bob@example.org"},
	)

	disabled := account("nc-alice", "alice@example.org")
	disabled.Enabled = false
	deleted := account("im-bob", "bob@example.org")
	deleted.Deleted = true

	instances := []Instance{
		h.instance("cloud", "nextcloud", core.MatchEmail, &fake{typ: "nextcloud", accounts: []core.Account{disabled}}),
		h.instance("photos", "immich", core.MatchEmail, &fake{typ: "immich", accounts: []core.Account{deleted}}),
	}
	report, err := h.engine.Observe(ctx, instances)
	if err != nil {
		t.Fatal(err)
	}
	if links, _ := h.db.ListLinks(ctx); len(links) != 0 {
		t.Fatalf("links = %+v, want none", links)
	}
	for _, p := range report.Providers {
		if p.Skipped != 1 {
			t.Errorf("%s skipped = %d, want 1", p.Name, p.Skipped)
		}
	}
	// They are not unmanaged either: an account nobody can write to is not a
	// match problem.
	if len(h.issues()[core.IssueUnmanaged]) != 0 {
		t.Errorf("unmanaged = %+v", h.issues()[core.IssueUnmanaged])
	}
}

// An orphaned person is excluded from the cycle but keeps their link and
// history (FR-9).
func TestOrphanedUsersAreExcluded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.identity(core.User{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"})
	provider := &fake{typ: "nextcloud", accounts: []core.Account{account("nc-alice", "alice@example.org")}}
	instances := []Instance{h.instance("cloud", "nextcloud", core.MatchEmail, provider)}
	if _, err := h.engine.Observe(ctx, instances); err != nil {
		t.Fatal(err)
	}

	// Alice leaves the directory.
	h.identity()
	if _, err := h.engine.Observe(ctx, instances); err != nil {
		t.Fatal(err)
	}

	links, _ := h.db.ListLinks(ctx)
	if len(links) != 1 {
		t.Errorf("links = %+v, want the link kept for history", links)
	}
	// Her account now matches nobody active, so it is unmanaged.
	if len(h.issues()[core.IssueUnmanaged]) != 1 {
		t.Errorf("unmanaged = %+v", h.issues())
	}
}

func TestProviderThatCannotReadUsersIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.identity(core.User{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"})

	caps := core.Capabilities{CanReadUsers: false}
	instances := []Instance{h.instance("odd", "zfs", core.MatchEmail, &fake{typ: "zfs", caps: &caps})}

	report, err := h.engine.Observe(ctx, instances)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Errorf("a missing capability degrades gracefully, it is not an error: %+v", report)
	}
	if report.Providers[0].Observed != 0 {
		t.Errorf("observed = %d", report.Providers[0].Observed)
	}
}

func TestObserveRecordsHealthWithoutTouchingWriteAccess(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	provider := &fake{
		typ:    "nextcloud",
		health: core.HealthResult{Reachable: true, Version: "34.0.4", InSupported: true},
	}
	instances := []Instance{h.instance("cloud", "nextcloud", core.MatchEmail, provider)}
	if _, err := h.engine.Observe(ctx, instances); err != nil {
		t.Fatal(err)
	}

	row, err := h.db.GetProviderByName(ctx, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	if row.Version != "34.0.4" || !row.Reachable {
		t.Errorf("row = %+v", row)
	}
	// A read never proves a write (ADR-0025).
	if row.WriteAccess != core.WriteUnproven {
		t.Errorf("write access = %v, want unproven", row.WriteAccess)
	}
	if row.LastObserveAt == nil || time.Since(*row.LastObserveAt) > time.Minute {
		t.Errorf("last observe at = %v", row.LastObserveAt)
	}
}

// fakeDirectory answers a sync from a script.
type fakeDirectory struct {
	read core.IdentityRead
	err  error
}

func (f fakeDirectory) Sync(context.Context) (core.IdentityRead, error) {
	return f.read, f.err
}
func (f fakeDirectory) Version(context.Context) (string, error) { return "LLDAP test", nil }

func TestSyncIdentityApplies(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	directory := fakeDirectory{read: core.IdentityRead{
		Snapshot: core.IdentitySnapshot{
			Source: core.SourceLDAP,
			Groups: []core.Group{{SourceUUID: "g-staff", Name: "staff"}},
			Users: []core.User{
				{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org", GroupUUIDs: []string{"g-staff"}},
			},
		},
		SkippedEntries:        []string{"uid=ghost,ou=people,dc=example,dc=org"},
		UnresolvedMemberships: []string{"cn=lldap_admin,ou=groups,dc=example,dc=org"},
	}}

	result, err := h.engine.SyncIdentity(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	if result.Users != 1 || result.Groups != 1 {
		t.Fatalf("result = %+v", result)
	}
	users, _ := h.db.ListUsers(ctx)
	if len(users) != 1 || len(users[0].GroupUUIDs) != 1 {
		t.Errorf("users = %+v", users)
	}
}

// A wrong filter or a base DN typo would otherwise orphan everyone at once.
func TestSyncIdentityRefusesToOrphanEveryone(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.identity(
		core.User{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"},
		core.User{SourceUUID: "u-bob", UID: "bob", Email: "bob@example.org"},
	)

	empty := fakeDirectory{read: core.IdentityRead{
		Snapshot: core.IdentitySnapshot{Source: core.SourceLDAP},
	}}
	if _, err := h.engine.SyncIdentity(ctx, empty); err == nil {
		t.Fatal("an empty directory answer must be refused while people are known")
	}

	users, _ := h.db.ListUsers(ctx)
	for _, u := range users {
		if u.Status != core.UserActive {
			t.Errorf("%s was orphaned anyway: %q", u.UID, u.Status)
		}
	}
}

// On a fresh install an empty directory is legitimate, not a failure.
func TestSyncIdentityAcceptsAnEmptyDirectoryOnAFreshInstall(t *testing.T) {
	h := newHarness(t)
	empty := fakeDirectory{read: core.IdentityRead{Snapshot: core.IdentitySnapshot{Source: core.SourceLDAP}}}
	if _, err := h.engine.SyncIdentity(context.Background(), empty); err != nil {
		t.Fatalf("no users and nobody known is not a problem: %v", err)
	}
}

func TestSyncIdentityWithNoDirectoryIsANoOp(t *testing.T) {
	h := newHarness(t)
	if _, err := h.engine.SyncIdentity(context.Background(), nil); err != nil {
		t.Fatalf("manual users exist with no directory at all (FR-2): %v", err)
	}
}

func TestSyncIdentityPropagatesAReadFailure(t *testing.T) {
	h := newHarness(t)
	broken := fakeDirectory{err: &core.UnreachableError{Provider: "ldap", Err: errors.New("no route to host")}}
	if _, err := h.engine.SyncIdentity(context.Background(), broken); err == nil {
		t.Fatal("a directory that cannot be read must not look like an empty one")
	}
}
