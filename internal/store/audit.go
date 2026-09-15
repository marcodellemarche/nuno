// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

// AuditResult is what happened to one change.
type AuditResult string

const (
	AuditApplied AuditResult = "applied"
	AuditFailed  AuditResult = "failed"
	// AuditRefused is a guardrail decision: the change was computed, shown,
	// and deliberately not made.
	AuditRefused AuditResult = "refused"
	AuditSkipped AuditResult = "skipped"
)

// RecordChange appends to the audit log. Every write is audited with its run
// id, and so is every refusal, because a run that refused something is not the
// same as a run that had nothing to do (FR-33, FR-34).
func (db *DB) RecordChange(ctx context.Context, runID int64, actor string, change core.Change, result AuditResult, detail string) error {
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO audit_log (run_id, at, actor, user_id, provider_id, external_id, field, from_value, to_value, result, detail)
		 VALUES (?, ?, ?, ?, ?, ?, 'quota', ?, ?, ?, ?)`,
		runID, formatTime(time.Now()), actor, change.UserID, change.ProviderID, change.ExternalID,
		change.From.Encode(), change.To.Encode(), string(result), detail)
	return err
}

// AuditRow is one entry with its names resolved, for display.
type AuditRow struct {
	ID       int64
	RunID    *int64
	At       time.Time
	Actor    string
	User     string
	Provider string
	External string
	From     core.Quota
	To       core.Quota
	Result   AuditResult
	Detail   string
}

// RecentChanges returns the newest audit entries across everyone, which is
// what the UI shows under the last run (FR-52).
func (db *DB) RecentChanges(ctx context.Context, limit int) ([]AuditRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.R.QueryContext(ctx,
		`SELECT a.id, a.run_id, a.at, a.actor, coalesce(u.uid, ''), coalesce(p.name, ''),
		        a.external_id, a.from_value, a.to_value, a.result, a.detail
		 FROM audit_log a
		 LEFT JOIN users u ON u.id = a.user_id
		 LEFT JOIN providers p ON p.id = a.provider_id
		 ORDER BY a.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []AuditRow
	for rows.Next() {
		var row AuditRow
		var at, from, to, result string
		var runID sql.NullInt64
		if err := rows.Scan(&row.ID, &runID, &at, &row.Actor, &row.User, &row.Provider,
			&row.External, &from, &to, &result, &row.Detail); err != nil {
			return nil, err
		}
		if runID.Valid {
			id := runID.Int64
			row.RunID = &id
		}
		if row.At, err = parseTime(at); err != nil {
			return nil, err
		}
		if row.From, err = core.DecodeQuota(from); err != nil {
			return nil, err
		}
		if row.To, err = core.DecodeQuota(to); err != nil {
			return nil, err
		}
		row.Result = AuditResult(result)
		entries = append(entries, row)
	}
	return entries, rows.Err()
}

// SeenGuardedChange records that a guarded change was seen, reporting whether
// this is the first time. A persistently guarded change reappears every cycle
// by design, so notification is keyed on it and fires on first appearance and
// on change, not once per cycle (FR-63).
func (db *DB) SeenGuardedChange(ctx context.Context, key string) (bool, error) {
	now := formatTime(time.Now())
	res, err := db.W.ExecContext(ctx,
		`INSERT INTO notified_changes (change_key, first_seen, last_seen) VALUES (?, ?, ?)
		 ON CONFLICT (change_key) DO UPDATE SET last_seen = excluded.last_seen`,
		key, now, now)
	if err != nil {
		return false, err
	}
	// SQLite reports one row changed for both the insert and the update, so
	// the timestamps decide which it was.
	var firstSeen, lastSeen string
	if err := db.R.QueryRowContext(ctx,
		`SELECT first_seen, last_seen FROM notified_changes WHERE change_key = ?`, key).
		Scan(&firstSeen, &lastSeen); err != nil {
		return false, err
	}
	_ = res
	return firstSeen == lastSeen, nil
}

// ForgetGuardedChanges drops keys that no longer appear, so a change that was
// fixed and comes back later notifies again.
func (db *DB) ForgetGuardedChanges(ctx context.Context, keep []string) error {
	if len(keep) == 0 {
		_, err := db.W.ExecContext(ctx, `DELETE FROM notified_changes`)
		return err
	}
	query := `DELETE FROM notified_changes WHERE change_key NOT IN (?`
	args := make([]any, 0, len(keep))
	args = append(args, keep[0])
	for _, key := range keep[1:] {
		query += ", ?"
		args = append(args, key)
	}
	query += ")"
	_, err := db.W.ExecContext(ctx, query, args...)
	return err
}
