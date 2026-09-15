// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/marcodellemarche/nuno/internal/config"
	"github.com/marcodellemarche/nuno/internal/core"
)

type fakeProvider struct {
	typ    string
	health core.HealthResult
}

func (f fakeProvider) Type() string                             { return f.typ }
func (f fakeProvider) Capabilities() core.Capabilities          { return core.Capabilities{} }
func (f fakeProvider) Health(context.Context) core.HealthResult { return f.health }
func (f fakeProvider) ListAccounts(context.Context) ([]core.Account, error) {
	return nil, nil
}
func (f fakeProvider) GetAccount(context.Context, string) (core.Account, error) {
	return core.Account{}, nil
}
func (f fakeProvider) NormalizeQuota(q core.Quota) core.Quota             { return q }
func (f fakeProvider) SetQuota(context.Context, string, core.Quota) error { return nil }

func TestBuildRegistryRegistersTheShippedAdapters(t *testing.T) {
	registry, err := buildRegistry()
	if err != nil {
		t.Fatal(err)
	}
	got := registry.Types()
	for _, want := range []string{"immich", "nextcloud"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not registered, got %v", want, got)
		}
	}
}

// A provider that cannot be built is reported and skipped. Nuno degrades
// instead of refusing to start (FR-55).
func TestBuildProvidersSkipsWhatItCannotBuild(t *testing.T) {
	registry, err := buildRegistry()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Env{
		"NUNO_PROVIDER_CLOUD_TYPE":     "nextcloud",
		"NUNO_PROVIDER_CLOUD_URL":      "https://cloud.example.org",
		"NUNO_PROVIDER_CLOUD_USERNAME": "nuno",
		"NUNO_PROVIDER_CLOUD_PASSWORD": "hunter2",
		// No API key, so this one cannot be built.
		"NUNO_PROVIDER_PHOTOS_TYPE": "immich",
		"NUNO_PROVIDER_PHOTOS_URL":  "https://photos.example.org",
		// An unknown type is skipped the same way.
		"NUNO_PROVIDER_FILES_TYPE": "seafile",
		"NUNO_PROVIDER_FILES_URL":  "https://files.example.org",
	})
	if err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	instances := buildProviders(cfg, registry, log)

	if len(instances) != 1 || instances[0].config.Name != "cloud" {
		t.Fatalf("built %d providers, want only cloud: %+v", len(instances), instances)
	}
	for _, want := range []string{"photos", "files", "NUNO_PROVIDER_PHOTOS", "NUNO_PROVIDER_FILES"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the log must name %s and how to fix it", want)
		}
	}
	if strings.Contains(logged.String(), "hunter2") {
		t.Error("the log leaked a credential")
	}
}

func TestProvidersHealthExitCodes(t *testing.T) {
	cases := []struct {
		name      string
		instances []providerInstance
		wantCode  int
		wantOut   []string
	}{
		{
			name:     "nothing configured",
			wantCode: ExitConfig,
			wantOut:  []string{"No providers are configured"},
		},
		{
			name: "healthy",
			instances: []providerInstance{{
				config:   config.Provider{Name: "cloud", Type: "nextcloud"},
				provider: fakeProvider{health: core.HealthResult{Reachable: true, Version: "34.0.4", InSupported: true}},
			}},
			wantCode: ExitClean,
			// Write access is never proven by a read.
			wantOut: []string{"cloud", "nextcloud", "34.0.4", "unproven"},
		},
		{
			name: "outside the supported major is a warning, not a failure",
			instances: []providerInstance{{
				config:   config.Provider{Name: "photos", Type: "immich"},
				provider: fakeProvider{health: core.HealthResult{Reachable: true, Version: "v4.0.0"}},
			}},
			wantCode: ExitClean,
			wantOut:  []string{"outside the supported major"},
		},
		{
			name: "unreachable is an error",
			instances: []providerInstance{{
				config: config.Provider{Name: "cloud", Type: "nextcloud"},
				provider: fakeProvider{health: core.HealthResult{
					Err: &core.UnreachableError{Provider: "nextcloud", Err: errors.New("connection refused")},
				}},
			}},
			wantCode: ExitError,
			wantOut:  []string{"connection refused"},
		},
		{
			name: "reachable but rejected is an error that says so",
			instances: []providerInstance{{
				config: config.Provider{Name: "cloud", Type: "nextcloud"},
				provider: fakeProvider{health: core.HealthResult{
					Reachable: true, Version: "34.0.4", InSupported: true,
					Err: &core.AuthError{Provider: "nextcloud", Status: 403, Message: "Password confirmation is required"},
				}},
			}},
			wantCode: ExitError,
			wantOut:  []string{"Password confirmation is required", "34.0.4"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			code := providersHealth(context.Background(), &out, c.instances)
			if code != c.wantCode {
				t.Errorf("code = %d, want %d", code, c.wantCode)
			}
			for _, want := range c.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output does not contain %q:\n%s", want, out.String())
				}
			}
		})
	}
}
