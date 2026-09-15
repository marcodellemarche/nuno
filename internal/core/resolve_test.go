// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"strings"
	"testing"
)

const (
	nextcloudID = int64(10)
	immichID    = int64(20)
)

func tier(id int64, name string, budget Quota, allocations ...Allocation) Tier {
	return Tier{ID: id, Name: name, Budget: budget, Allocations: allocations}
}

func alloc(providerID int64, mode AllocationMode, value int64) Allocation {
	return Allocation{ProviderID: providerID, Mode: mode, Value: value}
}

// The worked example from ADR-0012, which is the case whole-tier resolution
// got wrong: promoting a photographer into staff must not drop their Immich
// ceiling from 100 GiB to 12 GiB.
func TestResolutionIsPerProviderNotPerTier(t *testing.T) {
	photographers := tier(1, "photographers", MustBytes(100<<30), alloc(immichID, ModePercent, 100))
	staff := tier(2, "staff", MustBytes(120<<30),
		alloc(nextcloudID, ModePercent, 25),
		alloc(immichID, ModePercent, 10),
	)
	policy := Policy{
		Tiers:      map[int64]Tier{1: photographers, 2: staff},
		GroupTiers: map[string]int64{"g-photographers": 1, "g-staff": 2},
	}

	photographer := User{ID: 1, UID: "alice", GroupUUIDs: []string{"g-photographers"}}
	promoted := User{ID: 1, UID: "alice", GroupUUIDs: []string{"g-photographers", "g-staff"}}

	before := Resolve(photographer, immichID, policy, UserPolicy{})
	after := Resolve(promoted, immichID, policy, UserPolicy{})

	if !before.Present || !before.Ceiling.Equal(MustBytes(100<<30)) {
		t.Fatalf("before = %+v", before)
	}
	if !after.Present || !after.Ceiling.Equal(MustBytes(100<<30)) {
		t.Fatalf("after joining staff the Immich ceiling = %v, want it kept: the most generous per provider wins", after.Ceiling)
	}
	if after.TierName != "photographers" {
		t.Errorf("winning tier = %q", after.TierName)
	}

	// And the promotion does add what staff offers where photographers is
	// silent.
	nextcloud := Resolve(promoted, nextcloudID, policy, UserPolicy{})
	if !nextcloud.Present || !nextcloud.Ceiling.Equal(MustBytes(30<<30)) {
		t.Errorf("nextcloud = %+v, want 25 percent of 120 GiB", nextcloud)
	}
}

// Absent is not zero and not unlimited. It is the single most dangerous
// ambiguity in the model (FR-17).
func TestAnAbsentAllocationEmitsNothing(t *testing.T) {
	onlyImmich := tier(1, "photos", MustBytes(100<<30), alloc(immichID, ModeAbsolute, 50<<30))
	policy := Policy{
		Tiers:         map[int64]Tier{1: onlyImmich},
		DefaultTierID: ptrTo(int64(1)),
	}

	got := Resolve(User{ID: 1, UID: "alice"}, nextcloudID, policy, UserPolicy{})
	if got.Present {
		t.Fatalf("resolution = %+v, want absent", got)
	}
	if got.Ceiling.IsKnown() {
		t.Errorf("ceiling = %v, want nothing at all", got.Ceiling)
	}
	if got.Rule != RuleNone {
		t.Errorf("rule = %q", got.Rule)
	}
}

func TestUnlimitedIsTheTopElement(t *testing.T) {
	bounded := tier(1, "bounded", MustBytes(1<<40), alloc(immichID, ModeAbsolute, 1<<40))
	unbounded := tier(2, "unbounded", Unknown(), alloc(immichID, ModeUnlimited, 0))
	policy := Policy{
		Tiers:      map[int64]Tier{1: bounded, 2: unbounded},
		GroupTiers: map[string]int64{"g-a": 1, "g-b": 2},
	}
	user := User{ID: 1, UID: "alice", GroupUUIDs: []string{"g-a", "g-b"}}

	got := Resolve(user, immichID, policy, UserPolicy{})
	if !got.Present || !got.Ceiling.IsUnlimited() {
		t.Fatalf("resolution = %+v, want unlimited", got)
	}
	if got.TierName != "unbounded" {
		t.Errorf("tier = %q", got.TierName)
	}
	// A tier meant to be unbounded has no meaningful budget, and under
	// whole-tier resolution it competed as zero and lost to every bounded
	// tier. That is the bug ADR-0012 fixed.
	if len(got.Candidates) != 2 {
		t.Errorf("candidates = %+v, want both recorded", got.Candidates)
	}
}

