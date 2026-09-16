// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

type quotasData struct {
	layout
	Query   string
	ShowAll bool
	People  []personView
}

type personView struct {
	UID       string
	TierPills []string
	Used      string
	Budget    string
	Percent   int
	Fill      string
	Partial   bool
	Rows      []ceilingRow

	// Empty is true when this person has no linked account and no override,
	// which is what a directory service account looks like. The page hides
	// those by default: they are not customers (FR-57).
	Empty bool

	// Elsewhere are the services this person has no row for, which is what the
	// ghost row at the bottom of the card offers.
	Elsewhere []providerField
}

type ceilingRow struct {
	Provider string
	Type     string
	Used     string
	Ceiling  string
	// Edit is what the input starts with, which is empty whenever the current
	// value is not something anybody could have typed.
	Edit     string
	Percent  int
	Fill     string
	Status   string
	Linked   bool
	Override bool
}

// quotasHandler renders one card per person, with the ceilings editable in
// place. It is the page the admin surface opens on.
func quotasHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		data := quotasData{
			layout:  newLayout(opts, r, "Quotas"),
			Query:   strings.TrimSpace(r.URL.Query().Get("q")),
			ShowAll: r.URL.Query().Get("all") == "1",
		}
		providers := providerRows(ctx, opts, &data.layout)

		// The admin page shows everyone, including the service accounts in the
		// directory: an admin needs to see them to know they are not managed.
		usage, err := buildUsage(ctx, opts, false)
		if err != nil {
			opts.Log.Error("page: build usage", "error", err)
			data.Warnings = append(data.Warnings, "Usage could not be read from the database: "+err.Error())
		}

		users, err := opts.Store.ListUsers(ctx)
		if err != nil {
			opts.Log.Error("page: read users", "error", err)
		}
		byUID := map[string]core.User{}
		for _, u := range users {
			byUID[u.UID] = u
		}
		policy, err := opts.Store.LoadPolicy(ctx)
		if err != nil {
			opts.Log.Error("page: read policy", "error", err)
		}

		for _, entry := range usage.Users {
			if data.Query != "" && !strings.Contains(strings.ToLower(entry.User), strings.ToLower(data.Query)) {
				continue
			}
			view := personFor(ctx, opts, entry, byUID[entry.User], providers, policy)
			// A person with nothing to manage is hidden unless the admin asks
			// for them, or is searching by name. The service accounts in the
			// directory are exactly this shape and made the page noisy.
			if view.Empty && !data.ShowAll && data.Query == "" {
				continue
			}
			data.People = append(data.People, view)
		}
		render(w, opts, quotasTemplate, data)
	}
}

func personFor(ctx context.Context, opts Options, entry core.UsageUser, user core.User,
	providers []store.ProviderRow, policy core.Policy,
) personView {
	view := personView{
		UID:     entry.User,
		Used:    "unknown",
		Budget:  "unlimited",
		Partial: !entry.Complete,
		Percent: barWidth(entry.UsedPercent),
	}
	if entry.UsedBytes != nil {
		view.Used = core.FormatIEC(*entry.UsedBytes)
	}
	if entry.BudgetBytes != nil {
		view.Budget = core.FormatIEC(*entry.BudgetBytes)
	}
	view.Fill = totalFill(view.Percent)

	up, _, err := opts.Store.UserPolicy(ctx, user.ID)
	if err != nil {
		opts.Log.Error("page: read one person's policy", "error", err, "user", entry.User)
	}

	observed := map[string]core.UsageProvider{}
	for _, p := range entry.Providers {
		observed[p.Name] = p
	}

	for _, provider := range providers {
		resolution := core.Resolve(user, provider.ID, policy, up)
		if resolution.TierName != "" && !slices.Contains(view.TierPills, resolution.TierName) {
			view.TierPills = append(view.TierPills, resolution.TierName)
		}

		override, hasOverride := up.ProviderOverrides[provider.ID]
		usageRow, linked := observed[provider.Name]
		if !linked && !hasOverride {
			view.Elsewhere = append(view.Elsewhere, providerField{ID: provider.ID, Name: provider.Name, Type: provider.Type})
			continue
		}

		row := ceilingRow{
			Provider: provider.Name,
			Type:     provider.Type,
			Used:     "—",
			Ceiling:  "—",
			Linked:   linked,
			// An override equal to what the tier already gives changes nothing,
			// so the tag would only be noise. See overrideRedundant.
			Override: hasOverride && !overrideRedundant(user, provider.ID, policy, up),
			Status:   usageRow.Status,
		}
		if linked {
			if usageRow.UsedBytes != nil {
				row.Used = core.FormatIEC(*usageRow.UsedBytes)
			}
			row.Ceiling = quotaText(usageRow.QuotaBytes, usageRow.Status)
			row.Percent = barWidth(usageRow.UsedPercent)
			row.Fill = fillFor(row.Percent, usageRow.Status)
			if usageRow.QuotaBytes != nil && (usageRow.Status == core.StatusOK || usageRow.Status == core.StatusStale) {
				// The input holds a bare number of GiB; the unit is a suffix.
				row.Edit = core.FormatGiBNumber(*usageRow.QuotaBytes)
			}
		}
		if hasOverride {
			// An override is what an admin set, so it is what the field edits,
			// even on a service this person has no account on yet.
			row.Ceiling = override.String()
			row.Edit = editGiB(override)
		}
		view.Rows = append(view.Rows, row)
	}
	view.Empty = len(view.Rows) == 0
	return view
}

