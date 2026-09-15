// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

// ReplaceExternalAccounts stores what one provider reported in one cycle.
//
// It is only called after a successful observe. A provider that failed keeps
// its previous rows untouched, which is what lets the page show the last known
// numbers marked unavailable rather than nothing at all. Accounts the provider
// no longer reports are removed here, so a link pointing at one becomes
// dangling and is reported rather than written to (FR-9b).
func (db *DB) ReplaceExternalAccounts(ctx context.Context, providerID int64, accounts []core.Account, observedAt time.Time) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		seen := make(map[string]bool, len(accounts))
		for _, a := range accounts {
			if a.ExternalID == "" {
				return fmt.Errorf("provider %d reported an account with no external id", providerID)
			}
			if err := a.Quota.Valid(); err != nil {
				return fmt.Errorf("account %s: quota: %w", a.ExternalID, err)
			}
			if err := a.Used.ValidAsUsage(); err != nil {
				return fmt.Errorf("account %s: usage: %w", a.ExternalID, err)
			}
			seen[a.ExternalID] = true

			at := a.ObservedAt
			if at.IsZero() {
				at = observedAt
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO external_accounts
				   (provider_id, external_id, subject, username, email, email_normalized,
				    enabled, deleted, quota, used, never_used, observed_at, observe_ok)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
				 ON CONFLICT (provider_id, external_id) DO UPDATE SET
				   subject = excluded.subject, username = excluded.username,
				   email = excluded.email, email_normalized = excluded.email_normalized,
				   enabled = excluded.enabled, deleted = excluded.deleted,
				   quota = excluded.quota, used = excluded.used,
				   never_used = excluded.never_used,
				   observed_at = excluded.observed_at, observe_ok = 1`,
				providerID, a.ExternalID, a.Subject, a.Username, a.Email, NormalizeEmail(a.Email),
				boolToInt(a.Enabled), boolToInt(a.Deleted),
				a.Quota.Encode(), a.Used.Encode(), boolToInt(a.NeverUsed), formatTime(at),
			); err != nil {
				return fmt.Errorf("store account %s: %w", a.ExternalID, err)
			}
		}

		rows, err := tx.QueryContext(ctx, `SELECT external_id FROM external_accounts WHERE provider_id = ?`, providerID)
		if err != nil {
			return err
		}
		var vanished []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !seen[id] {
				vanished = append(vanished, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range vanished {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM external_accounts WHERE provider_id = ? AND external_id = ?`, providerID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListExternalAccounts returns observed accounts for one provider, ordered by
// external id so every derived output is stable.
func (db *DB) ListExternalAccounts(ctx context.Context, providerID int64) ([]core.ExternalAccount, error) {
	return db.queryAccounts(ctx,
		`SELECT provider_id, external_id, user_id, subject, username, email, enabled, deleted,
		        quota, used, never_used, observed_at, observe_ok
		 FROM external_accounts WHERE provider_id = ? ORDER BY external_id`, providerID)
}

// ListAllExternalAccounts returns every observed account, which is what the
// usage endpoint and the unmanaged list are built from.
func (db *DB) ListAllExternalAccounts(ctx context.Context) ([]core.ExternalAccount, error) {
	return db.queryAccounts(ctx,
		`SELECT provider_id, external_id, user_id, subject, username, email, enabled, deleted,
		        quota, used, never_used, observed_at, observe_ok
		 FROM external_accounts ORDER BY provider_id, external_id`)
}

func (db *DB) queryAccounts(ctx context.Context, query string, args ...any) ([]core.ExternalAccount, error) {
	rows, err := db.R.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []core.ExternalAccount
	for rows.Next() {
		var (
			a          core.ExternalAccount
			userID     sql.NullInt64
			quota      string
			used       string
			enabled    int
			deleted    int
			neverUsed  int
			observeOK  int
			observedAt string
		)
		if err := rows.Scan(&a.ProviderID, &a.ExternalID, &userID, &a.Subject, &a.Username, &a.Email,
			&enabled, &deleted, &quota, &used, &neverUsed, &observedAt, &observeOK); err != nil {
			return nil, err
		}
		if userID.Valid {
			id := userID.Int64
			a.UserID = &id
		}
		if a.Quota, err = core.DecodeQuota(quota); err != nil {
			return nil, fmt.Errorf("account %s: %w", a.ExternalID, err)
		}
		if a.Used, err = core.DecodeQuota(used); err != nil {
			return nil, fmt.Errorf("account %s: %w", a.ExternalID, err)
		}
		if a.ObservedAt, err = parseTime(observedAt); err != nil {
			return nil, err
		}
		a.Enabled = enabled == 1
		a.Deleted = deleted == 1
		a.NeverUsed = neverUsed == 1
		a.ObserveOK = observeOK == 1
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// SetAccountOwner points an observed account at a person, or clears it. The
// column is what makes an unmanaged account representable (FR-3).
func (db *DB) SetAccountOwner(ctx context.Context, providerID int64, externalID string, userID *int64) error {
	var owner sql.NullInt64
	if userID != nil {
		owner = sql.NullInt64{Int64: *userID, Valid: true}
	}
	_, err := db.W.ExecContext(ctx,
		`UPDATE external_accounts SET user_id = ? WHERE provider_id = ? AND external_id = ?`,
		owner, providerID, externalID)
	return err
}

// UpsertLink records a link. The unique constraint on (provider_id,
// external_id) is what stops two people's policies from writing to one
// account, so a conflict is an error and not something to overwrite.
func (db *DB) UpsertLink(ctx context.Context, link core.AccountLink) error {
	at := link.LinkedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO account_links (user_id, provider_id, external_id, origin, linked_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (user_id, provider_id) DO UPDATE SET
		   external_id = excluded.external_id, origin = excluded.origin, linked_at = excluded.linked_at`,
		link.UserID, link.ProviderID, link.ExternalID, string(link.Origin), formatTime(at))
	if err != nil {
		return fmt.Errorf("link user %d to %s on provider %d: %w", link.UserID, link.ExternalID, link.ProviderID, err)
	}
	return nil
}

func (db *DB) DeleteLink(ctx context.Context, userID, providerID int64) (bool, error) {
	res, err := db.W.ExecContext(ctx,
		`DELETE FROM account_links WHERE user_id = ? AND provider_id = ?`, userID, providerID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

func (db *DB) ListLinks(ctx context.Context) ([]core.AccountLink, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT user_id, provider_id, external_id, origin, linked_at FROM account_links
		 ORDER BY provider_id, external_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []core.AccountLink
	for rows.Next() {
		var l core.AccountLink
		var origin, linkedAt string
		if err := rows.Scan(&l.UserID, &l.ProviderID, &l.ExternalID, &origin, &linkedAt); err != nil {
			return nil, err
		}
		l.Origin = core.LinkOrigin(origin)
		if l.LinkedAt, err = parseTime(linkedAt); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// ReplaceLinkIssues swaps in the issues found this cycle. They are derived,
// so they are recomputed rather than accumulated: a stale issue list is worse
// than none, because it sends an admin to fix something already fixed.
func (db *DB) ReplaceLinkIssues(ctx context.Context, providerID int64, issues []core.LinkIssue) error {
	now := formatTime(time.Now())
	return db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM link_issues WHERE provider_id = ?`, providerID); err != nil {
			return err
		}
		for _, issue := range issues {
			var userID sql.NullInt64
			if issue.UserID != nil {
				userID = sql.NullInt64{Int64: *issue.UserID, Valid: true}
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO link_issues (provider_id, kind, user_id, external_id, detail, computed_at)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				providerID, string(issue.Kind), userID, issue.ExternalID, issue.Detail, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (db *DB) ListLinkIssues(ctx context.Context) ([]core.LinkIssue, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT provider_id, kind, user_id, external_id, detail FROM link_issues
		 ORDER BY provider_id, kind, external_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var issues []core.LinkIssue
	for rows.Next() {
		var issue core.LinkIssue
		var kind string
		var userID sql.NullInt64
		if err := rows.Scan(&issue.ProviderID, &kind, &userID, &issue.ExternalID, &issue.Detail); err != nil {
			return nil, err
		}
		issue.Kind = core.LinkIssueKind(kind)
		if userID.Valid {
			id := userID.Int64
			issue.UserID = &id
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}
