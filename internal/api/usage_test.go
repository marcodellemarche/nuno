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
	"strings"
	"testing"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

type fakeStore struct {
	users        []core.User
	providers    []store.ProviderRow
	accounts     []core.ExternalAccount
	issues       []core.LinkIssue
	keys         map[string]bool
	keyCount     int
	policy       core.Policy
	userPolicies map[int64]core.UserPolicy
	groups       []core.Group
	links        []core.AccountLink
	runs         []store.RunSummary
	changes      []store.AuditRow
	adminKeys    []store.AdminKeyRow
	err          error
}

func (f *fakeStore) ListUsers(context.Context) ([]core.User, error) { return f.users, f.err }
func (f *fakeStore) ListProviders(context.Context) ([]store.ProviderRow, error) {
	return f.providers, f.err
}
func (f *fakeStore) ListAllExternalAccounts(context.Context) ([]core.ExternalAccount, error) {
	return f.accounts, f.err
}
func (f *fakeStore) ListGroups(context.Context) ([]core.Group, error)         { return f.groups, f.err }
func (f *fakeStore) ListLinks(context.Context) ([]core.AccountLink, error)    { return f.links, f.err }
func (f *fakeStore) ListLinkIssues(context.Context) ([]core.LinkIssue, error) { return f.issues, f.err }
func (f *fakeStore) CheckAdminKey(_ context.Context, presented core.Secret) (bool, error) {
	return f.keys[presented.Reveal()], nil
}
func (f *fakeStore) CountAdminKeys(context.Context) (int, error) { return f.keyCount, nil }

func (f *fakeStore) LoadPolicy(context.Context) (core.Policy, error) { return f.policy, f.err }
func (f *fakeStore) UserPolicy(_ context.Context, userID int64) (core.UserPolicy, map[int64]store.OverrideOrigin, error) {
	up, ok := f.userPolicies[userID]
	if !ok {
		up = core.UserPolicy{ProviderOverrides: map[int64]core.Quota{}}
	}
	return up, nil, f.err
}
func (f *fakeStore) LastRuns(context.Context, int) ([]store.RunSummary, error) { return f.runs, f.err }
func (f *fakeStore) RecentChanges(context.Context, int) ([]store.AuditRow, error) {
	return f.changes, f.err
}
func (f *fakeStore) ListAdminKeys(context.Context) ([]store.AdminKeyRow, error) {
	return f.adminKeys, f.err
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func populated() *fakeStore {
	observedAt := time.Now().UTC().Add(-time.Minute)
	userID := int64(1)
	return &fakeStore{
		users: []core.User{{ID: 1, SourceUUID: "11111111", UID: "alice", Status: core.UserActive}},
		providers: []store.ProviderRow{{
			ProviderInstance: core.ProviderInstance{ID: 10, Type: "nextcloud", Name: "cloud"},
			Version:          "34.0.4", Reachable: true, InSupported: true,
			LastObserveAt: &observedAt, LastObserveOK: true,
		}},
		accounts: []core.ExternalAccount{{
			ProviderID: 10, ExternalID: "nc-alice", UserID: &userID,
			Quota: core.MustBytes(53687091200), Used: core.MustBytes(1073741824),
			Enabled: true, ObservedAt: observedAt, ObserveOK: true,
		}},
		keys:     map[string]bool{"good-key": true},
		keyCount: 1,
	}
}

func routes(t *testing.T, s Store, password core.Secret) http.Handler {
	t.Helper()
	return Routes(Options{
		Version: "test", Store: s, Log: discard(),
		AdminPassword: password, RefreshInterval: 15 * time.Minute,
	})
}

func TestUsageRequiresTheAdminKey(t *testing.T) {
	mux := routes(t, populated(), "")

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong key", "Bearer nope", http.StatusUnauthorized},
		{"not a bearer", "Basic abc", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"the key", "Bearer good-key", http.StatusOK},
		{"lower case scheme, as some widgets send it", "bearer good-key", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("code = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
			if c.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("a 401 must say what it wants")
			}
		})
	}
}

// With no key configured, saying so beats an unauthorized that looks like a
// wrong key (FR-46a).
func TestUsageSaysWhenNoKeyIsConfigured(t *testing.T) {
	s := populated()
	s.keyCount = 0
	mux := routes(t, s, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "NUNO_ADMIN_KEY") {
		t.Errorf("the error must name the setting: %s", rec.Body.String())
	}
}

func TestUsageServesTheContract(t *testing.T) {
	mux := routes(t, populated(), "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer good-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache control = %q: a dashboard must not cache a quota", got)
	}

	var response core.UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Schema != core.UsageSchema || len(response.Users) != 1 {
		t.Fatalf("response = %+v", response)
	}
	alice := response.Users[0]
	if alice.User != "alice" || alice.UserUUID != "11111111" {
		t.Errorf("user = %+v", alice)
	}
	if len(alice.Providers) != 1 || alice.Providers[0].Status != core.StatusOK {
		t.Errorf("providers = %+v", alice.Providers)
	}
}

// A shared dashboard wants people, not the service accounts in the directory
// that have no quota anywhere. The filter is opt-in, so the default response
// stays every user (FR-45).
func TestUsageWithAccountsDropsPeopleWhoHaveNone(t *testing.T) {
	s := populated()
	s.users = append(s.users, core.User{ID: 2, SourceUUID: "22222222", UID: "authelia", Status: core.UserActive})

	all := usageUsers(t, s, "/api/v1/usage")
	if len(all) != 2 {
		t.Fatalf("default response has %d users, want every user", len(all))
	}

	people := usageUsers(t, s, "/api/v1/usage?with_accounts=1")
	if len(people) != 1 || people[0].User != "alice" {
		t.Fatalf("filtered response = %+v, want only alice", people)
	}
}

