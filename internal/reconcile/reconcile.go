// SPDX-License-Identifier: AGPL-3.0-or-later

// Package reconcile owns the I/O sequencing: observe, link, and later plan and
// apply. It holds no domain rules, which live in core, and it is where
// contexts, deadlines, retries, pacing and per-provider degradation belong.
package reconcile

import (
	"log/slog"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

// Instance pairs a configured provider row with its live adapter.
type Instance struct {
	Row      store.ProviderRow
	Provider core.Provider
}

func (i Instance) Name() string { return i.Row.Name }

type Engine struct {
	db  *store.DB
	log *slog.Logger
	now func() time.Time
}

func New(db *store.DB, log *slog.Logger) *Engine {
	return &Engine{db: db, log: log, now: time.Now}
}

// ProviderResult is what one provider contributed to a cycle.
type ProviderResult struct {
	Name     string
	Type     string
	Observed int // accounts the provider reported
	Skipped  int // disabled or soft-deleted, so never linked or written (FR-8a)
	Linked   int
	Issues   int
	Err      error
}

func (r ProviderResult) OK() bool { return r.Err == nil }

// Report is the outcome of a cycle. It is deliberately honest about partial
// success: one provider failing degrades that provider and does not stop work
// on the others.
type Report struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Providers  []ProviderResult
}

// Failed lists the providers that could not be observed.
func (r Report) Failed() []ProviderResult {
	var failed []ProviderResult
	for _, p := range r.Providers {
		if !p.OK() {
			failed = append(failed, p)
		}
	}
	return failed
}

func (r Report) OK() bool { return len(r.Failed()) == 0 }

func (r Report) Totals() (observed, linked, issues int) {
	for _, p := range r.Providers {
		observed += p.Observed
		linked += p.Linked
		issues += p.Issues
	}
	return observed, linked, issues
}
