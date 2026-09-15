// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// UsageSchema is the top-level version a widget can branch on. It changes only
// when the shape does, which is why unknown got a status value rather than a
// new field layout (ADR-0026).
const UsageSchema = 1

// Per-provider status. A number in this response is readable only together
// with its status: null means unlimited under ok and stale, and means nothing
// at all under the other two.
const (
	// StatusOK means observed_at is within twice the refresh interval.
	StatusOK = "ok"
	// StatusStale means the reading is older than that.
	StatusStale = "stale"
	// StatusUnavailable means the last observe of that provider failed.
	StatusUnavailable = "unavailable"
	// StatusUnknown means the call succeeded but the values did not: a
	// Nextcloud quota object serialized as [], or Immich's two counters
	// disagreeing beyond the threshold.
	StatusUnknown = "unknown"
)

type UsageResponse struct {
	Schema int         `json:"schema"`
	Users  []UsageUser `json:"users"`
}

type UsageUser struct {
	User     string `json:"user"`
	UserUUID string `json:"user_uuid"`

	// BudgetBytes is the sum of the ceilings that are known, null when any of
	// them is unlimited. Before policy exists that is all it can mean
	// (ADR-0022); in M2 it becomes the resolved budget without changing shape.
	BudgetBytes *int64   `json:"budget_bytes"`
	UsedBytes   *int64   `json:"used_bytes"`
	UsedPercent *float64 `json:"used_percent"`

	// Complete is false when at least one of this person's providers
	// contributed no ceiling or no usage, so the sums above are partial.
	Complete bool `json:"complete"`

	// Summary is a human string for a dashboard widget, which cannot join two
	// fields into one label: "9.3 GiB / 200 GiB (4.6%)".
	Summary string `json:"summary"`

	Providers []UsageProvider `json:"providers"`
}

type UsageProvider struct {
	Type string `json:"type"`
	// Name distinguishes two instances of one type, which the schema models
	// from day one (FR-27).
	Name string `json:"name"`

	// QuotaBytes is always the observed ceiling, never a desired one: the
	// desired value belongs to a plan, and mixing them would make this
	// endpoint lie between a plan and its application (ADR-0022).
	QuotaBytes  *int64   `json:"quota_bytes"`
	UsedBytes   *int64   `json:"used_bytes"`
	UsedPercent *float64 `json:"used_percent"`

	// Managed marks a ceiling this policy still allocates. Before policy
	// exists nothing manages anything, so it is false everywhere.
	Managed    bool      `json:"managed"`
	Status     string    `json:"status"`
	ObservedAt time.Time `json:"observed_at"`

	// Summary is the same human string as the user's, per provider.
	Summary string `json:"summary"`
}

// UsageRow is one line of the flattened view, which is what a Homepage
// dynamic-list can render: a flat array of name and label. The nested shape is
// the contract; this is the same data arranged for a widget that cannot walk
// into an array of arrays.
type UsageRow struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
}

type UsageDetailResponse struct {
	Schema int        `json:"schema"`
	Rows   []UsageRow `json:"rows"`
}

// ObservedProvider is a provider instance plus whether its last read worked.
type ObservedProvider struct {
	ID            int64
	Type          string
	Name          string
	HasObserved   bool
	LastObserveOK bool
}

// UsageInput is everything BuildUsage needs. Passing it in keeps the function
// pure, which is what makes the contract testable without a database.
type UsageInput struct {
	Now             time.Time
	RefreshInterval time.Duration
	Users           []User
	Providers       []ObservedProvider
	Accounts        []ExternalAccount

	// Policy and UserPolicies are optional. When Policy has tiers, budget_bytes
	// is the resolved effective budget and managed reflects the policy, which
	// is what ADR-0022 promised for M2. When it has none, the budget falls back
	// to the sum of observed ceilings, the documented pre-policy meaning.
	Policy       Policy
	UserPolicies map[int64]UserPolicy
}

