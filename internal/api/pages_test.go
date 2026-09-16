// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/marcodellemarche/nuno/internal/core"
)

var pagePaths = []string{"/", "/tiers", "/services", "/accounts", "/activity"}

func pageBody(t *testing.T, mux http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// The inline editor is an enhancement. Every control it touches has to be a
// real form posting to a real route, or the page stops working when the script
// does not run (ADR-0030).
func TestEveryControlIsARealForm(t *testing.T) {
	store := populated()
	store.issues = []core.LinkIssue{{ProviderID: 10, Kind: core.IssueUnmanaged, ExternalID: "nc-ghost"}}
	mux := Routes(Options{Version: "test", Store: store, Actor: &fakeActor{}, Log: discard()})
	form := regexp.MustCompile(`<form[^>]*>`)

	for _, path := range pagePaths {
		t.Run(path, func(t *testing.T) {
			body := pageBody(t, mux, path)
			forms := form.FindAllString(body, -1)
			if len(forms) == 0 {
				t.Fatalf("%s offers nothing to do", path)
			}
			for _, tag := range forms {
				if strings.Contains(tag, `method="get"`) {
					continue
				}
				if !strings.Contains(tag, `method="post"`) {
					t.Errorf("%s: %s has no method", path, tag)
				}
				if !strings.Contains(tag, `action="/actions/`) {
					t.Errorf("%s: %s posts nowhere real", path, tag)
				}
			}
		})
	}
}

// FR-55: with no actor every page is read-only rather than offering buttons
// that cannot work.
func TestEveryPageIsReadOnlyWithoutAnActor(t *testing.T) {
	mux := routes(t, populated(), "")
	for _, path := range pagePaths {
		t.Run(path, func(t *testing.T) {
			body := pageBody(t, mux, path)
			if strings.Contains(body, "/actions/") {
				t.Error("the page offers an action it cannot perform")
			}
		})
	}
}

// A ceiling is edited where it is shown, so the row carries the whole form.
func TestAQuotaRowCarriesItsOwnOverrideForm(t *testing.T) {
	body := pageBody(t, actionRoutes(t, &fakeActor{}, ""), "/")
	for _, want := range []string{
		`action="/actions/override"`,
		`name="user" value="alice"`,
		`name="provider" value="cloud"`,
		`data-edit`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the quotas page is missing %q", want)
		}
	}
}

// The budget is an input percentages resolve against, not a total to retype, so
// the page shows it and carries it rather than offering it for editing
// (ADR-0020).
func TestTheTierPageShowsTheBudgetWithoutOfferingItForEditing(t *testing.T) {
	s := populated()
	s.policy = core.Policy{Tiers: map[int64]core.Tier{
		1: {ID: 1, Name: "standard", Budget: core.MustBytes(200 << 30), Allocations: []core.Allocation{
			{ProviderID: 10, Mode: core.ModePercent, Value: 50},
		}},
	}}
	mux := Routes(Options{Version: "test", Store: s, Actor: &fakeActor{}, Log: discard()})
	body := pageBody(t, mux, "/tiers")

	if !strings.Contains(body, "200 GiB") {
		t.Error("the budget is not shown")
	}
	if strings.Contains(body, `type="text" name="budget"`) && !strings.Contains(body, `name="name" placeholder="standard"`) {
		t.Error("the budget must not be an editable field on an existing tier")
	}
	if !strings.Contains(body, `type="hidden" name="budget" value="200 GiB"`) {
		t.Error("an allocation edit must carry the budget, or saving one would drop it")
	}
}

