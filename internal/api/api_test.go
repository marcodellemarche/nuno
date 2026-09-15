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
