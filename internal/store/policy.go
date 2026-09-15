// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

// SaveTier writes a tier and its allocations atomically, after validating it.
// A tier that cannot resolve honestly is refused here rather than resolved
// around later: Nextcloud reads a negative value as a sentinel, so an
// unvalidated negative would grant unlimited instead of failing (ADR-0020).
func (db *DB) SaveTier(ctx context.Context, tier core.Tier) (int64, error) {
	if err := tier.Validate(); err != nil {
		return 0, err
	}
	now := formatTime(time.Now())

	var id int64
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT id FROM tiers WHERE name = ?`, tier.Name).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			res, err := tx.ExecContext(ctx,
				`INSERT INTO tiers (name, budget, is_default, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
				tier.Name, tier.Budget.Encode(), boolToInt(tier.IsDefault), now, now)
			if err != nil {
				return fmt.Errorf("insert tier %q: %w", tier.Name, err)
			}
			if id, err = res.LastInsertId(); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if _, err := tx.ExecContext(ctx,
				`UPDATE tiers SET budget = ?, is_default = ?, updated_at = ? WHERE id = ?`,
				tier.Budget.Encode(), boolToInt(tier.IsDefault), now, id); err != nil {
				return fmt.Errorf("update tier %q: %w", tier.Name, err)
			}
		}

		// Allocations are replaced, because an allocation that disappeared
		// means the tier is now silent about that provider, which is a
		// different thing from zero.
		if _, err := tx.ExecContext(ctx, `DELETE FROM allocations WHERE tier_id = ?`, id); err != nil {
			return err
		}
		for _, a := range tier.Allocations {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO allocations (tier_id, provider_id, mode, value) VALUES (?, ?, ?, ?)`,
				id, a.ProviderID, string(a.Mode), a.Value); err != nil {
				return fmt.Errorf("allocation for provider %d: %w", a.ProviderID, err)
			}
		}
		return nil
	})
	return id, err
}

// SetDefaultTier moves the default, keeping at most one.
func (db *DB) SetDefaultTier(ctx context.Context, tierID int64) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE tiers SET is_default = 0 WHERE is_default = 1`); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE tiers SET is_default = 1 WHERE id = ?`, tierID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return fmt.Errorf("no tier %d", tierID)
		}
		return nil
	})
}

func (db *DB) DeleteTier(ctx context.Context, name string) (bool, error) {
	res, err := db.W.ExecContext(ctx, `DELETE FROM tiers WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

// MapGroupToTier is the edge group membership turns into a tier (FR-4).
func (db *DB) MapGroupToTier(ctx context.Context, groupUUID string, tierID int64) error {
	var groupID int64
	err := db.R.QueryRowContext(ctx, `SELECT id FROM "groups" WHERE source_uuid = ?`, groupUUID).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no group %q: run `nuno observe` to sync the directory first", groupUUID)
	}
	if err != nil {
		return err
	}
	_, err = db.W.ExecContext(ctx,
		`INSERT INTO group_tiers (group_id, tier_id) VALUES (?, ?)
		 ON CONFLICT (group_id) DO UPDATE SET tier_id = excluded.tier_id`,
		groupID, tierID)
	return err
}

