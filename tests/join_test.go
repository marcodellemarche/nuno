// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/providers/immich"
	"github.com/marcodellemarche/nuno/internal/providers/nextcloud"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The join ADR-0021 found, asserted through both adapters rather than by
// eyeballing the fixtures: the OIDC subject Immich reports as oauthId is
// byte-identical to the same person's Nextcloud account id. It is what lets
// the linker cluster provider accounts with certainty, where email is null on
// at least one real account.
func TestTheOIDCSubjectJoinsAccountsAcrossProviders(t *testing.T) {
	ctx := context.Background()

	ncServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "nextcloud-users-details.json"))
	}))
	defer ncServer.Close()

	nc, err := nextcloud.New(core.ProviderSettings{
		Instance:    core.ProviderInstance{Type: "nextcloud", Name: "cloud", ConfigRef: "X"},
		BaseURL:     ncServer.URL,
		Credentials: map[string]core.Secret{"username": "u", "password": "p"},
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	immichMux := http.NewServeMux()
	immichMux.HandleFunc("/api/admin/users", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "immich-admin-users.json"))
	})
	immichMux.HandleFunc("/api/server/statistics", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "immich-server-statistics.json"))
	})
	immichServer := httptest.NewServer(immichMux)
	defer immichServer.Close()

	photos, err := immich.New(core.ProviderSettings{
		Instance:    core.ProviderInstance{Type: "immich", Name: "photos", ConfigRef: "Y"},
		BaseURL:     immichServer.URL,
		Credentials: map[string]core.Secret{"api_key": "k"},
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ncAccounts, err := nc.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	immichAccounts, err := photos.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Cluster by subject, the way the linker will.
	bySubject := map[string][]string{}
	for _, a := range ncAccounts {
		if a.Subject != "" {
			bySubject[a.Subject] = append(bySubject[a.Subject], "nextcloud:"+a.ExternalID)
		}
	}
	joined := 0
	for _, a := range immichAccounts {
		if a.Subject == "" {
			continue
		}
		if peers, ok := bySubject[a.Subject]; ok {
			joined++
			if len(peers) != 1 {
				t.Errorf("subject %s matched %v on Nextcloud, want exactly one account", a.Subject, peers)
			}
		}
	}
	if joined != 1 {
		t.Fatalf("joined %d accounts across the two providers, want 1: the captured stack has one person on both", joined)
	}

	// The person the join finds is the one whose Nextcloud account is a UUID,
	// and the account it does not find is the local admin, which has a null
	// email and so cannot be matched any other way either.
	var unmatchedNextcloud []string
	for _, a := range ncAccounts {
		if a.Subject == "" {
			unmatchedNextcloud = append(unmatchedNextcloud, a.ExternalID)
		}
	}
	if len(unmatchedNextcloud) != 1 || unmatchedNextcloud[0] != "admin" {
		t.Errorf("accounts without a subject = %v, want just the local admin", unmatchedNextcloud)
	}
}
