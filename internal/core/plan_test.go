// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"strings"
	"testing"
)

func planAccount(desired, observed, used Quota) PlanAccount {
	return PlanAccount{
		User:       User{ID: 1, UID: "alice"},
		ProviderID: nextcloudID,
		Provider:   "cloud",
		ExternalID: "nc-alice",
		Desired:    desired,
		Present:    true,
		Observed:   observed,
		Used:       used,
		ObserveOK:  true,
		Rule:       RuleGroupTier,
		TierName:   "staff",
	}
}

func TestBuildPlanEmitsOnlyRealDifferences(t *testing.T) {
	plan := BuildPlan([]PlanAccount{
		// Already right.
		planAccount(MustBytes(50<<30), MustBytes(50<<30), MustBytes(1<<30)),
	})
	if !plan.Empty() {
		t.Fatalf("plan = %+v, want empty", plan.Changes)
	}

	plan = BuildPlan([]PlanAccount{
		planAccount(MustBytes(100<<30), MustBytes(50<<30), MustBytes(1<<30)),
	})
	if len(plan.Changes) != 1 {
		t.Fatalf("changes = %+v", plan.Changes)
	}
	change := plan.Changes[0]
	if change.Class != ClassSafe {
		t.Errorf("class = %q", change.Class)
	}
	if !change.From.Equal(MustBytes(50<<30)) || !change.To.Equal(MustBytes(100<<30)) {
		t.Errorf("change = %+v", change)
	}
	if change.Rule != RuleGroupTier || change.TierName != "staff" {
		t.Errorf("a change must carry why it exists: %+v", change)
	}
}

// Unlimited and a byte count are different values, and so are unlimited and
// unknown. A plan that treated them as equal would either never converge or
// write the most destructive value available.
func TestBuildPlanDistinguishesTheQuotaKinds(t *testing.T) {
	plan := BuildPlan([]PlanAccount{
		planAccount(Unlimited(), MustBytes(50<<30), MustBytes(1<<30)),
	})
	if len(plan.Changes) != 1 || plan.Changes[0].Class != ClassSafe {
		t.Fatalf("raising a bounded account to unlimited is a safe change: %+v", plan.Changes)
	}

	plan = BuildPlan([]PlanAccount{
		planAccount(Unlimited(), Unlimited(), MustBytes(1<<30)),
	})
	if !plan.Empty() {
		t.Errorf("unlimited to unlimited is not a change: %+v", plan.Changes)
	}
}

// An absent allocation emits no change at all. This is one of the four
// properties ARCHITECTURE section 10 requires a dedicated test for.
func TestAnAbsentAllocationEmitsNoChange(t *testing.T) {
	account := planAccount(Unknown(), MustBytes(50<<30), MustBytes(1<<30))
	account.Present = false

	plan := BuildPlan([]PlanAccount{account})
	if !plan.Empty() {
		t.Fatalf("plan = %+v, want nothing emitted for a provider no tier allocates", plan.Changes)
	}
	if plan.Unallocated != 1 {
		t.Errorf("unallocated = %d", plan.Unallocated)
	}
}

// A shrink at or below current usage is never applied without consent. The
// comparison is target <= used, because a quota exactly equal to usage already
// blocks uploads.
func TestShrinkBelowUsageIsGuarded(t *testing.T) {
	cases := []struct {
		name  string
		to    int64
		used  int64
		class ChangeClass
	}{
		{"below usage", 1 << 30, 5 << 30, ClassShrinkBelowUsage},
		{"exactly at usage", 5 << 30, 5 << 30, ClassShrinkBelowUsage},
		{"just above usage", 5<<30 + 1, 5 << 30, ClassSafe},
		{"a shrink that still leaves room", 10 << 30, 5 << 30, ClassSafe},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := BuildPlan([]PlanAccount{
				planAccount(MustBytes(c.to), MustBytes(50<<30), MustBytes(c.used)),
			})
			if len(plan.Changes) != 1 {
				t.Fatalf("changes = %+v", plan.Changes)
			}
			if got := plan.Changes[0].Class; got != c.class {
				t.Errorf("class = %q, want %q", got, c.class)
			}
		})
	}

	guarded := BuildPlan([]PlanAccount{
		planAccount(MustBytes(1<<30), MustBytes(50<<30), MustBytes(5<<30)),
	})
	if len(guarded.Applicable(false)) != 0 {
		t.Error("a shrink must not be applicable without consent")
	}
	if len(guarded.Applicable(true)) != 1 {
		t.Error("with consent it becomes applicable")
	}
	if !strings.Contains(guarded.Changes[0].Detail, "blocks uploads") {
		t.Errorf("detail = %q, want it to say what happens", guarded.Changes[0].Detail)
	}
}

