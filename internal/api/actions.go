// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/reconcile"
	"github.com/marcodellemarche/nuno/internal/store"
)

// Actor is a narrow port over the things a UI action does, so the HTTP layer
// holds no domain rules and the handlers can be tested without a stack.
type Actor interface {
	Reconcile(ctx context.Context, consentShrink bool, filter reconcile.PlanFilter) (reconcile.ApplyReport, error)
	Observe(ctx context.Context) error
	SaveTier(ctx context.Context, tier core.Tier) error
	SetOverride(ctx context.Context, uid, provider string, quota core.Quota, clear bool) error
	Link(ctx context.Context, uid, provider, externalID string, unlink bool) error
	MapGroup(ctx context.Context, groupUUID, tier string, unmap bool) error
	IssueKey(ctx context.Context, label string) (core.Secret, error)
	RevokeKey(ctx context.Context, id int64) error
}

// mutations register here. Each is a plain form post that works with no
// JavaScript at all, which is what "every UI action maps to an API route" is
// for (ADR-0006).
func registerActions(mux *http.ServeMux, opts Options) {
	if opts.Actor == nil {
		return
	}
	guard := func(next http.HandlerFunc) http.Handler {
		return proxy(opts.ProxySecret, basicAuth(opts.AdminPassword, sameOrigin(opts, next)))
	}
	mux.Handle("POST /actions/reconcile", guard(actionReconcile(opts)))
	mux.Handle("POST /actions/observe", guard(actionObserve(opts)))
	mux.Handle("POST /actions/tier", guard(actionTier(opts)))
	mux.Handle("POST /actions/override", guard(actionOverride(opts)))
	mux.Handle("POST /actions/link", guard(actionLink(opts)))
	mux.Handle("POST /actions/group", guard(actionGroup(opts)))
	mux.Handle("POST /actions/keys", guard(actionKeys(opts)))
}

// sameOrigin is the whole of the cross-site protection, and it is deliberately
// stateless. The admin surface authenticates with a basic credential, which a
// browser will happily replay for a form another site submits, so a mutation
// requires the request to say it came from here.
func sameOrigin(opts Options, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			opts.Log.Warn("refused a cross-site mutation", "site", site, "path", r.URL.Path)
			http.Error(w, "cross-site requests are refused", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Host != r.Host {
				opts.Log.Warn("refused a mutation from another origin", "origin", origin, "host", r.Host)
				http.Error(w, "cross-site requests are refused", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// redirect sends the browser back to the page with a message. Messages go in
// the query string, so nothing secret may travel this way.
func redirect(w http.ResponseWriter, r *http.Request, kind, message string) {
	respond(w, r, kind, message, "")
}

// respond answers the same action two ways. A plain form post is a redirect
// back to the page it came from; the inline editor asks for the value instead,
// so it can swap it in without a reload. One route, two shapes, and the form
// works with no JavaScript at all (ADR-0006, ADR-0030).
func respond(w http.ResponseWriter, r *http.Request, kind, message, value string) {
	respondWith(w, r, kind, message, value, nil)
}

// respondWith is respond plus derived numbers the page shows but the edited
// field does not: a budget that is the sum of the ceilings, a person's total.
// The inline editor patches them in place, so the page stays correct without a
// reload.
func respondWith(w http.ResponseWriter, r *http.Request, kind, message, value string, extra map[string]string) {
	if r.Header.Get("X-Requested-With") == "nuno-inline-edit" {
		code := http.StatusOK
		if kind == "err" {
			code = http.StatusBadRequest
		}
		body := map[string]string{kind: message, "value": value}
		for key, v := range extra {
			body[key] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(body)
		return
	}
	target := returnTo(r) + "?" + kind + "=" + url.QueryEscape(message)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// returnTo keeps a form post on the page it was submitted from. Only a local
// path is honoured, so the field cannot become an open redirect.
func returnTo(r *http.Request) string {
	target, _, _ := strings.Cut(r.PostFormValue("return"), "?")
	if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") {
		return target
	}
	return "/"
}

func actionReconcile(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "Malformed form")
			return
		}
		consent := r.PostFormValue("allow_shrink") != ""
		filter := reconcile.PlanFilter{
			UID:      strings.TrimSpace(r.PostFormValue("user")),
			Provider: strings.TrimSpace(r.PostFormValue("provider")),
		}

		// A UI reconcile can carry consent, unlike the timer, because a human
		// is the one clicking (FR-38, FR-39).
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()

		report, err := opts.Actor.Reconcile(ctx, consent, filter)
		if err != nil {
			redirect(w, r, "err", err.Error())
			return
		}
		message := report.Summary()
		if report.Guarded > 0 && !consent {
			message += ". Tick \"apply guarded changes\" to allow the ones below usage."
		}
		redirect(w, r, "ok", message)
	}
}

func actionObserve(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		if err := opts.Actor.Observe(ctx); err != nil {
			redirect(w, r, "err", err.Error())
			return
		}
		redirect(w, r, "ok", "Read every provider again")
	}
}

func actionTier(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "Malformed form")
			return
		}
		name := strings.TrimSpace(r.PostFormValue("name"))
		if name == "" {
			redirect(w, r, "err", "A tier needs a name")
			return
		}

		tier := core.Tier{Name: name, IsDefault: r.PostFormValue("is_default") != ""}
		if budget := strings.TrimSpace(r.PostFormValue("budget")); budget != "" {
			parsed, err := core.ParseSize(budget)
			if err != nil {
				redirect(w, r, "err", "Budget: "+err.Error())
				return
			}
			tier.Budget = parsed
		}

		// One field per provider, named alloc_<id>. An empty one means the
		// tier says nothing about that provider, which is not zero (FR-17).
		// The editor shows the unit outside the field, so a bare number is
		// GiB; percentages are no longer offered, and a tier whose ceilings
		// are absolute takes their sum as its budget.
		var absolute int64
		hasPercent, hasUnlimited := false, false
		for key, values := range r.PostForm {
			id, ok := strings.CutPrefix(key, "alloc_")
			if !ok || len(values) == 0 || strings.TrimSpace(values[0]) == "" {
				continue
			}
			providerID, err := strconv.ParseInt(id, 10, 64)
			if err != nil {
				continue
			}
			allocation, err := core.ParseAllocationInGiB(values[0])
			if err != nil {
				redirect(w, r, "err", fmt.Sprintf("Allocation %q: %v", values[0], err))
				return
			}
			allocation.ProviderID = providerID
			tier.Allocations = append(tier.Allocations, allocation)
			switch allocation.Mode {
			case core.ModeAbsolute:
				absolute += allocation.Value
			case core.ModePercent:
				hasPercent = true
			case core.ModeUnlimited:
				hasUnlimited = true
			}
		}
		if len(tier.Allocations) > 0 && !hasPercent && !hasUnlimited {
			budget, err := core.BytesQuota(absolute)
			if err != nil {
				redirect(w, r, "err", err.Error())
				return
			}
			tier.Budget = budget
		}

		if err := opts.Actor.SaveTier(r.Context(), tier); err != nil {
			redirect(w, r, "err", err.Error())
			return
		}
		message := "Saved tier " + name + ". Run a reconcile to apply it."
		if tier.Overcommitted() {
			message += " Its percentages add up to more than 100, which over-commits deliberately."
		}
		// The budget is the sum of the ceilings now, so the number at the top of
		// the card changes when one of them does.
		budgetText := "no budget"
		if tier.Budget.IsKnown() {
			budgetText = tier.Budget.String()
		}
		respondWith(w, r, "ok", message, editedAllocation(r, tier), map[string]string{
			"budget":     budgetText,
			"overcommit": boolFlag(tier.Overcommitted()),
		})
	}
}

