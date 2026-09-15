// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/reconcile"
)

type fakeActor struct {
	reconciled  bool
	consent     bool
	filter      reconcile.PlanFilter
	observed    bool
	tier        core.Tier
	overrideSet string
	cleared     bool
	linked      string
	unlinked    bool
	issued      string
	revoked     int64
	err         error
	report      reconcile.ApplyReport
}

func (f *fakeActor) Reconcile(_ context.Context, consent bool, filter reconcile.PlanFilter) (reconcile.ApplyReport, error) {
	f.reconciled, f.consent, f.filter = true, consent, filter
	return f.report, f.err
}
func (f *fakeActor) Observe(context.Context) error { f.observed = true; return f.err }
func (f *fakeActor) SaveTier(_ context.Context, tier core.Tier) error {
	f.tier = tier
	return f.err
}
func (f *fakeActor) SetOverride(_ context.Context, uid, provider string, quota core.Quota, clear bool) error {
	f.overrideSet = uid + "/" + provider + "/" + quota.Encode()
	f.cleared = clear
	return f.err
}
func (f *fakeActor) Link(_ context.Context, uid, provider, externalID string, unlink bool) error {
	f.linked = uid + "/" + provider + "/" + externalID
	f.unlinked = unlink
	return f.err
}
func (f *fakeActor) IssueKey(_ context.Context, label string) (core.Secret, error) {
	f.issued = label
	return core.Secret("issued-key-value"), f.err
}
func (f *fakeActor) RevokeKey(_ context.Context, id int64) error { f.revoked = id; return f.err }

func actionRoutes(t *testing.T, actor Actor, password core.Secret) http.Handler {
	t.Helper()
	return Routes(Options{
		Version: "test", Store: populated(), Actor: actor, Log: discard(), AdminPassword: password,
	})
}

