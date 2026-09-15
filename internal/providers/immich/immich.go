// SPDX-License-Identifier: AGPL-3.0-or-later

// Package immich adapts the Immich admin API. Verified against Immich v3.2.0
// (spec v3.2.1); see the provider notes in ARCHITECTURE.md section 2.
package immich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

// SupportedMajor is the major this adapter was verified against. Immich has
// followed semver since v2 and confines breaking changes to majors, so
// detection plus a warning is enough. See ADR-0015.
const SupportedMajor = 3

const (
	pathVersion    = "/api/server/version"
	pathStatistics = "/api/server/statistics"
	pathAdminUsers = "/api/admin/users"
)

type Provider struct {
	client   *client
	instance core.ProviderInstance
	now      func() time.Time
}

func New(s core.ProviderSettings) (core.Provider, error) {
	apiKey, ok := s.Credential("api_key")
	if !ok {
		return nil, fmt.Errorf("%s_API_KEY is required, scoped to adminUser.read and adminUser.update", s.Instance.ConfigRef)
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	c, err := newClient(s.BaseURL, apiKey, timeout)
	if err != nil {
		return nil, err
	}
	return &Provider{client: c, instance: s.Instance, now: time.Now}, nil
}

func (p *Provider) Type() string { return providerType }

func (p *Provider) Capabilities() core.Capabilities {
	return core.Capabilities{
		CanReadUsers:    true,
		CanReadUsage:    true,
		CanSetUserQuota: true,
		// Immich has no instance-wide default quota at all. The OAuth
		// storageQuotaClaim applies only at account creation and is never
		// re-synced, which is the strongest single reason this project exists.
		CanSetDefaultQuota: false,
		HasGroups:          false,
		// oauthId carries the OIDC subject, which is an exact join to the same
		// person's Nextcloud account id. Immich has no username, so email is
		// the fallback. See ADR-0021.
		MatchKeys: []core.MatchKey{core.MatchSubject, core.MatchEmail},
	}
}

// Health reads the version and proves the credential. Unlike Nextcloud,
// reading users here has no side effect, so it is a safe way to prove the
// key's scope. It does not probe writes: that is a separate step (ADR-0025).
func (p *Provider) Health(ctx context.Context) core.HealthResult {
	raw, err := p.client.get(ctx, pathVersion)
	if err != nil {
		return core.HealthResult{Reachable: !core.IsUnreachable(err), Err: err}
	}
	var v struct {
		Major int `json:"major"`
		Minor int `json:"minor"`
		Patch int `json:"patch"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return core.HealthResult{Reachable: true, Err: &core.ProviderError{
			Provider: providerType, Op: "GET " + pathVersion,
			Message: "version did not decode", Err: err,
		}}
	}
	result := core.HealthResult{
		Reachable:   true,
		Version:     fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch),
		InSupported: v.Major == SupportedMajor,
	}

	// The version endpoint needs no key, so it proves nothing about the
	// credential. This does.
	if _, err := p.client.get(ctx, pathAdminUsers); err != nil {
		result.Err = err
	}
	return result
}

// ListAccounts reads accounts and their configured quota, then crosses the
// cached usage counter against the live aggregate. The authoritative value for
// Used is the aggregate; the cached counter is only the cross-check, because
// it is adjusted on asset create and delete and fully recomputed only by a
// nightly job an admin can disable. See ADR-0014 and ADR-0021 point 5.
func (p *Provider) ListAccounts(ctx context.Context) ([]core.Account, error) {
	raw, err := p.client.get(ctx, pathAdminUsers)
	if err != nil {
		return nil, err
	}
	var users []wireUser
	if err := json.Unmarshal(raw, &users); err != nil {
		return nil, &core.ProviderError{
			Provider: providerType, Op: "GET " + pathAdminUsers,
			Message: "admin users did not decode", Err: err,
		}
	}

	aggregate, statsErr := p.usageByUser(ctx)

	observedAt := p.now().UTC()
	accounts := make([]core.Account, 0, len(users))
	for _, u := range users {
		account, err := p.decodeAccount(u, aggregate, statsErr != nil, observedAt)
		if err != nil {
			return accounts, err
		}
		accounts = append(accounts, account)
	}
	// A failed statistics call is not fatal: the accounts are still worth
	// reporting, with their usage unknown, which is what stops every write
	// against them.
	return accounts, nil
}

func (p *Provider) GetAccount(ctx context.Context, externalID string) (core.Account, error) {
	if externalID == "" {
		return core.Account{}, errors.New("external id cannot be empty")
	}
	raw, err := p.client.get(ctx, pathAdminUsers+"/"+url.PathEscape(externalID))
	if err != nil {
		return core.Account{}, err
	}
	var u wireUser
	if err := json.Unmarshal(raw, &u); err != nil {
		return core.Account{}, &core.ProviderError{
			Provider: providerType, Op: "GET " + pathAdminUsers + "/" + externalID,
			Message: "account did not decode", Err: err,
		}
	}
	aggregate, statsErr := p.usageByUser(ctx)
	return p.decodeAccount(u, aggregate, statsErr != nil, p.now().UTC())
}

// NormalizeQuota is the identity: Immich stores the bytes it is given.
func (p *Provider) NormalizeQuota(q core.Quota) core.Quota {
	if q.Kind == core.KindUnknown {
		return core.Unknown()
	}
	return q
}

// SetQuota writes one account's quota.
//
// The body always carries the key. An omitted quotaSizeInBytes means "do not
// touch" to Immich, so a nil pointer marshalled with omitempty would silently
// do nothing where unlimited was meant. See the hazard in ADR-0011.
func (p *Provider) SetQuota(ctx context.Context, externalID string, q core.Quota) error {
	if externalID == "" {
		return errors.New("external id cannot be empty")
	}
	var body setQuotaBody
	switch q.Kind {
	case core.KindUnlimited:
		body.QuotaSizeInBytes = nil // marshals to an explicit null
	case core.KindBytes:
		n := q.Bytes
		body.QuotaSizeInBytes = &n
	default:
		return fmt.Errorf("refusing to write a %s quota", q.Kind)
	}

	// PUT is deprecated in v3 in favour of PATCH, which carries the same DTO
	// and permission but is deliberately excluded from the OpenAPI spec, so
	// codegen will never reveal the successor. PUT is what works today.
	_, err := p.client.put(ctx, pathAdminUsers+"/"+url.PathEscape(externalID), body)
	return err
}

type setQuotaBody struct {
	QuotaSizeInBytes *int64 `json:"quotaSizeInBytes"`
}

type wireUser struct {
	ID                string  `json:"id"`
	Email             string  `json:"email"`
	Name              string  `json:"name"`
	OAuthID           string  `json:"oauthId"`
	Status            string  `json:"status"`
	DeletedAt         *string `json:"deletedAt"`
	QuotaSizeInBytes  *int64  `json:"quotaSizeInBytes"`
	QuotaUsageInBytes *int64  `json:"quotaUsageInBytes"`
}

// usageByUser reads the live per-user aggregate, which is the honest source
// for usage.
func (p *Provider) usageByUser(ctx context.Context) (map[string]int64, error) {
	raw, err := p.client.get(ctx, pathStatistics)
	if err != nil {
		return nil, err
	}
	var stats struct {
		UsageByUser []struct {
			UserID string `json:"userId"`
			Usage  int64  `json:"usage"`
		} `json:"usageByUser"`
	}
	if err := json.Unmarshal(raw, &stats); err != nil {
		return nil, &core.ProviderError{
			Provider: providerType, Op: "GET " + pathStatistics,
			Message: "statistics did not decode", Err: err,
		}
	}
	usage := make(map[string]int64, len(stats.UsageByUser))
	for _, entry := range stats.UsageByUser {
		usage[entry.UserID] = entry.Usage
	}
	return usage, nil
}

func (p *Provider) decodeAccount(u wireUser, aggregate map[string]int64, statsFailed bool, observedAt time.Time) (core.Account, error) {
	account := core.Account{
		ExternalID: u.ID,
		Subject:    u.OAuthID,
		// Immich has no username. Leaving it empty is the honest answer, and
		// it is why email and the subject are the only match keys.
		Username:   "",
		Email:      strings.TrimSpace(u.Email),
		Deleted:    u.DeletedAt != nil && *u.DeletedAt != "",
		Enabled:    u.Status == "active",
		ObservedAt: observedAt,
	}
	if account.Deleted {
		account.Enabled = false
	}

	configured, err := decodeQuota(u.QuotaSizeInBytes)
	if err != nil {
		return core.Account{}, &core.ProviderError{
			Provider: providerType, Op: "decode account " + u.ID, Message: err.Error(),
		}
	}
	account.Quota = configured

	used, err := decodeUsage(u.QuotaUsageInBytes, aggregate, u.ID, statsFailed)
	if err != nil {
		return core.Account{}, &core.ProviderError{
			Provider: providerType, Op: "decode account " + u.ID, Message: err.Error(),
		}
	}
	account.Used = used
	return account, nil
}

// decodeQuota reads quotaSizeInBytes. A null is an explicit unlimited, and 0
// is a legal quota that blocks all uploads.
func decodeQuota(raw *int64) (core.Quota, error) {
	if raw == nil {
		return core.Unlimited(), nil
	}
	if *raw < 0 {
		return core.Unknown(), fmt.Errorf("unexpected negative quota %d", *raw)
	}
	return core.BytesQuota(*raw)
}

// decodeUsage applies the divergence rule. Usage is Unknown when the cached
// counter and the live aggregate disagree beyond the threshold, because at
// that point neither can be trusted and FR-38a offers no override.
//
// An account absent from the aggregate has no assets, so its aggregate is
// zero. That is a measurement, not a gap: it still has to agree with the
// cached counter.
func decodeUsage(cached *int64, aggregate map[string]int64, userID string, statsFailed bool) (core.Quota, error) {
	if statsFailed || aggregate == nil {
		// Without the authoritative source there is nothing to conclude, and
		// the cached counter alone is not a trustworthy basis for the shrink
		// guardrail.
		return core.Unknown(), nil
	}
	live := aggregate[userID]
	if live < 0 {
		return core.Unknown(), nil
	}
	if cached != nil {
		if diverged(*cached, live) {
			return core.Unknown(), nil
		}
	}
	return core.BytesQuota(live)
}

// DivergenceFloor is the absolute half of the threshold. Measured divergence
// on a healthy account was 9.5 MB out of 9.8 GB, so a tighter floor would
// mark a working account permanently unwritable. See ADR-0021 point 5.
const DivergenceFloor = 64 << 20

func diverged(cached, live int64) bool {
	delta := cached - live
	if delta < 0 {
		delta = -delta
	}
	threshold := int64(DivergenceFloor)
	if onePercent := live / 100; onePercent > threshold {
		threshold = onePercent
	}
	return delta > threshold
}