// editGiB is what an editable field starts with: a bare number of GiB, so the
// unit can sit outside the input. A state a number cannot express (unlimited,
// unknown) is shown as it is, because hiding it would be worse.
func editGiB(q core.Quota) string {
	if q.IsBytes() {
		return core.FormatGiBNumber(q.Bytes)
	}
	return q.String()
}

// overrideRedundant reports whether an override is the same as what the tier
// chain already resolves to for that provider. Such an override is stored but
// changes nothing, and tagging it "override" makes an admin think a decision
// is in play when it is not.
func overrideRedundant(user core.User, providerID int64, policy core.Policy, up core.UserPolicy) bool {
	override, ok := up.ProviderOverrides[providerID]
	if !ok {
		return false
	}
	without := up
	without.ProviderOverrides = make(map[int64]core.Quota, len(up.ProviderOverrides))
	for id, quota := range up.ProviderOverrides {
		if id != providerID {
			without.ProviderOverrides[id] = quota
		}
	}
	resolved := core.Resolve(user, providerID, policy, without)
	return resolved.Present && resolved.Ceiling.Equal(override)
}

// totalFill is the colour of the whole-person bar. It is about how full the
// budget is, not about any one provider's status, so it is its own function:
// the override action has to answer with the same colour the page renders.
func totalFill(percent int) string {
	switch {
	case percent >= 90:
		return "fill-bad"
	case percent >= 70:
		return "fill-warn"
	}
	return ""
}

type tiersData struct {
	layout
	Tiers       []tierView
	Fields      []providerField
	Unmapped    []groupChip
	DefaultTier string
}

type tierView struct {
	Name        string
	Budget      string
	BudgetValue string
	IsDefault   bool
	Overcommit  bool
	Allocations []allocationView
	Groups      []groupChip
}

type allocationView struct {
	ProviderID int64
	Provider   string
	// Input is what the field starts with: a bare number of GiB when the tier
	// allocates an absolute amount, empty when the tier is silent about this
	// service (FR-17).
	Input string
	// Display is the human form shown when the field is not being edited. A
	// percentage is resolved against the budget here, so the page never shows
	// a percentage: a ceiling a person reads should not move when somebody
	// else's budget changes.
	Display string
	// Raw is the allocation as stored, which is what a form that is not
	// editing a ceiling carries, so "make default" does not silently rewrite a
	// percentage into an absolute amount.
	Raw string
}

type groupChip struct {
	Name    string
	UUID    string
	Members int
}

