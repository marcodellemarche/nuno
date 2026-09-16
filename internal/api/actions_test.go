// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
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
	mapped      string
	unmapped    bool
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
func (f *fakeActor) MapGroup(_ context.Context, groupUUID, tier string, unmap bool) error {
	f.mapped = groupUUID + "/" + tier
	f.unmapped = unmap
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

// The inline editor and the plain form are the same route. One gets the value
// to swap in, the other a redirect to follow (ADR-0030).
func TestTheInlineEditorGetsTheValueAndTheFormGetsARedirect(t *testing.T) {
	inline := map[string]string{"X-Requested-With": "nuno-inline-edit"}

	actor := &fakeActor{}
	mux := actionRoutes(t, actor, "")
	rec := post(t, mux, "/actions/override", url.Values{
		"user": {"alice"}, "provider": {"cloud"}, "quota": {"150GiB"},
	}, inline)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want the value back: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if body["value"] != "150 GiB" {
		t.Errorf("value = %q, want what the page should now show", body["value"])
	}
	// The person's total is the sum of their ceilings, so the card's header
	// comes back with the response too, or it would stay stale.
	if body["budget"] == "" || body["used"] == "" {
		t.Errorf("body = %q, want the recomputed totals as well", rec.Body.String())
	}
	if body["row_percent"] == "" {
		t.Errorf("body = %q, want the edited row's share bar recomputed too", rec.Body.String())
	}
	if actor.overrideSet == "" {
		t.Error("the same handler must still do the work")
	}

	// A bad value comes back as an error the row can show, not as a redirect
	// the fetch would silently follow.
	rec = post(t, mux, "/actions/override", url.Values{
		"user": {"alice"}, "provider": {"cloud"}, "quota": {"heaps"},
	}, inline)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["err"] == "" {
		t.Errorf("body = %q, want a message to show inline", rec.Body.String())
	}

	// With no such header nothing changes: the browser is sent back to the
	// page, which is what makes the form work with no JavaScript.
	rec = post(t, mux, "/actions/override", url.Values{
		"user": {"alice"}, "provider": {"cloud"}, "quota": {"150GiB"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("code = %d, want a redirect", rec.Code)
	}
}

// One action route serves five pages, so a form says where it came from.
func TestAFormComesBackToThePageItWasSubmittedFrom(t *testing.T) {
	cases := []struct {
		name string
		back string
		want string
	}{
		{"the page that submitted it", "/tiers", "/tiers?ok="},
		{"nothing at all", "", "/?ok="},
		{"another site", "//evil.example.org", "/?ok="},
		{"a path with a query of its own", "/tiers?q=x", "/tiers?ok="},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mux := actionRoutes(t, &fakeActor{}, "")
			rec := post(t, mux, "/actions/observe", url.Values{"return": {c.back}}, nil)
			if location := rec.Header().Get("Location"); !strings.HasPrefix(location, c.want) {
				t.Errorf("location = %q, want it to start with %q", location, c.want)
			}
		})
	}
}

func TestGroupChipsMapAndUnmap(t *testing.T) {
	actor := &fakeActor{}
	mux := actionRoutes(t, actor, "")

	if rec := post(t, mux, "/actions/group", url.Values{
		"group": {"g-staff"}, "tier": {"pro"},
	}, nil); rec.Code != http.StatusSeeOther || actor.mapped != "g-staff/pro" {
		t.Errorf("map: code = %d, mapped = %q", rec.Code, actor.mapped)
	}
	if actor.unmapped {
		t.Error("adding a chip must not unmap")
	}

	if rec := post(t, mux, "/actions/group", url.Values{
		"group": {"g-staff"}, "unmap": {"1"},
	}, nil); rec.Code != http.StatusSeeOther || !actor.unmapped {
		t.Errorf("unmap: code = %d, unmapped = %v", rec.Code, actor.unmapped)
	}

	// A chip with no group is a bug in the page, not something to act on.
	actor2 := &fakeActor{}
	mux2 := actionRoutes(t, actor2, "")
	rec := post(t, mux2, "/actions/group", url.Values{"tier": {"pro"}}, nil)
	if !strings.Contains(rec.Header().Get("Location"), "err=") || actor2.mapped != "" {
		t.Error("a group has to be named")
	}
}

// Saving one allocation must not drop the budget the tier resolves percentages
// against, which is why the row carries it (ADR-0020).
func TestEditingOneAllocationKeepsTheRestOfTheTier(t *testing.T) {
	actor := &fakeActor{}
	mux := actionRoutes(t, actor, "")

	rec := post(t, mux, "/actions/tier", url.Values{
		"name": {"standard"}, "budget": {"200 GiB"}, "is_default": {"1"},
		"alloc_10": {"25%"}, "alloc_20": {"50 GiB"}, "edited": {"10"},
	}, map[string]string{"X-Requested-With": "nuno-inline-edit"})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if !actor.tier.Budget.Equal(core.MustBytes(200<<30)) || !actor.tier.IsDefault {
		t.Errorf("tier = %+v, want the budget and the default flag kept", actor.tier)
	}
	if len(actor.tier.Allocations) != 2 {
		t.Errorf("allocations = %+v, want the untouched one carried along", actor.tier.Allocations)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["value"] != "25%" {
		t.Errorf("value = %q, want the allocation that was edited", body["value"])
	}
	if body["budget"] != "200 GiB" {
		t.Errorf("budget = %q, want the tier's budget back so the card header updates", body["budget"])
	}
	if body["overcommit"] != "0" {
		t.Errorf("overcommit = %q, want the derived flag back too", body["overcommit"])
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

// The precedence rule (a stricter tier does not restrict) lives in the README
// and docs, not on the page: the UI stays self-explanatory and free of
// paragraphs. What the page must still do is render every tier it knows.
func TestTheTiersPageRendersTheTiers(t *testing.T) {
	s := populated()
	s.policy = core.Policy{Tiers: map[int64]core.Tier{
		1: {ID: 1, Name: "standard", IsDefault: true, Budget: core.MustBytes(100 << 30)},
	}}
	mux := Routes(Options{Version: "test", Store: s, Actor: &fakeActor{}, Log: discard()})
	body := pageBody(t, mux, "/tiers")
	if !strings.Contains(body, "standard") {
		t.Error("the page must render the tiers it knows")
	}
}

// The editor shows the unit outside the field, so a bare number there is GiB,
// not bytes. Getting this wrong would write a 50-byte ceiling.
func TestABareNumberInTheEditorIsGiB(t *testing.T) {
	actor := &fakeActor{}
	mux := actionRoutes(t, actor, "")

	post(t, mux, "/actions/override", url.Values{
		"user": {"alice"}, "provider": {"cloud"}, "quota": {"50"},
	}, nil)
	if actor.overrideSet != "alice/cloud/bytes:53687091200" {
		t.Errorf("override = %q, want 50 GiB", actor.overrideSet)
	}
}

// A tier whose ceilings are absolute takes their sum as its budget: with no
// percentages left, the budget is what the ceilings add up to.
func TestATierWithAbsoluteAllocationsDerivesItsBudget(t *testing.T) {
	actor := &fakeActor{}
	mux := actionRoutes(t, actor, "")

	rec := post(t, mux, "/actions/tier", url.Values{
		"name": {"standard"}, "alloc_10": {"50"}, "alloc_20": {"150"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if !actor.tier.Budget.Equal(core.MustBytes(200 << 30)) {
		t.Errorf("budget = %v, want the sum of the ceilings", actor.tier.Budget)
	}
	for _, a := range actor.tier.Allocations {
		if a.Mode != core.ModeAbsolute {
			t.Errorf("allocation = %+v, want every one absolute", a)
		}
	}
}
