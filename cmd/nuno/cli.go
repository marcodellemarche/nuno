// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/reconcile"
	"github.com/marcodellemarche/nuno/internal/store"
)

// observeOnce runs one read-only cycle: sync the directory, read every
// provider, link what can be linked. It writes nothing to a provider.
func observeOnce(ctx context.Context, a *app, stdout io.Writer) int {
	engine := reconcile.New(a.db, a.log)

	if _, err := engine.SyncIdentity(ctx, a.directory); err != nil {
		a.log.Error("identity sync failed", "error", err)
		// Observing providers is still worth doing: it is what fills the
		// unmanaged list.
	}

	report, err := engine.Observe(ctx, a.instances)
	if err != nil {
		a.log.Error("observe failed", "error", err)
		return ExitError
	}

	table := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(table, "PROVIDER\tOBSERVED\tSKIPPED\tLINKED\tISSUES\tDETAIL")
	for _, p := range report.Providers {
		detail := ""
		if p.Err != nil {
			detail = p.Err.Error()
		}
		fmt.Fprintf(table, "%s\t%d\t%d\t%d\t%d\t%s\n", p.Name, p.Observed, p.Skipped, p.Linked, p.Issues, detail)
	}
	table.Flush()

	observed, linked, issues := report.Totals()
	fmt.Fprintf(stdout, "\n%d accounts observed, %d linked, %d issues, in %s\n",
		observed, linked, issues, report.FinishedAt.Sub(report.StartedAt).Round(time.Millisecond))

	if !report.OK() {
		// A degraded provider is reported, and the run is not clean.
		return ExitIncomplete
	}
	return ExitClean
}

// listAccounts shows every observed account and who owns it, plus the issues
// that need a human. This is the list FR-57 asks the UI for, in text.
func listAccounts(ctx context.Context, a *app, stdout io.Writer) int {
	providers, err := a.db.ListProviders(ctx)
	if err != nil {
		a.log.Error("read providers", "error", err)
		return ExitError
	}
	byID := map[int64]store.ProviderRow{}
	for _, p := range providers {
		byID[p.ID] = p
	}

	users, err := a.db.ListUsers(ctx)
	if err != nil {
		a.log.Error("read users", "error", err)
		return ExitError
	}
	userByID := map[int64]core.User{}
	for _, u := range users {
		userByID[u.ID] = u
	}

	accounts, err := a.db.ListAllExternalAccounts(ctx)
	if err != nil {
		a.log.Error("read accounts", "error", err)
		return ExitError
	}

	table := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(table, "PROVIDER\tACCOUNT\tPERSON\tSTATE\tQUOTA\tUSED")
	for _, account := range accounts {
		person := "unmanaged"
		if account.UserID != nil {
			if u, ok := userByID[*account.UserID]; ok {
				person = u.UID
			}
		}
		state := "ok"
		switch {
		case account.Deleted:
			state = "deleted"
		case !account.Enabled:
			state = "disabled"
		case account.NeverUsed:
			state = "never used"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			byID[account.ProviderID].Name, account.ExternalID, person, state,
			account.Quota.String(), account.Used.String())
	}
	table.Flush()

	issues, err := a.db.ListLinkIssues(ctx)
	if err != nil {
		a.log.Error("read link issues", "error", err)
		return ExitError
	}
	if len(issues) == 0 {
		return ExitClean
	}

	fmt.Fprintf(stdout, "\n%d issue(s) need a decision:\n\n", len(issues))
	issueTable := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(issueTable, "PROVIDER\tKIND\tPERSON\tACCOUNT\tDETAIL")
	for _, issue := range issues {
		person := ""
		if issue.UserID != nil {
			if u, ok := userByID[*issue.UserID]; ok {
				person = u.UID
			}
		}
		fmt.Fprintf(issueTable, "%s\t%s\t%s\t%s\t%s\n",
			byID[issue.ProviderID].Name, issue.Kind, person, issue.ExternalID, issue.Detail)
	}
	issueTable.Flush()
	return ExitClean
}

