// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"text/tabwriter"

	"github.com/marcodellemarche/nuno/internal/config"
	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/providers/immich"
	"github.com/marcodellemarche/nuno/internal/providers/nextcloud"
	"github.com/marcodellemarche/nuno/internal/reconcile"
	"github.com/marcodellemarche/nuno/internal/store"
)

// buildRegistry is the composition root for providers: the only place that
// imports the adapters. Adding one is a line here and a package under
// internal/providers, with no change to core (FR-25).
func buildRegistry() (*core.Registry, error) {
	registry := core.NewRegistry()
	for _, entry := range []struct {
		typ     string
		factory core.Factory
	}{
		{"nextcloud", nextcloud.New},
		{"immich", immich.New},
	} {
		if err := registry.Register(entry.typ, entry.factory); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// buildProviders constructs what is configured. An instance that cannot be
// built is reported and left out rather than fatal: Nuno degrades instead of
// crashing when a credential is missing (FR-55).
//
// With a database it reconciles each instance into the providers table and
// returns the stored row, which is what the engine needs. Without one it fills
// the row from configuration, so a read-only command like providers health
// needs neither a schema nor a lock. Such a row has no id and must never
// reach a write.
func buildProviders(ctx context.Context, cfg *config.Config, registry *core.Registry, db *store.DB, log *slog.Logger) []reconcile.Instance {
	instances := make([]reconcile.Instance, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		built, err := registry.Build(p.Settings())
		if err != nil {
			log.Error("provider is unavailable and will not be observed",
				"provider", p.Name, "type", p.Type, "config_ref", p.ConfigRef(), "error", err)
			continue
		}

		row := store.ProviderRow{ProviderInstance: p.Settings().Instance}
		if db != nil {
			if _, err := db.UpsertProvider(ctx, p.Settings().Instance); err != nil {
				log.Error("provider could not be registered and will not be observed",
					"provider", p.Name, "error", err)
				continue
			}
			stored, err := db.GetProviderByName(ctx, p.Name)
			if err != nil {
				log.Error("provider could not be read back", "provider", p.Name, "error", err)
				continue
			}
			row = stored
		}
		instances = append(instances, reconcile.Instance{Row: row, Provider: built})
	}
	return instances
}

// providersHealth reads each provider: reachable, version, whether the
// credential authenticates. It writes nothing, so it needs neither the
// database nor a run lock, and it is the first thing to run against a new
// stack. The write probe is a separate step and is not run here (ADR-0025).
func providersHealth(ctx context.Context, stdout io.Writer, instances []reconcile.Instance) int {
	if len(instances) == 0 {
		fmt.Fprintln(stdout, "No providers are configured. See NUNO_PROVIDER_<NAME>_* in .env.example.")
		return ExitConfig
	}

	table := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tTYPE\tREACHABLE\tVERSION\tSUPPORTED\tWRITE\tDETAIL")

	code := ExitClean
	for _, instance := range instances {
		result := instance.Provider.Health(ctx)

		detail := ""
		if result.Err != nil {
			detail = result.Err.Error()
			code = ExitError
		} else if !result.InSupported && result.Version != "" {
			// Outside the supported major Nuno warns and keeps going, so this
			// is a detail rather than a failure (ADR-0015).
			detail = "outside the supported major, continuing"
		}
		if !result.Reachable {
			code = ExitError
		}

		version := result.Version
		if version == "" {
			version = "unknown"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			instance.Row.Name,
			instance.Row.Type,
			yesNo(result.Reachable),
			version,
			yesNo(result.InSupported),
			// Write access is never proven by a read, and nothing has probed
			// it yet in this process.
			core.WriteUnproven,
			detail,
		)
	}
	table.Flush()
	return code
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
