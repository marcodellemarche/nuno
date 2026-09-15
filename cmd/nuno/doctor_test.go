// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/marcodellemarche/nuno/internal/config"
	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

// A key set in the environment is configured even before `nuno serve` has
// stored it. Reporting it as missing tells an operator to set a variable they
// already set, which is what happened on the first live run.
func TestAdminKeyInTheEnvironmentCountsAsConfigured(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "nuno.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(context.Background(), db, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}

	a := &app{
		cfg: &config.Config{AdminKey: core.Secret("0123456789abcdef")},
		db:  db,
	}

	var found bool
	for _, c := range a.checkConfiguration(context.Background()) {
		if c.Name != "admin key for the usage API" {
			continue
		}
		found = true
		if c.Status != statusOK {
			t.Errorf("status = %q, want %q: the env key is set (%s)", c.Status, statusOK, c.Detail)
		}
	}
	if !found {
		t.Fatal("the admin key check is missing")
	}
}

// With neither the environment nor the database holding a key, the check must
// still fail and name the fix.
func TestAdminKeyMissingEverywhereStillFails(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "nuno.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(context.Background(), db, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}

	a := &app{cfg: &config.Config{}, db: db}

	for _, c := range a.checkConfiguration(context.Background()) {
		if c.Name != "admin key for the usage API" {
			continue
		}
		if c.Status != statusFail {
			t.Errorf("status = %q, want %q", c.Status, statusFail)
		}
		if len(c.Remedy) == 0 {
			t.Error("a failure must carry the command that fixes it")
		}
		return
	}
	t.Fatal("the admin key check is missing")
}
