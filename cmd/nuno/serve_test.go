// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"testing"
	"time"
)

// Nuno starts before the services it reads when the stack is brought up
// together, so a failed first cycle must be retried soon rather than waiting a
// full refresh interval. The delay doubles and is capped, so a long outage is
// retried every minute instead of hammering.
func TestStartupBackoffGrowsAndCaps(t *testing.T) {
	backoff := startupBackoffMin
	want := []time.Duration{
		2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, time.Minute, time.Minute, time.Minute,
	}
	for i, expected := range want {
		if backoff != expected {
			t.Fatalf("step %d: backoff = %s, want %s", i, backoff, expected)
		}
		backoff = min(backoff*2, startupBackoffMax)
	}
}

func TestWaitOrDoneStopsWhenTheContextEnds(t *testing.T) {
	if !waitOrDone(context.Background(), 0) {
		t.Error("a zero wait must return immediately")
	}
	if !waitOrDone(context.Background(), time.Millisecond) {
		t.Error("a short wait with a live context must return true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if waitOrDone(ctx, time.Hour) {
		t.Error("a cancelled context must stop the wait")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the wait took %s, so it did not observe the cancellation", elapsed)
	}
}
