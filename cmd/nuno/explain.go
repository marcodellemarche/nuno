// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

// explain prints the whole chain that produced a number: group membership,
// candidate tiers, the winning ceiling per provider and why, the account link
// and its origin, and the last observed values.
//
// It is the first thing to reach for when a number looks wrong, and the best
// available test harness for the resolver (FR-71).
func explain(ctx context.Context, a *app, args []string, stdout io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(a.stderr, "usage: nuno explain <uid>")
		return ExitConfig
	}

	users, err := a.db.ListUsers(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	var user core.User
	var found bool
	var known []string
	for _, u := range users {
		known = append(known, u.UID)
		if strings.EqualFold(u.UID, args[0]) {
			user, found = u, true
		}
	}
	if !found {
		fmt.Fprintf(a.stderr, "nuno: no user %q (known: %s)\n", args[0], strings.Join(known, ", "))
		return ExitConfig
	}

	policy, err := a.db.LoadPolicy(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	userPolicy, origins, err := a.db.UserPolicy(ctx, user.ID)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	providers, err := a.db.ListProviders(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	links, err := a.db.ListLinks(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	accounts, err := a.db.ListAllExternalAccounts(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}

	fmt.Fprintf(stdout, "%s (%s, %s)\n", user.UID, user.Source, user.Status)
	if user.Email != "" {
		fmt.Fprintf(stdout, "  email      %s\n", user.Email)
	}
	fmt.Fprintf(stdout, "  identity   %s\n", user.SourceUUID)

	fmt.Fprintln(stdout, "\nGroups")
	if len(user.GroupUUIDs) == 0 {
		fmt.Fprintln(stdout, "  none, so the default tier applies")
	}
	groups, _ := a.db.ListGroups(ctx)
	groupNames := map[string]string{}
	for _, g := range groups {
		groupNames[g.SourceUUID] = g.Name
	}
	for _, uuid := range user.GroupUUIDs {
		name := groupNames[uuid]
		if name == "" {
			name = uuid
		}
		tierID, mapped := policy.GroupTiers[uuid]
		if !mapped {
			fmt.Fprintf(stdout, "  %-20s maps to no tier\n", name)
			continue
		}
		fmt.Fprintf(stdout, "  %-20s entitles them to %s\n", name, policy.Tiers[tierID].Name)
	}

	if userPolicy.TierOverride != nil {
		fmt.Fprintf(stdout, "\nTier override: %s, which replaces the group set entirely\n",
			policy.Tiers[*userPolicy.TierOverride].Name)
	}

	linkByProvider := map[int64]core.AccountLink{}
	for _, link := range links {
		if link.UserID == user.ID {
			linkByProvider[link.ProviderID] = link
		}
	}
	accountByKey := map[string]core.ExternalAccount{}
	for _, account := range accounts {
		accountByKey[fmt.Sprintf("%d/%s", account.ProviderID, account.ExternalID)] = account
	}

	var resolutions []core.Resolution
	for _, provider := range providers {
		fmt.Fprintf(stdout, "\n%s (%s)\n", provider.Name, provider.Type)

		link, linked := linkByProvider[provider.ID]
		if !linked {
			fmt.Fprintln(stdout, "  link       none, so Nuno writes nothing here (FR-7)")
		} else {
			fmt.Fprintf(stdout, "  link       %s, %s\n", link.ExternalID, link.Origin)
		}

		resolution := core.Resolve(user, provider.ID, policy, userPolicy)
		resolutions = append(resolutions, resolution)

		switch {
		case !resolution.Present:
			fmt.Fprintln(stdout, "  ceiling    none: no tier they are entitled to allocates this provider, so it is left alone")
		case resolution.Rule == core.RuleProviderOverride:
			origin := origins[provider.ID]
			fmt.Fprintf(stdout, "  ceiling    %s, from a per-provider override (%s), which beats every tier\n",
				resolution.Ceiling, origin)
		default:
			via := string(resolution.Rule)
			if resolution.GroupUUID != "" {
				via = fmt.Sprintf("group %s", groupNames[resolution.GroupUUID])
			}
			fmt.Fprintf(stdout, "  ceiling    %s, from tier %s via %s\n", resolution.Ceiling, resolution.TierName, via)
		}

		if len(resolution.Candidates) > 0 {
			fmt.Fprintln(stdout, "  considered")
			table := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
			for _, candidate := range resolution.Candidates {
				offered := "nothing here"
				if candidate.Allocates {
					offered = candidate.Offered.String()
				}
				marker := " "
				if candidate.Allocates && candidate.TierID == resolution.TierID {
					marker = "*"
				}
				fmt.Fprintf(table, "    %s %s\toffers %s\n", marker, candidate.TierName, offered)
			}
			table.Flush()
		}
		for _, note := range resolution.Notes {
			fmt.Fprintf(stdout, "  note       %s\n", note)
		}

		if linked {
			account, observed := accountByKey[fmt.Sprintf("%d/%s", provider.ID, link.ExternalID)]
			if !observed {
				fmt.Fprintln(stdout, "  observed   not in the last cycle, so this link is dangling and excluded")
			} else {
				fmt.Fprintf(stdout, "  observed   quota %s, used %s, at %s\n",
					account.Quota, account.Used, account.ObservedAt.Format("2006-01-02 15:04 UTC"))
				if account.NeverUsed {
					fmt.Fprintln(stdout, "             this account has never been logged into, so its zero is a fact rather than a gap")
				}
			}
		}
	}

	fmt.Fprintf(stdout, "\nEffective budget: %s\n", core.EffectiveBudget(resolutions))
	fmt.Fprintln(stdout, "That is the sum of the ceilings actually resolved, which may differ from any tier's budget.")

	if entries := recentAudit(ctx, a, user.ID); len(entries) > 0 {
		fmt.Fprintln(stdout, "\nRecent changes")
		table := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
		for _, entry := range entries {
			fmt.Fprintf(table, "  %s\t%s\t%s -> %s\t%s\n",
				entry.At.Format("2006-01-02 15:04"), entry.Provider, entry.From, entry.To, entry.Result)
		}
		table.Flush()
	}
	return ExitClean
}

// recentAudit is empty until the applier exists, and the section is skipped
// rather than showing an empty heading.
func recentAudit(ctx context.Context, a *app, userID int64) []store.AuditEntry {
	entries, err := a.db.RecentAudit(ctx, userID, 5)
	if err != nil {
		a.log.Debug("read audit entries", "error", err)
		return nil
	}
	return entries
}
