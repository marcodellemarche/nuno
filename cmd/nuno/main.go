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
	"github.com/marcodellemarche/nuno/internal/store"
)

// version is stamped at build time with -ldflags.
var version = "dev"

const usage = `nuno is a quota and usage control plane for self-hosted stacks.

Usage:
  nuno <command>

Commands:
  serve      run the server: migrate, then listen
  migrate    apply pending database migrations and exit
  version    print the version

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
		return withApp(stderr, serve)
	case "migrate":
		return withApp(stderr, migrateOnly)
	default:
		fmt.Fprintf(stderr, "nuno: unknown command %q\n\n%s", command, usage)
		return ExitConfig
	}
}

// app is what every command that touches state needs.
type app struct {
	cfg *config.Config
	log *slog.Logger
	db  *store.DB
}

// withApp loads configuration and opens the database. Only serve and migrate
// reach the migration path, so every other command will call CheckSchema
// instead (FR-73, ADR-0018).
func withApp(stderr io.Writer, fn func(context.Context, *app) int) int {
	env, err := config.Resolve()
	if err != nil {
		fmt.Fprintf(stderr, "nuno: %v\n", err)
		return ExitConfig
	}
	cfg, err := config.Load(env)
	if err != nil {
		// Every configuration problem at once, so one pass fixes all of them.
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(stderr, "nuno: config: %s\n", line)
		}
		return ExitConfig
	}

	log := newLogger(cfg.LogLevel)
	log.Info("nuno starting",
		"version", version,
		"addr", cfg.Addr,
		"data_dir", cfg.DataDir,
		"providers", providerNames(cfg),
	)
	warnOnMissingCredentials(cfg, log)

	db, err := store.Open(filepath.Join(cfg.DataDir, "nuno.db"))
	if err != nil {
		log.Error("open database", "error", err)
		return ExitConfig
	}
	defer db.Close()

	ctx, stop := signalContext()
	defer stop()

	return fn(ctx, &app{cfg: cfg, log: log, db: db})
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
