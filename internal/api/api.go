// SPDX-License-Identifier: AGPL-3.0-or-later

// Package api holds Nuno's HTTP surface.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

// Pinger is the part of the store /healthz needs. Keeping it this narrow means
// the handler can be tested without a database.
type Pinger interface {
	PingContext(ctx context.Context) error
}

type Options struct {
	Version string
	DB      Pinger
	Store   Store
	// Actor performs the mutations the page offers. With none the page is
	// read-only, which is what a misconfigured Nuno shows instead of
	// crashing (FR-55).
	Actor Actor
	Log   *slog.Logger

	// AdminPassword guards the admin surface when no proxy authenticates in
	// front of it. Empty means the surface relies on the proxy, which is why
	// it binds to loopback by default (NFR-14).
	AdminPassword core.Secret

	// RefreshInterval decides when a reading becomes stale, at twice this
	// value (ADR-0022).
	RefreshInterval time.Duration
}

// Routes builds the admin surface. It binds to localhost unless the
// configuration says otherwise, which is checked when the config is loaded
// (NFR-14).
func Routes(opts Options) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz(opts))

	if opts.Store == nil {
		// Without a store there is nothing to serve but health, which is
		// still worth answering.
		return mux
	}

	// Read-only, machine facing, and rate limited so a leaked key cannot be
	// brute forced or used to hammer the providers' numbers.
	mux.HandleFunc("GET /api/v1/usage", usageHandler(opts, newLimiter(60, 20)))

	page := basicAuth(opts.AdminPassword, pageHandler(opts))
	mux.Handle("GET /{$}", page)
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))
	registerActions(mux, opts)
	return mux
}

// healthz answers for the process, not for the providers: per-provider health
// is its own thing in the UI. It pings the database, because a process that
// cannot reach its own state is not healthy in any useful sense.
func healthz(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"status": "ok", "version": opts.Version}
		code := http.StatusOK

		if opts.DB != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := opts.DB.PingContext(ctx); err != nil {
				body["status"] = "unavailable"
				body["detail"] = "database unreachable"
				code = http.StatusServiceUnavailable
				opts.Log.Error("healthz: database unreachable", "error", err)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			opts.Log.Error("healthz: write response", "error", err)
		}
	}
}