// tiersHandler renders one card per tier: its allocations, and the directory
// groups mapped to it as chips.
func tiersHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		data := tiersData{layout: newLayout(opts, r, "Tiers")}
		providers := providerRows(ctx, opts, &data.layout)
		data.Fields = providerFields(providers)

		policy, err := opts.Store.LoadPolicy(ctx)
		if err != nil {
			opts.Log.Error("page: read policy", "error", err)
			data.Warnings = append(data.Warnings, "The policy could not be read: "+err.Error())
			render(w, opts, tiersTemplate, data)
			return
		}
		groups, err := opts.Store.ListGroups(ctx)
		if err != nil {
			opts.Log.Error("page: read groups", "error", err)
		}
		users, err := opts.Store.ListUsers(ctx)
		if err != nil {
			opts.Log.Error("page: read users", "error", err)
		}

		// Membership is already loaded with the people, so the chip counts are
		// a tally rather than another query.
		members := map[string]int{}
		for _, u := range users {
			for _, uuid := range u.GroupUUIDs {
				members[uuid]++
			}
		}

		chipsByTier := map[int64][]groupChip{}
		for _, g := range groups {
			chip := groupChip{Name: g.Name, UUID: g.SourceUUID, Members: members[g.SourceUUID]}
			tierID, mapped := policy.GroupTiers[g.SourceUUID]
			if !mapped {
				data.Unmapped = append(data.Unmapped, chip)
				continue
			}
			chipsByTier[tierID] = append(chipsByTier[tierID], chip)
		}

		for _, tier := range policy.Tiers {
			view := tierView{
				Name:       tier.Name,
				IsDefault:  tier.IsDefault,
				Overcommit: tier.Overcommitted(),
				Groups:     chipsByTier[tier.ID],
			}
			// The budget is an input that percentages resolve against, not a
			// total to retype, so it is shown and carried, never edited here
			// (ADR-0020).
			if tier.Budget.IsKnown() {
				view.Budget = tier.Budget.String()
				view.BudgetValue = tier.Budget.String()
			}
			if tier.IsDefault {
				data.DefaultTier = tier.Name
			}
			for _, field := range data.Fields {
				allocation := allocationView{ProviderID: field.ID, Provider: field.Name}
				for _, a := range tier.Allocations {
					if a.ProviderID != field.ID {
						continue
					}
					if resolved, ok := tier.AllocationFor(field.ID); ok && resolved.IsBytes() {
						allocation.Input = core.FormatGiBNumber(resolved.Bytes)
						allocation.Display = core.FormatIEC(resolved.Bytes)
					} else {
						allocation.Input = a.DescribeGiB()
						allocation.Display = a.Describe()
					}
					allocation.Raw = a.Describe()
				}
				view.Allocations = append(view.Allocations, allocation)
			}
			data.Tiers = append(data.Tiers, view)
		}
		slices.SortFunc(data.Tiers, func(a, b tierView) int { return strings.Compare(a.Name, b.Name) })
		render(w, opts, tiersTemplate, data)
	}
}

type servicesData struct {
	layout
	Providers []providerView
	Keys      []store.AdminKeyRow
}

type providerView struct {
	Name        string
	Type        string
	Version     string
	Reachable   bool
	InSupported bool
	WriteAccess string
	LastObserve *time.Time
	LastError   string
	Degraded    string
}

// servicesHandler renders one status card per configured provider, and the
// keys for the usage API below them: a key is about a service integration too.
func servicesHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		data := servicesData{layout: newLayout(opts, r, "Services")}
		for _, p := range providerRows(ctx, opts, &data.layout) {
			data.Providers = append(data.Providers, providerView{
				Name:        p.Name,
				Type:        p.Type,
				Version:     p.Version,
				Reachable:   p.Reachable,
				InSupported: p.InSupported,
				WriteAccess: p.WriteAccess.String(),
				LastObserve: p.LastObserveAt,
				LastError:   p.LastError,
				Degraded:    p.DegradedReason,
			})
		}
		keys, err := opts.Store.ListAdminKeys(ctx)
		if err != nil {
			opts.Log.Error("page: read admin keys", "error", err)
		}
		data.Keys = keys
		render(w, opts, servicesTemplate, data)
	}
}

