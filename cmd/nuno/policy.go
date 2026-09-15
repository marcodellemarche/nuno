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

// manageTiers is the policy surface until the UI has one.
func manageTiers(ctx context.Context, a *app, args []string, stdout io.Writer) int {
	usage := `usage:
  nuno tiers list
  nuno tiers set <name> [--budget=<size>] [--default] [<provider>=<25%|50GiB|unlimited> ...]
  nuno tiers delete <name>
  nuno tiers default <name>
  nuno tiers map <group> <tier>
  nuno tiers unmap <group>

A provider a tier says nothing about is left alone: absent is not zero and not
unlimited, so removing an allocation means "do not touch", not "take it away".`

	if len(args) == 0 {
		fmt.Fprintln(a.stderr, usage)
		return ExitConfig
	}

	switch args[0] {
	case "list":
		return listTiers(ctx, a, stdout)
	case "set":
		return setTier(ctx, a, args[1:], stdout)
	case "delete":
		if len(args) != 2 {
			fmt.Fprintln(a.stderr, "usage: nuno tiers delete <name>")
			return ExitConfig
		}
		deleted, err := a.db.DeleteTier(ctx, args[1])
		if err != nil {
			fmt.Fprintf(a.stderr, "nuno: %v\n", err)
			return ExitError
		}
		if !deleted {
			fmt.Fprintf(a.stderr, "nuno: no tier %q\n", args[1])
			return ExitConfig
		}
		fmt.Fprintf(stdout, "deleted tier %s. Nothing on any provider changed: run `nuno plan` to see what that means.\n", args[1])
		return ExitClean
	case "default":
		if len(args) != 2 {
			fmt.Fprintln(a.stderr, "usage: nuno tiers default <name>")
			return ExitConfig
		}
		tier, code := findTier(ctx, a, args[1])
		if code != ExitClean {
			return code
		}
		if err := a.db.SetDefaultTier(ctx, tier.ID); err != nil {
			fmt.Fprintf(a.stderr, "nuno: %v\n", err)
			return ExitError
		}
		fmt.Fprintf(stdout, "%s is now the default tier for anyone no group assigns\n", tier.Name)
		return ExitClean
	case "map":
		if len(args) != 3 {
			fmt.Fprintln(a.stderr, "usage: nuno tiers map <group> <tier>")
			return ExitConfig
		}
		return mapGroup(ctx, a, args[1], args[2], stdout)
	case "unmap":
		if len(args) != 2 {
			fmt.Fprintln(a.stderr, "usage: nuno tiers unmap <group>")
			return ExitConfig
		}
		uuid, code := findGroupUUID(ctx, a, args[1])
		if code != ExitClean {
			return code
		}
		removed, err := a.db.UnmapGroup(ctx, uuid)
		if err != nil {
			fmt.Fprintf(a.stderr, "nuno: %v\n", err)
			return ExitError
		}
		if !removed {
			fmt.Fprintf(a.stderr, "nuno: group %q was not mapped to a tier\n", args[1])
			return ExitConfig
		}
		fmt.Fprintf(stdout, "unmapped %s\n", args[1])
		return ExitClean
	}

	fmt.Fprintln(a.stderr, usage)
	return ExitConfig
}