// detail=service is what the shared dashboard uses: totals per service, with
// no person named.
func TestUsageDetailServiceAggregates(t *testing.T) {
	mux := routes(t, populated(), "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage?detail=service", nil)
	req.Header.Set("Authorization", "Bearer good-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	var response core.UsageDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Rows) != 1 || response.Rows[0].Name != "Nextcloud" {
		t.Fatalf("rows = %+v, want one Nextcloud row", response.Rows)
	}
	if !strings.Contains(response.Rows[0].Summary, "/") {
		t.Errorf("summary = %q, want used and ceiling in one string", response.Rows[0].Summary)
	}
}

func usageUsers(t *testing.T, s Store, path string) []core.UsageUser {
	t.Helper()
	mux := routes(t, s, "")
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer good-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: code = %d: %s", path, rec.Code, rec.Body.String())
	}
	var response core.UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Users
}

func TestUsageIsRateLimited(t *testing.T) {
	mux := routes(t, populated(), "")
	limited := false
	for i := 0; i < 40; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil)
		req.Header.Set("Authorization", "Bearer good-key")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			if rec.Header().Get("Retry-After") == "" {
				t.Error("a 429 must say when to come back")
			}
			break
		}
	}
	if !limited {
		t.Error("a leaked key must not be usable without limit")
	}
}

func TestUsageReportsAStoreFailureRatherThanEmptyData(t *testing.T) {
	s := populated()
	s.err = errors.New("database is locked")
	mux := routes(t, s, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer good-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500: an empty answer would read as nobody using anything", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "database is locked") {
		t.Error("an internal error must not echo the internals")
	}
}

// Each tab is a real page, so what used to be one render is five.
func TestEveryPageRenders(t *testing.T) {
	cases := []struct {
		name string
		path string
		want []string
	}{
		{"quotas", "/", []string{"alice", "cloud", "50 GiB", "1 GiB"}},
		{"tiers", "/tiers", []string{"Tiers", "most generous ceiling wins"}},
		{"services", "/services", []string{"Cloud", "34.0.4", "unproven", "Keys for the usage API"}},
		{"accounts", "/accounts", []string{"nc-alice", "alice", "Needs a decision"}},
		{"activity", "/activity", []string{"Recent runs", "Recent changes"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := populated()
			s.issues = []core.LinkIssue{{ProviderID: 10, Kind: core.IssueUnmanaged, ExternalID: "nc-ghost"}}
			mux := routes(t, s, "")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range c.want {
				if !strings.Contains(body, want) {
					t.Errorf("%s is missing %q", c.path, want)
				}
			}
			// The tab bar is on every page, so every page can reach the rest.
			for _, tab := range []string{`href="/tiers"`, `href="/services"`, `href="/accounts"`, `href="/activity"`} {
				if !strings.Contains(body, tab) {
					t.Errorf("%s does not link %s", c.path, tab)
				}
			}
		})
	}
}

// FR-55: misconfigured means read-only and honest, not a crash.
func TestThePageDegradesInsteadOfCrashing(t *testing.T) {
	cases := []struct {
		name  string
		store *fakeStore
		want  []string
	}{
		{
			name:  "nothing configured at all",
			store: &fakeStore{},
			want:  []string{"No providers are configured", "Nobody yet"},
		},
		{
			name: "a provider that has never been read",
			store: func() *fakeStore {
				s := populated()
				s.providers[0].LastObserveAt = nil
				return s
			}(),
			want: []string{"has never been observed", "nuno observe"},
		},
		{
			name: "a provider that could not be read",
			store: func() *fakeStore {
				s := populated()
				s.providers[0].LastObserveOK = false
				s.providers[0].LastError = "connection refused"
				s.accounts[0].ObserveOK = true
				return s
			}(),
			want: []string{"could not be read", "connection refused", "unavailable"},
		},
		{
			name: "a degraded provider says why",
			store: func() *fakeStore {
				s := populated()
				s.providers[0].DegradedReason = "oidc_login_default_quota is set"
				return s
			}(),
			want: []string{"degraded", "oidc_login_default_quota"},
		},
		{
			name: "the database is unreadable",
			store: func() *fakeStore {
				s := populated()
				s.err = errors.New("no such table")
				return s
			}(),
			want: []string{"could not be read"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mux := routes(t, c.store, "")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d, want a page that still renders", rec.Code)
			}
			for _, want := range c.want {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("the page does not mention %q", want)
				}
			}
		})
	}
}

// Unknown must not read as unlimited on the page either.
func TestThePageDistinguishesUnknownFromUnlimited(t *testing.T) {
	s := populated()
	s.accounts[0].Quota = core.Unknown()
	s.accounts[0].Used = core.Unknown()
	mux := routes(t, s, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "unknown") {
		t.Errorf("the page must say unknown: %s", body)
	}
	if strings.Contains(body, "unlimited") {
		t.Error("an unreadable ceiling must never render as unlimited")
	}
}

func TestTheAdminPasswordGuardsThePageButNotHealthz(t *testing.T) {
	mux := routes(t, populated(), core.Secret("hunter2"))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 with a password set", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("a 401 must prompt")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "hunter2")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d with the right password", rec.Code)
	}

	// A health check must not need a credential, or an orchestrator cannot
	// use it.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d", rec.Code)
	}
}

func TestTheStylesheetIsServedFromTheBinary(t *testing.T) {
	mux := routes(t, populated(), "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/nuno.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: the container must work with no outbound network", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "--bg") {
		t.Error("that is not the stylesheet")
	}
}

func TestOnlyHealthzIsServedWithoutAStore(t *testing.T) {
	mux := Routes(Options{Version: "test", Log: discard()})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404 rather than a panic", rec.Code)
	}
}