func (db *DB) UnmapGroup(ctx context.Context, groupUUID string) (bool, error) {
	res, err := db.W.ExecContext(ctx,
		`DELETE FROM group_tiers WHERE group_id = (SELECT id FROM "groups" WHERE source_uuid = ?)`, groupUUID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

func (db *DB) SetUserTierOverride(ctx context.Context, userID int64, tierID *int64) error {
	if tierID == nil {
		_, err := db.W.ExecContext(ctx, `DELETE FROM user_tier_overrides WHERE user_id = ?`, userID)
		return err
	}
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO user_tier_overrides (user_id, tier_id) VALUES (?, ?)
		 ON CONFLICT (user_id) DO UPDATE SET tier_id = excluded.tier_id`, userID, *tierID)
	return err
}

// OverrideOrigin says whether a per-provider override was set by hand or
// imported by nuno adopt, which is worth distinguishing when explaining a
// number.
type OverrideOrigin string

const (
	OverrideManual  OverrideOrigin = "manual"
	OverrideAdopted OverrideOrigin = "adopted"
)

func (db *DB) SetProviderOverride(ctx context.Context, userID, providerID int64, quota core.Quota, origin OverrideOrigin) error {
	if err := quota.Valid(); err != nil {
		return err
	}
	if !quota.IsKnown() {
		// An override is an instruction, and there is no instruction that
		// means "write a value nobody knows".
		return errors.New("an override cannot be unknown")
	}
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO user_provider_overrides (user_id, provider_id, quota, origin, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (user_id, provider_id) DO UPDATE SET
		   quota = excluded.quota, origin = excluded.origin, updated_at = excluded.updated_at`,
		userID, providerID, quota.Encode(), string(origin), formatTime(time.Now()))
	return err
}

func (db *DB) ClearProviderOverride(ctx context.Context, userID, providerID int64) (bool, error) {
	res, err := db.W.ExecContext(ctx,
		`DELETE FROM user_provider_overrides WHERE user_id = ? AND provider_id = ?`, userID, providerID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

// LoadPolicy reads everything resolution needs, in one place, so a resolver
// call cannot be made against a half-loaded policy.
func (db *DB) LoadPolicy(ctx context.Context) (core.Policy, error) {
	policy := core.Policy{
		Tiers:      map[int64]core.Tier{},
		GroupTiers: map[string]int64{},
	}

	rows, err := db.R.QueryContext(ctx, `SELECT id, name, budget, is_default FROM tiers ORDER BY name`)
	if err != nil {
		return policy, err
	}
	defer rows.Close()
	for rows.Next() {
		var tier core.Tier
		var budget string
		var isDefault int
		if err := rows.Scan(&tier.ID, &tier.Name, &budget, &isDefault); err != nil {
			return policy, err
		}
		if tier.Budget, err = core.DecodeQuota(budget); err != nil {
			return policy, fmt.Errorf("tier %q: %w", tier.Name, err)
		}
		tier.IsDefault = isDefault == 1
		if tier.IsDefault {
			id := tier.ID
			policy.DefaultTierID = &id
		}
		policy.Tiers[tier.ID] = tier
	}
	if err := rows.Err(); err != nil {
		return policy, err
	}

	allocations, err := db.R.QueryContext(ctx,
		`SELECT tier_id, provider_id, mode, value FROM allocations ORDER BY tier_id, provider_id`)
	if err != nil {
		return policy, err
	}
	defer allocations.Close()
	for allocations.Next() {
		var tierID int64
		var a core.Allocation
		var mode string
		if err := allocations.Scan(&tierID, &a.ProviderID, &mode, &a.Value); err != nil {
			return policy, err
		}
		a.Mode = core.AllocationMode(mode)
		tier, ok := policy.Tiers[tierID]
		if !ok {
			continue
		}
		tier.Allocations = append(tier.Allocations, a)
		policy.Tiers[tierID] = tier
	}
	if err := allocations.Err(); err != nil {
		return policy, err
	}

	groupTiers, err := db.R.QueryContext(ctx,
		`SELECT g.source_uuid, gt.tier_id FROM group_tiers gt JOIN "groups" g ON g.id = gt.group_id`)
	if err != nil {
		return policy, err
	}
	defer groupTiers.Close()
	for groupTiers.Next() {
		var uuid string
		var tierID int64
		if err := groupTiers.Scan(&uuid, &tierID); err != nil {
			return policy, err
		}
		policy.GroupTiers[uuid] = tierID
	}
	return policy, groupTiers.Err()
}

// UserPolicy reads one person's overrides.
func (db *DB) UserPolicy(ctx context.Context, userID int64) (core.UserPolicy, map[int64]OverrideOrigin, error) {
	up := core.UserPolicy{ProviderOverrides: map[int64]core.Quota{}}
	origins := map[int64]OverrideOrigin{}

	var tierID int64
	err := db.R.QueryRowContext(ctx, `SELECT tier_id FROM user_tier_overrides WHERE user_id = ?`, userID).Scan(&tierID)
	switch {
	case err == nil:
		up.TierOverride = &tierID
	case !errors.Is(err, sql.ErrNoRows):
		return up, nil, err
	}

	rows, err := db.R.QueryContext(ctx,
		`SELECT provider_id, quota, origin FROM user_provider_overrides WHERE user_id = ?`, userID)
	if err != nil {
		return up, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var providerID int64
		var quota, origin string
		if err := rows.Scan(&providerID, &quota, &origin); err != nil {
			return up, nil, err
		}
		decoded, err := core.DecodeQuota(quota)
		if err != nil {
			return up, nil, fmt.Errorf("override for provider %d: %w", providerID, err)
		}
		up.ProviderOverrides[providerID] = decoded
		origins[providerID] = OverrideOrigin(origin)
	}
	return up, origins, rows.Err()
}

// GroupsWithTiers lists the group-to-tier mapping for display.
func (db *DB) GroupsWithTiers(ctx context.Context) (map[string]string, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT g.name, t.name FROM group_tiers gt
		 JOIN "groups" g ON g.id = gt.group_id
		 JOIN tiers t ON t.id = gt.tier_id
		 ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mapping := map[string]string{}
	for rows.Next() {
		var group, tier string
		if err := rows.Scan(&group, &tier); err != nil {
			return nil, err
		}
		mapping[group] = tier
	}
	return mapping, rows.Err()
}

// RunMode is what a run was for.
type RunMode string

const (
	RunPlan   RunMode = "plan"
	RunDryRun RunMode = "dry-run"
	RunApply  RunMode = "apply"
)

// StartRun opens a run. A plan is persisted with the run it belongs to,
// because apply re-observes and refuses any change whose classification moved
// since (FR-38b).
func (db *DB) StartRun(ctx context.Context, mode RunMode, actor string, consentShrink bool) (int64, error) {
	res, err := db.W.ExecContext(ctx,
		`INSERT INTO reconcile_runs (started_at, mode, actor, consent_shrink, status)
		 VALUES (?, ?, ?, ?, 'running')`,
		formatTime(time.Now()), string(mode), actor, boolToInt(consentShrink))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) FinishRun(ctx context.Context, runID int64, status, summary string) error {
	_, err := db.W.ExecContext(ctx,
		`UPDATE reconcile_runs SET finished_at = ?, status = ?, summary = ? WHERE id = ?`,
		formatTime(time.Now()), status, summary, runID)
	return err
}

// SavePlanBody stores the plan as it was computed, so what was agreed to and
// what is applied can be compared.
func (db *DB) SavePlanBody(ctx context.Context, runID int64, body []byte) error {
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO plans (run_id, computed_at, body) VALUES (?, ?, ?)`,
		runID, formatTime(time.Now()), string(body))
	return err
}

// RunSummary is one row of the run history.
type RunSummary struct {
	ID         int64
	StartedAt  time.Time
	FinishedAt *time.Time
	Mode       RunMode
	Actor      string
	Consent    bool
	Status     string
	Summary    string
}

// LastRuns returns the most recent runs, newest first, which is what the UI
// and the CLI show (FR-52).
func (db *DB) LastRuns(ctx context.Context, limit int) ([]RunSummary, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.R.QueryContext(ctx,
		`SELECT id, started_at, finished_at, mode, actor, consent_shrink, status, summary
		 FROM reconcile_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunSummary
	for rows.Next() {
		var run RunSummary
		var mode, startedAt string
		var finishedAt sql.NullString
		var consent int
		if err := rows.Scan(&run.ID, &startedAt, &finishedAt, &mode, &run.Actor, &consent, &run.Status, &run.Summary); err != nil {
			return nil, err
		}
		run.Mode = RunMode(mode)
		run.Consent = consent == 1
		if run.StartedAt, err = parseTime(startedAt); err != nil {
			return nil, err
		}
		if run.FinishedAt, err = parseTimePtr(finishedAt); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// PlanBody returns the stored plan for a run.
func (db *DB) PlanBody(ctx context.Context, runID int64) ([]byte, time.Time, error) {
	var body, computedAt string
	err := db.R.QueryRowContext(ctx,
		`SELECT body, computed_at FROM plans WHERE run_id = ? ORDER BY id DESC LIMIT 1`, runID).
		Scan(&body, &computedAt)
	if err != nil {
		return nil, time.Time{}, err
	}
	at, err := parseTime(computedAt)
	return []byte(body), at, err
}

// AuditEntry is one recorded change. The table arrives with the applier; this
// reads it so nuno explain can show the chain now and the history later.
type AuditEntry struct {
	At       time.Time
	Actor    string
	Provider string
	From     string
	To       string
	Result   string
}

// RecentAudit returns the last changes for one person. It answers empty rather
// than failing when the table does not exist yet, so explain works before the
// applier lands.
func (db *DB) RecentAudit(ctx context.Context, userID int64, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := db.R.QueryContext(ctx,
		`SELECT a.at, a.actor, coalesce(p.name, ''), a.from_value, a.to_value, a.result
		 FROM audit_log a LEFT JOIN providers p ON p.id = a.provider_id
		 WHERE a.user_id = ? ORDER BY a.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()

	var entries []AuditEntry
	for rows.Next() {
		var entry AuditEntry
		var at string
		if err := rows.Scan(&at, &entry.Actor, &entry.Provider, &entry.From, &entry.To, &entry.Result); err != nil {
			return nil, err
		}
		if entry.At, err = parseTime(at); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
