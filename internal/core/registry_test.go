// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"strings"
	"testing"
)

type stubProvider struct{ typ string }

func (s stubProvider) Type() string                        { return s.typ }
func (s stubProvider) Capabilities() Capabilities          { return Capabilities{} }
func (s stubProvider) Health(context.Context) HealthResult { return HealthResult{} }
func (s stubProvider) ListAccounts(context.Context) ([]Account, error) {
	return nil, nil
}
func (s stubProvider) GetAccount(context.Context, string) (Account, error) { return Account{}, nil }
func (s stubProvider) NormalizeQuota(q Quota) Quota                        { return q }
func (s stubProvider) SetQuota(context.Context, string, Quota) error       { return nil }

func TestRegistryRejectsDuplicateAndUnknown(t *testing.T) {
	r := NewRegistry()
	factory := func(s ProviderSettings) (Provider, error) { return stubProvider{s.Instance.Type}, nil }

	if err := r.Register("nextcloud", factory); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("nextcloud", factory); err == nil {
		t.Fatal("registering a type twice must fail: two adapters for one type is a wiring bug")
	}
	if err := r.Register("", factory); err == nil {
		t.Fatal("an empty type must fail")
	}
	if err := r.Register("immich", nil); err == nil {
		t.Fatal("a nil factory must fail")
	}

	_, err := r.Build(ProviderSettings{Instance: ProviderInstance{Type: "seafile"}})
	if err == nil || !strings.Contains(err.Error(), "nextcloud") {
		t.Fatalf("an unknown type must fail and name what is registered, got %v", err)
	}

	p, err := r.Build(ProviderSettings{Instance: ProviderInstance{Type: "nextcloud", Name: "cloud"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Type() != "nextcloud" {
		t.Fatalf("Type() = %q", p.Type())
	}
}

func TestCredentialTreatsBlankAsAbsent(t *testing.T) {
	s := ProviderSettings{Credentials: map[string]Secret{"password": "", "api_key": "k"}}
	if _, ok := s.Credential("password"); ok {
		t.Error("a blank password is a misconfiguration, not a credential")
	}
	if _, ok := s.Credential("missing"); ok {
		t.Error("an absent credential must report absent")
	}
	v, ok := s.Credential("api_key")
	if !ok || v.Reveal() != "k" {
		t.Errorf("api_key = %q, %v", v.Reveal(), ok)
	}
}
