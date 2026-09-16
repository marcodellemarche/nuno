// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubPinger struct{ err error }

func (s stubPinger) PingContext(context.Context) error { return s.err }

func TestHealthz(t *testing.T) {
	cases := []struct {
		name     string
		db       Pinger
		wantCode int
		wantStat string
	}{
		{"healthy", stubPinger{}, http.StatusOK, "ok"},
		{"database down", stubPinger{errors.New("locked")}, http.StatusServiceUnavailable, "unavailable"},
		{"no database wired yet", nil, http.StatusOK, "ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mux := Routes(Options{
				Version: "test",
				DB:      c.db,
				Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			if rec.Code != c.wantCode {
				t.Errorf("code = %d, want %d", rec.Code, c.wantCode)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
			if body["status"] != c.wantStat {
				t.Errorf("status = %v, want %q", body["status"], c.wantStat)
			}
		})
	}
}

func TestHealthzRejectsOtherMethods(t *testing.T) {
	mux := Routes(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", rec.Code)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	mux := Routes(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / = %d, want 404 until a page exists", rec.Code)
	}
}

// The panel shares a Docker network with every service, so without the proxy
// secret any container could reach it directly and skip the SSO in front of
// the public name. The secret is what narrows that to the proxy alone.
func TestProxySecretGatesTheAdminSurface(t *testing.T) {
	mux := Routes(Options{
		Version: "test", Store: populated(), Actor: &fakeActor{}, Log: discard(),
		ProxySecret: "s3cret",
	})

	// Without the header, a direct hit on the container is refused.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET / without the secret = %d, want 403", rec.Code)
	}

	// The proxy injects it, and the page is served.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Nuno-Proxy-Secret", "s3cret")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / with the secret = %d, want 200", rec.Code)
	}

	// A mutation is gated too, or the gate would only be cosmetic.
	rec = post(t, mux, "/actions/observe", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /actions/observe without the secret = %d, want 403", rec.Code)
	}

	// Health and the key-authenticated usage endpoint stay reachable: a
	// dashboard widget reads usage server-side and never sees the proxy.
	for _, path := range []string{"/healthz", "/api/v1/usage"} {
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusForbidden {
			t.Errorf("%s must not be gated by the proxy secret", path)
		}
	}
}
