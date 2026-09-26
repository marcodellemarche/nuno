// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/marcodellemarche/nuno/internal/api"
	"github.com/marcodellemarche/nuno/internal/reconcile"
	"github.com/marcodellemarche/nuno/internal/store"
)

const shutdownGrace = 10 * time.Second

func serve(ctx context.Context, a *app) int {
	if err := store.Migrate(ctx, a.db, a.log); err != nil {
		a.log.Error("migrate", "error", err)
		return ExitConfig
	}
	if code := bootstrapAdminKey(ctx, a); code != ExitClean {
		return code
	}

	// The refresh loop, the scheduled reconcile and a UI action all sync
	// identity and observe, so they share one gate: two of them must not
	// recompute the identity snapshot and the observed accounts at the same
	// moment.
	var gate sync.Mutex

	srv := &http.Server{
		Addr: a.cfg.Addr,
		Handler: api.Routes(api.Options{
			Version:         version,
			DB:              a.db.R,
			Store:           a.db,
			Actor:           newActor(a, &gate),
			Log:             a.log,
			AdminPassword:   a.cfg.AdminPassword,
			ProxySecret:     a.cfg.ProxySecret,
			RefreshInterval: a.cfg.RefreshInterval,
			TrustedProxy:    a.cfg.TrustedProxy,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Usage is refreshed on a schedule, or the numbers on the page would be
	// as stale as the last time someone ran a command. Observing does not
	// write, so none of the caution around scheduled reconcile applies to it
	// (FR-48, ADR-0017).
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		refreshLoop(ctx, a, &gate)
	}()

	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		reconcileLoop(ctx, a, &gate)
	}()

	errc := make(chan error, 1)
	go func() {
		a.log.Info("listening", "addr", srv.Addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("listen", "addr", srv.Addr, "error", err)
			return ExitConfig
		}
		return ExitClean
	case <-ctx.Done():
		a.log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		a.log.Error("shutdown", "error", err)
		return ExitError
	}
	<-refreshDone
	<-reconcileDone
	return ExitClean
}

// bootstrapAdminKey stores the key from the environment, so the endpoint is
// usable before any credential UI exists. Removing the variable revokes it,
// and an operator can always recover access from the compose file (FR-46a,
// ADR-0022).
func bootstrapAdminKey(ctx context.Context, a *app) int {
	if !a.cfg.AdminKey.Empty() {
		if err := a.db.EnsureAdminKey(ctx, a.cfg.AdminKey, "NUNO_ADMIN_KEY"); err != nil {
			a.log.Error("store the admin key", "error", err)
			return ExitConfig
		}
	}
	revoked, err := a.db.RevokeEnvKeysExcept(ctx, a.cfg.AdminKey)
	if err != nil {
		a.log.Error("revoke stale admin keys", "error", err)
		return ExitConfig
	}
	if revoked > 0 {
		a.log.Info("revoked admin keys that are no longer in the environment", "count", revoked)
	}

	count, err := a.db.CountAdminKeys(ctx)
	if err != nil {
		a.log.Error("count admin keys", "error", err)
		return ExitConfig
	}
	if count == 0 {
		a.log.Warn("no admin key is configured, so GET /api/v1/usage will refuse every request",
			"fix", "set NUNO_ADMIN_KEY")
	}
	return ExitClean
}

// Startup backoff bounds. Nuno starts before the services it reads when the
// whole stack is brought up together, so the first cycle must retry rather
// than wait a full interval for a service that was a second too slow. The
// delay doubles from the minimum and stops at the maximum, so a long outage
// is retried every minute instead of hammering or going silent.
const (
	startupBackoffMin = 2 * time.Second
	startupBackoffMax = time.Minute
)

// waitOrDone sleeps for d, returning false if the context ends first.
func waitOrDone(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// refreshLoop observes on a timer. It runs once at startup so the page is not
// empty for the first interval, and it retries with backoff while the cycle
// is not clean, so a service that is not up yet is picked up as soon as it
// appears (FR-48).
func refreshLoop(ctx context.Context, a *app, gate *sync.Mutex) {
	interval := a.cfg.RefreshInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	engine := reconcile.New(a.db, a.log)

	backoff := startupBackoffMin
	for {
		ok := refreshOnce(ctx, a, engine, gate)
		if ok {
			backoff = startupBackoffMin
		} else {
			a.log.Info("usage refresh will be retried", "in", backoff.String())
		}
		next := interval
		if !ok {
			next = backoff
			backoff = min(backoff*2, startupBackoffMax)
		}
		if !waitOrDone(ctx, next) {
			return
		}
	}
}

// refreshOnce syncs identity and observes every provider. It reports whether
// the cycle was clean, which is what decides the next delay: a directory or a
// provider that could not be read is retried soon, not in fifteen minutes.
func refreshOnce(ctx context.Context, a *app, engine *reconcile.Engine, gate *sync.Mutex) bool {
	gate.Lock()
	defer gate.Unlock()

	clean := true
	if _, err := engine.SyncIdentity(ctx, a.directory); err != nil {
		a.log.Error("scheduled identity sync failed", "error", err)
		clean = false
	}
	report, err := engine.Observe(ctx, a.instances)
	if err != nil {
		a.log.Error("scheduled observe failed", "error", err)
		return false
	}
	observed, linked, issues := report.Totals()
	a.log.Info("usage refreshed",
		"accounts", observed, "linked", linked, "issues", issues,
		"failed_providers", len(report.Failed()))
	return clean && report.OK()
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// reconcileLoop applies on a timer. Without it Nuno reports rather than
// controls, and a new account keeps whatever default the service gave it
// (FR-36).
//
// It never carries consent: automation cannot cross the shrink guardrail
// unattended (FR-39). And it refuses to start without a webhook, because a
// timer that writes quotas and cannot tell anybody when it is blocked is
// worse than no timer at all.
func reconcileLoop(ctx context.Context, a *app, gate *sync.Mutex) {
	if a.cfg.ReconcileInterval <= 0 {
		a.log.Info("scheduled reconcile is off, so Nuno reports rather than controls",
			"fix", "set NUNO_RECONCILE_INTERVAL")
		return
	}
	if !a.notifier.Configured() {
		a.log.Error("refusing to run the scheduled reconcile without a webhook: a timer that writes quotas must be able to say when it is blocked",
			"interval", a.cfg.ReconcileInterval, "fix", "set NUNO_WEBHOOK_URL, or unset NUNO_RECONCILE_INTERVAL")
		return
	}

	engine := reconcile.New(a.db, a.log).WithNotifier(a.notifier, a.cfg.PublicURL)
	a.log.Info("scheduled reconcile is on", "interval", a.cfg.ReconcileInterval)

	// It runs once at startup as well as on the timer, so an account that
	// appeared since the last run gets its quota without waiting a full
	// interval. This is what makes a new person's ceiling correct before they
	// first open the service (FR-36, FR-38d).
	backoff := startupBackoffMin
	for {
		ok := runScheduledReconcile(ctx, a, engine, gate)
		if ok {
			backoff = startupBackoffMin
		} else {
			a.log.Info("scheduled reconcile will be retried", "in", backoff.String())
		}
		next := a.cfg.ReconcileInterval
		if !ok {
			next = backoff
			backoff = min(backoff*2, startupBackoffMax)
		}
		if !waitOrDone(ctx, next) {
			return
		}
	}
}

// runScheduledReconcile syncs, observes, plans and applies one cycle. It
// reports whether the cycle was clean enough to wait a full interval for the
// next one: a provider that could not be read is retried with backoff, while
// a change a guardrail refused is not, because retrying it unattended cannot
// change the answer (FR-39).
func runScheduledReconcile(ctx context.Context, a *app, engine *reconcile.Engine, gate *sync.Mutex) bool {
	gate.Lock()
	defer gate.Unlock()

	clean := true
	if _, err := engine.SyncIdentity(ctx, a.directory); err != nil {
		a.log.Error("scheduled identity sync failed", "error", err)
		clean = false
	}
	observation, err := engine.Observe(ctx, a.instances)
	if err != nil {
		a.log.Error("scheduled observe failed", "error", err)
		return false
	}
	if !observation.OK() {
		clean = false
	}

	computedAt := time.Now()
	result, err := engine.Plan(ctx, a.instances, reconcile.PlanFilter{})
	if err != nil {
		a.log.Error("scheduled plan failed", "error", err)
		return false
	}
	if result.Plan.Empty() {
		a.log.Info("scheduled reconcile had nothing to do",
			"considered", result.Considered, "providers_failed", len(observation.Failed()))
		return clean
	}

	report, err := engine.Apply(ctx, a.instances, result.Plan, reconcile.ApplyOptions{
		// Never consent. Automation cannot cross this line (FR-39).
		ConsentShrink: false,
		Actor:         "schedule",
		ComputedAt:    computedAt,
	})
	if err != nil {
		a.log.Error("scheduled apply failed", "error", err)
		return false
	}
	a.log.Info("scheduled reconcile finished",
		"applied", report.Applied, "failed", report.Failed,
		"guarded", report.Guarded, "unknown_state", report.Unknown,
		"moved", report.Moved, "throttled", report.Throttled, "run", report.RunID)
	if report.Failed > 0 || len(report.Errors) > 0 {
		clean = false
	}
	return clean
}