func actionOverride(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "Malformed form")
			return
		}
		uid := strings.TrimSpace(r.PostFormValue("user"))
		provider := strings.TrimSpace(r.PostFormValue("provider"))
		value := strings.TrimSpace(r.PostFormValue("quota"))
		clear := r.PostFormValue("clear") != "" || value == ""

		quota := core.Unknown()
		if !clear {
			parsed, err := core.ParseSizeInGiB(value)
			if err != nil {
				redirect(w, r, "err", err.Error())
				return
			}
			quota = parsed
		}
		if err := opts.Actor.SetOverride(r.Context(), uid, provider, quota, clear); err != nil {
			redirect(w, r, "err", err.Error())
			return
		}
		if clear {
			respondWith(w, r, "ok",
				fmt.Sprintf("Cleared the override for %s on %s: their tier decides again", uid, provider), "—",
				totalsFor(r, opts, uid, provider))
			return
		}
		respondWith(w, r, "ok",
			fmt.Sprintf("Override saved: %s now has %s on %s, above any tier. Run a reconcile to apply it.", uid, quota, provider),
			quota.String(), totalsFor(r, opts, uid, provider))
	}
}

// totalsFor recomputes everything a person's card shows after an override
// changed one of the ceilings: the header totals, the edited row's share bar,
// and whether the row still deserves the "override" tag. An empty map is fine:
// the edited value still lands, only the derived parts would be stale.
func totalsFor(r *http.Request, opts Options, uid, provider string) map[string]string {
	ctx := r.Context()
	usage, err := buildUsage(ctx, opts, false)
	if err != nil {
		opts.Log.Error("override: rebuild usage for the totals", "error", err)
		return nil
	}
	for _, user := range usage.Users {
		if user.User != uid {
			continue
		}
		used, budget := "unknown", "unlimited"
		if user.UsedBytes != nil {
			used = core.FormatIEC(*user.UsedBytes)
		}
		if user.BudgetBytes != nil {
			budget = core.FormatIEC(*user.BudgetBytes)
		}
		percent := barWidth(user.UsedPercent)
		extras := map[string]string{
			"used":    used,
			"budget":  budget,
			"percent": strconv.Itoa(percent),
			"fill":    totalFill(percent),
		}
		for _, p := range user.Providers {
			if p.Name != provider {
				continue
			}
			rowPercent := barWidth(p.UsedPercent)
			extras["row_percent"] = strconv.Itoa(rowPercent)
			extras["row_fill"] = fillFor(rowPercent, p.Status)
			break
		}
		extras["override"] = overrideTag(ctx, opts, uid, provider)
		return extras
	}
	return nil
}

