// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/marcodellemarche/nuno/internal/api"
	"github.com/marcodellemarche/nuno/internal/store"
)

const shutdownGrace = 10 * time.Second

func serve(ctx context.Context, a *app) int {
	if err := store.Migrate(ctx, a.db, a.log); err != nil {
		a.log.Error("migrate", "error", err)
		return ExitConfig
	}

	srv := &http.Server{
		Addr: a.cfg.Addr,
		Handler: api.Routes(api.Options{
			Version: version,
			DB:      a.db.R,
			Log:     a.log,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

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
	return ExitClean
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
