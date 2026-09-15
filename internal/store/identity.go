// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

type IdentityResult struct {
	Users    int
	Groups   int
	Orphaned int
	Restored int
}

// ReplaceIdentity applies a snapshot in one transaction. Identity is keyed on
// (source, source_uuid), so a renamed user or group keeps its links and
// overrides (FR-9a).
//
// A user missing from the snapshot is marked orphaned, never deleted: the
// audit history stays and reconcile skips them (FR-9). A group missing from
// the snapshot keeps its row, because a group-to-tier mapping must survive a
// directory hiccup; only the membership is recomputed.
func (db *DB) ReplaceIdentity(ctx context.Context, snap core.IdentitySnapshot) (IdentityResult, error) {
	if snap.Source == "" {
		return IdentityResult{}, fmt.Errorf("a snapshot needs a source")
	}
	var result IdentityResult
	now := formatTime(time.Now())

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		groupIDs := make(map[string]int64, len(snap.Groups))
		for _, g := range snap.Groups {
			if g.SourceUUID == "" {
				return fmt.Errorf("group %q has no source uuid, which is its identity", g.Name)
			}
			id, err := upsertGroup(ctx, tx, snap.Source, g, now)
			if err != nil {
				return err
			}
			groupIDs[g.SourceUUID] = id
			result.Groups++
		}

		seen := make([]string, 0, len(snap.Users))
		for _, u := range snap.Users {
			if u.SourceUUID == "" {
				return fmt.Errorf("user %q has no source uuid, which is its identity", u.UID)
			}
			id, restored, err := upsertUser(ctx, tx, snap.Source, u, now)
			if err != nil {
				return err
			}
			if restored {
				result.Restored++
			}
			if err := setUserGroups(ctx, tx, id, u.GroupUUIDs, groupIDs); err != nil {
				return err
			}
			seen = append(seen, u.SourceUUID)
			result.Users++
		}

		orphaned, err := markOrphaned(ctx, tx, snap.Source, seen, now)
		if err != nil {
			return err
		}
		result.Orphaned = orphaned
		return nil
	})
	return result, err
}

func upsertGroup(ctx context.Context, tx *sql.Tx, source core.IdentitySource, g core.Group, now string) (int64, error) {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO "groups" (source, source_uuid, name, display_name, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (source, source_uuid) DO UPDATE SET
		   name = excluded.name, display_name = excluded.display_name, updated_at = excluded.updated_at`,
		string(source), g.SourceUUID, g.Name, g.DisplayName, now, now)
	if err != nil {
		return 0, fmt.Errorf("upsert group %q: %w", g.Name, err)
	}
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM "groups" WHERE source = ? AND source_uuid = ?`,
		string(source), g.SourceUUID).Scan(&id)
	return id, err
}

