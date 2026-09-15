// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "nuno.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func migrated(t *testing.T) *DB {
	t.Helper()
	db := openTemp(t)
	if err := Migrate(context.Background(), db, discard()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestOpenAppliesThePragmas(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	var journal string
	if err := db.R.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	var sync int
	if err := db.W.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", sync)
	}
	if db.W.Stats().MaxOpenConnections != 1 {
		t.Errorf("the writer pool must hold one connection, got %d", db.W.Stats().MaxOpenConnections)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	db := migrated(t)
	_, err := db.W.Exec(
		`INSERT INTO external_accounts (provider_id, external_id, observed_at) VALUES (404, 'x', ?)`,
		time.Now().UTC().Format(time.RFC3339),
	)
	if err == nil {
		t.Fatal("an account on a provider that does not exist must be refused")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("err = %v, want a foreign key violation", err)
	}
}

func TestConstraintsRefuseInvalidEnums(t *testing.T) {
	db := migrated(t)
	now := time.Now().UTC().Format(time.RFC3339)

	// Matching on the naked id is what ADR-0013 forbids, and the schema says so too.
	_, err := db.W.Exec(
		`INSERT INTO providers (type, name, match_key, config_ref, created_at, updated_at) VALUES ('nextcloud', 'cloud', 'uid', 'X', ?, ?)`,
		now, now)
	if err == nil {
		t.Error("match_key 'uid' must be refused")
	}

	if _, err := db.W.Exec(
		`INSERT INTO providers (type, name, match_key, config_ref, created_at, updated_at) VALUES ('nextcloud', 'cloud', 'email', 'X', ?, ?)`,
		now, now); err != nil {
		t.Fatal(err)
	}
	var access string
	if err := db.R.QueryRow(`SELECT write_access FROM providers WHERE name = 'cloud'`).Scan(&access); err != nil {
		t.Fatal(err)
	}
	if access != "unproven" {
		t.Errorf("write_access = %q, want unproven on a provider that was never probed", access)
	}
}

func TestOneAccountCannotBeLinkedToTwoPeople(t *testing.T) {
	db := migrated(t)
	now := time.Now().UTC().Format(time.RFC3339)
	mustExec(t, db, `INSERT INTO providers (id, type, name, match_key, config_ref, created_at, updated_at) VALUES (1, 'immich', 'photos', 'email', 'X', ?, ?)`, now, now)
	for i, uuid := range []string{"uuid-a", "uuid-b"} {
		mustExec(t, db, `INSERT INTO users (id, source, source_uuid, uid, created_at, updated_at) VALUES (?, 'lldap', ?, ?, ?, ?)`,
			i+1, uuid, fmt.Sprintf("user%d", i+1), now, now)
	}
	mustExec(t, db, `INSERT INTO account_links (user_id, provider_id, external_id, origin, linked_at) VALUES (1, 1, 'acct', 'matched', ?)`, now)

	_, err := db.W.Exec(`INSERT INTO account_links (user_id, provider_id, external_id, origin, linked_at) VALUES (2, 1, 'acct', 'matched', ?)`, now)
	if err == nil {
		t.Fatal("two people linked to one account must be refused: that is how two policies write to one quota")
	}
}

func TestMigrateIsIdempotentAndGuardsTheVersion(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	target, err := TargetVersion()
	if err != nil {
		t.Fatal(err)
	}
	if target < 1 {
		t.Fatalf("TargetVersion() = %d", target)
	}

	if err := CheckSchema(ctx, db); !errors.Is(err, ErrSchemaOlder) {
		t.Fatalf("before migrating, CheckSchema() = %v, want ErrSchemaOlder", err)
	}

	if err := Migrate(ctx, db, discard()); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(ctx, db); err != nil {
		t.Fatalf("after migrating: %v", err)
	}
	if err := Migrate(ctx, db, discard()); err != nil {
		t.Fatalf("migrating twice must be a no-op: %v", err)
	}

	// A database written by a newer Nuno is refused in both paths.
	mustExec(t, db, `INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (?, 1, CURRENT_TIMESTAMP)`, target+1)
	if err := CheckSchema(ctx, db); !errors.Is(err, ErrSchemaNewer) {
		t.Errorf("CheckSchema() = %v, want ErrSchemaNewer", err)
	}
	if err := Migrate(ctx, db, discard()); !errors.Is(err, ErrSchemaNewer) {
		t.Errorf("Migrate() = %v, want ErrSchemaNewer", err)
	}
}

func TestBackupProducesAReadableCopy(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	now := time.Now().UTC().Format(time.RFC3339)
	mustExec(t, db, `INSERT INTO providers (type, name, match_key, config_ref, created_at, updated_at) VALUES ('nextcloud', 'cloud', 'email', 'X', ?, ?)`, now, now)

	path, err := Backup(ctx, db, 1)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != BackupDir(db.Path) {
		t.Errorf("backup landed in %s, want %s", filepath.Dir(path), BackupDir(db.Path))
	}

	copied, err := Open(path)
	if err != nil {
		t.Fatalf("the copy must be a usable database: %v", err)
	}
	defer copied.Close()
	var name string
	if err := copied.R.QueryRow(`SELECT name FROM providers`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "cloud" {
		t.Errorf("name = %q", name)
	}
}

func TestMigrateCopiesTheDatabaseFirstButNotWhenThereIsNothingToCopy(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	if err := Migrate(ctx, db, discard()); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(BackupDir(db.Path)); err == nil && len(entries) > 0 {
		t.Errorf("an empty database has nothing worth copying, found %d backups", len(entries))
	}
}

func TestPruneBackupsKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"nuno.v1.20260101T000000.000Z.db",
		"nuno.v1.20260102T000000.000Z.db",
		"nuno.v1.20260103T000000.000Z.db",
		"nuno.v1.20260104T000000.000Z.db",
		"notes.txt",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneBackups(dir, 2); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range left {
		got = append(got, e.Name())
	}
	want := map[string]bool{
		"nuno.v1.20260103T000000.000Z.db": true,
		"nuno.v1.20260104T000000.000Z.db": true,
		"notes.txt":                       true,
	}
	if len(got) != len(want) {
		t.Fatalf("left %v, want %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected survivor %s", n)
		}
	}
}

func mustExec(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if _, err := db.W.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}

	// A second lock from the same process still has to fail, or two commands
	// in one shell would both proceed.
	if _, err := Lock(dir); err == nil {
		t.Error("a second lock must fail while the first is held")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	again, err := Lock(dir)
	if err != nil {
		t.Fatalf("releasing must make the lock available again: %v", err)
	}
	if err := again(); err != nil {
		t.Fatal(err)
	}
}