// BuildUsage assembles the dashboard response.
//
// The array is sorted by user_uuid and every provider list by name, because a
// customapi widget addresses fields by path: an unstable order would silently
// swap two people's numbers on a dashboard (ADR-0022).
func BuildUsage(in UsageInput) UsageResponse {
	providersByID := make(map[int64]ObservedProvider, len(in.Providers))
	for _, p := range in.Providers {
		providersByID[p.ID] = p
	}

	accountsByUser := map[int64][]ExternalAccount{}
	for _, a := range in.Accounts {
		if a.UserID == nil {
			// An unmanaged account belongs to nobody, so it appears in the
			// link issues rather than in anyone's usage.
			continue
		}
		accountsByUser[*a.UserID] = append(accountsByUser[*a.UserID], a)
	}

	hasPolicy := len(in.Policy.Tiers) > 0

	response := UsageResponse{Schema: UsageSchema, Users: make([]UsageUser, 0, len(in.Users))}
	for _, user := range in.Users {
		entry := UsageUser{
			User:      user.UID,
			UserUUID:  user.SourceUUID,
			Complete:  true,
			Providers: make([]UsageProvider, 0, len(accountsByUser[user.ID])),
		}

		// Resolve the policy for every configured provider, not only the ones
		// this person has an account on: the effective budget is the sum of the
		// resolved ceilings, and managed is a property of the policy, not of
		// whether an account happens to exist (ADR-0020, FR-47).
		resolutions := make(map[int64]Resolution, len(in.Providers))
		ordered := make([]Resolution, 0, len(in.Providers))
		if hasPolicy {
			up := in.UserPolicies[user.ID]
			for _, p := range in.Providers {
				r := Resolve(user, p.ID, in.Policy, up)
				resolutions[p.ID] = r
				ordered = append(ordered, r)
			}
		}

		var budget int64
		var used int64
		anyUnlimited := false

		for _, account := range accountsByUser[user.ID] {
			provider, known := providersByID[account.ProviderID]
			if !known {
				continue
			}
			view := usageFor(in, provider, account, resolutions[account.ProviderID].Present)
			entry.Providers = append(entry.Providers, view)

			switch {
			case view.Status != StatusOK && view.Status != StatusStale:
				entry.Complete = false
			case view.QuotaBytes == nil && !account.Quota.IsUnlimited():
				entry.Complete = false
			}
			// Without policy the budget is the sum of observed ceilings. With
			// policy it is the resolved one, computed after the loop.
			if !hasPolicy {
				if account.Quota.IsUnlimited() && (view.Status == StatusOK || view.Status == StatusStale) {
					anyUnlimited = true
				}
				if view.QuotaBytes != nil {
					budget += *view.QuotaBytes
				}
			}
			if view.UsedBytes != nil {
				used += *view.UsedBytes
			} else {
				entry.Complete = false
			}
		}

		if hasPolicy {
			switch effective := EffectiveBudget(ordered); {
			case effective.IsUnlimited():
				anyUnlimited = true
			case effective.IsBytes():
				budget = effective.Bytes
			default:
				// The policy allocates nothing for this person, so there is no
				// ceiling to report. complete says so.
				entry.Complete = false
			}
		}

		slices.SortFunc(entry.Providers, func(a, b UsageProvider) int {
			if a.Name != b.Name {
				if a.Name < b.Name {
					return -1
				}
				return 1
			}
			return 0
		})

		if !anyUnlimited {
			total := budget
			entry.BudgetBytes = &total
		}
		total := used
		entry.UsedBytes = &total
		entry.UsedPercent = percent(entry.UsedBytes, entry.BudgetBytes)
		entry.Summary = summarize(entry.UsedBytes, entry.BudgetBytes)
		if !entry.Complete {
			entry.Summary += " (partial)"
		}

		response.Users = append(response.Users, entry)
	}

	slices.SortFunc(response.Users, func(a, b UsageUser) int {
		if a.UserUUID != b.UserUUID {
			if a.UserUUID < b.UserUUID {
				return -1
			}
			return 1
		}
		return 0
	})
	return response
}

func usageFor(in UsageInput, provider ObservedProvider, account ExternalAccount, managed bool) UsageProvider {
	view := UsageProvider{
		Type: provider.Type,
		Name: provider.Name,
		// Managed is whether the policy still allocates this provider for this
		// person, so a ceiling left over from a policy that no longer does is
		// visible (FR-47). It is false when there is no policy at all.
		Managed:    managed,
		ObservedAt: account.ObservedAt.UTC(),
		Status:     statusFor(in, provider, account),
	}
	if view.Status != StatusOK && view.Status != StatusStale {
		// The numbers mean nothing, so they are not reported at all.
		view.Summary = view.Status
		return view
	}

	if account.Quota.IsBytes() {
		ceiling := account.Quota.Bytes
		view.QuotaBytes = &ceiling
	}
	if account.Used.IsBytes() {
		used := account.Used.Bytes
		view.UsedBytes = &used
	}
	view.UsedPercent = percent(view.UsedBytes, view.QuotaBytes)
	view.Summary = summarize(view.UsedBytes, view.QuotaBytes)
	return view
}

