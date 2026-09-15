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

// Store is what the HTTP surface reads. Narrowing it to these five calls keeps
// the handlers testable without a database.
type Store interface {
	ListUsers(ctx context.Context) ([]core.User, error)
	ListProviders(ctx context.Context) ([]store.ProviderRow, error)
	ListAllExternalAccounts(ctx context.Context) ([]core.ExternalAccount, error)
	ListLinkIssues(ctx context.Context) ([]core.LinkIssue, error)
	CheckAdminKey(ctx context.Context, presented core.Secret) (bool, error)
	CountAdminKeys(ctx context.Context) (int, error)
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

		response, err := buildUsage(r.Context(), opts)
		if err != nil {
			opts.Log.Error("usage: build response", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			opts.Log.Error("usage: write response", "error", err)
		}
	}
}

func buildUsage(ctx context.Context, opts Options) (core.UsageResponse, error) {
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

	refresh := opts.RefreshInterval
	if refresh <= 0 {
		refresh = 15 * time.Minute
	}
	return core.BuildUsage(core.UsageInput{
		Now:             time.Now().UTC(),
		RefreshInterval: refresh,
		Users:           active,
		Providers:       observed,
		Accounts:        accounts,
	}), nil
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
