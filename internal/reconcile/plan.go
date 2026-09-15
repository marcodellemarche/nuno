// SPDX-License-Identifier: AGPL-3.0-or-later

package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

// PlanFilter narrows a plan to one person or one provider. It is the primary
// day-2 debugging tool and is nearly free once the planner takes a filter
// (FR-37).
type PlanFilter struct {
	UID      string
	Provider string
}

func (f PlanFilter) empty() bool { return f.UID == "" && f.Provider == "" }

// PlanResult is a plan plus what it was computed over, so a report can say
// what it did not look at.
type PlanResult struct {
	Plan core.Plan
	// Considered is the number of linked pairs the filter admitted.
	Considered int
	// Degraded lists providers that will refuse to apply even though the plan
	// shows their changes (FR-75).
	Degraded []string
	// Resolutions keeps the chain per pair, for nuno explain.
	Resolutions map[string]core.Resolution
}

// Plan computes desired against observed for every linked pair.
//
// Only linked accounts are considered: Nuno never writes to an account it
// cannot link unambiguously, and an unlinked one is surfaced as an issue
// instead (FR-7). Orphans are excluded, keeping their history (FR-9).
func (e *Engine) Plan(ctx context.Context, instances []Instance, filter PlanFilter) (PlanResult, error) {
	result := PlanResult{Resolutions: map[string]core.Resolution{}}

	policy, err := e.db.LoadPolicy(ctx)
	if err != nil {
		return result, fmt.Errorf("load policy: %w", err)
	}
	users, err := e.db.ListUsers(ctx)
	if err != nil {
		return result, fmt.Errorf("read users: %w", err)
	}
	links, err := e.db.ListLinks(ctx)
	if err != nil {
		return result, fmt.Errorf("read links: %w", err)
	}

	usersByID := map[int64]core.User{}
	for _, u := range users {
		usersByID[u.ID] = u
	}
	byProvider := map[int64]Instance{}
	for _, instance := range instances {
		byProvider[instance.Row.ID] = instance
		if instance.Row.Degraded() {
			result.Degraded = append(result.Degraded, instance.Row.Name)
		}
	}

	accounts := map[int64]map[string]core.ExternalAccount{}
	for _, instance := range instances {
		stored, err := e.db.ListExternalAccounts(ctx, instance.Row.ID)
		if err != nil {
			return result, fmt.Errorf("read accounts for %s: %w", instance.Row.Name, err)
		}
		byID := make(map[string]core.ExternalAccount, len(stored))
		for _, a := range stored {
			byID[a.ExternalID] = a
		}
		accounts[instance.Row.ID] = byID
	}

	// One policy read per person, not per pair.
	userPolicies := map[int64]core.UserPolicy{}

	var planAccounts []core.PlanAccount
	for _, link := range links {
		instance, known := byProvider[link.ProviderID]
		if !known {
			continue
		}
		user, active := usersByID[link.UserID]
		if !active || user.Status != core.UserActive {
			continue
		}
		if !filter.admits(user.UID, instance.Row.Name) {
			continue
		}

		account, observed := accounts[link.ProviderID][link.ExternalID]
		if !observed {
			// A link to an account nobody observed this cycle is reported by
			// the linker as dangling, and it is never written to (FR-9b).
			continue
		}

		up, cached := userPolicies[user.ID]
		if !cached {
			loaded, _, err := e.db.UserPolicy(ctx, user.ID)
			if err != nil {
				return result, fmt.Errorf("read the policy of %s: %w", user.UID, err)
			}
			up = loaded
			userPolicies[user.ID] = up
		}

		resolution := core.Resolve(user, link.ProviderID, policy, up)
		result.Resolutions[user.UID+"/"+instance.Row.Name] = resolution
		result.Considered++

		// Normalizing here rather than in core is deliberate: what a provider
		// will really store is the adapter's knowledge, and comparing raw
		// against observed is what makes a rewriting provider loop forever
		// (FR-32, ADR-0011).
		desired := core.Unknown()
		if resolution.Present {
			desired = instance.Provider.NormalizeQuota(resolution.Ceiling)
		}

		planAccounts = append(planAccounts, core.PlanAccount{
			User:       user,
			ProviderID: link.ProviderID,
			Provider:   instance.Row.Name,
			ExternalID: link.ExternalID,
			Desired:    desired,
			Present:    resolution.Present,
			Observed:   account.Quota,
			Used:       account.Used,
			NeverUsed:  account.NeverUsed,
			ObserveOK:  account.ObserveOK,
			Rule:       resolution.Rule,
			TierName:   resolution.TierName,
		})
	}

	result.Plan = core.BuildPlan(planAccounts)
	return result, nil
}