func TestPrecedence(t *testing.T) {
	groupTier := tier(1, "group", MustBytes(10<<30), alloc(nextcloudID, ModeAbsolute, 10<<30))
	userTier := tier(2, "user", MustBytes(20<<30), alloc(nextcloudID, ModeAbsolute, 20<<30))
	defaultTier := tier(3, "default", MustBytes(1<<30), alloc(nextcloudID, ModeAbsolute, 1<<30))
	policy := Policy{
		Tiers:         map[int64]Tier{1: groupTier, 2: userTier, 3: defaultTier},
		GroupTiers:    map[string]int64{"g": 1},
		DefaultTierID: ptrTo(int64(3)),
	}
	user := User{ID: 1, UID: "alice", GroupUUIDs: []string{"g"}}

	cases := []struct {
		name     string
		up       UserPolicy
		want     Quota
		wantRule ResolutionRule
	}{
		{"groups beat the default", UserPolicy{}, MustBytes(10 << 30), RuleGroupTier},
		{
			"a user tier override replaces the group set entirely",
			UserPolicy{TierOverride: ptrTo(int64(2))},
			MustBytes(20 << 30), RuleUserTier,
		},
		{
			"a provider override beats every tier",
			UserPolicy{
				TierOverride:      ptrTo(int64(2)),
				ProviderOverrides: map[int64]Quota{nextcloudID: MustBytes(99 << 30)},
			},
			MustBytes(99 << 30), RuleProviderOverride,
		},
		{
			"an unlimited provider override is honoured",
			UserPolicy{ProviderOverrides: map[int64]Quota{nextcloudID: Unlimited()}},
			Unlimited(), RuleProviderOverride,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Resolve(user, nextcloudID, policy, c.up)
			if !got.Present || !got.Ceiling.Equal(c.want) {
				t.Fatalf("ceiling = %v, want %v", got.Ceiling, c.want)
			}
			if got.Rule != c.wantRule {
				t.Errorf("rule = %q, want %q", got.Rule, c.wantRule)
			}
		})
	}

	// Someone in no mapped group falls to the default.
	stranger := User{ID: 2, UID: "bob"}
	got := Resolve(stranger, nextcloudID, policy, UserPolicy{})
	if got.Rule != RuleDefaultTier || !got.Ceiling.Equal(MustBytes(1<<30)) {
		t.Errorf("stranger = %+v, want the default tier", got)
	}
}

// The mixed-mode rule: absolute allocations come out of the budget first,
// then percentages apply to the remainder.
func TestMixedModes(t *testing.T) {
	mixed := tier(1, "mixed", MustBytes(100<<30),
		alloc(nextcloudID, ModeAbsolute, 20<<30),
		alloc(immichID, ModePercent, 50),
	)
	policy := Policy{Tiers: map[int64]Tier{1: mixed}, DefaultTierID: ptrTo(int64(1))}
	user := User{ID: 1, UID: "alice"}

	nextcloud := Resolve(user, nextcloudID, policy, UserPolicy{})
	if !nextcloud.Ceiling.Equal(MustBytes(20 << 30)) {
		t.Errorf("nextcloud = %v", nextcloud.Ceiling)
	}
	immich := Resolve(user, immichID, policy, UserPolicy{})
	if !immich.Ceiling.Equal(MustBytes(40 << 30)) {
		t.Errorf("immich = %v, want half of the 80 GiB left after the absolute allocation", immich.Ceiling)
	}
}

