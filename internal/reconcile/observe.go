// SPDX-License-Identifier: AGPL-3.0-or-later

package reconcile

import (
	"context"
	"fmt"
)

// Observe reads every provider and stores what it found, then links accounts
// to people. A provider that fails is degraded, not fatal: its accounts keep
// their last values and every future change against it is unknown-state.
//
// Observing is not entirely free of side effects. Reading user details on
// Nextcloud creates missing home folders, which is documented rather than
// avoidable (ADR-0014).
func (e *Engine) Observe(ctx context.Context, instances []Instance) (Report, error) {
	report := Report{StartedAt: e.now().UTC()}

	for _, instance := range instances {
		result := e.observeOne(ctx, instance)
		report.Providers = append(report.Providers, result)
	}

	// Linking runs once across every provider, after all of them have been
	// read, because the subject join carries a link from one provider to
	// another and must not depend on the order they were observed in.
	if err := e.link(ctx, instances, &report); err != nil {
		report.FinishedAt = e.now().UTC()
		return report, err
	}

	report.FinishedAt = e.now().UTC()
	return report, nil
}

func (e *Engine) observeOne(ctx context.Context, instance Instance) ProviderResult {
	result := ProviderResult{Name: instance.Row.Name, Type: instance.Row.Type}
	log := e.log.With("provider", instance.Row.Name, "type", instance.Row.Type)

	health := instance.Provider.Health(ctx)
	if err := e.db.SaveHealth(ctx, instance.Row.ID, health); err != nil {
		log.Error("store provider health", "error", err)
	}
	switch {
	case health.Err != nil:
		log.Warn("provider health check failed", "error", health.Err)
	case !health.InSupported && health.Version != "":
		// Outside the supported major Nuno warns once per run and keeps
		// going (NFR-12, ADR-0015).
		log.Warn("provider is outside the supported major version, continuing",
			"version", health.Version)
	}

	if !instance.Provider.Capabilities().CanReadUsers {
		// A provider that cannot list users is not a failure, it is a
		// capability. Nothing is guessed on its behalf (FR-23).
		log.Info("provider cannot read users, nothing to observe")
		return result
	}

	accounts, err := instance.Provider.ListAccounts(ctx)
	if err != nil {
		result.Err = err
		if saveErr := e.db.SaveObserveResult(ctx, instance.Row.ID, e.now().UTC(), false, err); saveErr != nil {
			log.Error("store observe result", "error", saveErr)
		}
		log.Error("observe failed, provider degraded and its accounts keep their last values", "error", err)
		return result
	}

	observedAt := e.now().UTC()
	for _, account := range accounts {
		if !account.Writable() {
			result.Skipped++
		}
	}
	result.Observed = len(accounts)

	if err := e.db.ReplaceExternalAccounts(ctx, instance.Row.ID, accounts, observedAt); err != nil {
		result.Err = fmt.Errorf("store accounts: %w", err)
		if saveErr := e.db.SaveObserveResult(ctx, instance.Row.ID, observedAt, false, result.Err); saveErr != nil {
			log.Error("store observe result", "error", saveErr)
		}
		return result
	}
	if err := e.db.SaveObserveResult(ctx, instance.Row.ID, observedAt, true, nil); err != nil {
		log.Error("store observe result", "error", err)
	}

	log.Info("observed", "accounts", result.Observed, "skipped", result.Skipped)
	return result
}
