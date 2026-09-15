// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
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
		return basicAuth(opts.AdminPassword, sameOrigin(opts, next))
	}
	mux.Handle("POST /actions/reconcile", guard(actionReconcile(opts)))
	mux.Handle("POST /actions/observe", guard(actionObserve(opts)))
	mux.Handle("POST /actions/tier", guard(actionTier(opts)))
	mux.Handle("POST /actions/override", guard(actionOverride(opts)))
	mux.Handle("POST /actions/link", guard(actionLink(opts)))
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
	target := "/?" + kind + "=" + url.QueryEscape(message)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func actionReconcile(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "malformed form")
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
		redirect(w, r, "ok", "read every provider again")
	}
}

func actionTier(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "malformed form")
			return
		}
		name := strings.TrimSpace(r.PostFormValue("name"))
		if name == "" {
			redirect(w, r, "err", "a tier needs a name")
			return
		}

		tier := core.Tier{Name: name, IsDefault: r.PostFormValue("is_default") != ""}
		if budget := strings.TrimSpace(r.PostFormValue("budget")); budget != "" {
			parsed, err := core.ParseSize(budget)
			if err != nil {
				redirect(w, r, "err", "budget: "+err.Error())
				return
			}
			tier.Budget = parsed
		}

		// One field per provider, named alloc_<id>. An empty one means the
		// tier says nothing about that provider, which is not zero (FR-17).
		for key, values := range r.PostForm {
			id, ok := strings.CutPrefix(key, "alloc_")
			if !ok || len(values) == 0 || strings.TrimSpace(values[0]) == "" {
				continue
			}
			providerID, err := strconv.ParseInt(id, 10, 64)
			if err != nil {
				continue
			}
			allocation, err := core.ParseAllocation(values[0])
			if err != nil {
				redirect(w, r, "err", fmt.Sprintf("allocation %q: %v", values[0], err))
				return
			}
			allocation.ProviderID = providerID
			tier.Allocations = append(tier.Allocations, allocation)
		}

		if err := opts.Actor.SaveTier(r.Context(), tier); err != nil {
			redirect(w, r, "err", err.Error())
			return
		}
		message := "saved tier " + name + ". Run a reconcile to apply it."
		if tier.Overcommitted() {
			message += " Its percentages add up to more than 100, which over-commits deliberately."
		}
		redirect(w, r, "ok", message)
	}
}

func actionOverride(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "malformed form")
			return
		}
		uid := strings.TrimSpace(r.PostFormValue("user"))
		provider := strings.TrimSpace(r.PostFormValue("provider"))
		value := strings.TrimSpace(r.PostFormValue("quota"))
		clear := r.PostFormValue("clear") != "" || value == ""

		quota := core.Unknown()
		if !clear {
			parsed, err := core.ParseSize(value)
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
			redirect(w, r, "ok", fmt.Sprintf("cleared the override for %s on %s: their tier decides again", uid, provider))
			return
		}
		redirect(w, r, "ok", fmt.Sprintf("%s now has %s on %s, above any tier. Run a reconcile to apply it.", uid, quota, provider))
	}
}

func actionLink(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "malformed form")
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
			redirect(w, r, "ok", fmt.Sprintf("unlinked %s from %s", uid, provider))
			return
		}
		redirect(w, r, "ok", fmt.Sprintf("linked %s to %s on %s", uid, externalID, provider))
	}
}

// actionKeys shows an issued key exactly once, in the response body, and never
// in a redirect: a query string lands in browser history and in proxy logs
// (FR-56).
func actionKeys(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, "err", "malformed form")
			return
		}

		if id := strings.TrimSpace(r.PostFormValue("revoke")); id != "" {
			keyID, err := strconv.ParseInt(id, 10, 64)
			if err != nil {
				redirect(w, r, "err", "not a key id")
				return
			}
			if err := opts.Actor.RevokeKey(r.Context(), keyID); err != nil {
				redirect(w, r, "err", err.Error())
				return
			}
			redirect(w, r, "ok", "revoked that key. Anything using it stops working now.")
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
		if err := pageTemplate.ExecuteTemplate(w, "key.html", struct {
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