// A change against unknown usage is never applied, with or without consent.
// The fourth dedicated property (ADR-0016).
func TestUnknownStateIsNeverApplicable(t *testing.T) {
	cases := []struct {
		name    string
		account PlanAccount
	}{
		{"usage could not be trusted", planAccount(MustBytes(1<<30), MustBytes(50<<30), Unknown())},
		{"the ceiling could not be read", planAccount(MustBytes(1<<30), Unknown(), MustBytes(1<<30))},
		{"the account was not observed", func() PlanAccount {
			a := planAccount(MustBytes(1<<30), MustBytes(50<<30), MustBytes(1<<30))
			a.ObserveOK = false
			return a
		}()},
		{"the ceiling could not be expressed", planAccount(Unknown(), MustBytes(50<<30), MustBytes(1<<30))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := BuildPlan([]PlanAccount{c.account})
			if len(plan.Changes) != 1 {
				t.Fatalf("an unknown state must be reported, not dropped: %+v", plan)
			}
			if plan.Changes[0].Class != ClassUnknownState {
				t.Fatalf("class = %q", plan.Changes[0].Class)
			}
			if len(plan.Applicable(false)) != 0 || len(plan.Applicable(true)) != 0 {
				t.Error("unknown state must not be applicable, with or without consent")
			}
			if plan.Changes[0].Class.Consentable() {
				t.Error("there is no consent for unknown state, and no flag for it")
			}
		})
	}
}

// A person who has never logged in has a known zero, so they get their quota
// before they first open the service (FR-38d, ADR-0024).
func TestANeverUsedAccountIsWritable(t *testing.T) {
	account := planAccount(MustBytes(50<<30), MustBytes(1<<30), MustBytes(0))
	account.NeverUsed = true

	plan := BuildPlan([]PlanAccount{account})
	if len(plan.Changes) != 1 || plan.Changes[0].Class != ClassSafe {
		t.Fatalf("changes = %+v, want one safe change", plan.Changes)
	}
	if len(plan.Applicable(false)) != 1 {
		t.Error("it must apply without any consent: the whole point is the quota waiting for them")
	}

	// Zero is the one target the guardrail still catches, because it blocks
	// every upload.
	account.Desired = MustBytes(0)
	plan = BuildPlan([]PlanAccount{account})
	if plan.Changes[0].Class != ClassShrinkBelowUsage {
		t.Errorf("class = %q, want the guardrail to catch a zero target", plan.Changes[0].Class)
	}
}

func TestPlanOrderingIsStable(t *testing.T) {
	bob := planAccount(MustBytes(2<<30), MustBytes(1<<30), MustBytes(0))
	bob.User = User{ID: 2, UID: "bob"}
	photos := planAccount(MustBytes(3<<30), MustBytes(1<<30), MustBytes(0))
	photos.Provider = "photos"

	plan := BuildPlan([]PlanAccount{bob, photos, planAccount(MustBytes(4<<30), MustBytes(1<<30), MustBytes(0))})
	var order []string
	for _, c := range plan.Changes {
		order = append(order, c.UserUID+"/"+c.Provider)
	}
	want := []string{"alice/cloud", "alice/photos", "bob/cloud"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestCountByClass(t *testing.T) {
	plan := BuildPlan([]PlanAccount{
		planAccount(MustBytes(100<<30), MustBytes(50<<30), MustBytes(1<<30)),
		planAccount(MustBytes(1<<30), MustBytes(50<<30), MustBytes(5<<30)),
		planAccount(MustBytes(1<<30), MustBytes(50<<30), Unknown()),
	})
	counts := plan.CountByClass()
	if counts[ClassSafe] != 1 || counts[ClassShrinkBelowUsage] != 1 || counts[ClassUnknownState] != 1 {
		t.Errorf("counts = %v", counts)
	}
}