func upsertUser(ctx context.Context, tx *sql.Tx, source core.IdentitySource, u core.User, now string) (id int64, restored bool, err error) {
	var previousStatus string
	err = tx.QueryRowContext(ctx, `SELECT id, status FROM users WHERE source = ? AND source_uuid = ?`,
		string(source), u.SourceUUID).Scan(&id, &previousStatus)
	switch {
	case err == nil:
		restored = previousStatus == string(core.UserOrphaned)
		_, err = tx.ExecContext(ctx,
			`UPDATE users SET uid = ?, email = ?, email_normalized = ?, display_name = ?, status = ?, updated_at = ?
			 WHERE id = ?`,
			u.UID, u.Email, core.NormalizeEmail(u.Email), u.DisplayName, string(core.UserActive), now, id)
		return id, restored, err
	case !isNoRows(err):
		return 0, false, err
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (source, source_uuid, uid, email, email_normalized, display_name, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(source), u.SourceUUID, u.UID, u.Email, core.NormalizeEmail(u.Email), u.DisplayName,
		string(core.UserActive), now, now)
	if err != nil {
		return 0, false, fmt.Errorf("insert user %q: %w", u.UID, err)
	}
	id, err = res.LastInsertId()
	return id, false, err
}

func setUserGroups(ctx context.Context, tx *sql.Tx, userID int64, groupUUIDs []string, groupIDs map[string]int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_groups WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, uuid := range groupUUIDs {
		groupID, ok := groupIDs[uuid]
		if !ok {
			// A membership naming a group the same read did not return means
			// the directory answered inconsistently. Skipping it silently
			// would drop someone's tier in M2.
			return fmt.Errorf("user %d belongs to group %q, which the directory did not return", userID, uuid)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_groups (user_id, group_id) VALUES (?, ?)
			 ON CONFLICT (user_id, group_id) DO NOTHING`, userID, groupID); err != nil {
			return err
		}
	}
	return nil
}

func markOrphaned(ctx context.Context, tx *sql.Tx, source core.IdentitySource, seen []string, now string) (int, error) {
	query := `UPDATE users SET status = ?, updated_at = ? WHERE source = ? AND status = ?`
	args := []any{string(core.UserOrphaned), now, string(source), string(core.UserActive)}
	if len(seen) > 0 {
		query += ` AND source_uuid NOT IN (?` + strings.Repeat(`, ?`, len(seen)-1) + `)`
		for _, uuid := range seen {
			args = append(args, uuid)
		}
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	return int(affected), err
}

// ListUsers returns everyone, orphans included, ordered by source uuid so the
// API's ordering is stable (FR-47).
func (db *DB) ListUsers(ctx context.Context) ([]core.User, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT id, source, source_uuid, uid, email, display_name, status FROM users ORDER BY source_uuid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []core.User
	byID := map[int64]int{}
	for rows.Next() {
		var u core.User
		var source, status string
		if err := rows.Scan(&u.ID, &source, &u.SourceUUID, &u.UID, &u.Email, &u.DisplayName, &status); err != nil {
			return nil, err
		}
		u.Source = core.IdentitySource(source)
		u.Status = core.UserStatus(status)
		byID[u.ID] = len(users)
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	memberships, err := db.R.QueryContext(ctx,
		`SELECT ug.user_id, g.source_uuid FROM user_groups ug JOIN "groups" g ON g.id = ug.group_id
		 ORDER BY ug.user_id, g.source_uuid`)
	if err != nil {
		return nil, err
	}
	defer memberships.Close()
	for memberships.Next() {
		var userID int64
		var groupUUID string
		if err := memberships.Scan(&userID, &groupUUID); err != nil {
			return nil, err
		}
		if index, ok := byID[userID]; ok {
			users[index].GroupUUIDs = append(users[index].GroupUUIDs, groupUUID)
		}
	}
	return users, memberships.Err()
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// AddManualUser inserts a person who exists in no directory, for services not
// behind SSO (FR-2). A manual user is never orphaned by a directory sync,
// because orphaning is scoped to the source that was read.
//
// The uuid is generated rather than derived from the uid, so renaming a manual
// user loses nothing either.
func (db *DB) AddManualUser(ctx context.Context, u core.User) (core.User, error) {
	if strings.TrimSpace(u.UID) == "" {
		return core.User{}, errors.New("a user needs a uid")
	}
	u.Source = core.SourceManual
	u.Status = core.UserActive
	if u.SourceUUID == "" {
		uuid, err := randomUUID()
		if err != nil {
			return core.User{}, err
		}
		u.SourceUUID = uuid
	}
	if u.DisplayName == "" {
		u.DisplayName = u.UID
	}

	now := formatTime(time.Now())
	var id int64
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx,
			`SELECT source FROM users WHERE uid = ? COLLATE NOCASE`, u.UID).Scan(&existing)
		switch {
		case err == nil:
			return fmt.Errorf("a %s user with uid %q already exists", existing, u.UID)
		case !isNoRows(err):
			return err
		}
		newID, _, err := upsertUser(ctx, tx, core.SourceManual, u, now)
		id = newID
		return err
	})
	if err != nil {
		return core.User{}, err
	}
	u.ID = id
	return u, nil
}

// randomUUID is a version 4 UUID. crypto/rand plus formatting beats a
// dependency for one call site.
func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// DeleteUser removes a person from Nuno. It never touches provider data: an
// account simply becomes unmanaged again (FR-8).
func (db *DB) DeleteUser(ctx context.Context, id int64) (bool, error) {
	res, err := db.W.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

// ListGroups returns the directory groups, which is what a tier mapping picks
// from.
func (db *DB) ListGroups(ctx context.Context) ([]core.Group, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT id, source, source_uuid, name, display_name FROM "groups" ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []core.Group
	for rows.Next() {
		var g core.Group
		var source string
		if err := rows.Scan(&g.ID, &source, &g.SourceUUID, &g.Name, &g.DisplayName); err != nil {
			return nil, err
		}
		g.Source = core.IdentitySource(source)
		groups = append(groups, g)
	}
	return groups, rows.Err()
}
