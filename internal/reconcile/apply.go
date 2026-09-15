// SPDX-License-Identifier: AGPL-3.0-or-later

package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/notify"
	"github.com/marcodellemarche/nuno/internal/store"
)

// DefaultPlanWindow is how old a plan may be when it is applied. Beyond it the
// observed state it was computed from is no longer evidence of anything
// (ADR-0016).
const DefaultPlanWindow = 5 * time.Minute

// writeSpacing keeps Nuno from hammering a provider even when it is well
// inside the rate limit. It is small enough not to matter for a homelab and
// large enough to be polite.
const writeSpacing = 200 * time.Millisecond

// ApplyOptions is the whole of an apply decision, in one place, so consent
// cannot be implied by a call site.
type ApplyOptions struct {
	// ConsentShrink allows shrink-below-usage changes. A scheduled run must
	// never set it (FR-39).
	ConsentShrink bool
	DryRun        bool
	Actor         string
	// PlanWindow overrides DefaultPlanWindow.
	PlanWindow time.Duration
	// ComputedAt is when the plan was computed. A plan older than the window
	// is refused outright.
	ComputedAt time.Time
}

// ApplyReport is honest about partial success, because there is no rollback
// for a quota already written: the run stops at the first error for that
// provider, earlier writes stand and are audited, and re-running converges
// (FR-34, ADR-0016).
type ApplyReport struct {
	RunID   int64
	Applied int
	Failed  int
	// Guarded is the count refused by the shrink guardrail without consent.
	Guarded int
	// Unknown is the count refused because the state could not be trusted.
	Unknown int
	// Moved is the count refused because the classification changed between
	// plan and apply (FR-38b).
	Moved int
	// Skipped covers providers that would not be written to at all, for
	// example a degraded one.
	Skipped   int
	Throttled bool
	DryRun    bool
	Errors    []error
}

// Clean reports whether everything the plan wanted actually happened.
func (r ApplyReport) Clean() bool {
	return r.Failed == 0 && r.Guarded == 0 && r.Unknown == 0 && r.Moved == 0 &&
		r.Skipped == 0 && !r.Throttled && len(r.Errors) == 0
}

// Incomplete reports whether work was left undone without anything failing,
// which is exit code 2 rather than 1 (FR-72).
func (r ApplyReport) Incomplete() bool {
	return r.Guarded > 0 || r.Unknown > 0 || r.Moved > 0 || r.Skipped > 0 || r.Throttled
}

func (r ApplyReport) Summary() string {
	return fmt.Sprintf("applied %d, failed %d, guarded %d, unknown state %d, moved %d, skipped %d, throttled %v",
		r.Applied, r.Failed, r.Guarded, r.Unknown, r.Moved, r.Skipped, r.Throttled)
}

