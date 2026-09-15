// SPDX-License-Identifier: AGPL-3.0-or-later

package immich

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

const fixtureDir = "../../../tests/fixtures"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

const (
	aliceID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	bobID   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// routes serves the two endpoints the adapter reads, so a test can replace
// either one.
func routes(t *testing.T, users, statistics []byte) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/users", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "abc123" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		w.Write(users)
	})
	mux.HandleFunc("/api/server/statistics", func(w http.ResponseWriter, r *http.Request) {
		if statistics == nil {
			http.Error(w, `{"message":"Internal server error"}`, http.StatusInternalServerError)
			return
		}
		w.Write(statistics)
	})
	mux.HandleFunc("/api/server/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"major":3,"minor":2,"patch":0}`))
	})
	return mux
}

func newTestProvider(t *testing.T, handler http.Handler) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := New(core.ProviderSettings{
		Instance:    core.ProviderInstance{Type: providerType, Name: "photos", ConfigRef: "NUNO_PROVIDER_PHOTOS"},
		BaseURL:     srv.URL,
		Credentials: map[string]core.Secret{"api_key": "abc123"},
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*Provider)
}

func accountsByID(t *testing.T, accounts []core.Account) map[string]core.Account {
	t.Helper()
	byID := map[string]core.Account{}
	for _, a := range accounts {
		byID[a.ExternalID] = a
	}
	return byID
}

// The contract test against both captured responses. The point it proves is
// that Used comes from the live aggregate, not from the cached counter: the
// two differ by 9.5 MB for one of these accounts.
func TestListAccountsAgainstTheCapturedResponses(t *testing.T) {
	p := newTestProvider(t, routes(t,
		readFixture(t, "immich-admin-users.json"),
		readFixture(t, "immich-server-statistics.json"),
	))

	accounts, err := p.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2", len(accounts))
	}
	byID := accountsByID(t, accounts)

	alice, ok := byID[aliceID]
	if !ok {
		t.Fatalf("alice is missing: %v", byID)
	}
	if !alice.Quota.Equal(core.MustBytes(161061273600)) {
		t.Errorf("alice quota = %v", alice.Quota)
	}
	// 9855362014 is usageByUser[].usage; 9864872002 is the cached counter.
	if !alice.Used.Equal(core.MustBytes(9855362014)) {
		t.Errorf("alice used = %v, want the live aggregate and not the cached counter", alice.Used)
	}
	// The cross-provider join: this is byte-identical to alice's Nextcloud
	// account id. See ADR-0021.
	if alice.Subject != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("alice subject = %q, want the OIDC subject from oauthId", alice.Subject)
	}
	if alice.Username != "" {
		t.Errorf("username = %q, want empty: Immich has no username", alice.Username)
	}
	if !alice.Writable() {
		t.Error("alice is active and not deleted")
	}

	bob, ok := byID[bobID]
	if !ok {
		t.Fatal("bob is missing")
	}
	// The two counters agree exactly for bob.
	if !bob.Used.Equal(core.MustBytes(35970729470)) {
		t.Errorf("bob used = %v", bob.Used)
	}
}

// The divergence threshold decides whether an account is writable at all, and
// there is no override for unknown usage, so the captured 0.096 percent must
// stay known. See ADR-0021 point 5.
func TestUsageDivergence(t *testing.T) {
	cases := []struct {
		name   string
		cached int64
		live   int64
		want   core.Quota
	}{
		{"exact agreement", 35970729470, 35970729470, core.MustBytes(35970729470)},
		{"the captured 9.5 MB out of 9.8 GB", 9864872002, 9855362014, core.MustBytes(9855362014)},
		{"just inside the absolute floor", 9855362014 + DivergenceFloor, 9855362014, core.MustBytes(9855362014)},
		{"one percent of a large account", 100_000_000_000 + 900_000_000, 100_000_000_000, core.MustBytes(100_000_000_000)},
		{"beyond one percent", 100_000_000_000 + 1_100_000_000, 100_000_000_000, core.Unknown()},
		{"small account beyond the floor", 200 << 20, 0, core.Unknown()},
		{"empty account agreeing at zero", 0, 0, core.MustBytes(0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			users := fmt.Sprintf(`[{"id":%q,"email":"x@example.org","status":"active","deletedAt":null,"oauthId":"","quotaSizeInBytes":1073741824,"quotaUsageInBytes":%d}]`, aliceID, c.cached)
			stats := fmt.Sprintf(`{"usageByUser":[{"userId":%q,"usage":%d}]}`, aliceID, c.live)

			p := newTestProvider(t, routes(t, []byte(users), []byte(stats)))
			accounts, err := p.ListAccounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !accounts[0].Used.Equal(c.want) {
				t.Errorf("used = %v, want %v", accounts[0].Used, c.want)
			}
			if err := accounts[0].Used.ValidAsUsage(); err != nil {
				t.Error(err)
			}
		})
	}
}