// The chips are the only place a group is mapped to a tier, so both directions
// have to be on the page.
func TestTheTierPageMapsAndUnmapsGroups(t *testing.T) {
	s := populated()
	s.groups = []core.Group{
		{ID: 1, SourceUUID: "g-staff", Name: "staff"},
		{ID: 2, SourceUUID: "g-interns", Name: "interns"},
	}
	s.users = append(s.users, core.User{ID: 2, UID: "bruno", Status: core.UserActive, GroupUUIDs: []string{"g-staff"}})
	s.policy = core.Policy{
		Tiers:      map[int64]core.Tier{1: {ID: 1, Name: "standard", IsDefault: true}},
		GroupTiers: map[string]int64{"g-staff": 1},
	}
	mux := Routes(Options{Version: "test", Store: s, Actor: &fakeActor{}, Log: discard()})
	body := pageBody(t, mux, "/tiers")

	for _, want := range []string{
		`action="/actions/group"`,
		`name="group" value="g-staff"`,
		`name="unmap" value="1"`,
		`<option value="g-interns">interns (0)</option>`,
		"Not mapped to a tier",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the tiers page is missing %q", want)
		}
	}
	// The count comes from the memberships already loaded with the people.
	if !strings.Contains(body, `staff <span class="count">1</span>`) {
		t.Error("a group chip must carry its member count")
	}
}

// FR-57: the table is the form. An account nobody owns is linked from its own
// row, and a manual link is undone from its own row.
func TestTheAccountsTableLinksAndUnlinksInPlace(t *testing.T) {
	s := populated()
	s.links = []core.AccountLink{{UserID: 1, ProviderID: 10, ExternalID: "nc-alice", Origin: core.LinkManual}}
	s.issues = []core.LinkIssue{
		{ProviderID: 10, Kind: core.IssueUnmanaged, ExternalID: "nc-ghost", Detail: "matches nobody"},
	}
	mux := Routes(Options{Version: "test", Store: s, Actor: &fakeActor{}, Log: discard()})
	body := pageBody(t, mux, "/accounts")

	for _, want := range []string{
		"Needs a decision",
		`<span class="kind kind-unmanaged" title="matches nobody">unmanaged</span>`,
		`name="external_id" value="nc-ghost"`,
		">Link<",
		">Unlink<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the accounts page is missing %q", want)
		}
	}

	// A matched link is not something an admin undoes: matching redoes itself
	// every observe.
	s.links[0].Origin = core.LinkMatched
	if strings.Contains(pageBody(t, mux, "/accounts"), ">Unlink<") {
		t.Error("only a manual link offers Unlink")
	}
}

// The guarded-changes control is a real checkbox, so consent survives with no
// script at all (FR-38, FR-39).
func TestTheGuardedToggleIsACheckbox(t *testing.T) {
	body := pageBody(t, actionRoutes(t, &fakeActor{}, ""), "/activity")
	if !strings.Contains(body, `<input type="checkbox" name="allow_shrink" value="1">`) {
		t.Error("the toggle must be a real checkbox")
	}
	if !strings.Contains(body, `class="toggle-track"`) {
		t.Error("the checkbox is styled as the switch the design asks for")
	}
}

// A person with no account anywhere still gets somewhere to set a ceiling, so
// a quota can be waiting before they first log in (ADR-0024).
func TestSomebodyWithNoAccountsStillGetsSomewhereToSetACeiling(t *testing.T) {
	s := populated()
	s.accounts = nil
	mux := Routes(Options{Version: "test", Store: s, Actor: &fakeActor{}, Log: discard()})
	body := pageBody(t, mux, "/")

	if !strings.Contains(body, "No linked account on any provider.") {
		t.Error("the card must say there is no account")
	}
	if !strings.Contains(body, "add a ceiling for another service") {
		t.Error("the card must still offer an override")
	}
}

func TestQuotasCanBeSearched(t *testing.T) {
	mux := actionRoutes(t, &fakeActor{}, "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?q=nobody", nil))
	if !strings.Contains(rec.Body.String(), "Nobody matches") {
		t.Error("a search that matches nobody must say so")
	}
	if strings.Contains(rec.Body.String(), "<h3>alice</h3>") {
		t.Error("the search did not filter")
	}
}

func TestTheScriptIsServedFromTheBinary(t *testing.T) {
	mux := routes(t, populated(), "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: the container must work with no outbound network", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nuno-inline-edit") {
		t.Error("that is not the inline editor")
	}
}
