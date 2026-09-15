// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Package integration exercises the adapters against real services. It is a
// manual target and a weekly job, never part of go test ./... , because
// keeping a live Immich and Nextcloud green is a recurring cost that would
// otherwise be paid in abandoned tests.
//
// Run it with the stack up:
//
//	docker compose -f docker-compose.test.yml up -d --wait
//	make integration
//
// Each test skips rather than fails when its service is not configured, so a
// partial environment still proves what it can.
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/identity/ldap"
	"github.com/marcodellemarche/nuno/internal/providers/immich"
	"github.com/marcodellemarche/nuno/internal/providers/nextcloud"
)

func env(t *testing.T, key, fallback string) string {
	t.Helper()
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Skipf("%s is not set, so there is nothing to test against", key)
	}
	return value
}

func timeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 60*time.Second)
}

func TestNextcloudReadPath(t *testing.T) {
	url := env(t, "NUNO_IT_NEXTCLOUD_URL", "http://127.0.0.1:8081")
	user := env(t, "NUNO_IT_NEXTCLOUD_USERNAME", "admin")
	password := requireEnv(t, "NUNO_IT_NEXTCLOUD_PASSWORD")

	provider, err := nextcloud.New(core.ProviderSettings{
		Instance:    core.ProviderInstance{Type: "nextcloud", Name: "it", ConfigRef: "NUNO_IT_NEXTCLOUD"},
		BaseURL:     url,
		Credentials: map[string]core.Secret{"username": core.Secret(user), "password": core.Secret(password)},
		Timeout:     30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := timeout(t)
	defer cancel()

	health := provider.Health(ctx)
	if !health.Reachable || health.Err != nil {
		t.Fatalf("health = %+v", health)
	}
	t.Logf("nextcloud %s, in supported major: %v", health.Version, health.InSupported)

	accounts, err := provider.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) == 0 {
		t.Fatal("a fresh Nextcloud still has its admin account")
	}
	for _, account := range accounts {
		if account.ExternalID == "" {
			t.Error("an account with no external id cannot be written to")
		}
		// These are the invariants the contract tests assert against
		// recorded responses. Here they are checked against the real thing.
		if err := account.Used.ValidAsUsage(); err != nil {
			t.Errorf("%s: %v", account.ExternalID, err)
		}
		if err := account.Quota.Valid(); err != nil {
			t.Errorf("%s: %v", account.ExternalID, err)
		}
		t.Logf("%s: quota %s, used %s, never used %v",
			account.ExternalID, account.Quota, account.Used, account.NeverUsed)
	}

	// One account read on its own must agree with the same account in the
	// list, because apply-time re-check uses the single read.
	one, err := provider.GetAccount(ctx, accounts[0].ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if !one.Quota.Equal(accounts[0].Quota) {
		t.Errorf("GetAccount says %v and ListAccounts says %v", one.Quota, accounts[0].Quota)
	}
}

// The lossy round trip is the reason NormalizeQuota exists. This is the
// measurement the captured fixture holds, taken again against a live
// instance.
func TestNextcloudNormalizationMatchesTheInstance(t *testing.T) {
	url := env(t, "NUNO_IT_NEXTCLOUD_URL", "http://127.0.0.1:8081")
	user := env(t, "NUNO_IT_NEXTCLOUD_USERNAME", "admin")
	password := requireEnv(t, "NUNO_IT_NEXTCLOUD_PASSWORD")

	provider, err := nextcloud.New(core.ProviderSettings{
		Instance:    core.ProviderInstance{Type: "nextcloud", Name: "it", ConfigRef: "NUNO_IT_NEXTCLOUD"},
		BaseURL:     url,
		Credentials: map[string]core.Secret{"username": core.Secret(user), "password": core.Secret(password)},
		Timeout:     30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := timeout(t)
	defer cancel()

	accounts, err := provider.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := accounts[0]
	original := target.Quota
	t.Logf("probing %s, whose quota is %s", target.ExternalID, original)

	// 26844594176 is the value the captured round trip lost: it comes back as
	// 26843545600 because it is stored through humanFileSize.
	const lossy = 26844594176
	want := provider.NormalizeQuota(core.MustBytes(lossy))

	if err := provider.SetQuota(ctx, target.ExternalID, core.MustBytes(lossy)); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if original.IsKnown() {
			if err := provider.SetQuota(restoreCtx, target.ExternalID, original); err != nil {
				t.Errorf("failed to restore %s to %s: %v", target.ExternalID, original, err)
			}
		}
	})

	after, err := provider.GetAccount(ctx, target.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Quota.Equal(want) {
		t.Fatalf("wrote %d, NormalizeQuota predicted %v, the instance stored %v",
			lossy, want, after.Quota)
	}
	t.Logf("wrote %d and the instance stored %v, as predicted", lossy, after.Quota)

	// And writing the normalized value back is a no-op, which is what keeps
	// the planner from looping forever.
	if err := provider.SetQuota(ctx, target.ExternalID, want); err != nil {
		t.Fatal(err)
	}
	again, err := provider.GetAccount(ctx, target.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Quota.Equal(want) {
		t.Errorf("the normalized value did not survive a second write: %v", again.Quota)
	}
}

// The Immich write path is the one thing that was never exercised against a
// live stack, which is why this test exists at all.
func TestImmichReadAndWritePath(t *testing.T) {
	url := env(t, "NUNO_IT_IMMICH_URL", "http://127.0.0.1:2283")
	apiKey := requireEnv(t, "NUNO_IT_IMMICH_API_KEY")

	provider, err := immich.New(core.ProviderSettings{
		Instance:    core.ProviderInstance{Type: "immich", Name: "it", ConfigRef: "NUNO_IT_IMMICH"},
		BaseURL:     url,
		Credentials: map[string]core.Secret{"api_key": core.Secret(apiKey)},
		Timeout:     30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := timeout(t)
	defer cancel()

	health := provider.Health(ctx)
	if !health.Reachable || health.Err != nil {
		t.Fatalf("health = %+v", health)
	}
	t.Logf("immich %s, in supported major: %v", health.Version, health.InSupported)

	accounts, err := provider.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) == 0 {
		t.Fatal("no accounts, so the key may lack adminUser.read")
	}

	target := accounts[0]
	original := target.Quota
	t.Logf("probing %s, whose quota is %s and usage is %s", target.ExternalID, original, target.Used)

	t.Cleanup(func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := provider.SetQuota(restoreCtx, target.ExternalID, original); err != nil {
			t.Errorf("failed to restore %s to %s: %v", target.ExternalID, original, err)
		}
	})

	// Immich stores the bytes it is given, so this is the identity.
	const want = 42 << 30
	if err := provider.SetQuota(ctx, target.ExternalID, core.MustBytes(want)); err != nil {
		t.Fatalf("write: %v, which may mean the key lacks adminUser.update", err)
	}
	after, err := provider.GetAccount(ctx, target.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Quota.Equal(core.MustBytes(want)) {
		t.Fatalf("wrote %d and read back %v", want, after.Quota)
	}

	// An explicit unlimited must be a null, not an omitted key, or Immich
	// reads it as "do not change" (ADR-0011).
	if err := provider.SetQuota(ctx, target.ExternalID, core.Unlimited()); err != nil {
		t.Fatal(err)
	}
	unlimited, err := provider.GetAccount(ctx, target.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if !unlimited.Quota.IsUnlimited() {
		t.Fatalf("wrote unlimited and read back %v: an omitted key means do not change", unlimited.Quota)
	}
	t.Log("unlimited round tripped, so the null is going out explicitly")
}

func TestLLDAPReadPath(t *testing.T) {
	url := env(t, "NUNO_IT_LDAP_URL", "ldap://127.0.0.1:3890")
	baseDN := env(t, "NUNO_IT_LDAP_BASE_DN", "dc=example,dc=org")
	bindDN := env(t, "NUNO_IT_LDAP_BIND_DN", "uid=admin,ou=people,dc=example,dc=org")
	password := requireEnv(t, "NUNO_IT_LDAP_BIND_PASSWORD")

	source, err := ldap.New(ldap.Config{
		URL: url, BaseDN: baseDN, BindDN: bindDN,
		BindPassword: core.Secret(password), Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := timeout(t)
	defer cancel()

	version, err := source.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("directory reports %q", version)

	read, err := source.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Snapshot.Users) == 0 {
		t.Fatal("a fresh LLDAP still has its admin user")
	}

	// The three assumptions ARCHITECTURE makes about LLDAP, checked against
	// the real thing rather than against a reading of its source.
	for _, user := range read.Snapshot.Users {
		if user.SourceUUID == "" {
			t.Errorf("%s has no uuid, so entryuuid is not being returned", user.UID)
		}
		if user.UID == "" {
			t.Errorf("a user with no uid came back: %+v", user)
		}
		t.Logf("%s (%s) in %d group(s)", user.UID, user.SourceUUID, len(user.GroupUUIDs))
	}
	for _, dn := range read.SkippedEntries {
		t.Logf("skipped %s", dn)
	}
	for _, dn := range read.UnresolvedMemberships {
		t.Logf("unresolved membership %s", dn)
	}
}
