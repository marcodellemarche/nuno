// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// BackupsKept is how many pre-migration copies survive. Enough to undo a bad
// upgrade, few enough not to fill the volume.
const BackupsKept = 5

// ErrSchemaNewer means the database was written by a newer Nuno. Running
// against it would read columns this binary does not know about and write ones
// it does, so it refuses (FR-73).
var ErrSchemaNewer = errors.New("database schema is newer than this binary")

// ErrSchemaOlder means migrations are pending. Every subcommand except serve
// and migrate refuses, rather than half-working.
var ErrSchemaOlder = errors.New("database schema is older than this binary")

func migrationsDir() (fs.FS, error) {
	return fs.Sub(migrationsFS, "migrations")
}

func newProvider(db *sql.DB) (*goose.Provider, error) {
	dir, err := migrationsDir()
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectSQLite3, db, dir, goose.WithVerbose(false))
}

// TargetVersion is the newest migration this binary carries. It reads the
// embedded files rather than asking goose, which needs a database connection
// to answer a question that does not depend on one.
func TargetVersion() (int64, error) {
	dir, err := migrationsDir()
	if err != nil {
		return 0, err
	}
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return 0, err
	}
	var target int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := goose.NumericComponent(e.Name())
		if err != nil {
			return 0, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		target = max(target, version)
	}
	if target == 0 {
		return 0, errors.New("no migrations are embedded in this binary")
	}
	return target, nil
}

// DBVersion reads the applied version without creating goose's bookkeeping
// table, so asking the question does not change the answer.
func DBVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var version sql.NullInt64
	err := db.QueryRowContext(ctx, "SELECT max(version_id) FROM "+goose.TableName()).Scan(&version)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version.Int64, nil
}

// CheckSchema is what every subcommand other than serve and migrate calls. It
// distinguishes the two directions, because they need different actions from
// the operator.
func CheckSchema(ctx context.Context, db *DB) error {
	target, err := TargetVersion()
	if err != nil {
		return err
	}
	current, err := DBVersion(ctx, db.R)
	if err != nil {
		return err
	}
	switch {
	case current > target:
		return fmt.Errorf("%w: database is at %d, this binary knows %d: run the newer Nuno, or restore a backup from %s",
			ErrSchemaNewer, current, target, BackupDir(db.Path))
	case current < target:
		return fmt.Errorf("%w: database is at %d, this binary expects %d: run `nuno migrate`",
			ErrSchemaOlder, current, target)
	}
	return nil
}

func BackupDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "backups")
}

// Migrate applies pending migrations after copying the database. Only nuno
// serve and nuno migrate reach this (FR-73).
func Migrate(ctx context.Context, db *DB, log *slog.Logger) error {
	target, err := TargetVersion()
	if err != nil {
		return err
	}
	current, err := DBVersion(ctx, db.W)
	if err != nil {
		return err
	}
	if current > target {
		return fmt.Errorf("%w: database is at %d, this binary knows %d", ErrSchemaNewer, current, target)
	}
	if current == target {
		log.Debug("schema is current", "version", current)
		return nil
	}

	// A database with no schema yet has nothing worth copying.
	if current > 0 {
		backup, err := Backup(ctx, db, current)
		if err != nil {
			return fmt.Errorf("pre-migration backup: %w", err)
		}
		log.Info("database copied before migrating", "backup", backup, "from_version", current)
	}

	provider, err := newProvider(db.W)
	if err != nil {
		return err
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrate from %d to %d: %w", current, target, err)
	}
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "name", r.Source.Path, "took", r.Duration)
	}
	return nil
}

// Backup copies the database with VACUUM INTO, which is safe on a live WAL
// database in a way that copying the file is not. See ADR-0019.
func Backup(ctx context.Context, db *DB, version int64) (string, error) {
	dir := BackupDir(db.Path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s.v%d.%s.db",
		strings.TrimSuffix(filepath.Base(db.Path), filepath.Ext(db.Path)),
		version,
		time.Now().UTC().Format("20060102T150405.000Z"),
	)
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("backup %s already exists", path)
	}

	// VACUUM INTO refuses to overwrite, so a failed copy never looks like a
	// good one.
	if _, err := db.W.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return "", fmt.Errorf("vacuum into %s: %w", path, err)
	}
	if err := pruneBackups(dir, BackupsKept); err != nil {
		return path, fmt.Errorf("copied to %s but pruning failed: %w", path, err)
	}
	return path, nil
}

func pruneBackups(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var backups []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".db") {
			backups = append(backups, e.Name())
		}
	}
	if len(backups) <= keep {
		return nil
	}
	// The timestamp is in the name and sorts lexicographically.
	slices.Sort(backups)
	var errs []error
	for _, name := range backups[:len(backups)-keep] {
		errs = append(errs, os.Remove(filepath.Join(dir, name)))
	}
	return errors.Join(errs...)
}
