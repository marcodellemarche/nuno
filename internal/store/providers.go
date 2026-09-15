// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

// ProviderRow is a configured instance plus what the last cycle learned about
// it. Health is transient but has to outlive the process: the usage API
// distinguishes "never read" from "read and failed".
type ProviderRow struct {
	core.ProviderInstance
	WriteAccess    core.WriteAccess
	WriteProbedAt  *time.Time
	Version        string
	Reachable      bool
	InSupported    bool
	LastObserveAt  *time.Time
	LastObserveOK  bool
	LastError      string
	DegradedReason string
}

// Degraded reports whether the provider may be read but not written. It is
// not the same as unreachable.
func (r ProviderRow) Degraded() bool { return r.DegradedReason != "" }

// UpsertProvider reconciles the configured instances into the table, keyed on
// name. The name is the operator's, and changing a provider's type under the
// same name is refused: the accounts linked to it would silently retarget.
func (db *DB) UpsertProvider(ctx context.Context, inst core.ProviderInstance) (int64, error) {
	if inst.Name == "" || inst.Type == "" {
		return 0, errors.New("a provider needs a name and a type")
	}
	now := formatTime(time.Now())

	var id int64
	var existingType string
	err := db.R.QueryRowContext(ctx, `SELECT id, type FROM providers WHERE name = ?`, inst.Name).
		Scan(&id, &existingType)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := db.W.ExecContext(ctx,
			`INSERT INTO providers (type, name, match_key, config_ref, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			inst.Type, inst.Name, string(inst.MatchKey), inst.ConfigRef, now, now)
		if err != nil {
			return 0, fmt.Errorf("insert provider %q: %w", inst.Name, err)
		}
		return res.LastInsertId()
	case err != nil:
		return 0, err
	case existingType != inst.Type:
		return 0, fmt.Errorf("provider %q is %s in the database and %s in the configuration: rename one, because the accounts linked to it belong to the old type",
			inst.Name, existingType, inst.Type)
	}

	if _, err := db.W.ExecContext(ctx,
		`UPDATE providers SET match_key = ?, config_ref = ?, updated_at = ? WHERE id = ?`,
		string(inst.MatchKey), inst.ConfigRef, now, id); err != nil {
		return 0, fmt.Errorf("update provider %q: %w", inst.Name, err)
	}
	return id, nil
}

const providerColumns = `id, type, name, match_key, config_ref, write_access, write_probed_at,
	version, reachable, in_supported, last_observe_at, last_observe_ok, last_error, degraded_reason`

func (db *DB) ListProviders(ctx context.Context) ([]ProviderRow, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+providerColumns+` FROM providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []ProviderRow
	for rows.Next() {
		row, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		providers = append(providers, row)
	}
	return providers, rows.Err()
}

func (db *DB) GetProviderByName(ctx context.Context, name string) (ProviderRow, error) {
	row := db.R.QueryRowContext(ctx, `SELECT `+providerColumns+` FROM providers WHERE name = ?`, name)
	return scanProvider(row)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanProvider(s scanner) (ProviderRow, error) {
	var (
		row         ProviderRow
		matchKey    string
		writeAccess string
		probedAt    sql.NullString
		observeAt   sql.NullString
		reachable   int
		inSupported int
		observeOK   int
	)
	if err := s.Scan(&row.ID, &row.Type, &row.Name, &matchKey, &row.ConfigRef,
		&writeAccess, &probedAt, &row.Version, &reachable, &inSupported,
		&observeAt, &observeOK, &row.LastError, &row.DegradedReason); err != nil {
		return ProviderRow{}, err
	}
	row.MatchKey = core.MatchKey(matchKey)
	access, err := parseWriteAccess(writeAccess)
	if err != nil {
		return ProviderRow{}, err
	}
	row.WriteAccess = access
	if row.WriteProbedAt, err = parseTimePtr(probedAt); err != nil {
		return ProviderRow{}, err
	}
	if row.LastObserveAt, err = parseTimePtr(observeAt); err != nil {
		return ProviderRow{}, err
	}
	row.Reachable = reachable == 1
	row.InSupported = inSupported == 1
	row.LastObserveOK = observeOK == 1
	return row, nil
}

func parseWriteAccess(s string) (core.WriteAccess, error) {
	switch s {
	case "unproven":
		return core.WriteUnproven, nil
	case "yes":
		return core.WriteYes, nil
	case "no":
		return core.WriteNo, nil
	}
	return core.WriteUnproven, fmt.Errorf("undecodable write access %q", s)
}

// SaveHealth records what a read-only health check found. It never touches
// write access, which only the probe can answer (ADR-0025).
func (db *DB) SaveHealth(ctx context.Context, providerID int64, h core.HealthResult) error {
	message := ""
	if h.Err != nil {
		message = h.Err.Error()
	}
	_, err := db.W.ExecContext(ctx,
		`UPDATE providers SET version = ?, reachable = ?, in_supported = ?, last_error = ?, updated_at = ?
		 WHERE id = ?`,
		h.Version, boolToInt(h.Reachable), boolToInt(h.InSupported), message, formatTime(time.Now()), providerID)
	return err
}

// SaveWriteAccess records the outcome of the write probe.
func (db *DB) SaveWriteAccess(ctx context.Context, providerID int64, access core.WriteAccess, at time.Time) error {
	_, err := db.W.ExecContext(ctx,
		`UPDATE providers SET write_access = ?, write_probed_at = ?, updated_at = ? WHERE id = ?`,
		access.String(), formatTime(at), formatTime(time.Now()), providerID)
	return err
}

// SaveObserveResult records whether the last observation of this provider
// succeeded. A failed observe leaves the accounts alone and marks the
// provider, which is what turns their status to unavailable in the API.
func (db *DB) SaveObserveResult(ctx context.Context, providerID int64, at time.Time, ok bool, failure error) error {
	message := ""
	if failure != nil {
		message = failure.Error()
	}
	_, err := db.W.ExecContext(ctx,
		`UPDATE providers SET last_observe_at = ?, last_observe_ok = ?, last_error = ?, updated_at = ?
		 WHERE id = ?`,
		formatTime(at), boolToInt(ok), message, formatTime(time.Now()), providerID)
	return err
}

// SetDegraded names why a provider may be read but not written, or clears it.
func (db *DB) SetDegraded(ctx context.Context, providerID int64, reason string) error {
	_, err := db.W.ExecContext(ctx,
		`UPDATE providers SET degraded_reason = ?, updated_at = ? WHERE id = ?`,
		reason, formatTime(time.Now()), providerID)
	return err
}
