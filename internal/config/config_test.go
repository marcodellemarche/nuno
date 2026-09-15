// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

func TestDefaults(t *testing.T) {
	cfg, err := Load(Env{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want the loopback default (NFR-14)", cfg.Addr)
	}
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if cfg.RefreshInterval != 15*time.Minute {
		t.Errorf("RefreshInterval = %s, want 15m per ADR-0022", cfg.RefreshInterval)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("Providers = %v, want none configured", cfg.Providers)
	}
}

func TestBindRefusesToListenWiderWithoutAnAcknowledgement(t *testing.T) {
	cases := []struct {
		addr string
		ack  bool
		ok   bool
	}{
		{"127.0.0.1:8080", false, true},
		{"localhost:8080", false, true},
		{"[::1]:8080", false, true},
		{"0.0.0.0:8080", false, false},
		{":8080", false, false},
		{"192.168.1.10:8080", false, false},
		{"0.0.0.0:8080", true, true},
		{"192.168.1.10:8080", true, true},
		{"127.0.0.1", false, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/ack=%v", c.addr, c.ack), func(t *testing.T) {
			env := Env{"NUNO_ADDR": c.addr}
			if c.ack {
				env["NUNO_ALLOW_PUBLIC_BIND"] = "true"
			}
			_, err := Load(env)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "NUNO_ADDR") {
				t.Errorf("the error must name the setting, got %v", err)
			}
		})
	}
}

func TestUsageListenerIsCheckedToo(t *testing.T) {
	_, err := Load(Env{"NUNO_USAGE_ADDR": "0.0.0.0:9090"})
	if err == nil || !strings.Contains(err.Error(), "NUNO_USAGE_ADDR") {
		t.Fatalf("err = %v, want the usage listener held to the same rule", err)
	}
}

func TestProviderDiscoveryFromThePrefix(t *testing.T) {
	cfg, err := Load(Env{
		"NUNO_PROVIDER_CLOUD_TYPE":      "nextcloud",
		"NUNO_PROVIDER_CLOUD_URL":       "https://cloud.example.org",
		"NUNO_PROVIDER_CLOUD_USERNAME":  "nuno",
		"NUNO_PROVIDER_CLOUD_PASSWORD":  "hunter2",
		"NUNO_PROVIDER_CLOUD_MATCH_KEY": "email",
		"NUNO_PROVIDER_PHOTOS_TYPE":     "immich",
		"NUNO_PROVIDER_PHOTOS_URL":      "https://photos.example.org",
		"NUNO_PROVIDER_PHOTOS_API_KEY":  "abc123",
		"NUNO_PROVIDER_PHOTOS_TIMEOUT":  "5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("got %d providers, want 2: %+v", len(cfg.Providers), cfg.Providers)
	}

	cloud, photos := cfg.Providers[0], cfg.Providers[1]
	if cloud.Name != "cloud" || cloud.Type != "nextcloud" {
		t.Errorf("cloud = %+v", cloud)
	}
	if cloud.ConfigRef() != "NUNO_PROVIDER_CLOUD" {
		t.Errorf("ConfigRef = %q", cloud.ConfigRef())
	}
	if cloud.MatchKey != core.MatchEmail {
		t.Errorf("MatchKey = %q", cloud.MatchKey)
	}
	if got, ok := cloud.Credential("password"); !ok || got.Reveal() != "hunter2" {
		t.Errorf("password = %q, %v", got.Reveal(), ok)
	}
	if got, ok := cloud.Credential("username"); !ok || got.Reveal() != "nuno" {
		t.Errorf("username = %q, %v", got.Reveal(), ok)
	}
	if _, ok := cloud.Credential("type"); ok {
		t.Error("TYPE is a setting, not a credential")
	}
	if photos.Timeout != 5*time.Second {
		t.Errorf("photos timeout = %s", photos.Timeout)
	}
	if got, ok := photos.Credential("api_key"); !ok || got.Reveal() != "abc123" {
		t.Errorf("api_key = %q, %v", got.Reveal(), ok)
	}
}

// A new adapter with a credential name core has never heard of must not need a
// change in this package (FR-25).
func TestUnknownProviderKeyBecomesACredential(t *testing.T) {
	cfg, err := Load(Env{
		"NUNO_PROVIDER_ZFS_TYPE":          "zfs",
		"NUNO_PROVIDER_ZFS_URL":           "unix:///var/run/zfs.sock",
		"NUNO_PROVIDER_ZFS_SOMETHING_NEW": "value",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cfg.Providers[0].Credential("something_new")
	if !ok || got.Reveal() != "value" {
		t.Fatalf("something_new = %q, %v", got.Reveal(), ok)
	}
}

func TestProviderRequiresTypeAndURL(t *testing.T) {
	_, err := Load(Env{"NUNO_PROVIDER_CLOUD_PASSWORD": "hunter2"})
	if err == nil {
		t.Fatal("a prefix with only a credential must fail")
	}
	for _, want := range []string{"TYPE", "URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %s, got %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error leaked the credential: %v", err)
	}
}

func TestInvalidMatchKeyIsRefused(t *testing.T) {
	_, err := Load(Env{
		"NUNO_PROVIDER_CLOUD_TYPE":      "nextcloud",
		"NUNO_PROVIDER_CLOUD_URL":       "https://cloud.example.org",
		"NUNO_PROVIDER_CLOUD_MATCH_KEY": "uid",
	})
	if err == nil || !strings.Contains(err.Error(), "MATCH_KEY") {
		t.Fatalf("err = %v: matching on the naked id is exactly what ADR-0013 forbids", err)
	}
}

func TestEveryErrorIsReportedAtOnce(t *testing.T) {
	_, err := Load(Env{
		"NUNO_LOG_LEVEL":         "chatty",
		"NUNO_REFRESH_INTERVAL":  "-5m",
		"NUNO_ALLOW_PUBLIC_BIND": "perhaps",
	})
	if err == nil {
		t.Fatal("want errors")
	}
	for _, want := range []string{"NUNO_LOG_LEVEL", "NUNO_REFRESH_INTERVAL", "NUNO_ALLOW_PUBLIC_BIND"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %s so one run fixes all of them, got %v", want, err)
		}
	}
}

func TestPrintingTheConfigCannotLeakASecret(t *testing.T) {
	cfg, err := Load(Env{
		"NUNO_ADMIN_KEY":               "admin-key-secret",
		"NUNO_ADMIN_PASSWORD":          "admin-password-secret",
		"NUNO_PROVIDER_CLOUD_TYPE":     "nextcloud",
		"NUNO_PROVIDER_CLOUD_URL":      "https://cloud.example.org",
		"NUNO_PROVIDER_CLOUD_PASSWORD": "provider-password-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		dump := fmt.Sprintf(format, cfg)
		for _, secret := range []string{"admin-key-secret", "admin-password-secret", "provider-password-secret"} {
			if strings.Contains(dump, secret) {
				t.Errorf("fmt %s leaked %s", format, secret)
			}
		}
	}
}