// An account with no assets is absent from the aggregate. That is a
// measurement of zero, not a gap, and ADR-0024 says Immich needs no
// never-logged-in rule because of it.
func TestAccountAbsentFromTheAggregate(t *testing.T) {
	users := fmt.Sprintf(`[{"id":%q,"email":"new@example.org","status":"active","deletedAt":null,"quotaSizeInBytes":1073741824,"quotaUsageInBytes":0}]`, aliceID)
	p := newTestProvider(t, routes(t, []byte(users), []byte(`{"usageByUser":[]}`)))

	accounts, err := p.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !accounts[0].Used.Equal(core.MustBytes(0)) {
		t.Errorf("used = %v, want a known zero: an account with no assets legitimately reports zero", accounts[0].Used)
	}
}

// Without the authoritative source there is nothing to conclude. The cached
// counter alone is not a trustworthy basis for the shrink guardrail.
func TestStatisticsFailureMakesUsageUnknownWithoutLosingTheAccounts(t *testing.T) {
	p := newTestProvider(t, routes(t, readFixture(t, "immich-admin-users.json"), nil))

	accounts, err := p.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("a failed statistics call must not lose the accounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2", len(accounts))
	}
	for _, a := range accounts {
		if a.Used.IsKnown() {
			t.Errorf("%s: used = %v, want unknown", a.ExternalID, a.Used)
		}
		// The configured quota is still readable and still worth reporting.
		if !a.Quota.IsKnown() {
			t.Errorf("%s: the configured quota came from the users endpoint and is still known", a.ExternalID)
		}
	}
}

func TestQuotaDecoding(t *testing.T) {
	cases := []struct {
		name  string
		quota string
		want  core.Quota
		ok    bool
	}{
		{"explicit null is unlimited", `null`, core.Unlimited(), true},
		{"zero is legal and blocks uploads", `0`, core.MustBytes(0), true},
		{"a byte count", `161061273600`, core.MustBytes(161061273600), true},
		{"a negative is not a sentinel here", `-3`, core.Unknown(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			users := fmt.Sprintf(`[{"id":%q,"email":"x@example.org","status":"active","deletedAt":null,"quotaSizeInBytes":%s,"quotaUsageInBytes":0}]`, aliceID, c.quota)
			stats := fmt.Sprintf(`{"usageByUser":[{"userId":%q,"usage":0}]}`, aliceID)
			p := newTestProvider(t, routes(t, []byte(users), []byte(stats)))

			accounts, err := p.ListAccounts(context.Background())
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
			if c.ok && !accounts[0].Quota.Equal(c.want) {
				t.Errorf("quota = %v, want %v", accounts[0].Quota, c.want)
			}
		})
	}
}

// Accounts are soft-deleted, and a deleted or inactive one is never linked or
// written (FR-8a).
func TestSoftDeletedAndInactiveAccountsAreNotWritable(t *testing.T) {
	users := fmt.Sprintf(`[
		{"id":%q,"email":"gone@example.org","status":"deleted","deletedAt":"2026-09-01T00:00:00.000Z","quotaSizeInBytes":1073741824,"quotaUsageInBytes":0},
		{"id":%q,"email":"pending@example.org","status":"removing","deletedAt":null,"quotaSizeInBytes":1073741824,"quotaUsageInBytes":0}
	]`, aliceID, bobID)
	stats := `{"usageByUser":[]}`
	p := newTestProvider(t, routes(t, []byte(users), []byte(stats)))

	accounts, err := p.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := accountsByID(t, accounts)
	if gone := byID[aliceID]; !gone.Deleted || gone.Writable() {
		t.Errorf("a soft-deleted account must not be writable: %+v", gone)
	}
	if pending := byID[bobID]; pending.Enabled || pending.Writable() {
		t.Errorf("a non-active status must not be writable: %+v", pending)
	}
}

