// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

// Store is what the HTTP surface reads. Narrowing it to these calls keeps the
// handlers testable without a database.
type Store interface {
	ListUsers(ctx context.Context) ([]core.User, error)
	ListGroups(ctx context.Context) ([]core.Group, error)
	ListProviders(ctx context.Context) ([]store.ProviderRow, error)
	ListAllExternalAccounts(ctx context.Context) ([]core.ExternalAccount, error)
	ListLinks(ctx context.Context) ([]core.AccountLink, error)
	ListLinkIssues(ctx context.Context) ([]core.LinkIssue, error)
	CheckAdminKey(ctx context.Context, presented core.Secret) (bool, error)
	CountAdminKeys(ctx context.Context) (int, error)

	LoadPolicy(ctx context.Context) (core.Policy, error)
	UserPolicy(ctx context.Context, userID int64) (core.UserPolicy, map[int64]store.OverrideOrigin, error)
	LastRuns(ctx context.Context, limit int) ([]store.RunSummary, error)
	RecentChanges(ctx context.Context, limit int) ([]store.AuditRow, error)
	ListAdminKeys(ctx context.Context) ([]store.AdminKeyRow, error)
}

// usageHandler serves GET /api/v1/usage, the contract a shared dashboard
// widget consumes (FR-45, ADR-0017).
func usageHandler(opts Options, limit *limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearer(r)
		if !ok {
			unauthorized(w, "an admin key is required in an Authorization: Bearer header")
			return
		}
		if !limit.allow(presented) {
			w.Header().Set("Retry-After", "60")
			writeJSONError(w, http.StatusTooManyRequests, "too many requests")
			return
		}

		// With no key configured the endpoint cannot be used at all, and
		// saying so beats an unauthorized that looks like a wrong key.
		count, err := opts.Store.CountAdminKeys(r.Context())
		if err != nil {
			opts.Log.Error("usage: count admin keys", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if count == 0 {
			writeJSONError(w, http.StatusServiceUnavailable,
				"no admin key is configured: set NUNO_ADMIN_KEY and restart")
			return
		}

		valid, err := opts.Store.CheckAdminKey(r.Context(), presented)
		if err != nil {
			opts.Log.Error("usage: check admin key", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !valid {
			// The key itself is never logged.
			opts.Log.Warn("usage: rejected an unknown admin key", "remote", r.RemoteAddr)
			unauthorized(w, "unknown or revoked admin key")
			return
		}

		// with_accounts=1 drops the people who have no account on any provider,
		// which is what a shared dashboard wants: the service accounts in the
		// directory are not people with a quota. The default stays every user
		// (FR-45), so the admin view is unaffected.
		withAccounts := r.URL.Query().Get("with_accounts") == "1"
		response, err := buildUsage(r.Context(), opts, withAccounts)
		if err != nil {
			opts.Log.Error("usage: build response", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		// detail=provider flattens the nested shape into one row per person and
		// provider, which is what a Homepage dynamic-list can render: it shows a
		// single name and a single label per row, and cannot walk into an array
		// of arrays.
		var payload any = response
		switch r.URL.Query().Get("detail") {
		case "provider":
			payload = core.BuildUsageDetail(response)
		case "service":
			// Aggregated per service, so a shared dashboard does not show one
			// person's quota to another.
			payload = core.BuildUsageByService(response)
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			opts.Log.Error("usage: write response", "error", err)
		}
	}
}

func buildUsage(ctx context.Context, opts Options, withAccounts bool) (core.UsageResponse, error) {
	users, err := opts.Store.ListUsers(ctx)
	if err != nil {
		return core.UsageResponse{}, err
	}
	providers, err := opts.Store.ListProviders(ctx)
	if err != nil {
		return core.UsageResponse{}, err
	}
	accounts, err := opts.Store.ListAllExternalAccounts(ctx)
	if err != nil {
		return core.UsageResponse{}, err
	}

	observed := make([]core.ObservedProvider, 0, len(providers))
	for _, p := range providers {
		observed = append(observed, core.ObservedProvider{
			ID:            p.ID,
			Type:          p.Type,
			Name:          p.Name,
			HasObserved:   p.LastObserveAt != nil,
			LastObserveOK: p.LastObserveOK,
		})
	}

	// An orphan keeps its history but is not part of the live picture.
	active := make([]core.User, 0, len(users))
	for _, u := range users {
		if u.Status == core.UserActive {
			active = append(active, u)
		}
	}

	// The policy makes budget_bytes the resolved budget and managed real, which
	// is what the frozen contract promises from M2 (ADR-0022, FR-47).
	policy, err := opts.Store.LoadPolicy(ctx)
	if err != nil {
		return core.UsageResponse{}, err
	}
	userPolicies := make(map[int64]core.UserPolicy, len(active))
	for _, u := range active {
		up, _, err := opts.Store.UserPolicy(ctx, u.ID)
		if err != nil {
			return core.UsageResponse{}, err
		}
		userPolicies[u.ID] = up
	}

	refresh := opts.RefreshInterval
	if refresh <= 0 {
		refresh = 15 * time.Minute
	}
	response := core.BuildUsage(core.UsageInput{
		Now:             time.Now().UTC(),
		RefreshInterval: refresh,
		Users:           active,
		Providers:       observed,
		Accounts:        accounts,
		Policy:          policy,
		UserPolicies:    userPolicies,
	})

	if withAccounts {
		kept := response.Users[:0]
		for _, u := range response.Users {
			if len(u.Providers) > 0 {
				kept = append(kept, u)
			}
		}
		response.Users = kept
	}
	return response, nil
}

func unauthorized(w http.ResponseWriter, detail string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="nuno"`)
	writeJSONError(w, http.StatusUnauthorized, detail)
}

func writeJSONError(w http.ResponseWriter, code int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": detail})
}