// linkAccount records a manual link, which always wins over matching and is
// never auto-invalidated (ADR-0013).
func linkAccount(ctx context.Context, a *app, args []string, stdout io.Writer) int {
	if len(args) != 3 {
		fmt.Fprintln(a.stderr, "usage: nuno link <uid> <provider> <external-id>")
		return ExitConfig
	}
	uid, providerName, externalID := args[0], args[1], args[2]

	user, provider, code := resolveTarget(ctx, a, uid, providerName)
	if code != ExitClean {
		return code
	}

	// Nuno never writes to an account it has not observed, so linking to one
	// it has never seen would be a link that can never be used.
	accounts, err := a.db.ListExternalAccounts(ctx, provider.ID)
	if err != nil {
		a.log.Error("read accounts", "error", err)
		return ExitError
	}
	var target *core.ExternalAccount
	for i := range accounts {
		if accounts[i].ExternalID == externalID {
			target = &accounts[i]
		}
	}
	if target == nil {
		fmt.Fprintf(a.stderr, "nuno: %s has no observed account %q: run `nuno observe` first, or check the id with `nuno accounts`\n",
			providerName, externalID)
		return ExitConfig
	}
	if !target.Writable() {
		fmt.Fprintf(a.stderr, "nuno: account %q on %s is disabled or deleted, so it is never linked or written (FR-8a)\n",
			externalID, providerName)
		return ExitConfig
	}

	if err := a.db.UpsertLink(ctx, core.AccountLink{
		UserID: user.ID, ProviderID: provider.ID, ExternalID: externalID, Origin: core.LinkManual,
	}); err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	if err := a.db.SetAccountOwner(ctx, provider.ID, externalID, &user.ID); err != nil {
		a.log.Error("set account owner", "error", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "linked %s to %s on %s\n", uid, externalID, providerName)
	return ExitClean
}

func unlinkAccount(ctx context.Context, a *app, args []string, stdout io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(a.stderr, "usage: nuno unlink <uid> <provider>")
		return ExitConfig
	}
	uid, providerName := args[0], args[1]

	user, provider, code := resolveTarget(ctx, a, uid, providerName)
	if code != ExitClean {
		return code
	}
	removed, err := a.db.DeleteLink(ctx, user.ID, provider.ID)
	if err != nil {
		a.log.Error("delete link", "error", err)
		return ExitError
	}
	if !removed {
		fmt.Fprintf(a.stderr, "nuno: %s had no link on %s\n", uid, providerName)
		return ExitConfig
	}
	fmt.Fprintf(stdout, "unlinked %s from %s\n", uid, providerName)
	return ExitClean
}

func resolveTarget(ctx context.Context, a *app, uid, providerName string) (core.User, store.ProviderRow, int) {
	users, err := a.db.ListUsers(ctx)
	if err != nil {
		a.log.Error("read users", "error", err)
		return core.User{}, store.ProviderRow{}, ExitError
	}
	var user core.User
	var found bool
	var known []string
	for _, u := range users {
		known = append(known, u.UID)
		if strings.EqualFold(u.UID, uid) {
			user, found = u, true
		}
	}
	if !found {
		fmt.Fprintf(a.stderr, "nuno: no user %q (known: %s)\n", uid, strings.Join(known, ", "))
		return core.User{}, store.ProviderRow{}, ExitConfig
	}

	provider, err := a.db.GetProviderByName(ctx, providerName)
	if err != nil {
		providers, listErr := a.db.ListProviders(ctx)
		if listErr == nil {
			var names []string
			for _, p := range providers {
				names = append(names, p.Name)
			}
			fmt.Fprintf(a.stderr, "nuno: no provider %q (configured: %s)\n", providerName, strings.Join(names, ", "))
		} else {
			fmt.Fprintf(a.stderr, "nuno: no provider %q: %v\n", providerName, err)
		}
		return core.User{}, store.ProviderRow{}, ExitConfig
	}
	return user, provider, ExitClean
}

// manageUsers handles the manual-user commands. A person who exists in no
// directory is how a service outside SSO gets a quota at all (FR-2).
func manageUsers(ctx context.Context, a *app, args []string, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "usage: nuno users list | nuno users add <uid> [email] | nuno users remove <uid>")
		return ExitConfig
	}

	switch args[0] {
	case "list":
		return listUsers(ctx, a, stdout)
	case "add":
		if len(args) < 2 {
			fmt.Fprintln(a.stderr, "usage: nuno users add <uid> [email]")
			return ExitConfig
		}
		email := ""
		if len(args) > 2 {
			email = args[2]
		}
		user, err := a.db.AddManualUser(ctx, core.User{UID: args[1], Email: email})
		if err != nil {
			fmt.Fprintf(a.stderr, "nuno: %v\n", err)
			return ExitConfig
		}
		fmt.Fprintf(stdout, "added manual user %s (%s)\n", user.UID, user.SourceUUID)
		fmt.Fprintln(stdout, "run `nuno observe` to link them to an account")
		return ExitClean
	case "remove":
		if len(args) != 2 {
			fmt.Fprintln(a.stderr, "usage: nuno users remove <uid>")
			return ExitConfig
		}
		users, err := a.db.ListUsers(ctx)
		if err != nil {
			a.log.Error("read users", "error", err)
			return ExitError
		}
		for _, u := range users {
			if strings.EqualFold(u.UID, args[1]) {
				// Removing a person never touches provider data: their
				// account simply becomes unmanaged again (FR-8).
				if _, err := a.db.DeleteUser(ctx, u.ID); err != nil {
					a.log.Error("delete user", "error", err)
					return ExitError
				}
				fmt.Fprintf(stdout, "removed %s: their accounts are unmanaged now, and their data is untouched\n", u.UID)
				return ExitClean
			}
		}
		fmt.Fprintf(a.stderr, "nuno: no user %q\n", args[1])
		return ExitConfig
	}

	fmt.Fprintf(a.stderr, "nuno: unknown users subcommand %q\n", args[0])
	return ExitConfig
}