// The one-character silent failure from ADR-0011: an omitted
// quotaSizeInBytes means "do not touch", so unlimited must be an explicit
// null in the body.
func TestSetQuotaAlwaysCarriesTheKey(t *testing.T) {
	cases := []struct {
		name string
		q    core.Quota
		want string
	}{
		{"unlimited is an explicit null", core.Unlimited(), `{"quotaSizeInBytes":null}`},
		{"a byte count", core.MustBytes(161061273600), `{"quotaSizeInBytes":161061273600}`},
		{"zero is written as zero", core.MustBytes(0), `{"quotaSizeInBytes":0}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotBody string
			var gotMethod, gotPath string
			p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody = mustReadString(t, r)
				gotMethod, gotPath = r.Method, r.URL.Path
				w.Write([]byte(`{}`))
			}))
			if err := p.SetQuota(context.Background(), aliceID, c.q); err != nil {
				t.Fatal(err)
			}
			if gotBody != c.want {
				t.Errorf("body = %s, want %s", gotBody, c.want)
			}
			if gotMethod != http.MethodPut || gotPath != pathAdminUsers+"/"+aliceID {
				t.Errorf("%s %s", gotMethod, gotPath)
			}
		})
	}
}

func TestSetQuotaRefusesUnknown(t *testing.T) {
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("SetQuota must not reach the instance with an unknown quota")
	}))
	if err := p.SetQuota(context.Background(), aliceID, core.Unknown()); err == nil {
		t.Fatal("want an error")
	}
}

func TestErrorsAreTyped(t *testing.T) {
	t.Run("a key without the right scopes is an auth error", func(t *testing.T) {
		p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Forbidden","error":"Forbidden","statusCode":403}`))
		}))
		_, err := p.ListAccounts(context.Background())
		if !core.IsAuth(err) {
			t.Fatalf("err = %v, want an AuthError", err)
		}
		if !contains(err.Error(), "adminUser.update") {
			t.Errorf("the error must name the scopes to check: %v", err)
		}
	})

	t.Run("throttling", func(t *testing.T) {
		p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		_, err := p.ListAccounts(context.Background())
		d, ok := core.Throttled(err)
		if !ok || d != 30*time.Second {
			t.Fatalf("err = %v, retryAfter = %s", err, d)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		p, err := New(core.ProviderSettings{
			Instance:    core.ProviderInstance{Type: providerType, ConfigRef: "X"},
			BaseURL:     "http://127.0.0.1:1",
			Credentials: map[string]core.Secret{"api_key": "k"},
			Timeout:     time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.ListAccounts(context.Background()); !core.IsUnreachable(err) {
			t.Fatalf("err = %v, want an UnreachableError", err)
		}
	})
}

func TestHealthReportsVersionAndProvesTheCredential(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		p := newTestProvider(t, routes(t, readFixture(t, "immich-admin-users.json"), readFixture(t, "immich-server-statistics.json")))
		got := p.Health(context.Background())
		if !got.Reachable || got.Err != nil {
			t.Fatalf("health = %+v", got)
		}
		if got.Version != "v3.2.0" || !got.InSupported {
			t.Errorf("version = %q, inSupported = %v", got.Version, got.InSupported)
		}
	})

	t.Run("reachable but the key is rejected", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/server/version", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"major":3,"minor":2,"patch":0}`))
		})
		mux.HandleFunc("/api/admin/users", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Invalid API key"}`))
		})
		p := newTestProvider(t, mux)

		got := p.Health(context.Background())
		if !got.Reachable {
			t.Error("the instance answered, so it is reachable")
		}
		if !core.IsAuth(got.Err) {
			t.Errorf("Err = %v, want an AuthError: the version endpoint needs no key and proves nothing", got.Err)
		}
		if got.Version != "v3.2.0" {
			t.Errorf("version = %q, want it reported even when the key fails", got.Version)
		}
	})

	t.Run("outside the supported major", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/server/version", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"major":4,"minor":0,"patch":0}`))
		})
		mux.HandleFunc("/api/admin/users", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
		p := newTestProvider(t, mux)

		got := p.Health(context.Background())
		if got.InSupported {
			t.Error("major 4 is outside the supported major, which warns rather than refusing")
		}
		if got.Err != nil {
			t.Errorf("an unsupported major is a warning, not an error: %v", got.Err)
		}
	})
}

func TestNormalizeQuotaIsTheIdentity(t *testing.T) {
	p := &Provider{}
	for _, q := range []core.Quota{core.Unlimited(), core.MustBytes(0), core.MustBytes(26844594176)} {
		if got := p.NormalizeQuota(q); !got.Equal(q) {
			t.Errorf("NormalizeQuota(%v) = %v: Immich stores the bytes it is given", q, got)
		}
	}
	if got := p.NormalizeQuota(core.Unknown()); got.IsKnown() {
		t.Errorf("NormalizeQuota(Unknown) = %v", got)
	}
}

func TestNewRequiresAnAPIKey(t *testing.T) {
	_, err := New(core.ProviderSettings{
		Instance: core.ProviderInstance{Type: providerType, ConfigRef: "NUNO_PROVIDER_PHOTOS"},
		BaseURL:  "https://photos.example.org",
	})
	if err == nil {
		t.Fatal("want an error naming the setting")
	}
	if !contains(err.Error(), "NUNO_PROVIDER_PHOTOS_API_KEY") {
		t.Errorf("err = %v, want it to name the missing setting", err)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func mustReadString(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