func listTiers(ctx context.Context, a *app, stdout io.Writer) int {
	policy, err := a.db.LoadPolicy(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	if len(policy.Tiers) == 0 {
		fmt.Fprintln(stdout, "No tiers. Create one with `nuno tiers set standard --budget=100GiB cloud=25% photos=75%`.")
		return ExitClean
	}

	providers, err := a.db.ListProviders(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	names := map[int64]string{}
	for _, p := range providers {
		names[p.ID] = p.Name
	}

	table := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(table, "TIER\tBUDGET\tDEFAULT\tALLOCATIONS\tNOTE")
	for _, tier := range sortedTiers(policy) {
		var allocations []string
		for _, allocation := range tier.Allocations {
			name := names[allocation.ProviderID]
			if name == "" {
				name = fmt.Sprintf("provider %d", allocation.ProviderID)
			}
			allocations = append(allocations, name+"="+allocation.Describe())
		}
		if len(allocations) == 0 {
			allocations = []string{"none, so this tier touches nothing"}
		}
		note := ""
		if tier.Overcommitted() {
			note = "over-commits storage deliberately"
		}
		isDefault := ""
		if tier.IsDefault {
			isDefault = "yes"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n",
			tier.Name, tier.Budget.String(), isDefault, strings.Join(allocations, " "), note)
	}
	table.Flush()

	mapping, err := a.db.GroupsWithTiers(ctx)
	if err == nil && len(mapping) > 0 {
		fmt.Fprintln(stdout, "\nGroups:")
		groupTable := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(groupTable, "GROUP\tTIER")
		for group, tier := range mapping {
			fmt.Fprintf(groupTable, "%s\t%s\n", group, tier)
		}
		groupTable.Flush()
	}

	// The operational consequence an admin meets on day one (FR-19a).
	fmt.Fprintln(stdout, "\nThe most generous ceiling wins, per provider. Adding somebody to a stricter")
	fmt.Fprintln(stdout, "tier does not restrict them: demoting means removing the generous group.")
	return ExitClean
}

func sortedTiers(policy core.Policy) []core.Tier {
	tiers := make([]core.Tier, 0, len(policy.Tiers))
	for _, tier := range policy.Tiers {
		tiers = append(tiers, tier)
	}
	for i := range tiers {
		for j := i + 1; j < len(tiers); j++ {
			if tiers[j].Name < tiers[i].Name {
				tiers[i], tiers[j] = tiers[j], tiers[i]
			}
		}
	}
	return tiers
}

func setTier(ctx context.Context, a *app, args []string, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "usage: nuno tiers set <name> [--budget=<size>] [--default] [<provider>=<allocation> ...]")
		return ExitConfig
	}

	policy, err := a.db.LoadPolicy(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	providers, err := a.db.ListProviders(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	byName := map[string]store.ProviderRow{}
	var configured []string
	for _, p := range providers {
		byName[p.Name] = p
		configured = append(configured, p.Name)
	}

	tier := core.Tier{Name: args[0]}
	// Editing an existing tier keeps what was not mentioned, except the
	// allocations, which are replaced when any is given.
	for _, existing := range policy.Tiers {
		if strings.EqualFold(existing.Name, tier.Name) {
			tier = existing
			tier.Allocations = nil
			break
		}
	}
	gaveAllocations := false

	for _, arg := range args[1:] {
		switch {
		case arg == "--default":
			tier.IsDefault = true
		case strings.HasPrefix(arg, "--budget="):
			budget, err := core.ParseSize(strings.TrimPrefix(arg, "--budget="))
			if err != nil {
				fmt.Fprintf(a.stderr, "nuno: --budget: %v\n", err)
				return ExitConfig
			}
			tier.Budget = budget
		case strings.HasPrefix(arg, "--"):
			fmt.Fprintf(a.stderr, "nuno: unknown option %q\n", arg)
			return ExitConfig
		default:
			name, spec, ok := strings.Cut(arg, "=")
			if !ok {
				fmt.Fprintf(a.stderr, "nuno: %q is not <provider>=<allocation>\n", arg)
				return ExitConfig
			}
			row, known := byName[name]
			if !known {
				fmt.Fprintf(a.stderr, "nuno: no provider %q (configured: %s)\n", name, strings.Join(configured, ", "))
				return ExitConfig
			}
			allocation, err := core.ParseAllocation(spec)
			if err != nil {
				fmt.Fprintf(a.stderr, "nuno: %s: %v\n", name, err)
				return ExitConfig
			}
			allocation.ProviderID = row.ID
			tier.Allocations = append(tier.Allocations, allocation)
			gaveAllocations = true
		}
	}

	if _, err := a.db.SaveTier(ctx, tier); err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitConfig
	}

	fmt.Fprintf(stdout, "saved tier %s\n", tier.Name)
	if !gaveAllocations && len(tier.Allocations) == 0 {
		fmt.Fprintln(stdout, "it allocates nothing, so it touches no provider at all")
	}
	if tier.Overcommitted() {
		fmt.Fprintln(stdout, "note: its percentages add up to more than 100, which over-commits storage deliberately")
	}
	fmt.Fprintln(stdout, "run `nuno plan` to see what this would change")
	return ExitClean
}

func mapGroup(ctx context.Context, a *app, groupName, tierName string, stdout io.Writer) int {
	tier, code := findTier(ctx, a, tierName)
	if code != ExitClean {
		return code
	}
	uuid, code := findGroupUUID(ctx, a, groupName)
	if code != ExitClean {
		return code
	}
	if err := a.db.MapGroupToTier(ctx, uuid, tier.ID); err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitConfig
	}
	fmt.Fprintf(stdout, "members of %s are now entitled to %s\n", groupName, tier.Name)
	return ExitClean
}

