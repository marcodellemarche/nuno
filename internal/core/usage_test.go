// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func input(accounts ...ExternalAccount) UsageInput {
	return UsageInput{
		Now:             now,
		RefreshInterval: 15 * time.Minute,
		Users: []User{
			{ID: 1, SourceUUID: "11111111", UID: "alice"},
			{ID: 2, SourceUUID: "22222222", UID: "bob"},
		},
		Providers: []ObservedProvider{
			{ID: 10, Type: "nextcloud", Name: "cloud", HasObserved: true, LastObserveOK: true},
			{ID: 20, Type: "immich", Name: "photos", HasObserved: true, LastObserveOK: true},
		},
		Accounts: accounts,
	}
}

func owned(providerID int64, userID int64, quota, used Quota) ExternalAccount {
	return ExternalAccount{
		ProviderID: providerID, ExternalID: "acct", UserID: &userID,
		Quota: quota, Used: used, Enabled: true,
		ObservedAt: now.Add(-time.Minute), ObserveOK: true,
	}
}

func TestBuildUsage(t *testing.T) {
	response := BuildUsage(input(
		owned(10, 1, MustBytes(53687091200), MustBytes(1073741824)),
		owned(20, 1, MustBytes(161061273600), MustBytes(48318382080)),
	))

	if response.Schema != UsageSchema {
		t.Errorf("schema = %d", response.Schema)
	}
	if len(response.Users) != 2 {
		t.Fatalf("got %d users, want both, including the one with no account", len(response.Users))
	}

	alice := response.Users[0]
	if alice.UserUUID != "11111111" {
		t.Fatalf("users must be sorted by uuid, got %s first", alice.UserUUID)
	}
	if alice.BudgetBytes == nil || *alice.BudgetBytes != 53687091200+161061273600 {
		t.Errorf("budget = %v, want the sum of observed ceilings", alice.BudgetBytes)
	}
	if alice.UsedBytes == nil || *alice.UsedBytes != 1073741824+48318382080 {
		t.Errorf("used = %v", alice.UsedBytes)
	}
	if !alice.Complete {
		t.Error("both providers answered, so the sums are complete")
	}
	if len(alice.Providers) != 2 || alice.Providers[0].Name != "cloud" {
		t.Errorf("providers = %+v, want them ordered by name", alice.Providers)
	}
	for _, p := range alice.Providers {
		if p.Status != StatusOK {
			t.Errorf("%s status = %q", p.Name, p.Status)
		}
		if p.Managed {
			t.Error("nothing manages anything before policy exists")
		}
	}

	// Someone with no account at all still appears, with nothing in it.
	bob := response.Users[1]
	if len(bob.Providers) != 0 {
		t.Errorf("bob providers = %+v", bob.Providers)
	}
	if bob.BudgetBytes == nil || *bob.BudgetBytes != 0 {
		t.Errorf("bob budget = %v, want zero rather than absent", bob.BudgetBytes)
	}
	if bob.UsedPercent != nil {
		t.Errorf("bob percent = %v, want null with no ceiling", bob.UsedPercent)
	}
}

// null means unlimited only under ok and stale. This is the case ADR-0026
// exists for: the read succeeded and the values are not conclusions.
func TestUnknownIsDistinctFromUnlimited(t *testing.T) {
	cases := []struct {
		name       string
		quota      Quota
		used       Quota
		wantStatus string
		wantNull   bool
		complete   bool
	}{
		{"unlimited is null and known", Unlimited(), MustBytes(100), StatusOK, true, true},
		{"an unreadable ceiling is unknown", Unknown(), MustBytes(100), StatusUnknown, true, false},
		{"unreadable usage is unknown too", MustBytes(1024), Unknown(), StatusUnknown, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response := BuildUsage(input(owned(10, 1, c.quota, c.used)))
			entry := response.Users[0]
			view := entry.Providers[0]

			if view.Status != c.wantStatus {
				t.Errorf("status = %q, want %q", view.Status, c.wantStatus)
			}
			if (view.QuotaBytes == nil) != c.wantNull {
				t.Errorf("quota_bytes = %v", view.QuotaBytes)
			}
			if entry.Complete != c.complete {
				t.Errorf("complete = %v, want %v", entry.Complete, c.complete)
			}

			// An unlimited ceiling must not leak into the sum as a number,
			// and an unknown one must not be counted as zero.
			if c.quota.IsUnlimited() && entry.BudgetBytes != nil {
				t.Errorf("budget = %v, want null when a ceiling is unlimited", entry.BudgetBytes)
			}
		})
	}
}

