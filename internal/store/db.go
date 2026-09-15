// SPDX-License-Identifier: AGPL-3.0-or-later

// Package store holds persistence. One SQLite file, one writer.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// The DSN lives here and nowhere else. The two candidate drivers differ in DSN
// syntax and driver name in ways that fail silently, so naming one is part of
// the decision. See ADR-0018.
const driverName = "sqlite"

// pragmas are applied to every connection in both pools.
var pragmas = []string{
	"journal_mode(WAL)",
	"busy_timeout(5000)",
	"foreign_keys(1)",
	"synchronous(NORMAL)",
}

// DB holds two pools over one file. Writes go through a single connection, so
// two goroutines never contend for the write lock; reads scale separately.
// Cross-process concurrency is handled by not having it: when a server is
// running the CLI is an HTTP client. See ADR-0018.
type DB struct {
	W    *sql.DB
	R    *sql.DB
	Path string
}

func DSN(path string) string {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	return "file:" + path + "?" + q.Encode()
}

// Open prepares both pools. It does not migrate: only nuno serve and nuno
// migrate do that (FR-73).
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("database path cannot be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("database directory: %w", err)
	}

	dsn := DSN(path)
	writer, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)

	reader, err := sql.Open(driverName, dsn)
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}
	reader.SetMaxOpenConns(4)
	reader.SetMaxIdleConns(4)

	db := &DB{W: writer, R: reader, Path: path}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.W.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s: %w", path, err)
	}
	if err := db.verifyPragmas(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// verifyPragmas checks that the DSN was honoured rather than assuming it. A
// silently ignored pragma is how foreign keys end up off in production.
func (db *DB) verifyPragmas(ctx context.Context) error {
	var journal string
	if err := db.W.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("read journal_mode: %w", err)
	}
	if journal != "wal" {
		return fmt.Errorf("journal_mode is %q, want wal", journal)
	}
	for _, pool := range []struct {
		name string
		db   *sql.DB
	}{{"writer", db.W}, {"reader", db.R}} {
		var fk int
		if err := pool.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			return fmt.Errorf("read foreign_keys on the %s pool: %w", pool.name, err)
		}
		if fk != 1 {
			return fmt.Errorf("foreign_keys is off on the %s pool", pool.name)
		}
	}
	return nil
}

func (db *DB) Close() error {
	var errs []error
	if db.R != nil {
		errs = append(errs, db.R.Close())
	}
	if db.W != nil {
		errs = append(errs, db.W.Close())
	}
	return errors.Join(errs...)
}

// Tx runs fn in a write transaction, rolling back on error.
func (db *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return errors.Join(err, rollbackErr)
		}
		return err
	}
	return tx.Commit()
}
