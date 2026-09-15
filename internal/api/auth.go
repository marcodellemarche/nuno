// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

// bearer pulls the credential out of the header. It is never logged.
func bearer(r *http.Request) (core.Secret, bool) {
	header := r.Header.Get("Authorization")
	value, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		// Some widgets send the scheme in lower case.
		value, ok = strings.CutPrefix(header, "bearer ")
	}
	value = strings.TrimSpace(value)
	if !ok || value == "" {
		return "", false
	}
	return core.Secret(value), true
}

// limiter is a small token bucket per credential, so a leaked key cannot be
// brute forced or used to hammer the providers' cached numbers. Keys are
// hashed before they reach the map, so the map never holds a credential.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    time.Duration
	burst   int
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perMinute, burst int) *limiter {
	return &limiter{
		buckets: map[string]*bucket{},
		rate:    time.Minute / time.Duration(max(perMinute, 1)),
		burst:   max(burst, 1),
		now:     time.Now,
	}
}

func (l *limiter) allow(credential core.Secret) bool {
	sum := sha256.Sum256([]byte(credential.Reveal()))
	key := hex.EncodeToString(sum[:8])

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		// Bound the map: a flood of distinct keys must not grow it forever.
		if len(l.buckets) > 1024 {
			l.buckets = map[string]*bucket{}
		}
		l.buckets[key] = &bucket{tokens: float64(l.burst) - 1, last: now}
		return true
	}

	b.tokens += now.Sub(b.last).Seconds() / l.rate.Seconds()
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// basicAuth guards the admin surface when no proxy provides authentication, so
// that "no proxy yet" never means "no auth" (NFR-14). With no password set it
// lets everything through, because the surface is on loopback unless the
// operator said otherwise.
func basicAuth(password core.Secret, next http.Handler) http.Handler {
	if password.Empty() {
		return next
	}
	expected := sha256.Sum256([]byte(password.Reveal()))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, presented, ok := r.BasicAuth()
		presentedSum := sha256.Sum256([]byte(presented))
		if !ok || subtle.ConstantTimeCompare(expected[:], presentedSum[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="nuno", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
