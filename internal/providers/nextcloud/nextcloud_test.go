// SPDX-License-Identifier: AGPL-3.0-or-later

package nextcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

func newTestProvider(t *testing.T, handler http.Handler) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := New(core.ProviderSettings{
		Instance: core.ProviderInstance{Type: providerType, Name: "cloud", ConfigRef: "NUNO_PROVIDER_CLOUD"},
		BaseURL:  srv.URL,
		Credentials: map[string]core.Secret{
			"username": "nuno-admin",
			"password": "hunter2",
		},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*Provider)
}

// serveFixture answers the details endpoint with the recorded response.
func serveFixture(t *testing.T, body []byte) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("OCS-APIRequest") != "true" {
			t.Errorf("missing OCS-APIRequest header: the instance would refuse this request")
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q: without it the response is XML", r.Header.Get("Accept"))
		}
		if user, pass, ok := r.BasicAuth(); !ok || user != "nuno-admin" || pass != "hunter2" {
			t.Errorf("basic auth = %q/%v", user, ok)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
}

// The contract test against the captured response. Its shapes are the ones
// that break naive decoding: users is a map, a quota of -3, a null email.
func TestListAccountsAgainstTheCapturedResponse(t *testing.T) {
	p := newTestProvider(t, serveFixture(t, readFixture(t, "nextcloud-users-details.json")))

	accounts, err := p.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2: ocs.data.users is a map keyed by account id, not an array", len(accounts))
	}

	byID := map[string]core.Account{}
	for _, a := range accounts {
		byID[a.ExternalID] = a
	}

	alice, ok := byID["11111111-1111-4111-8111-111111111111"]
	if !ok {
		t.Fatalf("the map key is the ExternalID, got %v", byID)
	}
	if !alice.Quota.Equal(core.MustBytes(53687091200)) {
		t.Errorf("alice quota = %v, want the configured quota from quota.quota", alice.Quota)
	}
	if !alice.Used.Equal(core.MustBytes(94422551)) {
		t.Errorf("alice used = %v", alice.Used)
	}
	if alice.NeverUsed {
		t.Error("alice has a login timestamp, so she is not a never-used account")
	}
	if alice.Email != "alice@example.org" {
		t.Errorf("alice email = %q", alice.Email)
	}
	// The account id is the OIDC subject here, which is the exact join to
	// Immich's oauthId. See ADR-0028.
	if alice.Subject != alice.ExternalID {
		t.Errorf("alice subject = %q, want the UUID account id", alice.Subject)
	}
	if !alice.Writable() {
		t.Error("alice is enabled and not deleted, so she is writable")
	}

	admin, ok := byID["admin"]
	if !ok {
		t.Fatal("the local admin account is missing")
	}
	// -3 is FileInfo::SPACE_UNLIMITED on the wire, not a byte count.
	if !admin.Quota.Equal(core.Unlimited()) {
		t.Errorf("admin quota = %v, want unlimited: -3 is the sentinel", admin.Quota)
	}
	if !admin.Used.Equal(core.MustBytes(65575770)) {
		t.Errorf("admin used = %v: total and free are -3 but used is real", admin.Used)
	}
	if admin.Email != "" {
		t.Errorf("admin email = %q, want empty: the captured account has a null email", admin.Email)
	}
	if admin.Subject != "" {
		t.Errorf("admin subject = %q, want empty: a local account id is not a subject", admin.Subject)
	}
}

