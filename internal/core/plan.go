// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"fmt"
	"slices"
)

// ChangeClass decides what may happen to a change, and the two risky classes
// have two distinct rules rather than one shared consent (ADR-0016).
type ChangeClass string

const (
	// ClassSafe is applied without asking.
	ClassSafe ChangeClass = "safe"

	// ClassShrinkBelowUsage lands at or below what the person already stores,
	// which blocks uploads immediately. Consent-gated, and never available to
	// a scheduled run (FR-38, FR-39).
	ClassShrinkBelowUsage ChangeClass = "shrink-below-usage"

	// ClassUnknownState means the state it would be written against could not
	// be trusted. Never applied, with no flag to force it, because there is
	// no safe consent for writing against state you do not have (FR-38a).
	ClassUnknownState ChangeClass = "unknown-state"
)

// Consentable reports whether an operator can allow this class at all.
func (c ChangeClass) Consentable() bool { return c == ClassShrinkBelowUsage }

// Change is one quota Nuno wants to write.
type Change struct {
	UserID     int64
	UserUID    string
	ProviderID int64
	Provider   string
	ExternalID string

	From Quota
	To   Quota

	Class  ChangeClass
	Detail string

	// Rule and Tier record why this ceiling, so a plan explains itself
	// (FR-19).
	Rule     ResolutionRule
	TierName string
}

// PlanAccount is one linked pair, with the state a decision needs.
//
// Desired is already normalized by the caller: what a provider will really
// store is the adapter's knowledge, not the core's, and comparing raw against
// observed is what makes a provider that rewrites values loop forever
// (FR-32, ADR-0011).
type PlanAccount struct {
	User       User
	ProviderID int64
	Provider   string
	ExternalID string

	Desired   Quota
	Present   bool
	Observed  Quota
	Used      Quota
	NeverUsed bool
	ObserveOK bool

	Rule     ResolutionRule
	TierName string
}

// Plan is what a run would do.
type Plan struct {
	Changes []Change
	// Skipped counts pairs that resolved to no allocation at all, which is
	// not a change and not a problem (FR-17).
	Unallocated int
}

func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// CountByClass is what the report and the exit code are built from.
func (p Plan) CountByClass() map[ChangeClass]int {
	counts := map[ChangeClass]int{}
	for _, c := range p.Changes {
		counts[c.Class]++
	}
	return counts
}

// Applicable returns the changes that may be written, given consent. It is the
// only place the two guardrails are enforced together, so neither can be
// forgotten at a call site.
func (p Plan) Applicable(consentShrink bool) []Change {
	var out []Change
	for _, c := range p.Changes {
		switch c.Class {
		case ClassSafe:
			out = append(out, c)
		case ClassShrinkBelowUsage:
			if consentShrink {
				out = append(out, c)
			}
		}
		// unknown-state is never applicable, with or without consent.
	}
	return out
}

// BuildPlan computes desired against observed and classifies every difference.
// It is pure, so plan, apply, plan again converges by construction rather than
// by hope (FR-30, FR-32).
func BuildPlan(accounts []PlanAccount) Plan {
	var plan Plan
	for _, account := range accounts {
		if !account.Present {
			// No tier the person is entitled to allocates this provider, so
			// the provider keeps what it has and nothing is emitted.
			plan.Unallocated++
			continue
		}

		class, detail := classify(account)

		// A comparison against a value nobody read cannot say "no change", so
		// unknown state is reported rather than quietly dropped.
		if class != ClassUnknownState && account.Desired.Equal(account.Observed) {
			continue
		}

		plan.Changes = append(plan.Changes, Change{
			UserID:     account.User.ID,
			UserUID:    account.User.UID,
			ProviderID: account.ProviderID,
			Provider:   account.Provider,
			ExternalID: account.ExternalID,
			From:       account.Observed,
			To:         account.Desired,
			Class:      class,
			Detail:     detail,
			Rule:       account.Rule,
			TierName:   account.TierName,
		})
	}

	// Stable order, so two plans over one state read the same and a diff
	// between them is meaningful.
	slices.SortFunc(plan.Changes, func(a, b Change) int {
		if a.UserUID != b.UserUID {
			if a.UserUID < b.UserUID {
				return -1
			}
			return 1
		}
		if a.Provider != b.Provider {
			if a.Provider < b.Provider {
				return -1
			}
			return 1
		}
		return 0
	})
	return plan
}

func classify(account PlanAccount) (ChangeClass, string) {
	switch {
	case !account.ObserveOK:
		return ClassUnknownState, "this account was not read successfully in this cycle"
	case !account.Observed.IsKnown():
		return ClassUnknownState, "the provider did not report a configured quota for this account"
	case !account.Used.IsKnown():
		return ClassUnknownState, "the provider's usage for this account could not be trusted"
	case !account.Desired.IsKnown():
		// Normalization refused the value, which means writing it would be
		// writing something else.
		return ClassUnknownState, "the desired ceiling could not be expressed for this provider"
	}

	// The comparison is target <= used, not <: a quota exactly equal to usage
	// already blocks uploads (ADR-0016).
	if account.Desired.IsBytes() && account.Used.IsBytes() && account.Desired.Bytes <= account.Used.Bytes {
		if account.NeverUsed && account.Desired.Bytes > 0 {
			// Cannot happen while usage is a known zero, and saying so beats
			// a silent branch.
			return ClassSafe, ""
		}
		return ClassShrinkBelowUsage, fmt.Sprintf("%s is at or below the %s already stored, which blocks uploads immediately",
			account.Desired, account.Used)
	}
	return ClassSafe, ""
}