func findTier(ctx context.Context, a *app, name string) (core.Tier, int) {
	policy, err := a.db.LoadPolicy(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return core.Tier{}, ExitError
	}
	var known []string
	for _, tier := range policy.Tiers {
		known = append(known, tier.Name)
		if strings.EqualFold(tier.Name, name) {
			return tier, ExitClean
		}
	}
	fmt.Fprintf(a.stderr, "nuno: no tier %q (known: %s)\n", name, strings.Join(known, ", "))
	return core.Tier{}, ExitConfig
}

func findGroupUUID(ctx context.Context, a *app, name string) (string, int) {
	groups, err := a.db.ListGroups(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return "", ExitError
	}
	var known []string
	for _, group := range groups {
		known = append(known, group.Name)
		if strings.EqualFold(group.Name, name) {
			return group.SourceUUID, ExitClean
		}
	}
	fmt.Fprintf(a.stderr, "nuno: no group %q (known: %s). Run `nuno observe` to sync the directory.\n",
		name, strings.Join(known, ", "))
	return "", ExitConfig
}

// setOverride is the per-user, per-provider override, which sits above every
// tier (FR-15).
func setOverride(ctx context.Context, a *app, args []string, stdout io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(a.stderr, "usage: nuno override <uid> <provider> <size|unlimited|clear>")
		return ExitConfig
	}
	uid, providerName := args[0], args[1]
	user, provider, code := resolveTarget(ctx, a, uid, providerName)
	if code != ExitClean {
		return code
	}

	if len(args) == 3 && args[2] == "clear" {
		cleared, err := a.db.ClearProviderOverride(ctx, user.ID, provider.ID)
		if err != nil {
			fmt.Fprintf(a.stderr, "nuno: %v\n", err)
			return ExitError
		}
		if !cleared {
			fmt.Fprintf(a.stderr, "nuno: %s had no override on %s\n", uid, providerName)
			return ExitConfig
		}
		fmt.Fprintf(stdout, "cleared the override for %s on %s: their tier decides again\n", uid, providerName)
		return ExitClean
	}
	if len(args) != 3 {
		fmt.Fprintln(a.stderr, "usage: nuno override <uid> <provider> <size|unlimited|clear>")
		return ExitConfig
	}

	quota, err := core.ParseSize(args[2])
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitConfig
	}
	if err := a.db.SetProviderOverride(ctx, user.ID, provider.ID, quota, store.OverrideManual); err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "%s now has %s on %s, above any tier\n", uid, quota, providerName)
	fmt.Fprintln(stdout, "run `nuno plan` to see what this would change")
	return ExitClean
}

// showPlan prints what a run would do, and returns 2 when the plan is not
// empty, following the dry-run contract (FR-72).
func showPlan(ctx context.Context, a *app, args []string, stdout io.Writer) int {
	filter, code := parseFilter(a, args)
	if code != ExitClean {
		return code
	}

	engine := reconcile.New(a.db, a.log)
	result, err := engine.Plan(ctx, a.instances, filter)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	if _, err := engine.SavePlan(ctx, store.RunPlan, "cli", false, result); err != nil {
		a.log.Error("persist the plan", "error", err)
	}

	writePlan(stdout, result)
	if result.Plan.Empty() {
		return ExitClean
	}
	return ExitIncomplete
}