type accountsData struct {
	layout
	Resolved []accountRow
	Pending  []accountRow
	Users    []core.User
}

type accountRow struct {
	ID       int
	Account  string
	Provider string
	Person   string
	Kind     string
	Detail   string

	// Manual marks a link somebody made by hand, which is the only kind an
	// admin can undo: matching redoes itself every observe (ADR-0013).
	Manual bool

	// Pick is what the row asks for: a person for an account nobody owns, an
	// account for a person who has none, or nothing at all.
	Pick     string
	Confirm  bool
	Accounts []string
}

// accountsHandler renders every observed account in one table, resolved ones
// first and the ones needing a decision below the divider (FR-57).
func accountsHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		data := accountsData{layout: newLayout(opts, r, "Accounts")}
		providers := providerRows(ctx, opts, &data.layout)

		providerNames := map[int64]string{}
		for _, p := range providers {
			providerNames[p.ID] = p.Name
		}
		users, err := opts.Store.ListUsers(ctx)
		if err != nil {
			opts.Log.Error("page: read users", "error", err)
		}
		userNames := map[int64]string{}
		for _, u := range users {
			userNames[u.ID] = u.UID
			if u.Status == core.UserActive {
				data.Users = append(data.Users, u)
			}
		}

		accounts, err := opts.Store.ListAllExternalAccounts(ctx)
		if err != nil {
			opts.Log.Error("page: read accounts", "error", err)
			data.Warnings = append(data.Warnings, "Accounts could not be read: "+err.Error())
		}
		links, err := opts.Store.ListLinks(ctx)
		if err != nil {
			opts.Log.Error("page: read links", "error", err)
		}
		manual := map[string]bool{}
		for _, link := range links {
			if link.Origin == core.LinkManual {
				manual[linkKey(link.ProviderID, link.ExternalID)] = true
			}
		}

		unowned := map[int64][]string{}
		for _, account := range accounts {
			if account.UserID == nil {
				unowned[account.ProviderID] = append(unowned[account.ProviderID], account.ExternalID)
				continue
			}
			data.Resolved = append(data.Resolved, accountRow{
				Account:  account.ExternalID,
				Provider: providerNames[account.ProviderID],
				Person:   userNames[*account.UserID],
				Manual:   manual[linkKey(account.ProviderID, account.ExternalID)],
			})
		}

		issues, err := opts.Store.ListLinkIssues(ctx)
		if err != nil {
			opts.Log.Error("page: read link issues", "error", err)
		}
		for i, issue := range issues {
			row := accountRow{
				ID:       i,
				Account:  issue.ExternalID,
				Provider: providerNames[issue.ProviderID],
				Kind:     string(issue.Kind),
				Detail:   issue.Detail,
			}
			if issue.UserID != nil {
				row.Person = userNames[*issue.UserID]
			}
			switch {
			case issue.ExternalID != "":
				// An account nobody owns: the row asks which person it is.
				row.Pick = "person"
				row.Confirm = row.Person != ""
			case len(unowned[issue.ProviderID]) > 0:
				// A person with no account here, so the row offers the ones
				// this provider has and nobody owns.
				row.Pick = "account"
				row.Accounts = unowned[issue.ProviderID]
			}
			data.Pending = append(data.Pending, row)
		}
		render(w, opts, accountsTemplate, data)
	}
}

func linkKey(providerID int64, externalID string) string {
	return strconv.FormatInt(providerID, 10) + "\x00" + externalID
}

type activityData struct {
	layout
	Runs    []store.RunSummary
	Changes []store.AuditRow
}

// activityHandler keeps the two run actions and the history that follows them.
func activityHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		data := activityData{layout: newLayout(opts, r, "Activity")}
		providerRows(ctx, opts, &data.layout)

		runs, err := opts.Store.LastRuns(ctx, 5)
		if err != nil {
			opts.Log.Error("page: read runs", "error", err)
		}
		changes, err := opts.Store.RecentChanges(ctx, 10)
		if err != nil {
			opts.Log.Error("page: read recent changes", "error", err)
		}
		data.Runs, data.Changes = runs, changes
		render(w, opts, activityTemplate, data)
	}
}