// Apply writes a plan.
//
// Every change is re-observed and re-classified first, because the class was
// computed at plan time and consent is given at apply time with a human in
// between: a phone finishing a backup in those forty seconds turns a safe
// change into an unguarded shrink below usage, which is precisely what the
// guardrail exists to prevent (FR-38b, ADR-0016).
func (e *Engine) Apply(ctx context.Context, instances []Instance, plan core.Plan, opts ApplyOptions) (ApplyReport, error) {
	report := ApplyReport{DryRun: opts.DryRun}

	window := opts.PlanWindow
	if window <= 0 {
		window = DefaultPlanWindow
	}
	if !opts.ComputedAt.IsZero() && e.now().Sub(opts.ComputedAt) > window {
		return report, fmt.Errorf("this plan was computed %s ago, which is past the %s window: compute a new one",
			e.now().Sub(opts.ComputedAt).Round(time.Second), window)
	}

	mode := store.RunApply
	if opts.DryRun {
		mode = store.RunDryRun
	}
	runID, err := e.db.StartRun(ctx, mode, opts.Actor, opts.ConsentShrink)
	if err != nil {
		return report, err
	}
	report.RunID = runID

	byProvider := map[int64]Instance{}
	for _, instance := range instances {
		byProvider[instance.Row.ID] = instance
	}

	// Counting the classes the plan refused up front keeps the report honest
	// even when nothing is applied.
	for _, change := range plan.Changes {
		switch change.Class {
		case core.ClassUnknownState:
			report.Unknown++
			e.record(ctx, runID, opts.Actor, change, store.AuditRefused, change.Detail)
		case core.ClassShrinkBelowUsage:
			if !opts.ConsentShrink {
				report.Guarded++
				e.record(ctx, runID, opts.Actor, change, store.AuditRefused,
					"refused by the shrink guardrail: no consent was given")
			}
		}
	}

	pacers := map[int64]*pacer{}
	stopped := map[int64]bool{}

	for _, change := range plan.Applicable(opts.ConsentShrink) {
		instance, known := byProvider[change.ProviderID]
		if !known {
			report.Skipped++
			continue
		}
		if stopped[change.ProviderID] {
			// The run stopped for this provider at the first error. The rest
			// of its changes are not attempted, and that is reported.
			report.Skipped++
			e.record(ctx, runID, opts.Actor, change, store.AuditSkipped,
				"not attempted: this provider stopped at an earlier error")
			continue
		}

		// A provider with a competing writer is read but never written,
		// because a value Nuno sets there does not survive (FR-75).
		if instance.Row.Degraded() {
			report.Skipped++
			e.record(ctx, runID, opts.Actor, change, store.AuditSkipped,
				"refused: "+instance.Row.DegradedReason)
			continue
		}

		fresh, class, err := e.recheck(ctx, instance, change)
		if err != nil {
			report.Failed++
			report.Errors = append(report.Errors, err)
			stopped[change.ProviderID] = true
			e.record(ctx, runID, opts.Actor, change, store.AuditFailed, err.Error())
			continue
		}
		if class != change.Class {
			// Refuse anything whose classification moved, in either
			// direction: the plan that was agreed to is no longer the plan.
			report.Moved++
			e.record(ctx, runID, opts.Actor, change, store.AuditRefused,
				fmt.Sprintf("classification moved from %s to %s between plan and apply", change.Class, class))
			continue
		}
		if fresh.Quota.Equal(change.To) {
			// Somebody else already set it, or the plan was stale in a
			// harmless way. Nothing to do, and nothing to report as done.
			continue
		}

		if opts.DryRun {
			report.Applied++
			continue
		}

		pace, ok := pacers[change.ProviderID]
		if !ok {
			pace = newPacer(instance.Provider)
			pacers[change.ProviderID] = pace
		}
		if !pace.allow() {
			// The provider's own limit, reached. A throttled run stops
			// cleanly and resumes next cycle (FR-38c).
			report.Throttled = true
			stopped[change.ProviderID] = true
			e.record(ctx, runID, opts.Actor, change, store.AuditSkipped,
				"not attempted: this provider's write limit for the window was reached")
			continue
		}
		if err := sleepCtx(ctx, writeSpacing); err != nil {
			return e.finish(ctx, report, err)
		}

		if err := instance.Provider.SetQuota(ctx, change.ExternalID, change.To); err != nil {
			if _, throttled := core.Throttled(err); throttled {
				report.Throttled = true
				stopped[change.ProviderID] = true
				e.record(ctx, runID, opts.Actor, change, store.AuditSkipped, "throttled by the provider")
				continue
			}
			report.Failed++
			report.Errors = append(report.Errors, err)
			stopped[change.ProviderID] = true
			e.record(ctx, runID, opts.Actor, change, store.AuditFailed, err.Error())
			continue
		}

		report.Applied++
		e.record(ctx, runID, opts.Actor, change, store.AuditApplied, "")
		e.log.Info("quota applied",
			"user", change.UserUID, "provider", change.Provider,
			"from", change.From.String(), "to", change.To.String(), "run", runID)
	}

	e.notifyOutcome(ctx, plan, report, opts)
	return e.finish(ctx, report, nil)
}

