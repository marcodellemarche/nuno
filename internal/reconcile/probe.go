// SPDX-License-Identifier: AGPL-3.0-or-later

package reconcile

import (
	"context"
	"errors"
	"fmt"

	"github.com/marcodellemarche/nuno/internal/core"
)

// ProbeResult is what the write probe learned, and why.
type ProbeResult struct {
	Provider string
	Access   core.WriteAccess
	// Account is the one that was probed, empty when nothing was eligible.
	Account string
	Detail  string
	Err     error
}

// ProbeWrite proves whether the credential can actually write, by writing a
// value back unchanged and reading it again.
//
// It runs here rather than in Health() for three reasons (ADR-0025): Health
// runs before linking and writing to an unlinked account is forbidden (FR-7),
// a probe spends the same rate limit as the applier, and rewriting a value
// that is not already a fixed point of NormalizeQuota would change somebody's
// quota during what is meant to be a read.
//
// With no eligible account the answer is Unproven, which is not the same as No.
func (e *Engine) ProbeWrite(ctx context.Context, instance Instance) ProbeResult {
	result := ProbeResult{Provider: instance.Row.Name, Access: core.WriteUnproven}

	if !instance.Provider.Capabilities().CanSetUserQuota {
		result.Detail = "this provider cannot set a user quota at all"
		return result
	}

	candidate, detail, err := e.probeTarget(ctx, instance)
	if err != nil {
		result.Err = err
		return result
	}
	if candidate == nil {
		result.Detail = detail
		return result
	}
	result.Account = candidate.ExternalID

	before := candidate.Quota
	if err := instance.Provider.SetQuota(ctx, candidate.ExternalID, before); err != nil {
		if core.IsAuth(err) {
			// This is the answer the probe exists for: on Nextcloud an app
			// password gets 403 here every time, by design.
			result.Access = core.WriteNo
			result.Detail = err.Error()
			e.persistProbe(ctx, instance, result)
			return result
		}
		if _, throttled := core.Throttled(err); throttled {
			result.Detail = "throttled before the probe could finish, which is not a failure: it resumes next cycle"
			result.Err = err
			return result
		}
		result.Err = err
		return result
	}

	after, err := instance.Provider.GetAccount(ctx, candidate.ExternalID)
	if err != nil {
		result.Err = fmt.Errorf("the write was accepted but the account could not be read back: %w", err)
		return result
	}
	if !after.Quota.Equal(before) {
		// The instance stored something else. Writing more would be writing
		// blind.
		result.Access = core.WriteNo
		result.Detail = fmt.Sprintf("wrote %s and read back %s: the instance rewrites what it is given beyond what NormalizeQuota accounts for",
			before, after.Quota)
		e.persistProbe(ctx, instance, result)
		return result
	}

	result.Access = core.WriteYes
	result.Detail = fmt.Sprintf("wrote %s back to %s unchanged", before, candidate.ExternalID)
	e.persistProbe(ctx, instance, result)
	return result
}

// probeTarget picks the account to probe: linked, writable, observed in this
// cycle, and already a fixed point of normalization so the write cannot change
// it. The choice is deterministic, so two runs probe the same account.
func (e *Engine) probeTarget(ctx context.Context, instance Instance) (*core.ExternalAccount, string, error) {
	if instance.Row.ID == 0 {
		return nil, "", errors.New("this provider is not registered in the database, so it has no links to probe against")
	}
	accounts, err := e.db.ListExternalAccounts(ctx, instance.Row.ID)
	if err != nil {
		return nil, "", err
	}

	var linked, unnormalized int
	for i := range accounts {
		account := accounts[i]
		if account.UserID == nil || !account.Writable() {
			continue
		}
		linked++
		if !account.Quota.IsKnown() {
			continue
		}
		if !instance.Provider.NormalizeQuota(account.Quota).Equal(account.Quota) {
			// Rewriting this would change the value, which a probe must never
			// do. It is worth knowing about separately: something set it
			// outside the API.
			unnormalized++
			continue
		}
		return &account, "", nil
	}

	switch {
	case linked == 0:
		return nil, "no linked account to probe against: run `nuno observe`, or link one with `nuno link <uid> " +
			instance.Row.Name + " <external-id>`", nil
	case unnormalized == linked:
		return nil, fmt.Sprintf("all %d linked accounts hold a quota this provider would rewrite, so probing one would change it: adopt the values first", linked), nil
	}
	return nil, fmt.Sprintf("none of the %d linked accounts has a readable quota to write back", linked), nil
}

func (e *Engine) persistProbe(ctx context.Context, instance Instance, result ProbeResult) {
	if err := e.db.SaveWriteAccess(ctx, instance.Row.ID, result.Access, e.now().UTC()); err != nil {
		e.log.Error("store write access", "provider", instance.Row.Name, "error", err)
	}
}

// ProbeAll probes every provider, one after another. Probes are writes, so
// they are not run concurrently.
func (e *Engine) ProbeAll(ctx context.Context, instances []Instance) []ProbeResult {
	results := make([]ProbeResult, 0, len(instances))
	for _, instance := range instances {
		results = append(results, e.ProbeWrite(ctx, instance))
	}
	return results
}

// CheckCompetingWriters asks each provider what it can see, and records the
// reason a provider is degraded. An adapter that cannot answer is not assumed
// clean (FR-75, ADR-0027).
func (e *Engine) CheckCompetingWriters(ctx context.Context, instance Instance) ([]core.CompetingWriter, []core.CompetingWriter, error) {
	detector, ok := instance.Provider.(core.WriterDetector)
	if !ok {
		return nil, nil, nil
	}

	found, err := detector.CompetingWriters(ctx)
	if err != nil {
		return nil, detector.UndetectableWriters(), err
	}

	reason := ""
	if len(found) > 0 {
		reason = found[0].Setting + " is set: " + found[0].Detail
	}
	if instance.Row.ID != 0 && instance.Row.DegradedReason != reason {
		if err := e.db.SetDegraded(ctx, instance.Row.ID, reason); err != nil {
			e.log.Error("record the degraded reason", "provider", instance.Row.Name, "error", err)
		}
	}
	return found, detector.UndetectableWriters(), nil
}
