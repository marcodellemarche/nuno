// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

func provider(t *testing.T, db *DB, name, typ string) int64 {
	t.Helper()
	id, err := db.UpsertProvider(context.Background(), core.ProviderInstance{
		Type: typ, Name: name, MatchKey: core.MatchEmail, ConfigRef: "NUNO_PROVIDER_" + strings.ToUpper(name),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUpsertProviderIsIdempotentAndRefusesARetype(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)

	first := provider(t, db, "cloud", "nextcloud")
	again := provider(t, db, "cloud", "nextcloud")
	if first != again {
		t.Fatalf("ids %d and %d: the same configured instance must keep its row", first, again)
	}

	// Changing the type under one name would silently retarget every account
	// already linked to it.
	_, err := db.UpsertProvider(ctx, core.ProviderInstance{Type: "immich", Name: "cloud", MatchKey: core.MatchEmail})
	if err == nil {
		t.Fatal("changing a provider's type under the same name must be refused")
	}

	row, err := db.GetProviderByName(ctx, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	if row.WriteAccess != core.WriteUnproven {
		t.Errorf("write access = %v, want unproven before any probe", row.WriteAccess)
	}
	if row.Degraded() {
		t.Error("a fresh provider is not degraded")
	}
}

func TestProviderStateSurvivesAndStaysSeparate(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	id := provider(t, db, "cloud", "nextcloud")
	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	if err := db.SaveHealth(ctx, id, core.HealthResult{Reachable: true, Version: "34.0.4", InSupported: true}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetProviderByName(ctx, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	// A read never proves a write, so health must not move write access.
	if row.WriteAccess != core.WriteUnproven {
		t.Errorf("write access = %v after a health read, want unproven", row.WriteAccess)
	}
	if row.Version != "34.0.4" || !row.Reachable || !row.InSupported {
		t.Errorf("row = %+v", row)
	}

	if err := db.SaveWriteAccess(ctx, id, core.WriteYes, at); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveObserveResult(ctx, id, at, false, &core.UnreachableError{Provider: "nextcloud", Err: context.DeadlineExceeded}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDegraded(ctx, id, "oidc_login_default_quota is set"); err != nil {
		t.Fatal(err)
	}

	row, err = db.GetProviderByName(ctx, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	if row.WriteAccess != core.WriteYes || row.WriteProbedAt == nil || !row.WriteProbedAt.Equal(at) {
		t.Errorf("write access = %v at %v", row.WriteAccess, row.WriteProbedAt)
	}
	if row.LastObserveOK || row.LastObserveAt == nil || !strings.Contains(row.LastError, "unreachable") {
		t.Errorf("observe state = %+v", row)
	}
	if !row.Degraded() {
		t.Error("a named reason means degraded")
	}
}

func TestReplaceIdentity(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)

	snap := core.IdentitySnapshot{
		Source: core.SourceLDAP,
		Groups: []core.Group{{SourceUUID: "g-photographers", Name: "photographers"}},
		Users: []core.User{
			{SourceUUID: "u-alice", UID: "alice", Email: "Alice@Example.org ", DisplayName: "Alice", GroupUUIDs: []string{"g-photographers"}},
			{SourceUUID: "u-bob", UID: "bob", Email: "bob@example.org"},
		},
	}
	result, err := db.ReplaceIdentity(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if result.Users != 2 || result.Groups != 1 || result.Orphaned != 0 {
		t.Fatalf("result = %+v", result)
	}

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("got %d users", len(users))
	}
	alice := users[0]
	if alice.SourceUUID != "u-alice" {
		t.Fatalf("users are ordered by source uuid, got %v", users)
	}
	if len(alice.GroupUUIDs) != 1 || alice.GroupUUIDs[0] != "g-photographers" {
		t.Errorf("alice groups = %v", alice.GroupUUIDs)
	}

	// A rename keeps the row: identity is the uuid, uid and email are
	// attributes (FR-9a).
	aliceID := alice.ID
	snap.Users[0].UID = "alice.renamed"
	snap.Users[0].Email = "alice.new@example.org"
	if _, err := db.ReplaceIdentity(ctx, snap); err != nil {
		t.Fatal(err)
	}
	users, _ = db.ListUsers(ctx)
	if users[0].ID != aliceID {
		t.Errorf("a rename moved the row from %d to %d, which would lose every link and override", aliceID, users[0].ID)
	}
	if users[0].UID != "alice.renamed" {
		t.Errorf("uid = %q", users[0].UID)
	}

	// Disappearing from the directory means orphaned, not deleted (FR-9).
	snap.Users = snap.Users[:1]
	result, err = db.ReplaceIdentity(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if result.Orphaned != 1 {
		t.Errorf("orphaned = %d, want 1", result.Orphaned)
	}
	users, _ = db.ListUsers(ctx)
	if len(users) != 2 {
		t.Fatalf("an orphan keeps its row and its history, got %d users", len(users))
	}
	var bob core.User
	for _, u := range users {
		if u.SourceUUID == "u-bob" {
			bob = u
		}
	}
	if bob.Status != core.UserOrphaned {
		t.Errorf("bob status = %q, want orphaned", bob.Status)
	}

	// Coming back restores them.
	snap.Users = append(snap.Users, core.User{SourceUUID: "u-bob", UID: "bob", Email: "bob@example.org"})
	result, err = db.ReplaceIdentity(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if result.Restored != 1 {
		t.Errorf("restored = %d, want 1", result.Restored)
	}
}

// A membership naming a group the same read did not return means the
// directory answered inconsistently. Dropping it silently would move someone
// to the default tier in M2.
func TestReplaceIdentityRefusesAMembershipWithNoGroup(t *testing.T) {
	db := migrated(t)
	_, err := db.ReplaceIdentity(context.Background(), core.IdentitySnapshot{
		Source: core.SourceLDAP,
		Users:  []core.User{{SourceUUID: "u-alice", UID: "alice", GroupUUIDs: []string{"g-missing"}}},
	})
	if err == nil {
		t.Fatal("want an error naming the missing group")
	}
	if !strings.Contains(err.Error(), "g-missing") {
		t.Errorf("err = %v", err)
	}
}

func TestReplaceExternalAccounts(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	id := provider(t, db, "cloud", "nextcloud")
	observedAt := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	accounts := []core.Account{
		{ExternalID: "11111111-1111-4111-8111-111111111111", Subject: "11111111-1111-4111-8111-111111111111",
			Username: "11111111-1111-4111-8111-111111111111", Email: "Alice@Example.org",
			Enabled: true, Quota: core.MustBytes(53687091200), Used: core.MustBytes(94422551), ObservedAt: observedAt},
		{ExternalID: "admin", Username: "admin", Enabled: true,
			Quota: core.Unlimited(), Used: core.MustBytes(65575770), ObservedAt: observedAt},
		{ExternalID: "newcomer", Username: "newcomer", Enabled: true,
			Quota: core.MustBytes(1073741824), Used: core.MustBytes(0), NeverUsed: true, ObservedAt: observedAt},
	}
	if err := db.ReplaceExternalAccounts(ctx, id, accounts, observedAt); err != nil {
		t.Fatal(err)
	}

	stored, err := db.ListExternalAccounts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 {
		t.Fatalf("got %d accounts", len(stored))
	}
	byID := map[string]core.ExternalAccount{}
	for _, a := range stored {
		byID[a.ExternalID] = a
	}
	// Unlimited and unknown must not collide, which is why the tagged form is
	// stored rather than a nullable integer.
	if !byID["admin"].Quota.Equal(core.Unlimited()) {
		t.Errorf("admin quota = %v", byID["admin"].Quota)
	}
	if !byID["newcomer"].NeverUsed || !byID["newcomer"].Used.Equal(core.MustBytes(0)) {
		t.Errorf("newcomer = %+v, want a known zero marked never used", byID["newcomer"])
	}
	if byID["11111111-1111-4111-8111-111111111111"].UserID != nil {
		t.Error("an account nobody linked is unmanaged, so user_id is null")
	}

	// A vanished account is removed, so a link to it becomes dangling and is
	// reported rather than written to.
	if err := db.ReplaceExternalAccounts(ctx, id, accounts[:1], observedAt); err != nil {
		t.Fatal(err)
	}
	stored, _ = db.ListExternalAccounts(ctx, id)
	if len(stored) != 1 {
		t.Fatalf("got %d accounts, want the vanished ones gone", len(stored))
	}
}

func TestReplaceExternalAccountsRejectsImpossibleValues(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	id := provider(t, db, "photos", "immich")
	now := time.Now()

	// Usage cannot be unlimited (ADR-0021 point 8).
	err := db.ReplaceExternalAccounts(ctx, id, []core.Account{
		{ExternalID: "a", Used: core.Unlimited(), Quota: core.Unknown()},
	}, now)
	if err == nil {
		t.Error("an unlimited usage must be refused")
	}

	err = db.ReplaceExternalAccounts(ctx, id, []core.Account{{ExternalID: ""}}, now)
	if err == nil {
		t.Error("an account with no external id must be refused")
	}
}

func TestLinksAndIssues(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	cloud := provider(t, db, "cloud", "nextcloud")
	if _, err := db.ReplaceIdentity(ctx, core.IdentitySnapshot{
		Source: core.SourceLDAP,
		Users: []core.User{
			{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"},
			{SourceUUID: "u-bob", UID: "bob", Email: "bob@example.org"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	users, _ := db.ListUsers(ctx)
	alice, bob := users[0], users[1]

	if err := db.UpsertLink(ctx, core.AccountLink{
		UserID: alice.ID, ProviderID: cloud, ExternalID: "acct-1", Origin: core.LinkMatched,
	}); err != nil {
		t.Fatal(err)
	}
	// Re-running the same match must not fail or duplicate.
	if err := db.UpsertLink(ctx, core.AccountLink{
		UserID: alice.ID, ProviderID: cloud, ExternalID: "acct-1", Origin: core.LinkMatched,
	}); err != nil {
		t.Fatal(err)
	}

	// Two people on one account is the case the constraint exists for.
	err := db.UpsertLink(ctx, core.AccountLink{
		UserID: bob.ID, ProviderID: cloud, ExternalID: "acct-1", Origin: core.LinkManual,
	})
	if err == nil {
		t.Fatal("linking one account to a second person must be refused")
	}

	links, err := db.ListLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("got %d links", len(links))
	}

	deleted, err := db.DeleteLink(ctx, alice.ID, cloud)
	if err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	if again, _ := db.DeleteLink(ctx, alice.ID, cloud); again {
		t.Error("deleting a link that is gone must report nothing removed")
	}

	issues := []core.LinkIssue{
		{ProviderID: cloud, Kind: core.IssueUnmanaged, ExternalID: "admin", Detail: "matches nobody"},
		{ProviderID: cloud, Kind: core.IssueUnlinked, UserID: &bob.ID, Detail: "no account on this provider"},
	}
	if err := db.ReplaceLinkIssues(ctx, cloud, issues); err != nil {
		t.Fatal(err)
	}
	// Derived state is replaced, never accumulated: a stale issue sends an
	// admin to fix something already fixed.
	if err := db.ReplaceLinkIssues(ctx, cloud, issues); err != nil {
		t.Fatal(err)
	}
	stored, err := db.ListLinkIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("got %d issues, want 2", len(stored))
	}
	if err := db.ReplaceLinkIssues(ctx, cloud, nil); err != nil {
		t.Fatal(err)
	}
	if stored, _ = db.ListLinkIssues(ctx); len(stored) != 0 {
		t.Errorf("got %d issues, want them cleared", len(stored))
	}
}

func TestAdminKeys(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	const value = core.Secret("an-admin-key")

	if ok, err := db.CheckAdminKey(ctx, value); err != nil || ok {
		t.Fatalf("an unknown key must be rejected: %v, %v", ok, err)
	}
	if err := db.EnsureAdminKey(ctx, value, "env"); err != nil {
		t.Fatal(err)
	}
	// Restarting with the same key must not accumulate rows.
	if err := db.EnsureAdminKey(ctx, value, "env"); err != nil {
		t.Fatal(err)
	}
	if count, err := db.CountAdminKeys(ctx); err != nil || count != 1 {
		t.Fatalf("count = %d, %v", count, err)
	}
	if ok, err := db.CheckAdminKey(ctx, value); err != nil || !ok {
		t.Fatalf("the stored key must be accepted: %v, %v", ok, err)
	}
	if ok, _ := db.CheckAdminKey(ctx, core.Secret("")); ok {
		t.Error("an empty key must never be valid")
	}

	// The plaintext must not be in the database.
	var hash string
	if err := db.R.QueryRowContext(ctx, `SELECT hash FROM admin_keys`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "an-admin-key") {
		t.Error("the key value reached the database")
	}

	// Removing the variable from compose revokes it.
	revoked, err := db.RevokeEnvKeysExcept(ctx, core.Secret(""))
	if err != nil || revoked != 1 {
		t.Fatalf("revoked = %d, %v", revoked, err)
	}
	if ok, _ := db.CheckAdminKey(ctx, value); ok {
		t.Error("a revoked key must not be valid")
	}
	// Re-adding the same value brings it back rather than failing on the
	// unique hash.
	if err := db.EnsureAdminKey(ctx, value, "env"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.CheckAdminKey(ctx, value); !ok {
		t.Error("restoring the variable must restore access")
	}
	if err := db.EnsureAdminKey(ctx, core.Secret(""), "env"); err == nil {
		t.Error("an empty admin key must be refused rather than stored")
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)

	if _, ok, err := db.GetSetting(ctx, "absent"); err != nil || ok {
		t.Fatalf("ok = %v, %v", ok, err)
	}
	if err := db.SetSetting(ctx, "k", ""); err != nil {
		t.Fatal(err)
	}
	value, ok, err := db.GetSetting(ctx, "k")
	if err != nil || !ok || value != "" {
		t.Fatalf("an empty value that was set is not the same as absent: %q, %v, %v", value, ok, err)
	}
	if err := db.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if value, _, _ := db.GetSetting(ctx, "k"); value != "v" {
		t.Errorf("value = %q", value)
	}
}

func TestTheSecondMigrationIsApplied(t *testing.T) {
	target, err := TargetVersion()
	if err != nil {
		t.Fatal(err)
	}
	if target < 2 {
		t.Fatalf("TargetVersion() = %d, want the provider status migration included", target)
	}
	db := migrated(t)
	version, err := DBVersion(context.Background(), db.R)
	if err != nil {
		t.Fatal(err)
	}
	if version != target {
		t.Errorf("database is at %d, want %d", version, target)
	}
}

func TestManualUsers(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)

	alice, err := db.AddManualUser(ctx, core.User{UID: "alice", Email: "alice@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if alice.Source != core.SourceManual || alice.SourceUUID == "" || alice.ID == 0 {
		t.Fatalf("user = %+v, want a manual user with a generated uuid", alice)
	}
	if alice.DisplayName != "alice" {
		t.Errorf("display name = %q, want the uid as a fallback", alice.DisplayName)
	}

	// Two people with one uid would be ambiguous everywhere downstream.
	if _, err := db.AddManualUser(ctx, core.User{UID: "Alice"}); err == nil {
		t.Error("a duplicate uid must be refused, case-insensitively")
	}
	if _, err := db.AddManualUser(ctx, core.User{UID: "  "}); err == nil {
		t.Error("a blank uid must be refused")
	}

	// A directory sync must not orphan someone who exists in no directory.
	if _, err := db.ReplaceIdentity(ctx, core.IdentitySnapshot{
		Source: core.SourceLDAP,
		Users:  []core.User{{SourceUUID: "u-bob", UID: "bob"}},
	}); err != nil {
		t.Fatal(err)
	}
	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Source == core.SourceManual && u.Status != core.UserActive {
			t.Errorf("the manual user was orphaned by an unrelated sync: %+v", u)
		}
	}

	deleted, err := db.DeleteUser(ctx, alice.ID)
	if err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	if again, _ := db.DeleteUser(ctx, alice.ID); again {
		t.Error("deleting someone already gone must report nothing removed")
	}
}

func TestPolicyRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	cloud := provider(t, db, "cloud", "nextcloud")
	photos := provider(t, db, "photos", "immich")

	standard := core.Tier{
		Name:      "standard",
		Budget:    core.MustBytes(100 << 30),
		IsDefault: true,
		Allocations: []core.Allocation{
			{ProviderID: cloud, Mode: core.ModePercent, Value: 25},
			{ProviderID: photos, Mode: core.ModePercent, Value: 75},
		},
	}
	tierID, err := db.SaveTier(ctx, standard)
	if err != nil {
		t.Fatal(err)
	}

	// A tier that cannot resolve honestly is refused here, not later.
	_, err = db.SaveTier(ctx, core.Tier{
		Name: "broken", Budget: core.MustBytes(10 << 30),
		Allocations: []core.Allocation{{ProviderID: cloud, Mode: core.ModeAbsolute, Value: 11 << 30}},
	})
	if err == nil {
		t.Error("absolute allocations above the budget must be refused at write time")
	}

	policy, err := db.LoadPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Tiers) != 1 {
		t.Fatalf("tiers = %+v", policy.Tiers)
	}
	loaded := policy.Tiers[tierID]
	if !loaded.Budget.Equal(core.MustBytes(100<<30)) || len(loaded.Allocations) != 2 {
		t.Errorf("tier = %+v", loaded)
	}
	if policy.DefaultTierID == nil || *policy.DefaultTierID != tierID {
		t.Errorf("default = %v", policy.DefaultTierID)
	}

	// Saving again with one allocation removed means the tier is now silent
	// about that provider, which is not the same as zero.
	standard.Allocations = standard.Allocations[:1]
	if _, err := db.SaveTier(ctx, standard); err != nil {
		t.Fatal(err)
	}
	policy, _ = db.LoadPolicy(ctx)
	if got := len(policy.Tiers[tierID].Allocations); got != 1 {
		t.Errorf("allocations = %d, want the removed one gone rather than zeroed", got)
	}
	if _, allocates := policy.Tiers[tierID].AllocationFor(photos); allocates {
		t.Error("a removed allocation must read as absent")
	}
}

func TestOnlyOneDefaultTier(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)

	first, err := db.SaveTier(ctx, core.Tier{Name: "a", IsDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	// The schema refuses a second default outright.
	if _, err := db.SaveTier(ctx, core.Tier{Name: "b", IsDefault: true}); err == nil {
		t.Error("two default tiers must be impossible")
	}

	second, err := db.SaveTier(ctx, core.Tier{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetDefaultTier(ctx, second); err != nil {
		t.Fatal(err)
	}
	policy, _ := db.LoadPolicy(ctx)
	if policy.DefaultTierID == nil || *policy.DefaultTierID != second {
		t.Errorf("default = %v, want it moved", policy.DefaultTierID)
	}
	if policy.Tiers[first].IsDefault {
		t.Error("the old default must have been cleared")
	}
	if err := db.SetDefaultTier(ctx, 999); err == nil {
		t.Error("a tier that does not exist cannot be the default")
	}
}

func TestGroupAndUserOverrides(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	cloud := provider(t, db, "cloud", "nextcloud")

	if _, err := db.ReplaceIdentity(ctx, core.IdentitySnapshot{
		Source: core.SourceLDAP,
		Groups: []core.Group{{SourceUUID: "g-staff", Name: "staff"}},
		Users:  []core.User{{SourceUUID: "u-alice", UID: "alice", GroupUUIDs: []string{"g-staff"}}},
	}); err != nil {
		t.Fatal(err)
	}
	users, _ := db.ListUsers(ctx)
	alice := users[0]

	tierID, err := db.SaveTier(ctx, core.Tier{
		Name: "staff", Budget: core.MustBytes(50 << 30),
		Allocations: []core.Allocation{{ProviderID: cloud, Mode: core.ModeAbsolute, Value: 50 << 30}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.MapGroupToTier(ctx, "g-staff", tierID); err != nil {
		t.Fatal(err)
	}
	// Mapping a group the directory has not returned is refused, with the
	// command that fixes it.
	if err := db.MapGroupToTier(ctx, "g-unknown", tierID); err == nil {
		t.Error("an unknown group must be refused")
	}

	policy, _ := db.LoadPolicy(ctx)
	if policy.GroupTiers["g-staff"] != tierID {
		t.Errorf("group tiers = %+v", policy.GroupTiers)
	}

	if err := db.SetUserTierOverride(ctx, alice.ID, &tierID); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProviderOverride(ctx, alice.ID, cloud, core.MustBytes(1<<40), OverrideAdopted); err != nil {
		t.Fatal(err)
	}
	// An override is an instruction, and there is no instruction meaning
	// "write a value nobody knows".
	if err := db.SetProviderOverride(ctx, alice.ID, cloud, core.Unknown(), OverrideManual); err == nil {
		t.Error("an unknown override must be refused")
	}

	up, origins, err := db.UserPolicy(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if up.TierOverride == nil || *up.TierOverride != tierID {
		t.Errorf("tier override = %v", up.TierOverride)
	}
	if got := up.ProviderOverrides[cloud]; !got.Equal(core.MustBytes(1 << 40)) {
		t.Errorf("provider override = %v", got)
	}
	if origins[cloud] != OverrideAdopted {
		t.Errorf("origin = %q, want it distinguishable from a hand-set one", origins[cloud])
	}

	// Resolution over what was stored must produce the override.
	resolution := core.Resolve(alice, cloud, policy, up)
	if resolution.Rule != core.RuleProviderOverride || !resolution.Ceiling.Equal(core.MustBytes(1<<40)) {
		t.Errorf("resolution = %+v", resolution)
	}

	cleared, err := db.ClearProviderOverride(ctx, alice.ID, cloud)
	if err != nil || !cleared {
		t.Fatalf("clear = %v, %v", cleared, err)
	}
	if err := db.SetUserTierOverride(ctx, alice.ID, nil); err != nil {
		t.Fatal(err)
	}
	up, _, _ = db.UserPolicy(ctx, alice.ID)
	if up.TierOverride != nil || len(up.ProviderOverrides) != 0 {
		t.Errorf("overrides = %+v, want them cleared", up)
	}

	// Removing a group mapping leaves the tier and the group alone.
	unmapped, err := db.UnmapGroup(ctx, "g-staff")
	if err != nil || !unmapped {
		t.Fatalf("unmap = %v, %v", unmapped, err)
	}
	policy, _ = db.LoadPolicy(ctx)
	if len(policy.GroupTiers) != 0 || len(policy.Tiers) != 1 {
		t.Errorf("policy = %+v", policy)
	}
}

func TestIssueAndRevokeAdminKeys(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)

	value, err := db.IssueAdminKey(ctx, "homepage widget")
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Reveal()) != 64 {
		t.Errorf("a key is 32 random bytes hex encoded, got %d characters", len(value.Reveal()))
	}
	if ok, _ := db.CheckAdminKey(ctx, value); !ok {
		t.Fatal("the issued key must work")
	}

	// Two keys are never the same, and the value is not in the database.
	second, err := db.IssueAdminKey(ctx, "another")
	if err != nil {
		t.Fatal(err)
	}
	if second.Reveal() == value.Reveal() {
		t.Fatal("two issued keys must differ")
	}
	var stored string
	if err := db.R.QueryRowContext(ctx, `SELECT group_concat(hash) FROM admin_keys`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, value.Reveal()) {
		t.Error("the key value reached the database")
	}

	keys, err := db.ListAdminKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Label != "homepage widget" || keys[0].Source != AdminKeyFromUI {
		t.Fatalf("keys = %+v", keys)
	}

	if err := db.RevokeAdminKey(ctx, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.CheckAdminKey(ctx, value); ok {
		t.Error("a revoked key must stop working")
	}

	// An env key is not revocable here: removing the variable is how that one
	// goes, so recovery from the compose file always works.
	if err := db.EnsureAdminKey(ctx, core.Secret("from-env"), "NUNO_ADMIN_KEY"); err != nil {
		t.Fatal(err)
	}
	keys, _ = db.ListAdminKeys(ctx)
	var envKey AdminKeyRow
	for _, key := range keys {
		if key.Source == AdminKeyFromEnv {
			envKey = key
		}
	}
	if err := db.RevokeAdminKey(ctx, envKey.ID); err == nil {
		t.Error("revoking an env key must be refused, naming what to do instead")
	}
	if err := db.RevokeAdminKey(ctx, 9999); err == nil {
		t.Error("a key that does not exist cannot be revoked")
	}
}
