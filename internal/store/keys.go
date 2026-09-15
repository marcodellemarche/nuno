// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

// HashKey is how a capability key is stored. The value itself never reaches
// the database, so a stolen backup grants nothing.
func HashKey(value core.Secret) string {
	sum := sha256.Sum256([]byte(value.Reveal()))
	return hex.EncodeToString(sum[:])
}

// AdminKeySource says where a key came from. An env-bootstrapped key must
// always be recoverable from the compose file, so it is never revoked by the
// UI (ADR-0022).
type AdminKeySource string

const (
	AdminKeyFromEnv AdminKeySource = "env"
	AdminKeyFromUI  AdminKeySource = "ui"
)

// EnsureAdminKey stores a key from the environment, idempotently. Restarting
// with the same NUNO_ADMIN_KEY must not accumulate rows or revive a key the
// operator revoked by removing it.
func (db *DB) EnsureAdminKey(ctx context.Context, value core.Secret, label string) error {
	if value.Empty() {
		return errors.New("refusing to store an empty admin key")
	}
	now := formatTime(time.Now())
	hash := HashKey(value)
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO admin_keys (hash, label, source, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (hash) DO UPDATE SET label = excluded.label, revoked_at = NULL`,
		hash, label, string(AdminKeyFromEnv), now)
	return err
}

// RevokeEnvKeysExcept removes env-sourced keys that are no longer in the
// environment, so deleting NUNO_ADMIN_KEY from compose actually revokes it.
// Keys issued through the UI are untouched.
func (db *DB) RevokeEnvKeysExcept(ctx context.Context, keep core.Secret) (int, error) {
	now := formatTime(time.Now())
	query := `UPDATE admin_keys SET revoked_at = ? WHERE source = ? AND revoked_at IS NULL`
	args := []any{now, string(AdminKeyFromEnv)}
	if !keep.Empty() {
		query += ` AND hash <> ?`
		args = append(args, HashKey(keep))
	}
	res, err := db.W.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	return int(affected), err
}

// CheckAdminKey reports whether a presented key is valid, and records the use.
// The comparison is constant time, and a revoked key is never valid.
func (db *DB) CheckAdminKey(ctx context.Context, presented core.Secret) (bool, error) {
	if presented.Empty() {
		return false, nil
	}
	hash := HashKey(presented)

	var stored string
	err := db.R.QueryRowContext(ctx,
		`SELECT hash FROM admin_keys WHERE hash = ? AND revoked_at IS NULL`, hash).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) != 1 {
		return false, nil
	}

	// Best effort: a failure to record the use must not deny a valid key.
	if _, err := db.W.ExecContext(ctx,
		`UPDATE admin_keys SET last_used_at = ? WHERE hash = ?`, formatTime(time.Now()), hash); err != nil {
		return true, nil
	}
	return true, nil
}

// CountAdminKeys reports how many usable keys exist, which is what the
// startup log and doctor report.
func (db *DB) CountAdminKeys(ctx context.Context) (int, error) {
	var count int
	err := db.R.QueryRowContext(ctx,
		`SELECT count(*) FROM admin_keys WHERE revoked_at IS NULL`).Scan(&count)
	return count, err
}

func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, formatTime(time.Now()))
	return err
}

// GetSetting returns the value and whether it was set, because an absent
// setting and an empty one are different things.
func (db *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := db.R.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// AdminKeyRow is one key, without its value, which is not recoverable.
type AdminKeyRow struct {
	ID         int64
	Label      string
	Source     AdminKeySource
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

func (r AdminKeyRow) Revoked() bool { return r.RevokedAt != nil }

// IssueAdminKey mints a key and returns the value exactly once. It is stored
// only as a hash, so a lost key is rotated and never recovered (FR-56).
func (db *DB) IssueAdminKey(ctx context.Context, label string) (core.Secret, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	value := core.Secret(hex.EncodeToString(raw[:]))

	if _, err := db.W.ExecContext(ctx,
		`INSERT INTO admin_keys (hash, label, source, created_at) VALUES (?, ?, ?, ?)`,
		HashKey(value), label, string(AdminKeyFromUI), formatTime(time.Now())); err != nil {
		return "", err
	}
	return value, nil
}

// RevokeAdminKey revokes one by id. An env-sourced key is refused, because an
// operator must always be able to recover access from the compose file
// (ADR-0022): removing the variable is how that one goes.
func (db *DB) RevokeAdminKey(ctx context.Context, id int64) error {
	var source string
	err := db.R.QueryRowContext(ctx, `SELECT source FROM admin_keys WHERE id = ?`, id).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no key %d", id)
	}
	if err != nil {
		return err
	}
	if AdminKeySource(source) == AdminKeyFromEnv {
		return errors.New("this key comes from NUNO_ADMIN_KEY: remove it from the environment and restart, so recovery from the compose file always works")
	}
	_, err = db.W.ExecContext(ctx,
		`UPDATE admin_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		formatTime(time.Now()), id)
	return err
}

// ListAdminKeys lists the keys, values excluded because they were never
// stored.
func (db *DB) ListAdminKeys(ctx context.Context) ([]AdminKeyRow, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT id, label, source, created_at, last_used_at, revoked_at FROM admin_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []AdminKeyRow
	for rows.Next() {
		var key AdminKeyRow
		var source, createdAt string
		var lastUsed, revoked sql.NullString
		if err := rows.Scan(&key.ID, &key.Label, &source, &createdAt, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		key.Source = AdminKeySource(source)
		if key.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		if key.LastUsedAt, err = parseTimePtr(lastUsed); err != nil {
			return nil, err
		}
		if key.RevokedAt, err = parseTimePtr(revoked); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
