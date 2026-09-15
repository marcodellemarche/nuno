// SPDX-License-Identifier: AGPL-3.0-or-later

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

// applyHarness sets up one person linked on one provider, with a tier that
// wants a given ceiling.
func applyHarness(t *testing.T, provider core.Provider, want core.Quota) (*harness, []Instance) {
	t.Helper()
	h := newHarness(t)
	h.identity(core.User{SourceUUID: "u-alice", UID: "alice", Email: "alice@example.org"})
	instances := []Instance{h.instance("cloud", "nextcloud", core.MatchEmail, provider)}
	if _, err := h.engine.Observe(context.Background(), instances); err != nil {
		t.Fatal(err)
	}

	allocation := core.Allocation{ProviderID: instances[0].Row.ID}
	switch want.Kind {
	case core.KindUnlimited:
		allocation.Mode = core.ModeUnlimited
	default:
		allocation.Mode = core.ModeAbsolute
		allocation.Value = want.Bytes
	}
	if _, err := h.db.SaveTier(context.Background(), core.Tier{
		Name: "standard", Budget: want, IsDefault: true,
		Allocations: []core.Allocation{allocation},
	}); err != nil {
		t.Fatal(err)
	}
	return h, instances
}

func planNow(t *testing.T, h *harness, instances []Instance) core.Plan {
	t.Helper()
	result, err := h.engine.Plan(context.Background(), instances, PlanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return result.Plan
}

// Plan, apply, plan again yields an empty second plan. The first of the four
// properties ARCHITECTURE section 10 requires.
func TestApplyThenPlanAgainIsEmpty(t *testing.T) {
	ctx := context.Background()
	provider := &fake{typ: "nextcloud", accounts: []core.Account{account("nc-alice", "alice@example.org")}}
	h, instances := applyHarness(t, provider, core.MustBytes(50<<30))

	plan := planNow(t, h, instances)
	if len(plan.Changes) != 1 {
		t.Fatalf("plan = %+v", plan.Changes)
	}

	report, err := h.engine.Apply(ctx, instances, plan, ApplyOptions{Actor: "test", ComputedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != 1 || !report.Clean() {
		t.Fatalf("report = %+v, errors %v", report, report.Errors)
	}
	if got := provider.writes["nc-alice"]; !got.Equal(core.MustBytes(50 << 30)) {
		t.Errorf("wrote %v", got)
	}

	// The provider now reports what was written, which is what makes the
	// second plan empty.
	provider.accounts[0].Quota = core.MustBytes(50 << 30)
	if _, err := h.engine.Observe(ctx, instances); err != nil {
		t.Fatal(err)
	}
	if again := planNow(t, h, instances); !again.Empty() {
		t.Fatalf("the second plan is not empty: %+v", again.Changes)
	}

	// And the write is in the audit log, tied to its run.
	entries, err := h.db.RecentChanges(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit = %+v", entries)
	}
	entry := entries[0]
	if entry.Result != store.AuditApplied || entry.User != "alice" || entry.Provider != "cloud" {
		t.Errorf("entry = %+v", entry)
	}
	if !entry.To.Equal(core.MustBytes(50<<30)) || entry.RunID == nil || *entry.RunID != report.RunID {
		t.Errorf("entry = %+v, want it tied to run %d", entry, report.RunID)
	}
}

// A shrink at or below current usage is never applied without consent.
func TestApplyRefusesAShrinkWithoutConsent(t *testing.T) {
	ctx := context.Background()
	used := account("nc-alice", "alice@example.org")
	used.Quota = core.MustBytes(50 << 30)
	used.Used = core.MustBytes(10 << 30)
	provider := &fake{typ: "nextcloud", accounts: []core.Account{used}}
	h, instances := applyHarness(t, provider, core.MustBytes(5<<30))

	plan := planNow(t, h, instances)
	if plan.Changes[0].Class != core.ClassShrinkBelowUsage {
		t.Fatalf("class = %q", plan.Changes[0].Class)
	}

	report, err := h.engine.Apply(ctx, instances, plan, ApplyOptions{Actor: "test", ComputedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != 0 || report.Guarded != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Clean() {
		t.Error("a run that refused something is not clean")
	}
	if !report.Incomplete() {
		t.Error("it is incomplete, which is exit code 2")
	}
	if len(provider.writes) != 0 {
		t.Errorf("wrote %v", provider.writes)
	}

	// The refusal is audited: a guardrail that skipped silently would be the
	// one thing AGENTS forbids.
	entries, _ := h.db.RecentChanges(ctx, 10)
	if len(entries) != 1 || entries[0].Result != store.AuditRefused {
		t.Fatalf("audit = %+v", entries)
	}

	// With consent it goes through.
	report, err = h.engine.Apply(ctx, instances, plan, ApplyOptions{
		Actor: "test", ComputedAt: time.Now(), ConsentShrink: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != 1 {
		t.Fatalf("with consent: %+v", report)
	}
}

// A change against unknown usage is never applied, with or without consent.
func TestApplyNeverWritesAgainstUnknownState(t *testing.T) {
	ctx := context.Background()
	blind := account("nc-alice", "alice@example.org")
	blind.Quota = core.MustBytes(50 << 30)
	blind.Used = core.Unknown()
	provider := &fake{typ: "nextcloud", accounts: []core.Account{blind}}
	h, instances := applyHarness(t, provider, core.MustBytes(100<<30))

	plan := planNow(t, h, instances)
	if plan.Changes[0].Class != core.ClassUnknownState {
		t.Fatalf("class = %q", plan.Changes[0].Class)
	}

	for _, consent := range []bool{false, true} {
		report, err := h.engine.Apply(ctx, instances, plan, ApplyOptions{
			Actor: "test", ComputedAt: time.Now(), ConsentShrink: consent,
		})
		if err != nil {
			t.Fatal(err)
		}
		if report.Applied != 0 || report.Unknown != 1 {
			t.Fatalf("consent=%v: report = %+v", consent, report)
		}
		if len(provider.writes) != 0 {
			t.Fatalf("consent=%v: wrote %v", consent, provider.writes)
		}
	}
}

// The class is computed at plan time and consent is given at apply time, with
// a human in between (FR-38b).
func TestApplyRefusesAChangeWhoseClassificationMoved(t *testing.T) {
	ctx := context.Background()
	a := account("nc-alice", "alice@example.org")
	a.Quota = core.MustBytes(50 << 30)
	a.Used = core.MustBytes(1 << 30)
	provider := &fake{typ: "nextcloud", accounts: []core.Account{a}}
	h, instances := applyHarness(t, provider, core.MustBytes(5<<30))

	plan := planNow(t, h, instances)
	if plan.Changes[0].Class != core.ClassSafe {
		t.Fatalf("the plan should be safe to begin with: %q", plan.Changes[0].Class)
	}

	// A phone finishes a backup between the plan and the apply.
	provider.accounts[0].Used = core.MustBytes(10 << 30)

	report, err := h.engine.Apply(ctx, instances, plan, ApplyOptions{Actor: "test", ComputedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != 0 || report.Moved != 1 {
		t.Fatalf("report = %+v, want the change refused because its class moved", report)
	}
	if len(provider.writes) != 0 {
		t.Errorf("wrote %v: that is the unguarded shrink the re-check exists to stop", provider.writes)
	}
}

func TestApplyRefusesAStalePlan(t *testing.T) {
	provider := &fake{typ: "nextcloud", accounts: []core.Account{account("nc-alice", "alice@example.org")}}
	h, instances := applyHarness(t, provider, core.MustBytes(50<<30))
	plan := planNow(t, h, instances)

	_, err := h.engine.Apply(context.Background(), instances, plan, ApplyOptions{
		Actor:      "test",
		ComputedAt: time.Now().Add(-time.Hour),
	})
	if err == nil {
		t.Fatal("a plan older than the window must be refused outright")
	}
	if len(provider.writes) != 0 {
		t.Errorf("wrote %v", provider.writes)
	}
}

// There is no rollback for a quota already written, so the honest behaviour is
// to stop for that provider, keep what landed, and report it (FR-34).
func TestApplyStopsAtTheFirstErrorForThatProviderAndKeepsWhatLanded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.identity(
		core.User{SourceUUID: "u-a", UID: "aaa", Email: "aaa@example.org"},
		core.User{SourceUUID: "u-b", UID: "bbb", Email: "bbb@example.org"},
		core.User{SourceUUID: "u-c", UID: "ccc", Email: "ccc@example.org"},
	)
	failing := &failOnSecondWrite{fake: fake{typ: "nextcloud", accounts: []core.Account{
		account("nc-a", "aaa@example.org"),
		account("nc-b", "bbb@example.org"),
		account("nc-c", "ccc@example.org"),
	}}}
	instances := []Instance{h.instance("cloud", "nextcloud", core.MatchEmail, failing)}
	if _, err := h.engine.Observe(ctx, instances); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SaveTier(ctx, core.Tier{
		Name: "standard", Budget: core.MustBytes(50 << 30), IsDefault: true,
		Allocations: []core.Allocation{{ProviderID: instances[0].Row.ID, Mode: core.ModeAbsolute, Value: 50 << 30}},
	}); err != nil {
		t.Fatal(err)
	}

	plan := planNow(t, h, instances)
	if len(plan.Changes) != 3 {
		t.Fatalf("plan = %+v", plan.Changes)
	}

	report, err := h.engine.Apply(ctx, instances, plan, ApplyOptions{Actor: "test", ComputedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != 1 || report.Failed != 1 || report.Skipped != 1 {
		t.Fatalf("report = %+v, want one applied, one failed, one not attempted", report)
	}
	if report.Clean() {
		t.Error("a failed run is not clean")
	}
	// The write that landed stands, and is audited.
	if len(failing.writes) != 1 {
		t.Errorf("writes = %v, want the first one kept", failing.writes)
	}
	entries, _ := h.db.RecentChanges(ctx, 10)
	var applied, failed, skipped int
	for _, entry := range entries {
		switch entry.Result {
		case store.AuditApplied:
			applied++
		case store.AuditFailed:
			failed++
		case store.AuditSkipped:
			skipped++
		}
	}
	if applied != 1 || failed != 1 || skipped != 1 {
		t.Errorf("audit = %+v", entries)
	}
}

// A degraded provider is read but never written, because a value Nuno sets
// there does not survive (FR-75).
func TestApplySkipsADegradedProvider(t *testing.T) {
	ctx := context.Background()
	provider := &fake{typ: "nextcloud", accounts: []core.Account{account("nc-alice", "alice@example.org")}}
	h, instances := applyHarness(t, provider, core.MustBytes(50<<30))
	plan := planNow(t, h, instances)

	if err := h.db.SetDegraded(ctx, instances[0].Row.ID, "oidc_login_default_quota is set"); err != nil {
		t.Fatal(err)
	}
	row, err := h.db.GetProviderByName(ctx, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	instances[0].Row = row

	report, err := h.engine.Apply(ctx, instances, plan, ApplyOptions{Actor: "test", ComputedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != 0 || report.Skipped != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(provider.writes) != 0 {
		t.Errorf("wrote %v to a provider that fights back", provider.writes)
	}
	entries, _ := h.db.RecentChanges(ctx, 5)
	if len(entries) != 1 || entries[0].Detail == "" {
		t.Errorf("the skip must say why: %+v", entries)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	provider := &fake{typ: "nextcloud", accounts: []core.Account{account("nc-alice", "alice@example.org")}}
	h, instances := applyHarness(t, provider, core.MustBytes(50<<30))
	plan := planNow(t, h, instances)

	report, err := h.engine.Apply(context.Background(), instances, plan, ApplyOptions{
		Actor: "test", ComputedAt: time.Now(), DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != 1 {
		t.Errorf("a dry run counts what it would do: %+v", report)
	}
	if len(provider.writes) != 0 {
		t.Errorf("a dry run wrote %v", provider.writes)
	}
	runs, _ := h.db.LastRuns(context.Background(), 1)
	if len(runs) != 1 || runs[0].Mode != store.RunDryRun {
		t.Errorf("runs = %+v", runs)
	}
}

// Throttling is an incomplete run, not a failure (FR-38c).
func TestThrottlingStopsCleanly(t *testing.T) {
	ctx := context.Background()
	throttling := &throttlingProvider{fake: fake{typ: "nextcloud",
		accounts: []core.Account{account("nc-alice", "alice@example.org")}}}
	h, instances := applyHarness(t, throttling, core.MustBytes(50<<30))
	plan := planNow(t, h, instances)

	report, err := h.engine.Apply(ctx, instances, plan, ApplyOptions{Actor: "test", ComputedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 0 {
		t.Errorf("throttling is not a failure: %+v", report)
	}
	if !report.Throttled || !report.Incomplete() {
		t.Errorf("report = %+v, want throttled and incomplete", report)
	}
}

// The pacer stops before exhausting the provider's own budget, leaving
// headroom for the probe and anything else on the same credential.
func TestPacerRespectsThePublishedLimit(t *testing.T) {
	limited := newPacer(&limitedProvider{})
	allowed := 0
	for i := 0; i < 100; i++ {
		if limited.allow() {
			allowed++
		}
	}
	if allowed == 0 || allowed >= 50 {
		t.Errorf("allowed %d writes against a limit of 50, want fewer with headroom", allowed)
	}

	unlimited := newPacer(&fake{typ: "immich"})
	for i := 0; i < 100; i++ {
		if !unlimited.allow() {
			t.Fatal("a provider that publishes no limit has no ceiling to stop at")
		}
	}
}

type failOnSecondWrite struct {
	fake
	calls int
}

func (f *failOnSecondWrite) SetQuota(ctx context.Context, id string, q core.Quota) error {
	f.calls++
	if f.calls == 2 {
		return &core.ProviderError{Provider: "nextcloud", Op: "PUT", Status: 500, Message: "server error"}
	}
	return f.fake.SetQuota(ctx, id, q)
}

type throttlingProvider struct{ fake }

func (t *throttlingProvider) SetQuota(context.Context, string, core.Quota) error {
	return &core.ThrottledError{Provider: "nextcloud", RetryAfter: time.Minute}
}

type limitedProvider struct{ fake }

func (l *limitedProvider) WriteLimit() (int, time.Duration) { return 50, 10 * time.Minute }