func writePlan(stdout io.Writer, result reconcile.PlanResult) {
	if result.Plan.Empty() {
		fmt.Fprintf(stdout, "Nothing to change across %d linked accounts.\n", result.Considered)
		if result.Plan.Unallocated > 0 {
			fmt.Fprintf(stdout, "%d of them are on providers no tier allocates, which are left alone.\n",
				result.Plan.Unallocated)
		}
		return
	}

	table := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(table, "PERSON\tSERVICE\tFROM\tTO\tCLASS\tWHY")
	for _, change := range result.Plan.Changes {
		why := string(change.Rule)
		if change.TierName != "" {
			why += " (" + change.TierName + ")"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			change.UserUID, change.Provider, change.From.String(), change.To.String(), change.Class, why)
	}
	table.Flush()

	counts := result.Plan.CountByClass()
	fmt.Fprintf(stdout, "\n%d change(s): %d safe, %d guarded, %d against unknown state\n",
		len(result.Plan.Changes), counts[core.ClassSafe],
		counts[core.ClassShrinkBelowUsage], counts[core.ClassUnknownState])

	for _, change := range result.Plan.Changes {
		if change.Detail != "" {
			fmt.Fprintf(stdout, "  %s on %s: %s\n", change.UserUID, change.Provider, change.Detail)
		}
	}
	if counts[core.ClassShrinkBelowUsage] > 0 {
		fmt.Fprintln(stdout, "\nA guarded change is applied only with explicit consent, and never by the timer.")
	}
	if counts[core.ClassUnknownState] > 0 {
		fmt.Fprintln(stdout, "A change against unknown state is never applied, and there is no flag for it.")
	}
	if len(result.Degraded) > 0 {
		fmt.Fprintf(stdout, "\nDegraded, so nothing will be applied there: %s. Run `nuno doctor`.\n",
			strings.Join(result.Degraded, ", "))
	}
}

func parseFilter(a *app, args []string) (reconcile.PlanFilter, int) {
	var filter reconcile.PlanFilter
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--user="):
			filter.UID = strings.TrimPrefix(arg, "--user=")
		case strings.HasPrefix(arg, "--provider="):
			filter.Provider = strings.TrimPrefix(arg, "--provider=")
		default:
			fmt.Fprintf(a.stderr, "nuno: unknown option %q (want --user=<uid> or --provider=<name>)\n", arg)
			return filter, ExitConfig
		}
	}
	return filter, ExitClean
}

// adoptQuotas imports what each provider already has, so the first plan
// against an existing stack is empty by construction (FR-70).
func adoptQuotas(ctx context.Context, a *app, args []string, stdout io.Writer) int {
	filter, code := parseFilter(a, args)
	if code != ExitClean {
		return code
	}

	engine := reconcile.New(a.db, a.log)
	adopted, skipped, err := engine.Adopt(ctx, a.instances, filter)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}

	fmt.Fprintf(stdout, "adopted %d ceiling(s) as per-user overrides\n", adopted)
	for _, line := range skipped {
		fmt.Fprintf(stdout, "  skipped %s\n", line)
	}
	if adopted > 0 {
		fmt.Fprintln(stdout, "`nuno plan` should now be empty. Anything it still proposes is a real difference.")
	}
	if len(skipped) > 0 {
		return ExitIncomplete
	}
	return ExitClean
}