func listUsers(ctx context.Context, a *app, stdout io.Writer) int {
	users, err := a.db.ListUsers(ctx)
	if err != nil {
		a.log.Error("read users", "error", err)
		return ExitError
	}
	if len(users) == 0 {
		fmt.Fprintln(stdout, "No users. Configure NUNO_LDAP_URL, or add one with `nuno users add <uid> [email]`.")
		return ExitClean
	}

	table := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(table, "UID\tSOURCE\tSTATUS\tEMAIL\tGROUPS")
	for _, u := range users {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\n", u.UID, u.Source, u.Status, u.Email, len(u.GroupUUIDs))
	}
	table.Flush()
	return ExitClean
}

// showUsage prints the same picture the page and the endpoint show, for a
// terminal. The three read from one domain function, so they cannot disagree.
func showUsage(ctx context.Context, a *app, stdout io.Writer) int {
	users, err := a.db.ListUsers(ctx)
	if err != nil {
		a.log.Error("read users", "error", err)
		return ExitError
	}
	providers, err := a.db.ListProviders(ctx)
	if err != nil {
		a.log.Error("read providers", "error", err)
		return ExitError
	}
	accounts, err := a.db.ListAllExternalAccounts(ctx)
	if err != nil {
		a.log.Error("read accounts", "error", err)
		return ExitError
	}

	observed := make([]core.ObservedProvider, 0, len(providers))
	for _, p := range providers {
		observed = append(observed, core.ObservedProvider{
			ID: p.ID, Type: p.Type, Name: p.Name,
			HasObserved: p.LastObserveAt != nil, LastObserveOK: p.LastObserveOK,
		})
	}
	active := make([]core.User, 0, len(users))
	for _, u := range users {
		if u.Status == core.UserActive {
			active = append(active, u)
		}
	}

	policy, err := a.db.LoadPolicy(ctx)
	if err != nil {
		a.log.Error("read policy", "error", err)
		return ExitError
	}
	userPolicies := make(map[int64]core.UserPolicy, len(active))
	for _, u := range active {
		up, _, err := a.db.UserPolicy(ctx, u.ID)
		if err != nil {
			a.log.Error("read user policy", "error", err, "user", u.UID)
			return ExitError
		}
		userPolicies[u.ID] = up
	}

	response := core.BuildUsage(core.UsageInput{
		Now:             time.Now().UTC(),
		RefreshInterval: a.cfg.RefreshInterval,
		Users:           active,
		Providers:       observed,
		Accounts:        accounts,
		Policy:          policy,
		UserPolicies:    userPolicies,
	})

	if len(response.Users) == 0 {
		fmt.Fprintln(stdout, "Nobody to report on. Add someone with `nuno users add <uid> [email]`, then run `nuno observe`.")
		return ExitClean
	}

	table := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(table, "PERSON\tSERVICE\tUSED\tCEILING\tSHARE\tSTATUS")
	for _, user := range response.Users {
		total := "unknown"
		if user.UsedBytes != nil {
			total = core.FormatIEC(*user.UsedBytes)
		}
		budget := "unlimited"
		if user.BudgetBytes != nil {
			budget = core.FormatIEC(*user.BudgetBytes)
		}
		complete := ""
		if !user.Complete {
			complete = "partial"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			user.User, "(total)", total, budget, sharePercent(user.UsedPercent), complete)

		for _, p := range user.Providers {
			used := "-"
			if p.UsedBytes != nil {
				used = core.FormatIEC(*p.UsedBytes)
			}
			ceiling := "unlimited"
			switch {
			case p.Status == core.StatusUnknown || p.Status == core.StatusUnavailable:
				ceiling = p.Status
			case p.QuotaBytes != nil:
				ceiling = core.FormatIEC(*p.QuotaBytes)
			}
			fmt.Fprintf(table, "\t%s\t%s\t%s\t%s\t%s\n",
				p.Name, used, ceiling, sharePercent(p.UsedPercent), p.Status)
		}
	}
	table.Flush()
	return ExitClean
}

func sharePercent(percent *float64) string {
	if percent == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *percent)
}
