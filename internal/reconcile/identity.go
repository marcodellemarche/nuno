// SPDX-License-Identifier: AGPL-3.0-or-later

package reconcile

import (
	"context"
	"fmt"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

// SyncIdentity reads the directory and applies it as one snapshot.
//
// A directory that answers with nothing, where Nuno already knows people, is
// refused rather than applied: a wrong filter or a base DN typo would
// otherwise orphan everyone at once, and every one of them would be excluded
// from reconcile until an admin noticed. Failing loud costs one cycle;
// applying it costs the whole policy.
func (e *Engine) SyncIdentity(ctx context.Context, directory core.Directory) (store.IdentityResult, error) {
	if directory == nil {
		e.log.Info("no identity source is configured, so only manual users exist")
		return store.IdentityResult{}, nil
	}

	read, err := directory.Sync(ctx)
	if err != nil {
		return store.IdentityResult{}, fmt.Errorf("read the identity source: %w", err)
	}

	for _, dn := range read.SkippedEntries {
		e.log.Warn("directory entry has no stable uuid and was skipped", "dn", dn)
	}
	for _, dn := range read.UnresolvedMemberships {
		e.log.Warn("membership names a group the directory did not return, so it was dropped", "group_dn", dn)
	}

	if len(read.Snapshot.Users) == 0 {
		known, err := e.countActiveUsers(ctx)
		if err != nil {
			return store.IdentityResult{}, err
		}
		if known > 0 {
			return store.IdentityResult{}, fmt.Errorf(
				"the identity source returned no users while Nuno knows %d: refusing to orphan everyone, check NUNO_LDAP_USER_FILTER and NUNO_LDAP_BASE_DN", known)
		}
	}

	result, err := e.db.ReplaceIdentity(ctx, read.Snapshot)
	if err != nil {
		return result, fmt.Errorf("store the identity snapshot: %w", err)
	}
	e.log.Info("identity synced",
		"users", result.Users, "groups", result.Groups,
		"orphaned", result.Orphaned, "restored", result.Restored)
	return result, nil
}

func (e *Engine) countActiveUsers(ctx context.Context) (int, error) {
	users, err := e.db.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	active := 0
	for _, u := range users {
		if u.Status == core.UserActive {
			active++
		}
	}
	return active, nil
}
