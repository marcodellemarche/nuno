// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestWebhookPostsTheEvent(t *testing.T) {
	var got Event
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	hook := NewWebhook(srv.URL, "nuno.example.org", time.Second, discard())
	err := hook.Notify(context.Background(), Event{
		Kind:    KindGuarded,
		Summary: "one change refused by the shrink guardrail",
		Details: []Detail{{User: "alice", Provider: "photos", From: "150 GiB", To: "20 GiB", Reason: "below usage"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "application/json" {
		t.Errorf("content type = %q", contentType)
	}
	if got.Kind != KindGuarded || got.Host != "nuno.example.org" {
		t.Errorf("event = %+v", got)
	}
	if got.At.IsZero() {
		t.Error("an event must carry when it happened")
	}
	if len(got.Details) != 1 || got.Details[0].User != "alice" {
		t.Errorf("details = %+v", got.Details)
	}
}

func TestWebhookReportsAFailureWithoutLeakingTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// A webhook URL often carries a token in its path or query.
	hook := NewWebhook(srv.URL+"/hook?token=secret-token", "", time.Second, discard())
	err := hook.Notify(context.Background(), Event{Kind: KindFailed, Summary: "x"})
	if err == nil {
		t.Fatal("a non-2xx response must be reported")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("the error leaked the token: %v", err)
	}
}

func TestUnconfiguredIsANoOp(t *testing.T) {
	var hook *Webhook
	if hook.Configured() {
		t.Error("a nil webhook is not configured")
	}
	if err := (&Webhook{}).Notify(context.Background(), Event{}); err != nil {
		t.Errorf("an unconfigured webhook must be a no-op: %v", err)
	}
	if err := (Disabled{}).Notify(context.Background(), Event{}); err != nil {
		t.Error(err)
	}
	if (Disabled{}).Configured() {
		t.Error("Disabled is not configured")
	}
}