func TestTierValidation(t *testing.T) {
	cases := []struct {
		name string
		tier Tier
		ok   bool
	}{
		{"absolute only, no budget needed", tier(1, "t", Unknown(), alloc(immichID, ModeAbsolute, 1<<30)), true},
		{"unlimited only, no budget needed", tier(1, "t", Unknown(), alloc(immichID, ModeUnlimited, 0)), true},
		{"percent with a budget", tier(1, "t", MustBytes(1<<30), alloc(immichID, ModePercent, 50)), true},
		{"no name", tier(1, "", MustBytes(1<<30)), false},
		{"percent with no budget to resolve against", tier(1, "t", Unknown(), alloc(immichID, ModePercent, 50)), false},
		{"percent above 100", tier(1, "t", MustBytes(1<<30), alloc(immichID, ModePercent, 101)), false},
		{"negative absolute", tier(1, "t", MustBytes(1<<30), alloc(immichID, ModeAbsolute, -1)), false},
		{"two allocations for one provider", tier(1, "t", MustBytes(1<<30),
			alloc(immichID, ModeAbsolute, 1), alloc(immichID, ModeAbsolute, 2)), false},
		{"unknown mode", tier(1, "t", MustBytes(1<<30), alloc(immichID, "share", 1)), false},
		{
			// Nextcloud reads a negative as a sentinel, so this must be
			// refused at write time rather than clamped later.
			"absolute allocations above the budget",
			tier(1, "t", MustBytes(10<<30), alloc(immichID, ModeAbsolute, 11<<30)),
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.tier.Validate()
			if (err == nil) != c.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

// Over-committing is legal and deliberate, so it warns rather than failing.
func TestOvercommitWarnsRatherThanFailing(t *testing.T) {
	over := tier(1, "over", MustBytes(100<<30),
		alloc(nextcloudID, ModePercent, 75),
		alloc(immichID, ModePercent, 75),
	)
	if err := over.Validate(); err != nil {
		t.Fatalf("over-commit is legal: %v", err)
	}
	if !over.Overcommitted() {
		t.Error("it must still be reported")
	}

	policy := Policy{Tiers: map[int64]Tier{1: over}, DefaultTierID: ptrTo(int64(1))}
	got := Resolve(User{ID: 1, UID: "alice"}, immichID, policy, UserPolicy{})
	if len(got.Notes) == 0 || !strings.Contains(got.Notes[0], "over-commit") {
		t.Errorf("notes = %v, want the plan to say so", got.Notes)
	}
}

func TestResolveRecordsTheWholeChain(t *testing.T) {
	small := tier(1, "small", MustBytes(10<<30), alloc(immichID, ModeAbsolute, 10<<30))
	large := tier(2, "large", MustBytes(50<<30), alloc(immichID, ModeAbsolute, 50<<30))
	silent := tier(3, "silent", MustBytes(50<<30), alloc(nextcloudID, ModeAbsolute, 1<<30))
	policy := Policy{
		Tiers:      map[int64]Tier{1: small, 2: large, 3: silent},
		GroupTiers: map[string]int64{"g-a": 1, "g-b": 2, "g-c": 3},
	}
	user := User{ID: 1, UID: "alice", GroupUUIDs: []string{"g-a", "g-b", "g-c"}}

	got := Resolve(user, immichID, policy, UserPolicy{})
	if got.TierName != "large" {
		t.Errorf("winner = %q", got.TierName)
	}
	if len(got.Candidates) != 3 {
		t.Fatalf("candidates = %+v, want all three considered", got.Candidates)
	}
	// nuno explain prints this chain, so the tier that offered nothing has to
	// appear as having offered nothing.
	var silentCandidate Candidate
	for _, c := range got.Candidates {
		if c.TierName == "silent" {
			silentCandidate = c
		}
	}
	if silentCandidate.Allocates {
		t.Errorf("candidate = %+v, want it recorded as allocating nothing here", silentCandidate)
	}
}

func TestAGroupMappedToAMissingTierIsSaidOutLoud(t *testing.T) {
	policy := Policy{Tiers: map[int64]Tier{}, GroupTiers: map[string]int64{"g": 99}}
	got := Resolve(User{ID: 1, UID: "alice", GroupUUIDs: []string{"g"}}, immichID, policy, UserPolicy{})
	if got.Present {
		t.Error("a missing tier offers nothing")
	}
	if len(got.Notes) == 0 || !strings.Contains(got.Notes[0], "does not exist") {
		t.Errorf("notes = %v", got.Notes)
	}
}

func TestEffectiveBudget(t *testing.T) {
	cases := []struct {
		name        string
		resolutions []Resolution
		want        Quota
	}{
		{"sum of ceilings", []Resolution{
			{Present: true, Ceiling: MustBytes(10)},
			{Present: true, Ceiling: MustBytes(15)},
		}, MustBytes(25)},
		{"unlimited wins", []Resolution{
			{Present: true, Ceiling: MustBytes(10)},
			{Present: true, Ceiling: Unlimited()},
		}, Unlimited()},
		{"absent contributes nothing", []Resolution{
			{Present: true, Ceiling: MustBytes(10)},
			{Present: false},
		}, MustBytes(10)},
		{"nothing resolved at all", []Resolution{{Present: false}}, Unknown()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffectiveBudget(c.resolutions); !got.Equal(c.want) {
				t.Errorf("EffectiveBudget() = %v, want %v", got, c.want)
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }
