// SPDX-License-Identifier: AGPL-3.0-or-later

package reconcile

import (
	"context"
	"fmt"

	"github.com/marcodellemarche/nuno/internal/core"
)

// link matches observed accounts to people and records every problem it could
// not resolve. Writing a quota to the wrong account is the worst thing Nuno
// can do, so ambiguity is never resolved by guessing: it is surfaced and
// excluded (FR-7).
//
// Links and match outcomes are separate. account_links holds only real links;
// everything unresolved lands in link_issues, recomputed each cycle (ADR-0013).
func (e *Engine) link(ctx context.Context, instances []Instance, report *Report) error {
	users, err := e.db.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("read users: %w", err)
	}
	active := make([]core.User, 0, len(users))
	for _, u := range users {
		// An orphan is excluded from reconcile but keeps its history (FR-9).
		if u.Status == core.UserActive {
			active = append(active, u)
		}
	}

	existingLinks, err := e.db.ListLinks(ctx)
	if err != nil {
		return fmt.Errorf("read links: %w", err)
	}

	states, err := e.loadStates(ctx, instances, report, existingLinks)
	if err != nil {
		return err
	}

	for _, state := range states {
		e.revalidate(state, active)
		e.matchDirect(state, active)
	}

	// The subject join runs across providers, after every direct match, so it
	// cannot depend on the order providers were observed in (ADR-0021).
	e.matchBySubject(states)

	for _, state := range states {
		e.collectIssues(state, active)
		if err := e.persist(ctx, state); err != nil {
			return err
		}
	}
	return nil
}

// providerState is one provider's view during a linking pass.
type providerState struct {
	instance Instance
	result   *ProviderResult
	accounts []core.ExternalAccount
	byID     map[string]core.ExternalAccount
	linked   map[int64]core.AccountLink // user id to link, existing and new
	owner    map[string]int64           // external id to user id
	newLinks []core.AccountLink
	issues   []core.LinkIssue
	dangling map[int64]bool // links excluded from this cycle
}

func (e *Engine) loadStates(ctx context.Context, instances []Instance, report *Report, existing []core.AccountLink) ([]*providerState, error) {
	results := map[string]*ProviderResult{}
	for i := range report.Providers {
		results[report.Providers[i].Name] = &report.Providers[i]
	}

	var states []*providerState
	for _, instance := range instances {
		result, ok := results[instance.Row.Name]
		if !ok {
			continue
		}
		// A provider whose observe failed is skipped whole: its stored
		// accounts are from an older cycle, and Nuno never links or writes
		// against an account it did not observe now (FR-9b). Its previous
		// issues are left in place rather than recomputed from stale data.
		if !result.OK() || !instance.Provider.Capabilities().CanReadUsers {
			continue
		}

		accounts, err := e.db.ListExternalAccounts(ctx, instance.Row.ID)
		if err != nil {
			return nil, fmt.Errorf("read accounts for %s: %w", instance.Row.Name, err)
		}

		state := &providerState{
			instance: instance,
			result:   result,
			accounts: accounts,
			byID:     make(map[string]core.ExternalAccount, len(accounts)),
			linked:   map[int64]core.AccountLink{},
			owner:    map[string]int64{},
			dangling: map[int64]bool{},
		}
		for _, a := range accounts {
			state.byID[a.ExternalID] = a
		}
		for _, link := range existing {
			if link.ProviderID == instance.Row.ID {
				state.linked[link.UserID] = link
			}
		}
		states = append(states, state)
	}
	return states, nil
}

// revalidate checks the links that already exist against what was observed
// now. A link is never silently retargeted: a problem is reported and the
// account is left alone.
func (e *Engine) revalidate(state *providerState, users []core.User) {
	byID := map[int64]core.User{}
	for _, u := range users {
		byID[u.ID] = u
	}

	for userID, link := range state.linked {
		user, isActive := byID[userID]
		if !isActive {
			// The person is orphaned or gone. The link stays for history and
			// is excluded from this cycle.
			state.dangling[userID] = true
			continue
		}

		account, observed := state.byID[link.ExternalID]
		if !observed {
			state.issues = append(state.issues, core.LinkIssue{
				ProviderID: state.instance.Row.ID,
				Kind:       core.IssueDangling,
				UserID:     ptr(userID),
				ExternalID: link.ExternalID,
				Detail:     fmt.Sprintf("%s is linked to an account the provider no longer reports", user.UID),
			})
			state.dangling[userID] = true
			continue
		}
		if !account.Writable() {
			state.issues = append(state.issues, core.LinkIssue{
				ProviderID: state.instance.Row.ID,
				Kind:       core.IssueDangling,
				UserID:     ptr(userID),
				ExternalID: link.ExternalID,
				Detail:     fmt.Sprintf("%s is linked to an account that is disabled or deleted", user.UID),
			})
			state.dangling[userID] = true
			continue
		}

		// A manual link is never auto-invalidated, only reported (ADR-0013).
		if link.Origin == core.LinkMatched {
			key := state.instance.Row.MatchKey
			want := userMatchValue(user, key)
			got := accountMatchValue(account, key)
			if want != "" && want != got {
				state.issues = append(state.issues, core.LinkIssue{
					ProviderID: state.instance.Row.ID,
					Kind:       core.IssueStale,
					UserID:     ptr(userID),
					ExternalID: link.ExternalID,
					Detail: fmt.Sprintf("the %s of %s no longer matches this account (%q against %q): an admin decides, Nuno does not retarget a write",
						key, user.UID, want, got),
				})
				state.dangling[userID] = true
				continue
			}
		}

		state.owner[link.ExternalID] = userID
	}
}

// matchDirect pairs people with accounts on the provider's configured match
// key. Anything other than exactly one candidate on each side is ambiguous,
// and ambiguity produces no link.
func (e *Engine) matchDirect(state *providerState, users []core.User) {
	key := state.instance.Row.MatchKey
	if key == "" {
		return
	}

	usersByKey := map[string][]core.User{}
	for _, u := range users {
		if _, alreadyLinked := state.linked[u.ID]; alreadyLinked {
			continue
		}
		// The subject is not a directory attribute: LLDAP has an entryuuid,
		// and the OIDC subject is issued by the identity provider. So an
		// instance matching on subject gets no direct matches here and
		// depends on the cross-provider join instead (ADR-0028).
		if value := userMatchValue(u, key); value != "" {
			usersByKey[value] = append(usersByKey[value], u)
		}
	}

	accountsByKey := map[string][]core.ExternalAccount{}
	for _, a := range state.accounts {
		if !a.Writable() {
			continue
		}
		if _, taken := state.owner[a.ExternalID]; taken {
			continue
		}
		if value := accountMatchValue(a, key); value != "" {
			accountsByKey[value] = append(accountsByKey[value], a)
		}
	}

	for value, candidates := range usersByKey {
		matched := accountsByKey[value]
		switch {
		case len(matched) == 0:
			// Reported later as unlinked, once, rather than here per key.
			continue
		case len(candidates) == 1 && len(matched) == 1:
			e.adopt(state, candidates[0].ID, matched[0].ExternalID, core.LinkMatched)
		default:
			for _, u := range candidates {
				state.issues = append(state.issues, core.LinkIssue{
					ProviderID: state.instance.Row.ID,
					Kind:       core.IssueAmbiguous,
					UserID:     ptr(u.ID),
					Detail: fmt.Sprintf("%d people and %d accounts share the %s %q: link one explicitly with `nuno link`",
						len(candidates), len(matched), key, value),
				})
			}
		}
	}
}

// matchBySubject carries a link from one provider to another. Two accounts
// reporting the same OIDC subject belong to the same person, with certainty
// and without heuristics, which recovers accounts that email cannot match
// (ADR-0021).
func (e *Engine) matchBySubject(states []*providerState) {
	// Whose subject is it? Only accounts that are already linked can say.
	userBySubject := map[string]int64{}
	conflicting := map[string]bool{}
	for _, state := range states {
		for externalID, userID := range state.owner {
			subject := state.byID[externalID].Subject
			if subject == "" {
				continue
			}
			if existing, seen := userBySubject[subject]; seen && existing != userID {
				conflicting[subject] = true
				continue
			}
			userBySubject[subject] = userID
		}
	}

	for _, state := range states {
		for _, a := range state.accounts {
			if !a.Writable() || a.Subject == "" {
				continue
			}
			if _, taken := state.owner[a.ExternalID]; taken {
				continue
			}
			userID, known := userBySubject[a.Subject]
			if !known || conflicting[a.Subject] {
				continue
			}
			if _, hasOne := state.linked[userID]; hasOne {
				// That person already holds a different account here. Two
				// accounts for one person on one provider is ambiguous, not
				// something to pick between.
				state.issues = append(state.issues, core.LinkIssue{
					ProviderID: state.instance.Row.ID,
					Kind:       core.IssueAmbiguous,
					UserID:     ptr(userID),
					ExternalID: a.ExternalID,
					Detail:     "this account shares an OIDC subject with a person who is already linked to another account here",
				})
				continue
			}
			e.adopt(state, userID, a.ExternalID, core.LinkMatched)
		}
	}
}