// runReconcile computes a plan and applies it. It is the CLI half of FR-35,
// and the exit codes are the contract in FR-72: 0 clean, 1 error, 2 applied
// but incomplete.
func runReconcile(ctx context.Context, a *app, args []string, stdout io.Writer) int {
	var (
		dryRun  bool
		consent bool
		filter  reconcile.PlanFilter
	)
	for _, arg := range args {
		switch {
		case arg == "--dry-run":
			dryRun = true
		case arg == "--allow-shrink":
			consent = true
		case strings.HasPrefix(arg, "--user="):
			filter.UID = strings.TrimPrefix(arg, "--user=")
		case strings.HasPrefix(arg, "--provider="):
			filter.Provider = strings.TrimPrefix(arg, "--provider=")
		default:
			fmt.Fprintf(a.stderr, "nuno: unknown option %q\n", arg)
			fmt.Fprintln(a.stderr, "usage: nuno reconcile [--dry-run] [--allow-shrink] [--user=<uid>] [--provider=<name>]")
			return ExitConfig
		}
	}

	engine := reconcile.New(a.db, a.log).WithNotifier(a.notifier, a.cfg.PublicURL)

	// Observe first: a plan is only as good as the state it was computed
	// from, and apply re-reads anyway.
	if _, err := engine.SyncIdentity(ctx, a.directory); err != nil {
		a.log.Error("identity sync failed, continuing with what is known", "error", err)
	}
	observation, err := engine.Observe(ctx, a.instances)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}

	computedAt := time.Now()
	result, err := engine.Plan(ctx, a.instances, filter)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	writePlan(stdout, result)

	if result.Plan.Empty() {
		if !observation.OK() {
			fmt.Fprintf(stdout, "\n%d provider(s) could not be read, so this is not a clean run.\n",
				len(observation.Failed()))
			return ExitIncomplete
		}
		return ExitClean
	}

	report, err := engine.Apply(ctx, a.instances, result.Plan, reconcile.ApplyOptions{
		ConsentShrink: consent,
		DryRun:        dryRun,
		Actor:         "cli",
		ComputedAt:    computedAt,
	})
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}

	fmt.Fprintln(stdout)
	if dryRun {
		fmt.Fprintf(stdout, "dry run: %d change(s) would be applied. Nothing was written.\n", report.Applied)
	} else {
		fmt.Fprintf(stdout, "%s\n", report.Summary())
	}
	for _, err := range report.Errors {
		fmt.Fprintf(stdout, "  error: %v\n", err)
	}
	if report.Guarded > 0 && !consent {
		fmt.Fprintln(stdout, "Re-run with --allow-shrink to apply the guarded changes, once you mean it.")
	}
	if report.Throttled {
		fmt.Fprintln(stdout, "The provider's write limit was reached. The rest resumes on the next run.")
	}

	switch {
	case report.Failed > 0 || len(report.Errors) > 0:
		return ExitError
	case dryRun:
		// A non-empty dry run is incomplete by the contract, the same way
		// terraform plan reports pending work.
		return ExitIncomplete
	case report.Incomplete() || !observation.OK():
		return ExitIncomplete
	}
	return ExitClean
}

// showRuns prints the recent run history and the last changes, which is what
// the UI shows under the last reconcile (FR-52).
func showRuns(ctx context.Context, a *app, stdout io.Writer) int {
	runs, err := a.db.LastRuns(ctx, 10)
	if err != nil {
		fmt.Fprintf(a.stderr, "nuno: %v\n", err)
		return ExitError
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "No runs yet. `nuno plan` records one without changing anything.")
		return ExitClean
	}

	table := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(table, "RUN\tWHEN\tMODE\tACTOR\tCONSENT\tSTATUS\tSUMMARY")
	for _, run := range runs {
		consent := ""
		if run.Consent {
			consent = "shrink"
		}
		fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			run.ID, run.StartedAt.Format("2006-01-02 15:04"), run.Mode, run.Actor, consent, run.Status, run.Summary)
	}
	table.Flush()

	changes, err := a.db.RecentChanges(ctx, 15)
	if err != nil || len(changes) == 0 {
		return ExitClean
	}
	fmt.Fprintln(stdout, "\nRecent changes")
	changeTable := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(changeTable, "WHEN\tPERSON\tSERVICE\tFROM\tTO\tRESULT\tDETAIL")
	for _, change := range changes {
		fmt.Fprintf(changeTable, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			change.At.Format("01-02 15:04"), change.User, change.Provider,
			change.From.String(), change.To.String(), change.Result, change.Detail)
	}
	changeTable.Flush()
	return ExitClean
}
