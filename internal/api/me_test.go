// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func meRoutes(t *testing.T, s Store, trusted string) http.Handler {
	t.Helper()
	return Routes(Options{
		Version: "test", Store: s, Log: discard(),
		RefreshInterval: 15 * time.Minute, TrustedProxy: trusted,
	})
}

// The page shows the caller their own quota, and only theirs, which is what a
// shared dashboard cannot do with a server-side widget.
func TestMeServesTheCallersOwnQuota(t *testing.T) {
	mux := meRoutes(t, populated(), "10.0.0.0/8")

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("Remote-User", "alice")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Nextcloud") {
		t.Errorf("the page must name the service: %s", body)
	}
	if !strings.Contains(body, "/ 50 GiB") {
		t.Errorf("the page must carry the ceiling: %s", body)
	}
}

// A header is believed only from the proxy. Otherwise any container on the
// same network could read somebody else's quota.
func TestMeRefusesAnUntrustedSource(t *testing.T) {
	mux := meRoutes(t, populated(), "10.0.0.0/8")

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.RemoteAddr = "192.168.1.9:5555"
	req.Header.Set("Remote-User", "alice")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 for a source outside the trusted proxy", rec.Code)
	}
}

// With nothing configured, no header is trusted at all.
func TestMeTrustsNothingByDefault(t *testing.T) {
	mux := meRoutes(t, populated(), "")

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("Remote-User", "alice")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 when no proxy is configured", rec.Code)
	}
}

func TestMeWithoutAUserIsUnauthorized(t *testing.T) {
	mux := meRoutes(t, populated(), "10.0.0.0/8")

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 when the proxy names nobody", rec.Code)
	}
}

func TestMeForAStrangerIsNotFound(t *testing.T) {
	mux := meRoutes(t, populated(), "10.0.0.0/8")

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("Remote-User", "nobody")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 for an unknown user", rec.Code)
	}
}

func TestFromTrustedProxy(t *testing.T) {
	cases := []struct {
		name    string
		remote  string
		trusted string
		want    bool
	}{
		{"inside the network", "10.1.2.3:5555", "10.0.0.0/8", true},
		{"outside the network", "192.168.1.1:5555", "10.0.0.0/8", false},
		{"a single host", "10.1.2.3:5555", "10.1.2.3", true},
		{"a different single host", "10.1.2.4:5555", "10.1.2.3", false},
		{"a list", "172.18.0.5:5555", "10.0.0.0/8,172.18.0.0/16", true},
		{"nothing configured", "10.1.2.3:5555", "", false},
		{"unparseable remote", "not-an-address", "10.0.0.0/8", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/me", nil)
			req.RemoteAddr = c.remote
			if got := fromTrustedProxy(req, c.trusted); got != c.want {
				t.Errorf("fromTrustedProxy(%q, %q) = %v, want %v", c.remote, c.trusted, got, c.want)
			}
		})
	}
}

// The page is a widget body: it must not offer any way into the admin surface.
func TestMeCarriesNoAdminLinks(t *testing.T) {
	mux := meRoutes(t, populated(), "10.0.0.0/8")
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("Remote-User", "alice")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if body := rec.Body.String(); strings.Contains(body, "/actions/") {
		t.Errorf("the widget body must not carry admin actions: %s", body)
	}
}