func TestGetAccountReadsOneAccount(t *testing.T) {
	// The single-user endpoint returns the user object directly under
	// ocs.data, without the users map around it.
	var details struct {
		OCS struct {
			Data struct {
				Users map[string]json.RawMessage `json:"users"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if err := json.Unmarshal(readFixture(t, "nextcloud-users-details.json"), &details); err != nil {
		t.Fatal(err)
	}
	one := details.OCS.Data.Users["11111111-1111-4111-8111-111111111111"]

	body := []byte(`{"ocs":{"meta":{"status":"ok","statuscode":200,"message":"OK"},"data":` + string(one) + `}}`)
	p := newTestProvider(t, serveFixture(t, body))

	account, err := p.GetAccount(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if !account.Quota.Equal(core.MustBytes(53687091200)) {
		t.Errorf("quota = %v", account.Quota)
	}
	if account.ExternalID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("ExternalID = %q", account.ExternalID)
	}
}

// The degraded shapes, which the captures do not contain. ADR-0024 split the
// zero-usage case in two, and getting this wrong means either refusing to
// provision anyone or writing against a fabricated zero.
func TestUsageRule(t *testing.T) {
	cases := []struct {
		name       string
		quota      string
		firstLogin string
		wantConf   core.Quota
		wantUsed   core.Quota
		neverUsed  bool
	}{
		{
			name: "complete object", quota: `{"free":1,"used":94422551,"total":53687091200,"relative":0.18,"quota":53687091200}`,
			firstLogin: "1789301106",
			wantConf:   core.MustBytes(53687091200), wantUsed: core.MustBytes(94422551),
		},
		{
			name: "storage error serializes the object as an array", quota: `[]`,
			firstLogin: "1789301106",
			wantConf:   core.Unknown(), wantUsed: core.Unknown(),
		},
		{
			name: "absent quota object", quota: `null`,
			firstLogin: "1789301106",
			wantConf:   core.Unknown(), wantUsed: core.Unknown(),
		},
		{
			name: "missing total", quota: `{"used":0,"relative":0,"quota":53687091200}`,
			firstLogin: "1789301106",
			wantConf:   core.MustBytes(53687091200), wantUsed: core.Unknown(),
		},
		{
			name: "missing relative", quota: `{"used":0,"total":53687091200,"quota":53687091200}`,
			firstLogin: "1789301106",
			wantConf:   core.MustBytes(53687091200), wantUsed: core.Unknown(),
		},
		{
			name: "never logged in, complete object: known and zero", quota: `{"free":53687091200,"used":0,"total":53687091200,"relative":0,"quota":53687091200}`,
			firstLogin: "0",
			wantConf:   core.MustBytes(53687091200), wantUsed: core.MustBytes(0),
			neverUsed: true,
		},
		{
			name: "never logged in with an incomplete object stays unknown", quota: `{"used":0,"quota":53687091200}`,
			firstLogin: "0",
			wantConf:   core.MustBytes(53687091200), wantUsed: core.Unknown(),
		},
		{
			name: "unlimited with real usage", quota: `{"free":-3,"used":65575770,"total":-3,"relative":0,"quota":-3}`,
			firstLogin: "1789296679",
			wantConf:   core.Unlimited(), wantUsed: core.MustBytes(65575770),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"ocs":{"meta":{"status":"ok","statuscode":200},"data":{"users":{"u1":{"id":"u1","enabled":true,"email":null,"firstLoginTimestamp":%s,"quota":%s}}}}}`,
				c.firstLogin, c.quota)
			p := newTestProvider(t, serveFixture(t, []byte(body)))

			accounts, err := p.ListAccounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := accounts[0]
			if !got.Quota.Equal(c.wantConf) {
				t.Errorf("configured = %v, want %v", got.Quota, c.wantConf)
			}
			if !got.Used.Equal(c.wantUsed) {
				t.Errorf("used = %v, want %v", got.Used, c.wantUsed)
			}
			if got.NeverUsed != c.neverUsed {
				t.Errorf("NeverUsed = %v, want %v", got.NeverUsed, c.neverUsed)
			}
			if err := got.Used.ValidAsUsage(); err != nil {
				t.Errorf("usage is not a valid usage value: %v", err)
			}
		})
	}
}

// The write probe capture: an app password gets 403 in both the HTTP status
// and the envelope, which is the v2 convention. See ADR-0021 point 1.
func TestWritePathErrorsAgainstTheCapturedProbe(t *testing.T) {
	var capture struct {
		Probes []struct {
			Auth       string `json:"auth"`
			HTTPStatus int    `json:"http_status"`
			OCSMeta    struct {
				Status     string `json:"status"`
				StatusCode int    `json:"statuscode"`
				Message    string `json:"message"`
			} `json:"ocs_meta"`
		} `json:"probes"`
	}
	if err := json.Unmarshal(readFixture(t, "nextcloud-write-probe.json"), &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Probes) < 2 {
		t.Fatal("the fixture did not decode")
	}
	appPassword, admin := capture.Probes[0], capture.Probes[1]

	t.Run("app password cannot write", func(t *testing.T) {
		p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(appPassword.HTTPStatus)
			fmt.Fprintf(w, `{"ocs":{"meta":{"status":%q,"statuscode":%d,"message":%q},"data":[]}}`,
				appPassword.OCSMeta.Status, appPassword.OCSMeta.StatusCode, appPassword.OCSMeta.Message)
		}))
		err := p.SetQuota(context.Background(), "u1", core.MustBytes(53687091200))
		if !core.IsAuth(err) {
			t.Fatalf("err = %v, want an AuthError: %q", err, appPassword.OCSMeta.Message)
		}
	})

	t.Run("a real admin password writes", func(t *testing.T) {
		var gotForm string
		p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				t.Errorf("method = %s, want PUT", r.Method)
			}
			r.ParseForm()
			gotForm = r.Form.Encode()
			fmt.Fprintf(w, `{"ocs":{"meta":{"status":%q,"statuscode":%d,"message":%q},"data":[]}}`,
				admin.OCSMeta.Status, admin.OCSMeta.StatusCode, admin.OCSMeta.Message)
		}))
		if err := p.SetQuota(context.Background(), "u1", core.MustBytes(53687091200)); err != nil {
			t.Fatal(err)
		}
		if gotForm != "key=quota&value=53687091200" {
			t.Errorf("form = %q", gotForm)
		}
	})

	t.Run("unlimited is written as none", func(t *testing.T) {
		var gotValue string
		p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			gotValue = r.Form.Get("value")
			w.Write([]byte(`{"ocs":{"meta":{"status":"ok","statuscode":200},"data":[]}}`))
		}))
		if err := p.SetQuota(context.Background(), "u1", core.Unlimited()); err != nil {
			t.Fatal(err)
		}
		if gotValue != "none" {
			t.Errorf("value = %q, want \"none\": a numeric -3 is parsed as a byte count", gotValue)
		}
	})

	t.Run("an unknown quota is never written", func(t *testing.T) {
		p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("SetQuota must not reach the instance with an unknown quota")
		}))
		if err := p.SetQuota(context.Background(), "u1", core.Unknown()); err == nil {
			t.Fatal("want an error")
		}
	})
}

