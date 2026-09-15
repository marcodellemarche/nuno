// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"fmt"
	"strings"
)

// AllocationMode is how a tier maps a budget onto one provider.
type AllocationMode string

const (
	// ModeAbsolute is a byte count.
	ModeAbsolute AllocationMode = "absolute"
	// ModePercent is a share of the tier's budget, after the absolute
	// allocations are taken out of it (ADR-0003, ADR-0020).
	ModePercent AllocationMode = "percent"
	// ModeUnlimited lifts the ceiling, and only an explicit allocation can.
	ModeUnlimited AllocationMode = "unlimited"
)

func (m AllocationMode) Valid() bool {
	switch m {
	case ModeAbsolute, ModePercent, ModeUnlimited:
		return true
	}
	return false
}

// Allocation is one tier's offer on one provider instance.
//
// There is no row for a provider a tier is silent about. Absent is not zero
// and not unlimited: the planner emits no change and the provider keeps
// whatever it has. This is the single most dangerous ambiguity in the model,
// which is why absence is the lack of a row rather than a value (FR-17).
type Allocation struct {
	ProviderID int64
	Mode       AllocationMode
	// Value is bytes for absolute and 0 to 100 for percent. It is ignored for
	// unlimited.
	Value int64
}

// Tier is a named set of independent per-provider offers, not a single ranked
// number (ADR-0012).
type Tier struct {
	ID   int64
	Name string
	// Budget is an input: the base percent allocations resolve against. A
	// tier with only absolute or unlimited allocations may leave it unset,
	// which is why it is tagged rather than an int64 (ADR-0020).
	Budget      Quota
	IsDefault   bool
	Allocations []Allocation
}

// Validate refuses a tier that cannot resolve honestly. These are checked when
// a tier is written and when one is imported, never resolved around, because
// Nextcloud reads a negative value as a sentinel: an unvalidated negative
// would grant unlimited instead of failing (ADR-0020).
func (t Tier) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("a tier needs a name")
	}
	if err := t.Budget.Valid(); err != nil {
		return fmt.Errorf("tier %q: budget: %w", t.Name, err)
	}

	seen := map[int64]bool{}
	var absolute int64
	hasPercent := false
	for _, a := range t.Allocations {
		if !a.Mode.Valid() {
			return fmt.Errorf("tier %q: unknown allocation mode %q", t.Name, a.Mode)
		}
		if seen[a.ProviderID] {
			return fmt.Errorf("tier %q: two allocations for provider %d", t.Name, a.ProviderID)
		}
		seen[a.ProviderID] = true

		switch a.Mode {
		case ModeAbsolute:
			if a.Value < 0 {
				return fmt.Errorf("tier %q: a negative allocation is never a ceiling", t.Name)
			}
			absolute += a.Value
		case ModePercent:
			if a.Value < 0 || a.Value > 100 {
				return fmt.Errorf("tier %q: a percent allocation must be between 0 and 100, got %d", t.Name, a.Value)
			}
			hasPercent = true
		}
	}

	if hasPercent && !t.Budget.IsBytes() {
		return fmt.Errorf("tier %q: a percent allocation needs a budget in bytes to resolve against, and this budget is %s",
			t.Name, t.Budget)
	}
	if t.Budget.IsBytes() && absolute > t.Budget.Bytes {
		return fmt.Errorf("tier %q: its absolute allocations add up to %s, more than its %s budget",
			t.Name, FormatIEC(absolute), t.Budget)
	}
	return nil
}

// Overcommitted reports whether the percentages add up to more than what is
// left after the absolute allocations. It is legal and deliberate, and it
// warns on the tier row and in the plan rather than being refused (ADR-0003).
func (t Tier) Overcommitted() bool {
	var percent int64
	for _, a := range t.Allocations {
		if a.Mode == ModePercent {
			percent += a.Value
		}
	}
	return percent > 100
}

// AllocationFor returns this tier's offer for one provider, and whether it
// made one at all.
func (t Tier) AllocationFor(providerID int64) (Quota, bool) {
	for _, a := range t.Allocations {
		if a.ProviderID != providerID {
			continue
		}
		switch a.Mode {
		case ModeUnlimited:
			return Unlimited(), true
		case ModeAbsolute:
			q, err := BytesQuota(a.Value)
			if err != nil {
				return Unknown(), false
			}
			return q, true
		case ModePercent:
			return t.percentOf(a.Value)
		}
	}
	return Unknown(), false
}

// percentOf applies the mixed-mode rule: absolute allocations come out of the
// budget first, then percentages apply to the remainder (ADR-0003, ADR-0020).
func (t Tier) percentOf(share int64) (Quota, bool) {
	if !t.Budget.IsBytes() {
		// Validate refuses this at write time. Reaching it means something
		// bypassed that, and resolving it would be inventing a ceiling.
		return Unknown(), false
	}
	remainder := t.Budget.Bytes
	for _, a := range t.Allocations {
		if a.Mode == ModeAbsolute {
			remainder -= a.Value
		}
	}
	if remainder < 0 {
		// Clamped at zero rather than resolved negative.
		remainder = 0
	}
	q, err := BytesQuota(remainder * share / 100)
	if err != nil {
		return Unknown(), false
	}
	return q, true
}

// GroupTier maps a directory group to a tier. It is the edge FR-4 depends on,
// and it is keyed on the group's uuid so renaming a group does not move
// everyone to the default tier.
type GroupTier struct {
	GroupUUID string
	TierID    int64
}

// UserProviderOverride sits above every tier. It is where nuno adopt stores
// what it imports, which is why two people with different hand-set quotas do
// not need one tier each (FR-15, ADR-0020).
type UserProviderOverride struct {
	UserID     int64
	ProviderID int64
	Quota      Quota
}