// summarize is the one human string a widget can render: used, ceiling and the
// share, in one label. A nil ceiling is unlimited, a nil used is unknown.
func summarize(used, ceiling *int64) string {
	switch {
	case used == nil:
		return "unknown"
	case ceiling == nil:
		return FormatIEC(*used) + " (unlimited)"
	case *ceiling <= 0:
		return FormatIEC(*used)
	default:
		share := float64(*used) / float64(*ceiling) * 100
		return fmt.Sprintf("%s / %s (%.1f%%)", FormatIEC(*used), FormatIEC(*ceiling), share)
	}
}

// BuildUsageDetail flattens the response into rows a Homepage dynamic-list can
// render: one total row per person, then one row per provider. The name carries
// the person and, for a provider row, the service, because the widget shows a
// single name field.
func BuildUsageDetail(response UsageResponse) UsageDetailResponse {
	detail := UsageDetailResponse{Schema: response.Schema, Rows: make([]UsageRow, 0, len(response.Users))}
	for _, user := range response.Users {
		status := StatusOK
		if !user.Complete {
			status = "partial"
		}
		detail.Rows = append(detail.Rows, UsageRow{Name: user.User, Summary: user.Summary, Status: status})
		for _, provider := range user.Providers {
			detail.Rows = append(detail.Rows, UsageRow{
				Name:    user.User + " \u00b7 " + titleCase(provider.Type),
				Summary: provider.Summary,
				Status:  provider.Status,
			})
		}
	}
	return detail
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// BuildUsageByService aggregates the response per provider type, so a shared
// dashboard can show how full each service is without exposing who used what.
// The ceiling is the sum of the people's ceilings, which is a policy number,
// not the disk capacity: it answers "how much of what we allocated is used".
// Unmanaged accounts are not counted, because they belong to nobody.
func BuildUsageByService(response UsageResponse) UsageDetailResponse {
	type total struct {
		used      int64
		quota     int64
		unlimited bool
		known     bool
		partial   bool
	}
	byType := map[string]*total{}
	var order []string
	for _, user := range response.Users {
		for _, p := range user.Providers {
			t, seen := byType[p.Type]
			if !seen {
				t = &total{}
				byType[p.Type] = t
				order = append(order, p.Type)
			}
			if p.Status != StatusOK && p.Status != StatusStale {
				t.partial = true
				continue
			}
			t.known = true
			if p.UsedBytes != nil {
				t.used += *p.UsedBytes
			}
			if p.QuotaBytes == nil {
				t.unlimited = true
			} else {
				t.quota += *p.QuotaBytes
			}
		}
	}
	slices.Sort(order)

	rows := make([]UsageRow, 0, len(order))
	for _, typ := range order {
		t := byType[typ]
		status := StatusOK
		if t.partial {
			status = "partial"
		}
		rows = append(rows, UsageRow{
			Name:    titleCase(typ),
			Summary: summarizeTotal(t.used, t.quota, t.unlimited, t.known),
			Status:  status,
		})
	}
	return UsageDetailResponse{Schema: response.Schema, Rows: rows}
}

func summarizeTotal(used, quota int64, unlimited, known bool) string {
	switch {
	case !known:
		return "unknown"
	case unlimited:
		return FormatIEC(used) + " (unlimited)"
	case quota <= 0:
		return FormatIEC(used)
	default:
		share := float64(used) / float64(quota) * 100
		return fmt.Sprintf("%s / %s (%.1f%%)", FormatIEC(used), FormatIEC(quota), share)
	}
}

func statusFor(in UsageInput, provider ObservedProvider, account ExternalAccount) string {
	if !provider.HasObserved || !provider.LastObserveOK || !account.ObserveOK {
		return StatusUnavailable
	}
	// Unknown is about the values, not the call: the read succeeded and these
	// are simply not conclusions anyone can draw (ADR-0026).
	if !account.Quota.IsKnown() || !account.Used.IsKnown() {
		return StatusUnknown
	}
	if in.RefreshInterval > 0 && in.Now.Sub(account.ObservedAt) > 2*in.RefreshInterval {
		return StatusStale
	}
	return StatusOK
}

// percent is null rather than NaN when there is no meaningful denominator: Go
// refuses to marshal NaN, which would turn the most privileged users into a
// 500 (ADR-0017).
func percent(used, ceiling *int64) *float64 {
	if used == nil || ceiling == nil || *ceiling <= 0 {
		return nil
	}
	value := float64(*used) / float64(*ceiling) * 100
	rounded := float64(int64(value*10+0.5)) / 10
	return &rounded
}