func TestTransportErrorsAreTyped(t *testing.T) {
	t.Run("throttling is not a failure", func(t *testing.T) {
		p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"ocs":{"meta":{"status":"failure","statuscode":429,"message":"Too many requests"},"data":[]}}`))
		}))
		err := p.SetQuota(context.Background(), "u1", core.MustBytes(1024))
		retryAfter, ok := core.Throttled(err)
		if !ok {
			t.Fatalf("err = %v, want a ThrottledError: 50 writes per 10 minutes is a limit, not a failure", err)
		}
		if retryAfter != 2*time.Minute {
			t.Errorf("RetryAfter = %s, want 2m from the header", retryAfter)
		}
	})

	t.Run("a status that disagrees with the envelope is trusted neither way", func(t *testing.T) {
		p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The v1 convention: an error body under HTTP 200. On v2 this is a
			// contradiction, not a value.
			w.Write([]byte(`{"ocs":{"meta":{"status":"failure","statuscode":997,"message":"Unauthorised"},"data":[]}}`))
		}))
		_, err := p.ListAccounts(context.Background())
		var provErr *core.ProviderError
		if !asProviderError(err, &provErr) {
			t.Fatalf("err = %v, want a ProviderError", err)
		}
	})

	t.Run("an unreachable instance is unreachable, not unauthorized", func(t *testing.T) {
		p, err := New(core.ProviderSettings{
			Instance:    core.ProviderInstance{Type: providerType, Name: "cloud", ConfigRef: "X"},
			BaseURL:     "http://127.0.0.1:1",
			Credentials: map[string]core.Secret{"username": "u", "password": "p"},
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

func TestHealthReadsWithoutProbingWrites(t *testing.T) {
	var methods []string
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		w.Write([]byte(`{"ocs":{"meta":{"status":"ok","statuscode":200},"data":{"version":{"major":34,"string":"34.0.4"}}}}`))
	}))

	got := p.Health(context.Background())
	if !got.Reachable || got.Err != nil {
		t.Fatalf("health = %+v", got)
	}
	if got.Version != "34.0.4" || !got.InSupported {
		t.Errorf("version = %q, inSupported = %v", got.Version, got.InSupported)
	}
	// ADR-0025: Health reads. It must not write, and it must not read user
	// details either, which is what creates home folders.
	for _, m := range methods {
		if m != "GET "+pathCapabilities {
			t.Errorf("Health called %s: it must only read capabilities", m)
		}
	}
}

func TestNormalizeQuotaRefusesUnknown(t *testing.T) {
	p := &Provider{}
	if got := p.NormalizeQuota(core.Unknown()); got.IsKnown() {
		t.Errorf("NormalizeQuota(Unknown) = %v, want Unknown", got)
	}
	if got := p.NormalizeQuota(core.Unlimited()); !got.Equal(core.Unlimited()) {
		t.Errorf("NormalizeQuota(Unlimited) = %v, want Unlimited", got)
	}
	if got := p.NormalizeQuota(core.MustBytes(26844594176)); !got.Equal(core.MustBytes(26843545600)) {
		t.Errorf("NormalizeQuota = %v, want the value the instance really stores", got)
	}
}

func TestNewRequiresWriteCapableCredentials(t *testing.T) {
	base := core.ProviderSettings{
		Instance: core.ProviderInstance{Type: providerType, Name: "cloud", ConfigRef: "NUNO_PROVIDER_CLOUD"},
		BaseURL:  "https://cloud.example.org",
	}
	for _, creds := range []map[string]core.Secret{
		{},
		{"username": "nuno"},
		{"password": "hunter2"},
	} {
		s := base
		s.Credentials = creds
		if _, err := New(s); err == nil {
			t.Errorf("New with credentials %v must fail", len(creds))
		}
	}

	s := base
	s.BaseURL = "ftp://cloud.example.org"
	s.Credentials = map[string]core.Secret{"username": "nuno", "password": "hunter2"}
	if _, err := New(s); err == nil {
		t.Error("a non-HTTP URL must fail")
	}
}

func asProviderError(err error, target **core.ProviderError) bool {
	return errors.As(err, target)
}
