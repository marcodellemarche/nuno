// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"fmt"
	"slices"
)

// ResolutionRule names which precedence step produced a ceiling. It is stored
// and shown, because an admin must never have to guess why someone got 500 GB
// (FR-19).
type ResolutionRule string

const (
	RuleProviderOverride ResolutionRule = "provider-override"
	RuleUserTier         ResolutionRule = "user-tier"
	RuleGroupTier        ResolutionRule = "group-tier"
	RuleDefaultTier      ResolutionRule = "default-tier"
	RuleNone             ResolutionRule = "none"
)

// Resolution is one ceiling and the chain that produced it.
type Resolution struct {
	// Present is false when no tier the person is entitled to allocates this
	// provider. Absent is not zero and not unlimited: nothing is emitted and
	// the provider keeps what it has (FR-17).
	Present bool
	Ceiling Quota

	Rule      ResolutionRule
	TierID    int64
	TierName  string
	GroupUUID string

	// Candidates is every tier considered, with what it offered, so nuno
	// explain can print the whole chain rather than the winner alone (FR-71).
	Candidates []Candidate

	// Notes carry configuration problems worth showing next to the number,
	// like a deliberate over-commit.
	Notes []string
}

type Candidate struct {
	TierID   int64
	TierName string
	// GroupUUID is set when this tier came from a group membership.
	GroupUUID string
	Offered   Quota
	// Allocates is false when this tier is silent about the provider.
	Allocates bool
}

// Policy is everything resolution reads. Passing it in keeps Resolve pure,
// which is what lets the whole precedence chain be tested without a database.
type Policy struct {
	Tiers         map[int64]Tier
	GroupTiers    map[string]int64
	DefaultTierID *int64
}

// UserPolicy is one person's policy inputs.
type UserPolicy struct {
	// ProviderOverrides sit above every tier, per provider (FR-15).
	ProviderOverrides map[int64]Quota
	// TierOverride wins outright over all group-derived tiers (FR-14).
	TierOverride *int64
}

// Resolve computes the ceiling for one person on one provider.
//
// Resolution happens per provider, taking the most generous resolved ceiling
// across the tiers the person is entitled to, with Unlimited as the top
// element and an absent allocation contributing nothing. A tier is a set of
// independent per-provider offers, so the total budget is not what is compared
// (ADR-0012).
//
// Precedence, completely: provider override, user tier override, the most
// generous group-derived tier, then the default tier (ADR-0020).
func Resolve(user User, providerID int64, policy Policy, up UserPolicy) Resolution {
	if quota, ok := up.ProviderOverrides[providerID]; ok {
		return Resolution{
			Present: true,
			Ceiling: quota,
			Rule:    RuleProviderOverride,
		}
	}

	candidates, rule := candidateTiers(user, policy, up)
	result := Resolution{Rule: RuleNone, Candidates: make([]Candidate, 0, len(candidates))}

	for _, candidate := range candidates {
		tier, known := policy.Tiers[candidate.TierID]
		if !known {
			// A mapping to a tier that no longer exists is worth saying out
			// loud rather than resolving around.
			result.Notes = append(result.Notes,
				fmt.Sprintf("a group maps to tier %d, which does not exist", candidate.TierID))
			continue
		}

		offered, allocates := tier.AllocationFor(providerID)
		entry := Candidate{
			TierID:    tier.ID,
			TierName:  tier.Name,
			GroupUUID: candidate.GroupUUID,
			Offered:   offered,
			Allocates: allocates,
		}
		result.Candidates = append(result.Candidates, entry)
		if tier.Overcommitted() {
			note := fmt.Sprintf("tier %q allocates more than 100 percent of its budget, which over-commits storage deliberately", tier.Name)
			if !slices.Contains(result.Notes, note) {
				result.Notes = append(result.Notes, note)
			}
		}

		if !allocates {
			continue
		}
		if !result.Present || moreGenerous(offered, result.Ceiling) {
			result.Present = true
			result.Ceiling = offered
			result.Rule = rule
			result.TierID = tier.ID
			result.TierName = tier.Name
			result.GroupUUID = candidate.GroupUUID
		}
	}

	slices.SortFunc(result.Candidates, func(a, b Candidate) int {
		if a.TierName != b.TierName {
			if a.TierName < b.TierName {
				return -1
			}
			return 1
		}
		return 0
	})
	return result
}

type tierCandidate struct {
	TierID    int64
	GroupUUID string
}

// candidateTiers applies precedence. A user tier override replaces the group
// set entirely, and the default tier applies only when nothing else does.
func candidateTiers(user User, policy Policy, up UserPolicy) ([]tierCandidate, ResolutionRule) {
	if up.TierOverride != nil {
		return []tierCandidate{{TierID: *up.TierOverride}}, RuleUserTier
	}

	var fromGroups []tierCandidate
	for _, groupUUID := range user.GroupUUIDs {
		tierID, mapped := policy.GroupTiers[groupUUID]
		if !mapped {
			continue
		}
		fromGroups = append(fromGroups, tierCandidate{TierID: tierID, GroupUUID: groupUUID})
	}
	if len(fromGroups) > 0 {
		// Sorted so the recorded reason is stable when two groups map to
		// tiers that offer the same ceiling.
		slices.SortFunc(fromGroups, func(a, b tierCandidate) int {
			if a.GroupUUID != b.GroupUUID {
				if a.GroupUUID < b.GroupUUID {
					return -1
				}
				return 1
			}
			return 0
		})
		return fromGroups, RuleGroupTier
	}

	if policy.DefaultTierID != nil {
		return []tierCandidate{{TierID: *policy.DefaultTierID}}, RuleDefaultTier
	}
	return nil, RuleNone
}

// moreGenerous is the order resolution maximizes over: Unlimited is the top
// element, and bytes compare as numbers. Unknown never wins, because a value
// nobody read is not an entitlement.
func moreGenerous(candidate, current Quota) bool {
	switch {
	case !candidate.IsKnown():
		return false
	case current.IsUnlimited():
		return false
	case candidate.IsUnlimited():
		return true
	case !current.IsKnown():
		return true
	}
	return candidate.Bytes > current.Bytes
}

// EffectiveBudget is the output half of the word: the sum of the ceilings
// actually resolved for a person, unlimited if any of them is. It is what the
// UI and the API report, and it can legitimately differ from any tier's
// budget (ADR-0020).
func EffectiveBudget(resolutions []Resolution) Quota {
	var total int64
	found := false
	for _, r := range resolutions {
		if !r.Present {
			continue
		}
		if r.Ceiling.IsUnlimited() {
			return Unlimited()
		}
		if r.Ceiling.IsBytes() {
			total += r.Ceiling.Bytes
			found = true
		}
	}
	if !found {
		return Unknown()
	}
	q, err := BytesQuota(total)
	if err != nil {
		return Unknown()
	}
	return q
}