func TestStatusReflectsFreshnessAndFailure(t *testing.T) {
	t.Run("stale beyond twice the refresh interval", func(t *testing.T) {
		in := input()
		account := owned(10, 1, MustBytes(1024), MustBytes(512))
		account.ObservedAt = now.Add(-31 * time.Minute)
		in.Accounts = []ExternalAccount{account}

		view := BuildUsage(in).Users[0].Providers[0]
		if view.Status != StatusStale {
			t.Errorf("status = %q, want stale: half an hour without a successful read", view.Status)
		}
		// A stale number is still a number, so it is still reported.
		if view.QuotaBytes == nil {
			t.Error("a stale reading is the last known one, not nothing")
		}
	})

	t.Run("a failed observe makes it unavailable", func(t *testing.T) {
		in := input(owned(10, 1, MustBytes(1024), MustBytes(512)))
		in.Providers[0].LastObserveOK = false

		entry := BuildUsage(in).Users[0]
		if entry.Providers[0].Status != StatusUnavailable {
			t.Errorf("status = %q", entry.Providers[0].Status)
		}
		if entry.Providers[0].QuotaBytes != nil {
			t.Error("numbers from a failed read must not be presented as current")
		}
		if entry.Complete {
			t.Error("complete must be false when a provider did not contribute")
		}
	})

	t.Run("a provider that was never observed is unavailable, not ok", func(t *testing.T) {
		in := input(owned(10, 1, MustBytes(1024), MustBytes(512)))
		in.Providers[0].HasObserved = false
		if got := BuildUsage(in).Users[0].Providers[0].Status; got != StatusUnavailable {
			t.Errorf("status = %q", got)
		}
	})
}

// Go refuses to marshal NaN, which would turn the most privileged users into a
// 500 (ADR-0017).
func TestUsedPercentIsNullRatherThanNaN(t *testing.T) {
	for _, quota := range []Quota{Unlimited(), MustBytes(0)} {
		response := BuildUsage(input(owned(10, 1, quota, MustBytes(0))))
		view := response.Users[0].Providers[0]
		if view.UsedPercent != nil {
			t.Errorf("quota %v: percent = %v, want null", quota, view.UsedPercent)
		}

		body, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("the response must always marshal: %v", err)
		}
		if strings.Contains(string(body), "NaN") {
			t.Errorf("body contains NaN: %s", body)
		}
	}
}

func TestUsedPercentIsRounded(t *testing.T) {
	response := BuildUsage(input(owned(10, 1, MustBytes(1000), MustBytes(234))))
	view := response.Users[0].Providers[0]
	if view.UsedPercent == nil || *view.UsedPercent != 23.4 {
		t.Errorf("percent = %v, want 23.4", view.UsedPercent)
	}
}

// An unmanaged account belongs to nobody, so it appears in the link issues
// rather than in anyone's usage.
func TestUnmanagedAccountsAreNotInAnyonesUsage(t *testing.T) {
	in := input()
	in.Accounts = []ExternalAccount{{
		ProviderID: 10, ExternalID: "admin", UserID: nil,
		Quota: Unlimited(), Used: MustBytes(65575770),
		Enabled: true, ObservedAt: now, ObserveOK: true,
	}}
	for _, entry := range BuildUsage(in).Users {
		if len(entry.Providers) != 0 {
			t.Errorf("%s has %+v", entry.User, entry.Providers)
		}
	}
}

// A widget addresses fields by path, so the order has to be stable across
// calls regardless of the order rows come back in.
func TestOrderingIsStable(t *testing.T) {
	in := input(
		owned(20, 1, MustBytes(1), MustBytes(1)),
		owned(10, 1, MustBytes(2), MustBytes(2)),
	)
	in.Users[0], in.Users[1] = in.Users[1], in.Users[0]

	first := BuildUsage(in)
	second := BuildUsage(in)
	if first.Users[0].UserUUID != "11111111" || first.Users[1].UserUUID != "22222222" {
		t.Errorf("users = %v, %v", first.Users[0].UserUUID, first.Users[1].UserUUID)
	}
	if first.Users[0].Providers[0].Name != "cloud" {
		t.Errorf("providers = %+v", first.Users[0].Providers)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Error("two builds of one input must serialize identically")
	}
}

// The shape is a frozen contract, so the field names are asserted rather than
// assumed.
func TestTheSerializedShapeIsTheContract(t *testing.T) {
	body, err := json.Marshal(BuildUsage(input(owned(10, 1, MustBytes(1024), MustBytes(512)))))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"schema":1`, `"users":`, `"user":`, `"user_uuid":`, `"budget_bytes":`,
		`"used_bytes":`, `"used_percent":`, `"complete":`, `"providers":`,
		`"type":"nextcloud"`, `"name":"cloud"`, `"quota_bytes":`, `"managed":false`,
		`"status":"ok"`, `"observed_at":`,
	} {
		if !strings.Contains(string(body), field) {
			t.Errorf("the response is missing %s:\n%s", field, body)
		}
	}
}