// notifyOutcome sends what an operator needs to know: a failure, and a change
// a guardrail refused.
//
// A guarded change reappears every cycle by design, so notification is keyed
// on (user, provider, from, to) and fires on first appearance and on change,
// never once per cycle (FR-63).
func (e *Engine) notifyOutcome(ctx context.Context, plan core.Plan, report ApplyReport, opts ApplyOptions) {
	if opts.DryRun || !e.notifier.Configured() {
		return
	}

	if report.Failed > 0 || len(report.Errors) > 0 {
		details := make([]notify.Detail, 0, len(report.Errors))
		for _, err := range report.Errors {
			details = append(details, notify.Detail{Reason: err.Error()})
		}
		event := notify.Event{
			Kind:    notify.KindFailed,
			At:      e.now().UTC(),
			Summary: fmt.Sprintf("reconcile failed: %s", report.Summary()),
			Details: details,
		}
		if err := e.notifier.Notify(ctx, event); err != nil {
			e.log.Error("send the failure notification", "error", err)
		}
	}

	var fresh []notify.Detail
	var keys []string
	for _, change := range plan.Changes {
		if change.Class == core.ClassSafe {
			continue
		}
		if change.Class == core.ClassShrinkBelowUsage && opts.ConsentShrink {
			continue
		}
		key := fmt.Sprintf("%s/%s/%s/%s", change.UserUID, change.Provider, change.From.Encode(), change.To.Encode())
		keys = append(keys, key)

		first, err := e.db.SeenGuardedChange(ctx, key)
		if err != nil {
			e.log.Error("track a guarded change", "error", err)
			continue
		}
		if !first {
			continue
		}
		fresh = append(fresh, notify.Detail{
			User: change.UserUID, Provider: change.Provider,
			From: change.From.String(), To: change.To.String(),
			Reason: string(change.Class) + ": " + change.Detail,
		})
	}
	// A change that was fixed and comes back later must notify again.
	if err := e.db.ForgetGuardedChanges(ctx, keys); err != nil {
		e.log.Error("prune guarded change keys", "error", err)
	}

	if len(fresh) > 0 {
		event := notify.Event{
			Kind:    notify.KindGuarded,
			At:      e.now().UTC(),
			Summary: fmt.Sprintf("%d change(s) were refused by a guardrail and need a decision", len(fresh)),
			Details: fresh,
		}
		if err := e.notifier.Notify(ctx, event); err != nil {
			e.log.Error("send the guarded notification", "error", err)
		}
	}
}

func (e *Engine) finish(ctx context.Context, report ApplyReport, cause error) (ApplyReport, error) {
	status := "clean"
	switch {
	case report.Failed > 0 || len(report.Errors) > 0:
		status = "failed"
	case report.Incomplete():
		status = "incomplete"
	}
	if report.DryRun {
		status = "dry-run"
	}
	if err := e.db.FinishRun(ctx, report.RunID, status, report.Summary()); err != nil {
		e.log.Error("close the run", "run", report.RunID, "error", err)
	}
	return report, cause
}

// recheck re-reads one account and classifies the change against what is
// there now.
func (e *Engine) recheck(ctx context.Context, instance Instance, change core.Change) (core.Account, core.ChangeClass, error) {
	account, err := instance.Provider.GetAccount(ctx, change.ExternalID)
	if err != nil {
		return core.Account{}, "", fmt.Errorf("re-read %s on %s: %w", change.ExternalID, change.Provider, err)
	}
	if !account.Writable() {
		return account, "", fmt.Errorf("%s on %s is disabled or deleted now", change.ExternalID, change.Provider)
	}

	plan := core.BuildPlan([]core.PlanAccount{{
		User:       core.User{ID: change.UserID, UID: change.UserUID},
		ProviderID: change.ProviderID,
		Provider:   change.Provider,
		ExternalID: change.ExternalID,
		Desired:    change.To,
		Present:    true,
		Observed:   account.Quota,
		Used:       account.Used,
		NeverUsed:  account.NeverUsed,
		ObserveOK:  true,
	}})
	if len(plan.Changes) == 0 {
		// Nothing to do any more, which is the same as a safe no-op.
		return account, change.Class, nil
	}
	return account, plan.Changes[0].Class, nil
}

func (e *Engine) record(ctx context.Context, runID int64, actor string, change core.Change, result store.AuditResult, detail string) {
	if err := e.db.RecordChange(ctx, runID, actor, change, result, detail); err != nil {
		e.log.Error("record an audit entry", "error", err,
			"user", change.UserUID, "provider", change.Provider, "result", result)
	}
}

// pacer counts writes against a provider's published limit. A provider that
// publishes none has no ceiling to stop at.
type pacer struct {
	limit     int
	remaining int
	unlimited bool
}

func newPacer(provider core.Provider) *pacer {
	limited, ok := provider.(core.RateLimited)
	if !ok {
		return &pacer{unlimited: true}
	}
	calls, _ := limited.WriteLimit()
	if calls <= 0 {
		return &pacer{unlimited: true}
	}
	// Leave headroom: the probe and anything else sharing the credential draw
	// on the same budget.
	reserved := calls / 10
	return &pacer{limit: calls, remaining: calls - reserved}
}

func (p *pacer) allow() bool {
	if p.unlimited {
		return true
	}
	if p.remaining <= 0 {
		return false
	}
	p.remaining--
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