func post(t *testing.T, mux http.Handler, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestActionsWorkAsPlainForms(t *testing.T) {
	actor := &fakeActor{}
	mux := actionRoutes(t, actor, "")

	rec := post(t, mux, "/actions/reconcile", url.Values{"allow_shrink": {"1"}, "user": {"alice"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d, want a redirect back to the page: %s", rec.Code, rec.Body.String())
	}
	if !actor.reconciled || !actor.consent || actor.filter.UID != "alice" {
		t.Errorf("actor = %+v", actor)
	}

	if rec := post(t, mux, "/actions/observe", nil, nil); rec.Code != http.StatusSeeOther || !actor.observed {
		t.Errorf("observe: code = %d, observed = %v", rec.Code, actor.observed)
	}
}

func TestReconcileWithoutConsentSaysHowToConsent(t *testing.T) {
	actor := &fakeActor{report: reconcile.ApplyReport{Guarded: 1}}
	mux := actionRoutes(t, actor, "")

	rec := post(t, mux, "/actions/reconcile", nil, nil)
	if actor.consent {
		t.Error("consent must not be implied by clicking the button")
	}
	location := rec.Header().Get("Location")
	if !strings.Contains(location, "guarded") {
		t.Errorf("the message must mention the guarded change: %q", location)
	}
}

func TestTierFormTreatsAnEmptyAllocationAsAbsent(t *testing.T) {
	actor := &fakeActor{}
	mux := actionRoutes(t, actor, "")

	rec := post(t, mux, "/actions/tier", url.Values{
		"name":     {"standard"},
		"budget":   {"200GiB"},
		"alloc_10": {"25%"},
		// Left blank on purpose: the tier says nothing about provider 20.
		"alloc_20":   {""},
		"is_default": {"1"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if actor.tier.Name != "standard" || !actor.tier.IsDefault {
		t.Fatalf("tier = %+v", actor.tier)
	}
	if !actor.tier.Budget.Equal(core.MustBytes(200 << 30)) {
		t.Errorf("budget = %v", actor.tier.Budget)
	}
	if len(actor.tier.Allocations) != 1 || actor.tier.Allocations[0].ProviderID != 10 {
		t.Fatalf("allocations = %+v, want only the one that was filled in", actor.tier.Allocations)
	}
	if actor.tier.Allocations[0].Mode != core.ModePercent || actor.tier.Allocations[0].Value != 25 {
		t.Errorf("allocation = %+v", actor.tier.Allocations[0])
	}
}

func TestBadInputIsReportedNotApplied(t *testing.T) {
	cases := []struct {
		name string
		path string
		form url.Values
		want string
	}{
		{"a tier with no name", "/actions/tier", url.Values{"budget": {"1GiB"}}, "needs a name"},
		{"an unparseable budget", "/actions/tier", url.Values{"name": {"x"}, "budget": {"lots"}}, "budget"},
		{"an unparseable allocation", "/actions/tier", url.Values{"name": {"x"}, "alloc_10": {"loads"}}, "allocation"},
		{"an unparseable override", "/actions/override", url.Values{"user": {"alice"}, "provider": {"cloud"}, "quota": {"heaps"}}, "size"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actor := &fakeActor{}
			mux := actionRoutes(t, actor, "")
			rec := post(t, mux, c.path, c.form, nil)

			location := rec.Header().Get("Location")
			if !strings.Contains(location, "err=") {
				t.Fatalf("location = %q, want an error message", location)
			}
			// The message is url encoded, so it is decoded before being read.
			message, err := url.QueryUnescape(strings.TrimPrefix(location, "/?err="))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.ToLower(message), c.want) {
				t.Errorf("message = %q, want it to mention %q", message, c.want)
			}
			if actor.tier.Name != "" || actor.overrideSet != "" {
				t.Error("nothing should have been applied")
			}
		})
	}
}

func TestOverrideClear(t *testing.T) {
	actor := &fakeActor{}
	mux := actionRoutes(t, actor, "")

	post(t, mux, "/actions/override", url.Values{
		"user": {"alice"}, "provider": {"cloud"}, "clear": {"1"},
	}, nil)
	if !actor.cleared {
		t.Error("clear must reach the actor")
	}

	// An empty value means clear too, rather than writing a zero.
	actor2 := &fakeActor{}
	mux2 := actionRoutes(t, actor2, "")
	post(t, mux2, "/actions/override", url.Values{
		"user": {"alice"}, "provider": {"cloud"}, "quota": {""},
	}, nil)
	if !actor2.cleared {
		t.Error("an empty ceiling must clear rather than set zero, which would block every upload")
	}
}

// A key is shown in the response body, never in a redirect: a query string
// lands in browser history and in proxy logs (FR-56).
func TestAnIssuedKeyIsShownOnceAndNotInAURL(t *testing.T) {
	actor := &fakeActor{}
	mux := actionRoutes(t, actor, "")

	rec := post(t, mux, "/actions/keys", url.Values{"label": {"homepage widget"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want the key rendered directly", rec.Code)
	}
	if actor.issued != "homepage widget" {
		t.Errorf("label = %q", actor.issued)
	}
	if !strings.Contains(rec.Body.String(), "issued-key-value") {
		t.Error("the key must be shown once")
	}
	if strings.Contains(rec.Header().Get("Location"), "issued-key-value") {
		t.Error("the key must never travel in a URL")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the page holding a key must not be cached")
	}

	rec = post(t, mux, "/actions/keys", url.Values{"revoke": {"7"}}, nil)
	if rec.Code != http.StatusSeeOther || actor.revoked != 7 {
		t.Errorf("revoke: code = %d, revoked = %d", rec.Code, actor.revoked)
	}
}

// The admin surface authenticates with a basic credential, which a browser
// will replay for a form another site submits.
func TestCrossSiteMutationsAreRefused(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"same origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusSeeOther},
		{"typed in the address bar", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusSeeOther},
		{"another site", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"another origin header", map[string]string{"Origin": "https://evil.example.org"}, http.StatusForbidden},
		{"no headers at all, as curl sends", nil, http.StatusSeeOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actor := &fakeActor{}
			mux := actionRoutes(t, actor, "")
			rec := post(t, mux, "/actions/observe", nil, c.headers)
			if rec.Code != c.want {
				t.Fatalf("code = %d, want %d", rec.Code, c.want)
			}
			if c.want == http.StatusForbidden && actor.observed {
				t.Error("the action ran anyway")
			}
		})
	}
}

func TestActionsNeedTheAdminPassword(t *testing.T) {
	actor := &fakeActor{}
	mux := actionRoutes(t, actor, core.Secret("hunter2"))

	rec := post(t, mux, "/actions/observe", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if actor.observed {
		t.Error("an unauthenticated action ran")
	}
}

// With no actor the page is read-only and the routes do not exist, rather
// than offering buttons that cannot work (FR-55).
func TestWithNoActorThePageIsReadOnly(t *testing.T) {
	mux := Routes(Options{Version: "test", Store: populated(), Log: discard()})

	if rec := post(t, mux, "/actions/reconcile", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", rec.Code)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "/actions/reconcile") {
		t.Error("the page must not offer an action it cannot perform")
	}
}

// The page must say the thing that surprises every admin on day one (FR-19a).
func TestThePageSaysAStricterTierDoesNotRestrict(t *testing.T) {
	mux := actionRoutes(t, &fakeActor{}, "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{"most generous ceiling wins", "does not restrict"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
}
