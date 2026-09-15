// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/reconcile"
	"github.com/marcodellemarche/nuno/internal/store"
)

// serverActor performs the mutations the UI offers. It holds the run lock, so
// two clicks cannot start two reconciles against one state: the process-level
// lock keeps other processes out, and this keeps this process honest.
type serverActor struct {
	app    *app
	engine *reconcile.Engine
	mu     sync.Mutex
}

func newActor(a *app) *serverActor {
	return &serverActor{
		app:    a,
		engine: reconcile.New(a.db, a.log).WithNotifier(a.notifier, a.cfg.PublicURL),
	}
}

var errRunInProgress = errors.New("a run is already in progress: wait for it to finish")

func (s *serverActor) Reconcile(ctx context.Context, consentShrink bool, filter reconcile.PlanFilter) (reconcile.ApplyReport, error) {
	if !s.mu.TryLock() {
		return reconcile.ApplyReport{}, errRunInProgress
	}
	defer s.mu.Unlock()

	if _, err := s.engine.SyncIdentity(ctx, s.app.directory); err != nil {
		s.app.log.Error("identity sync failed, continuing with what is known", "error", err)
	}
	observation, err := s.engine.Observe(ctx, s.app.instances)
	if err != nil {
		return reconcile.ApplyReport{}, err
	}

	computedAt := time.Now()
	result, err := s.engine.Plan(ctx, s.app.instances, filter)
	if err != nil {
		return reconcile.ApplyReport{}, err
	}
	if result.Plan.Empty() {
		if !observation.OK() {
			return reconcile.ApplyReport{Skipped: len(observation.Failed())},
				fmt.Errorf("%d provider(s) could not be read, so nothing was planned for them", len(observation.Failed()))
		}
		return reconcile.ApplyReport{}, nil
	}

	return s.engine.Apply(ctx, s.app.instances, result.Plan, reconcile.ApplyOptions{
		ConsentShrink: consentShrink,
		Actor:         "ui",
		ComputedAt:    computedAt,
	})
}

func (s *serverActor) Observe(ctx context.Context) error {
	if !s.mu.TryLock() {
		return errRunInProgress
	}
	defer s.mu.Unlock()

	if _, err := s.engine.SyncIdentity(ctx, s.app.directory); err != nil {
		s.app.log.Error("identity sync failed", "error", err)
	}
	report, err := s.engine.Observe(ctx, s.app.instances)
	if err != nil {
		return err
	}
	if !report.OK() {
		var names []string
		for _, failed := range report.Failed() {
			names = append(names, failed.Name)
		}
		return fmt.Errorf("read what it could, but these failed: %s", strings.Join(names, ", "))
	}
	return nil
}

func (s *serverActor) SaveTier(ctx context.Context, tier core.Tier) error {
	_, err := s.app.db.SaveTier(ctx, tier)
	return err
}

func (s *serverActor) SetOverride(ctx context.Context, uid, provider string, quota core.Quota, clear bool) error {
	user, row, err := s.resolve(ctx, uid, provider)
	if err != nil {
		return err
	}
	if clear {
		cleared, err := s.app.db.ClearProviderOverride(ctx, user.ID, row.ID)
		if err != nil {
			return err
		}
		if !cleared {
			return fmt.Errorf("%s had no override on %s", uid, provider)
		}
		return nil
	}
	return s.app.db.SetProviderOverride(ctx, user.ID, row.ID, quota, store.OverrideManual)
}

func (s *serverActor) Link(ctx context.Context, uid, provider, externalID string, unlink bool) error {
	user, row, err := s.resolve(ctx, uid, provider)
	if err != nil {
		return err
	}
	if unlink {
		removed, err := s.app.db.DeleteLink(ctx, user.ID, row.ID)
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("%s had no link on %s", uid, provider)
		}
		return nil
	}

	// Nuno never writes to an account it has not observed, so a link to one
	// it has never seen would be a link that can never be used.
	accounts, err := s.app.db.ListExternalAccounts(ctx, row.ID)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if account.ExternalID != externalID {
			continue
		}
		if !account.Writable() {
			return fmt.Errorf("account %q on %s is disabled or deleted, so it is never linked", externalID, provider)
		}
		if err := s.app.db.UpsertLink(ctx, core.AccountLink{
			UserID: user.ID, ProviderID: row.ID, ExternalID: externalID, Origin: core.LinkManual,
		}); err != nil {
			return err
		}
		return s.app.db.SetAccountOwner(ctx, row.ID, externalID, &user.ID)
	}
	return fmt.Errorf("%s has no observed account %q: read the provider again first", provider, externalID)
}

func (s *serverActor) IssueKey(ctx context.Context, label string) (core.Secret, error) {
	return s.app.db.IssueAdminKey(ctx, label)
}

func (s *serverActor) RevokeKey(ctx context.Context, id int64) error {
	return s.app.db.RevokeAdminKey(ctx, id)
}

func (s *serverActor) resolve(ctx context.Context, uid, provider string) (core.User, store.ProviderRow, error) {
	users, err := s.app.db.ListUsers(ctx)
	if err != nil {
		return core.User{}, store.ProviderRow{}, err
	}
	var user core.User
	var found bool
	for _, candidate := range users {
		if strings.EqualFold(candidate.UID, uid) {
			user, found = candidate, true
		}
	}
	if !found {
		return core.User{}, store.ProviderRow{}, fmt.Errorf("no user %q", uid)
	}

	row, err := s.app.db.GetProviderByName(ctx, provider)
	if err != nil {
		return core.User{}, store.ProviderRow{}, fmt.Errorf("no provider %q", provider)
	}
	return user, row, nil
}
