// SPDX-License-Identifier: AGPL-3.0-or-later

// Command nuno is the whole of Nuno: the server and the CLI in one binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/marcodellemarche/nuno/internal/config"
	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/identity/ldap"
	"github.com/marcodellemarche/nuno/internal/reconcile"
	"github.com/marcodellemarche/nuno/internal/store"
)

// version is stamped at build time with -ldflags.
var version = "dev"

const usage = `nuno is a quota and usage control plane for self-hosted stacks.

Usage:
  nuno <command>

Commands:
  serve               run the server: migrate, then listen
  migrate             apply pending database migrations and exit
  providers health    read each provider: reachable, version, credential
  observe             sync the directory and read every provider, writing nothing
  accounts            list observed accounts, who owns them, and what needs a decision
  users               list, add or remove people who exist in no directory
  link                link a person to an account explicitly
  unlink              remove a link
  version             print the version

Configuration is read from the environment, and from the env-style file named
by NUNO_CONFIG when it is set. See .env.example.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return ExitConfig
	}

	switch command := args[0]; command {
	case "version":
		fmt.Fprintln(stdout, version)
		return ExitClean
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return ExitClean
	case "serve":
		return withAppMigrating(stderr, serve)
	case "migrate":
		return withAppMigrating(stderr, migrateOnly)
	case "observe":
		return withApp(stderr, func(ctx context.Context, a *app) int { return observeOnce(ctx, a, stdout) })
	case "accounts":
		return withApp(stderr, func(ctx context.Context, a *app) int { return listAccounts(ctx, a, stdout) })
	case "users":
		return withApp(stderr, func(ctx context.Context, a *app) int { return manageUsers(ctx, a, args[1:], stdout) })
	case "link":
		return withApp(stderr, func(ctx context.Context, a *app) int { return linkAccount(ctx, a, args[1:], stdout) })
	case "unlink":
		return withApp(stderr, func(ctx context.Context, a *app) int { return unlinkAccount(ctx, a, args[1:], stdout) })
	case "providers":
		if len(args) < 2 || args[1] != "health" {
			fmt.Fprintf(stderr, "nuno: usage: nuno providers health\n")
			return ExitConfig
		}
		return withConfig(stderr, func(ctx context.Context, cfg *config.Config, log *slog.Logger, instances []reconcile.Instance) int {
			return providersHealth(ctx, stdout, instances)
		})
	default:
		fmt.Fprintf(stderr, "nuno: unknown command %q\n\n%s", command, usage)
		return ExitConfig
	}
}

// withConfig loads configuration and builds the providers, without opening
// the database. A command that only reads providers needs no schema and no
// run lock.
func withConfig(stderr io.Writer, fn func(context.Context, *config.Config, *slog.Logger, []reconcile.Instance) int) int {
	cfg, log, code := loadConfig(stderr)
	if cfg == nil {
		return code
	}
	registry, err := buildRegistry()
	if err != nil {
		log.Error("build the provider registry", "error", err)
		return ExitConfig
	}
	instances := buildProviders(context.Background(), cfg, registry, nil, log)

	ctx, stop := signalContext()
	defer stop()
	return fn(ctx, cfg, log, instances)
}

// app is what every command that touches state needs.
type app struct {
	cfg       *config.Config
	log       *slog.Logger
	db        *store.DB
	directory core.Directory
	instances []reconcile.Instance
	stderr    io.Writer
}

// withApp loads configuration and opens the database. Only serve and migrate
// reach the migration path, so every other command will call CheckSchema
// instead (FR-73, ADR-0018).
func withApp(stderr io.Writer, fn func(context.Context, *app) int) int {
	return withAppSchema(stderr, false, fn)
}

// withAppMigrating is for the two commands allowed to migrate (FR-73).
func withAppMigrating(stderr io.Writer, fn func(context.Context, *app) int) int {
	return withAppSchema(stderr, true, fn)
}

func withAppSchema(stderr io.Writer, migrates bool, fn func(context.Context, *app) int) int {
	cfg, log, code := loadConfig(stderr)
	if cfg == nil {
		return code
	}

	// One process writes at a time, by construction (ADR-0018). The HTTP
	// client mode that would let a command run against a live server is not
	// written yet, so an offline command refuses rather than corrupting.
	release, err := store.Lock(cfg.DataDir)
	if err != nil {
		log.Error("another process is using this data directory", "error", err)
		return ExitConfig
	}
	defer release()

	db, err := store.Open(filepath.Join(cfg.DataDir, "nuno.db"))
	if err != nil {
		log.Error("open database", "error", err)
		return ExitConfig
	}
	defer db.Close()

	ctx, stop := signalContext()
	defer stop()

	// Only serve and migrate apply migrations. Every other command refuses on
	// a mismatch rather than half-working against a schema it does not know
	// (FR-73, ADR-0018).
	if !migrates {
		if err := store.CheckSchema(ctx, db); err != nil {
			log.Error("database schema mismatch", "error", err)
			return ExitConfig
		}
	}

	registry, err := buildRegistry()
	if err != nil {
		log.Error("build the provider registry", "error", err)
		return ExitConfig
	}
	directory, err := buildDirectory(cfg)
	if err != nil {
		log.Error("identity source is unavailable", "error", err)
		return ExitConfig
	}

	return fn(ctx, &app{
		cfg:       cfg,
		log:       log,
		db:        db,
		directory: directory,
		instances: buildProviders(ctx, cfg, registry, db, log),
		stderr:    stderr,
	})
}

// loadConfig resolves the environment and reports every problem at once, so
// one pass fixes all of them. A nil config means the code is the answer.
func loadConfig(stderr io.Writer) (*config.Config, *slog.Logger, int) {
	env, err := config.Resolve()
	if err != nil {
		fmt.Fprintf(stderr, "nuno: %v\n", err)
		return nil, nil, ExitConfig
	}
	cfg, err := config.Load(env)
	if err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(stderr, "nuno: config: %s\n", line)
		}
		return nil, nil, ExitConfig
	}

	log := newLogger(cfg.LogLevel)
	log.Info("nuno starting",
		"version", version,
		"addr", cfg.Addr,
		"data_dir", cfg.DataDir,
		"providers", providerNames(cfg),
	)
	warnOnMissingCredentials(cfg, log)
	return cfg, log, ExitClean
}

func migrateOnly(ctx context.Context, a *app) int {
	if err := store.Migrate(ctx, a.db, a.log); err != nil {
		a.log.Error("migrate", "error", err)
		if errors.Is(err, store.ErrSchemaNewer) {
			return ExitConfig
		}
		return ExitError
	}
	version, err := store.DBVersion(ctx, a.db.R)
	if err != nil {
		a.log.Error("read schema version", "error", err)
		return ExitError
	}
	a.log.Info("schema is current", "version", version)
	return ExitClean
}

func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func providerNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		names = append(names, p.Name+"("+p.Type+")")
	}
	return names
}

// warnOnMissingCredentials says what will not work rather than failing: the UI
// is read-only when Nuno is misconfigured, it does not crash (FR-55).
func warnOnMissingCredentials(cfg *config.Config, log *slog.Logger) {
	if cfg.AdminKey.Empty() {
		log.Warn("NUNO_ADMIN_KEY is not set, so /api/v1/usage will refuse every request")
	}
	if cfg.AdminPassword.Empty() {
		log.Warn("NUNO_ADMIN_PASSWORD is not set, so the admin surface relies entirely on the proxy in front of it")
	}
	if len(cfg.Providers) == 0 {
		log.Warn("no providers are configured, so there is nothing to observe")
	}
	for _, p := range cfg.Providers {
		if p.MatchKey == "" {
			log.Warn("provider has no match key, so linking will need manual links",
				"provider", p.Name, "setting", p.ConfigRef()+"_MATCH_KEY")
		}
		if len(p.Credentials) == 0 {
			log.Warn("provider has no credentials", "provider", p.Name, "config_ref", p.ConfigRef())
		}
		log.Debug("provider configured",
			"provider", p.Name, "type", p.Type, "url", core.RedactURL(p.BaseURL), "match_key", p.MatchKey)
	}
}

// buildDirectory wires the identity source. It is optional: with none
// configured only manual users exist (FR-2).
func buildDirectory(cfg *config.Config) (core.Directory, error) {
	if !cfg.LDAP.Configured() {
		return nil, nil
	}
	return ldap.New(ldap.Config{
		URL:          cfg.LDAP.URL,
		BaseDN:       cfg.LDAP.BaseDN,
		BindDN:       cfg.LDAP.BindDN,
		BindPassword: cfg.LDAP.BindPassword,
		UserFilter:   cfg.LDAP.UserFilter,
		GroupFilter:  cfg.LDAP.GroupFilter,
		Timeout:      cfg.LDAP.Timeout,
	})
}