func (f PlanFilter) admits(uid, provider string) bool {
	if f.empty() {
		return true
	}
	if f.UID != "" && !strings.EqualFold(f.UID, uid) {
		return false
	}
	if f.Provider != "" && !strings.EqualFold(f.Provider, provider) {
		return false
	}
	return true
}

// SavePlan records a plan against a run, so what was agreed to can be
// compared with what is applied.
func (e *Engine) SavePlan(ctx context.Context, mode store.RunMode, actor string, consentShrink bool, result PlanResult) (int64, error) {
	runID, err := e.db.StartRun(ctx, mode, actor, consentShrink)
	if err != nil {
		return 0, err
	}

	body, err := json.Marshal(struct {
		Changes     []core.Change `json:"changes"`
		Unallocated int           `json:"unallocated"`
		Considered  int           `json:"considered"`
		Degraded    []string      `json:"degraded"`
	}{
		Changes:     result.Plan.Changes,
		Unallocated: result.Plan.Unallocated,
		Considered:  result.Considered,
		Degraded:    result.Degraded,
	})
	if err != nil {
		return runID, err
	}
	if err := e.db.SavePlanBody(ctx, runID, body); err != nil {
		return runID, err
	}

	counts := result.Plan.CountByClass()
	summary := fmt.Sprintf("%d changes (%d safe, %d guarded, %d unknown state) over %d linked accounts",
		len(result.Plan.Changes), counts[core.ClassSafe],
		counts[core.ClassShrinkBelowUsage], counts[core.ClassUnknownState], result.Considered)

	status := "clean"
	if len(result.Plan.Changes) > 0 {
		status = "pending"
	}
	if err := e.db.FinishRun(ctx, runID, status, summary); err != nil {
		return runID, err
	}
	return runID, nil
}

// Adopt imports each provider's current quota as the person's override, so the
// first plan against an existing stack is empty by construction (FR-70).
//
// It only adopts what it can read: an unknown ceiling is not an instruction,
// and adopting it would mean writing a value nobody read.
func (e *Engine) Adopt(ctx context.Context, instances []Instance, filter PlanFilter) (int, []string, error) {
	links, err := e.db.ListLinks(ctx)
	if err != nil {
		return 0, nil, err
	}
	users, err := e.db.ListUsers(ctx)
	if err != nil {
		return 0, nil, err
	}
	usersByID := map[int64]core.User{}
	for _, u := range users {
		usersByID[u.ID] = u
	}
	byProvider := map[int64]Instance{}
	for _, instance := range instances {
		byProvider[instance.Row.ID] = instance
	}

	adopted := 0
	var skipped []string
	for _, link := range links {
		instance, known := byProvider[link.ProviderID]
		user, isUser := usersByID[link.UserID]
		if !known || !isUser || user.Status != core.UserActive {
			continue
		}
		if !filter.admits(user.UID, instance.Row.Name) {
			continue
		}

		accounts, err := e.db.ListExternalAccounts(ctx, link.ProviderID)
		if err != nil {
			return adopted, skipped, err
		}
		var account *core.ExternalAccount
		for i := range accounts {
			if accounts[i].ExternalID == link.ExternalID {
				account = &accounts[i]
			}
		}
		if account == nil {
			continue
		}
		if !account.Quota.IsKnown() {
			skipped = append(skipped, fmt.Sprintf("%s on %s: the provider did not report a ceiling to adopt",
				user.UID, instance.Row.Name))
			continue
		}

		if err := e.db.SetProviderOverride(ctx, user.ID, link.ProviderID, account.Quota, store.OverrideAdopted); err != nil {
			return adopted, skipped, err
		}
		adopted++
	}
	return adopted, skipped, nil
}
