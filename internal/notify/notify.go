// SPDX-License-Identifier: AGPL-3.0-or-later

// Package notify delivers outbound notifications. A plain webhook with a JSON
// body, roughly thirty lines and no dependency: Apprise as a service is a
// separate Python container, which contradicts NFR-1 and NFR-10 (ADR-0019).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

// Kind is what happened. A consumer routes on it.
type Kind string

const (
	// KindFailed is a reconcile that failed (FR-60).
	KindFailed Kind = "reconcile_failed"
	// KindGuarded is a change refused by a guardrail, reported on first
	// appearance and on change rather than once per cycle (FR-63).
	KindGuarded Kind = "changes_guarded"
	// KindDegraded is a provider Nuno will read but not write.
	KindDegraded Kind = "provider_degraded"
)

// Event is the body. The shape is small and stable, because a webhook
// consumer is a contract too.
type Event struct {
	Kind    Kind      `json:"kind"`
	At      time.Time `json:"at"`
	Summary string    `json:"summary"`
	Host    string    `json:"host,omitempty"`
	Details []Detail  `json:"details,omitempty"`
}

// Detail is one line a human reads in a notification.
type Detail struct {
	User     string `json:"user,omitempty"`
	Provider string `json:"provider,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Notifier is the port, so a caller does not care whether anything is
// configured.
type Notifier interface {
	Notify(ctx context.Context, event Event) error
	Configured() bool
}

// Webhook posts events to a URL.
type Webhook struct {
	url    string
	host   string
	client *http.Client
	log    *slog.Logger
}

func NewWebhook(url, host string, timeout time.Duration, log *slog.Logger) *Webhook {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Webhook{url: url, host: host, client: &http.Client{Timeout: timeout}, log: log}
}

func (w *Webhook) Configured() bool { return w != nil && w.url != "" }

func (w *Webhook) Notify(ctx context.Context, event Event) error {
	if !w.Configured() {
		return nil
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	event.Host = w.host

	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nuno")

	resp, err := w.client.Do(req)
	if err != nil {
		// For most webhook endpoints the token is the path, so an error
		// names the host and nothing else.
		return fmt.Errorf("post to %s: %w", core.RedactURLToHost(w.url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("post to %s: %s", core.RedactURLToHost(w.url), resp.Status)
	}
	return nil
}

// Disabled is what a caller gets when no webhook is configured. Notifying
// through it is a no-op rather than a nil check at every call site.
type Disabled struct{}

func (Disabled) Notify(context.Context, Event) error { return nil }
func (Disabled) Configured() bool                    { return false }