// overrideTag is "1" when the override on this provider changes the outcome
// and the row should say so, "0" when it is the tier's own value in disguise.
func overrideTag(ctx context.Context, opts Options, uid, providerName string) string {
	users, err := opts.Store.ListUsers(ctx)
	if err != nil {
		return "0"
	}
	var user core.User
	found := false
	for _, u := range users {
		if u.UID == uid {
			user, found = u, true
			break
		}
	}
	if !found {
		return "0"
	}
	providers, err := opts.Store.ListProviders(ctx)
	if err != nil {
		return "0"
	}
	var providerID int64
	known := false
	for _, p := range providers {
		if p.Name == providerName {
			providerID, known = p.ID, true
			break
		}
	}
	if !known {
		return "0"
	}
	policy, err := opts.Store.LoadPolicy(ctx)
	if err != nil {
		return "0"
	}
	up, _, err := opts.Store.UserPolicy(ctx, user.ID)
	if err != nil {
		return "0"
	}
	if _, ok := up.ProviderOverrides[providerID]; !ok || overrideRedundant(user, providerID, policy, up) {
		return "0"
	}
	return "1"
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func actionLink(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "Malformed form")
			return
		}
		uid := strings.TrimSpace(r.PostFormValue("user"))
		provider := strings.TrimSpace(r.PostFormValue("provider"))
		externalID := strings.TrimSpace(r.PostFormValue("external_id"))
		unlink := r.PostFormValue("unlink") != ""

		if err := opts.Actor.Link(r.Context(), uid, provider, externalID, unlink); err != nil {
			redirect(w, r, "err", err.Error())
			return
		}
		if unlink {
			redirect(w, r, "ok", fmt.Sprintf("Unlinked %s from %s", uid, provider))
			return
		}
		redirect(w, r, "ok", fmt.Sprintf("Linked %s to %s on %s", uid, externalID, provider))
	}
}

// editedAllocation names the value the inline editor should show back. Only
// that form says which allocation it changed; a plain form post says nothing
// and gets a redirect.
func editedAllocation(r *http.Request, tier core.Tier) string {
	id := strings.TrimSpace(r.PostFormValue("edited"))
	if id == "" {
		return ""
	}
	providerID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return ""
	}
	for _, allocation := range tier.Allocations {
		if allocation.ProviderID == providerID {
			return allocation.Describe()
		}
	}
	// An allocation that was cleared is not zero: the tier is silent about
	// that service now (FR-17).
	return "none"
}

// actionGroup maps a directory group to a tier, or takes the mapping away,
// which is what the chips on the Tiers page do.
func actionGroup(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "Malformed form")
			return
		}
		group := strings.TrimSpace(r.PostFormValue("group"))
		tier := strings.TrimSpace(r.PostFormValue("tier"))
		unmap := r.PostFormValue("unmap") != ""
		if group == "" {
			redirect(w, r, "err", "No group was chosen")
			return
		}

		if err := opts.Actor.MapGroup(r.Context(), group, tier, unmap); err != nil {
			redirect(w, r, "err", err.Error())
			return
		}
		if unmap {
			respond(w, r, "ok",
				"That group no longer maps to a tier, so its members fall back to the default one", "")
			return
		}
		respond(w, r, "ok", "That group now follows "+tier+". Run a reconcile to apply it.", "")
	}
}

// actionKeys shows an issued key exactly once, in the response body, and never
// in a redirect: a query string lands in browser history and in proxy logs
// (FR-56).
func actionKeys(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "Malformed form")
			return
		}

		if id := strings.TrimSpace(r.PostFormValue("revoke")); id != "" {
			keyID, err := strconv.ParseInt(id, 10, 64)
			if err != nil {
				redirect(w, r, "err", "Not a key id")
				return
			}
			if err := opts.Actor.RevokeKey(r.Context(), keyID); err != nil {
				redirect(w, r, "err", err.Error())
				return
			}
			redirect(w, r, "ok", "Revoked that key. Anything using it stops working now.")
			return
		}

		label := strings.TrimSpace(r.PostFormValue("label"))
		if label == "" {
			label = "unnamed"
		}
		value, err := opts.Actor.IssueKey(r.Context(), label)
		if err != nil {
			redirect(w, r, "err", err.Error())
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := keyTemplate.ExecuteTemplate(w, "key.html", struct {
			Label string
			Value string
		}{Label: label, Value: value.Reveal()}); err != nil {
			opts.Log.Error("render the issued key", "error", err)
		}
	}
}

// providerNames is used by the page to label allocation fields.
func providerFields(providers []store.ProviderRow) []providerField {
	fields := make([]providerField, 0, len(providers))
	for _, p := range providers {
		fields = append(fields, providerField{ID: p.ID, Name: p.Name, Type: p.Type})
	}
	return fields
}

type providerField struct {
	ID   int64
	Name string
	Type string
}
