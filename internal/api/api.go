// SPDX-License-Identifier: AGPL-3.0-or-later

// Package api holds Nuno's HTTP surface.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
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

	// ProxySecret, when set, is a header only the proxy knows. The admin
	// surface answers only requests that carry it, so a container sharing the
	// Docker network cannot reach the panel directly and skip the SSO in front
	// of the public name. Empty means no gate (NFR-14).
	ProxySecret core.Secret

	// RefreshInterval decides when a reading becomes stale, at twice this
	// value (ADR-0022).
	RefreshInterval time.Duration

	// TrustedProxy is the network the forward-auth headers may come from. A
	// Remote-User header is believed only when the request arrives from it, so
	// a container on the same network cannot claim to be somebody else. Empty
	// means no header is trusted (NFR-14).
	TrustedProxy string
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

	// The per-person page a proxy-authenticated member opens, so a shared
	// dashboard can show each person their own quota without showing anyone
	// else's. It trusts the forward-auth header, and only from the proxy.
	gated := func(next http.Handler) http.Handler { return proxy(opts.ProxySecret, next) }
	mux.Handle("GET /me", gated(meHandler(opts)))

	// Five pages, one per tab, each a real route: the tab bar is navigation,
	// not a client-side toggle.
	admin := func(next http.HandlerFunc) http.Handler { return basicAuth(opts.AdminPassword, next) }
	mux.Handle("GET /{$}", gated(admin(quotasHandler(opts))))
	mux.Handle("GET /tiers", gated(admin(tiersHandler(opts))))
	mux.Handle("GET /services", gated(admin(servicesHandler(opts))))
	mux.Handle("GET /accounts", gated(admin(accountsHandler(opts))))
	mux.Handle("GET /activity", gated(admin(activityHandler(opts))))
	mux.Handle("GET /static/", gated(http.StripPrefix("/static/", staticHandler())))
	registerActions(mux, opts)
	return mux
}

// proxy gates the admin surface and /me on the shared secret Caddy injects.
// The usage endpoint is deliberately not gated: a dashboard widget reads it
// server-side, over the Docker network, and authenticates with its own key.
func proxy(secret core.Secret, next http.Handler) http.Handler {
	if secret.Empty() {
		return next
	}
	expected := sha256.Sum256([]byte(secret.Reveal()))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := sha256.Sum256([]byte(r.Header.Get("X-Nuno-Proxy-Secret")))
		if subtle.ConstantTimeCompare(expected[:], presented[:]) != 1 {
			http.Error(w, "this surface is reachable only through the proxy", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
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