func (e *Engine) adopt(state *providerState, userID int64, externalID string, origin core.LinkOrigin) {
	link := core.AccountLink{
		UserID:     userID,
		ProviderID: state.instance.Row.ID,
		ExternalID: externalID,
		Origin:     origin,
		LinkedAt:   e.now().UTC(),
	}
	state.linked[userID] = link
	state.owner[externalID] = userID
	state.newLinks = append(state.newLinks, link)
}

// collectIssues records what is left: people with no account here, and
// accounts belonging to nobody.
func (e *Engine) collectIssues(state *providerState, users []core.User) {
	for _, u := range users {
		if _, linked := state.linked[u.ID]; linked && !state.dangling[u.ID] {
			continue
		}
		if state.dangling[u.ID] {
			continue // already reported as dangling or stale
		}
		if hasAmbiguity(state.issues, u.ID) {
			continue
		}
		state.issues = append(state.issues, core.LinkIssue{
			ProviderID: state.instance.Row.ID,
			Kind:       core.IssueUnlinked,
			UserID:     ptr(u.ID),
			Detail:     fmt.Sprintf("%s has no account on this provider: Nuno reports it and never creates one (FR-8)", u.UID),
		})
	}

	for _, a := range state.accounts {
		if !a.Writable() {
			continue
		}
		if _, owned := state.owner[a.ExternalID]; owned {
			continue
		}
		state.issues = append(state.issues, core.LinkIssue{
			ProviderID: state.instance.Row.ID,
			Kind:       core.IssueUnmanaged,
			ExternalID: a.ExternalID,
			Detail:     "this account matches nobody in the identity source, so Nuno never writes to it",
		})
	}
}

func hasAmbiguity(issues []core.LinkIssue, userID int64) bool {
	for _, issue := range issues {
		if issue.Kind == core.IssueAmbiguous && issue.UserID != nil && *issue.UserID == userID {
			return true
		}
	}
	return false
}

func (e *Engine) persist(ctx context.Context, state *providerState) error {
	for _, link := range state.newLinks {
		if err := e.db.UpsertLink(ctx, link); err != nil {
			return err
		}
	}

	// external_accounts.user_id is what makes an unmanaged account
	// representable, so it is set for every account, owned or not (FR-3).
	for _, a := range state.accounts {
		var owner *int64
		if userID, ok := state.owner[a.ExternalID]; ok {
			owner = ptr(userID)
		}
		if err := e.db.SetAccountOwner(ctx, state.instance.Row.ID, a.ExternalID, owner); err != nil {
			return err
		}
	}

	if err := e.db.ReplaceLinkIssues(ctx, state.instance.Row.ID, state.issues); err != nil {
		return err
	}

	state.result.Linked = len(state.owner)
	state.result.Issues = len(state.issues)

	// Zero links where both people and accounts exist means the match key is
	// wrong, and an admin should be told that rather than handed N unlinked
	// rows to scroll through (ADR-0013). Zero accounts is a different thing
	// and not a misconfiguration.
	if len(state.owner) == 0 && len(state.accounts) > 0 && countLinkable(state) > 0 {
		e.log.Error("no account on this provider matched anyone: the match key is probably wrong",
			"provider", state.instance.Row.Name,
			"match_key", state.instance.Row.MatchKey,
			"accounts", len(state.accounts),
			"setting", state.instance.Row.ConfigRef+"_MATCH_KEY")
	}
	return nil
}

func countLinkable(state *providerState) int {
	count := 0
	for _, a := range state.accounts {
		if a.Writable() {
			count++
		}
	}
	return count
}

func userMatchValue(u core.User, key core.MatchKey) string {
	switch key {
	case core.MatchEmail:
		return core.NormalizeEmail(u.Email)
	case core.MatchUsername:
		return core.NormalizeUsername(u.UID)
	}
	return ""
}

func accountMatchValue(a core.ExternalAccount, key core.MatchKey) string {
	switch key {
	case core.MatchEmail:
		return core.NormalizeEmail(a.Email)
	case core.MatchUsername:
		return core.NormalizeUsername(a.Username)
	case core.MatchSubject:
		return a.Subject
	}
	return ""
}

func ptr[T any](v T) *T { return &v }
