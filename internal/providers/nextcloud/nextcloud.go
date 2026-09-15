// SPDX-License-Identifier: AGPL-3.0-or-later

// Package nextcloud adapts the OCS Provisioning API. Verified against
// Nextcloud 34.0.4; see the provider notes in ARCHITECTURE.md section 2.
package nextcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

// SupportedMajor is what this adapter was verified against. Outside it Nuno
// warns once per run and keeps going: the Provisioning API is stable and a
// version matrix is not worth its cost. See ADR-0015.
const SupportedMajor = 34

const (
	pathCapabilities = "/ocs/v2.php/cloud/capabilities"
	pathUserDetails  = "/ocs/v2.php/cloud/users/details"
	pathUsers        = "/ocs/v2.php/cloud/users"
)

type Provider struct {
	client   *client
	instance core.ProviderInstance
	now      func() time.Time
}

// New builds the adapter. Registration happens in the composition root, which
// is the only place that imports this package.
func New(s core.ProviderSettings) (core.Provider, error) {
	username, ok := s.Credential("username")
	if !ok {
		return nil, fmt.Errorf("%s_USERNAME is required", s.Instance.ConfigRef)
	}
	password, ok := s.Credential("password")
	if !ok {
		return nil, fmt.Errorf("%s_PASSWORD is required: an app password cannot write a quota, so this must be a real admin password or an address whitelisted in allowed_no_password_confirmation_ranges", s.Instance.ConfigRef)
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	c, err := newClient(s.BaseURL, username.Reveal(), password, timeout)
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
		// Nextcloud has an instance default, but it is system config that OCS
		// does not expose, and FR-24 is Later anyway. See ADR-0019.
		CanSetDefaultQuota: false,
		// Group membership comes from the identity source, not from here.
		HasGroups: false,
		// The naked id is never a match key on its own, but it carries the
		// OIDC subject when oidc_login registered the account. See ADR-0028.
		MatchKeys: []core.MatchKey{core.MatchSubject, core.MatchEmail, core.MatchUsername},
	}
}

// Health reads: reachability, version, and whether the credential
// authenticates. It does not probe writes, and it does not read user details,
// so it has no side effect on the instance. See ADR-0025.
func (p *Provider) Health(ctx context.Context) core.HealthResult {
	data, err := p.client.get(ctx, pathCapabilities)
	if err != nil {
		// An auth failure means the instance answered, so it is reachable.
		return core.HealthResult{Reachable: !core.IsUnreachable(err), Err: err}
	}

	var payload struct {
		Version struct {
			Major  int    `json:"major"`
			String string `json:"string"`
		} `json:"version"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return core.HealthResult{
			Reachable: true,
			Err: &core.ProviderError{
				Provider: providerType, Op: "GET " + pathCapabilities,
				Message: "capabilities carried no version", Err: err,
			},
		}
	}
	version := payload.Version.String
	if version == "" {
		version = strconv.Itoa(payload.Version.Major)
	}
	return core.HealthResult{
		Reachable:   true,
		Version:     version,
		InSupported: payload.Version.Major == SupportedMajor,
	}
}

// ListAccounts reads every account with its configured quota and usage.
//
// This call is not free of side effects: resolving a user's details creates
// the home folder and its files directory when they are missing, so an
// observe pass materializes home directories for people who never logged in.
// Documented rather than avoided, because there is no read that does not.
func (p *Provider) ListAccounts(ctx context.Context) ([]core.Account, error) {
	data, err := p.client.get(ctx, pathUserDetails)
	if err != nil {
		return nil, err
	}

	// ocs.data.users is a map keyed by account id, not an array, and that key
	// is the ExternalID. See ADR-0021 point 2.
	var payload struct {
		Users map[string]json.RawMessage `json:"users"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &core.ProviderError{
			Provider: providerType, Op: "GET " + pathUserDetails,
			Message: "ocs.data did not decode", Err: err,
		}
	}
	if payload.Users == nil {
		return nil, &core.ProviderError{
			Provider: providerType, Op: "GET " + pathUserDetails,
			Message: "ocs.data has no users object",
		}
	}

	observedAt := p.now().UTC()
	accounts := make([]core.Account, 0, len(payload.Users))
	var errs []error
	for id, raw := range payload.Users {
		account, err := p.decodeAccount(id, raw, observedAt)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		accounts = append(accounts, account)
	}
	if len(errs) > 0 {
		// One undecodable account must not hide the others, and must not pass
		// silently either.
		return accounts, errors.Join(errs...)
	}
	return accounts, nil
}

// GetAccount reads one account, which is what the apply-time re-check needs:
// re-listing has side effects and does not scale.
func (p *Provider) GetAccount(ctx context.Context, externalID string) (core.Account, error) {
	if externalID == "" {
		return core.Account{}, errors.New("external id cannot be empty")
	}
	data, err := p.client.get(ctx, pathUsers+"/"+url.PathEscape(externalID))
	if err != nil {
		return core.Account{}, err
	}
	return p.decodeAccount(externalID, data, p.now().UTC())
}

func (p *Provider) NormalizeQuota(q core.Quota) core.Quota {
	switch q.Kind {
	case core.KindUnlimited:
		return q
	case core.KindBytes:
		n, err := normalizeBytes(q.Bytes)
		if err != nil {
			// Out of the representable range. Unknown is the only honest
			// answer, and nothing is written against it.
			return core.Unknown()
		}
		normalized, err := core.BytesQuota(n)
		if err != nil {
			return core.Unknown()
		}
		return normalized
	}
	// NormalizeQuota(Unknown) is a programming error, and returning Unknown
	// keeps it from becoming a write.
	return core.Unknown()
}

// SetQuota writes one account's quota. The value is expected to be normalized
// already: the planner compares normalized to observed, so writing something
// else would make the next plan non-empty.
func (p *Provider) SetQuota(ctx context.Context, externalID string, q core.Quota) error {
	if externalID == "" {
		return errors.New("external id cannot be empty")
	}
	value, err := formatQuotaValue(q)
	if err != nil {
		return err
	}
	form := url.Values{"key": {"quota"}, "value": {value}}
	_, err = p.client.put(ctx, pathUsers+"/"+url.PathEscape(externalID), form)
	if err == nil {
		return nil
	}

	// files/allow_unlimited_quota can refuse an explicit unlimited. That is a
	// capability answer, not a generic failure. See ADR-0014.
	var provErr *core.ProviderError
	if q.Kind == core.KindUnlimited && errors.As(err, &provErr) {
		return &core.UnsupportedError{
			Provider: providerType,
			Feature:  "unlimited quota",
			Detail:   fmt.Sprintf("the instance refused value=none (%s): check files/allow_unlimited_quota and files/max_quota", provErr.Message),
		}
	}
	return err
}

// wireUser is one entry of ocs.data.users. Every field is optional, so each
// one is a pointer or a raw message: a zero value that came from an absent
// field is exactly the confusion this adapter exists to avoid.
type wireUser struct {
	ID                  string          `json:"id"`
	Enabled             *bool           `json:"enabled"`
	Email               *string         `json:"email"`
	DisplayName         string          `json:"displayname"`
	FirstLoginTimestamp *int64          `json:"firstLoginTimestamp"`
	Quota               json.RawMessage `json:"quota"`
}

// wireQuota is the quota object. On a storage error the whole thing
// serializes as [], which is why the caller inspects the raw bytes first.
type wireQuota struct {
	Quota    any             `json:"quota"`
	Used     *int64          `json:"used"`
	Total    json.RawMessage `json:"total"`
	Relative *float64        `json:"relative"`
}

func (p *Provider) decodeAccount(externalID string, raw json.RawMessage, observedAt time.Time) (core.Account, error) {
	var u wireUser
	if err := json.Unmarshal(raw, &u); err != nil {
		return core.Account{}, &core.ProviderError{
			Provider: providerType, Op: "decode account " + externalID,
			Message: "account did not decode", Err: err,
		}
	}

	id := externalID
	if id == "" {
		id = u.ID
	}
	account := core.Account{
		ExternalID: id,
		// The id doubles as the username here. It may be a UUID, and nothing
		// downstream treats it as meaningful.
		Username:   id,
		Subject:    subjectFrom(id),
		Enabled:    u.Enabled != nil && *u.Enabled,
		Deleted:    false, // Nextcloud has no soft deletion
		ObservedAt: observedAt,
	}
	if u.Email != nil {
		account.Email = strings.TrimSpace(*u.Email)
	}

	configured, used, neverUsed, err := decodeQuotaObject(u.Quota, u.FirstLoginTimestamp)
	if err != nil {
		return core.Account{}, &core.ProviderError{
			Provider: providerType, Op: "decode account " + id,
			Message: err.Error(),
		}
	}
	account.Quota = configured
	account.Used = used
	account.NeverUsed = neverUsed
	return account, nil
}

// decodeQuotaObject applies the usage rule, which ADR-0024 split by what the
// response actually shows:
//
//   - absent, [], or missing total, relative or used: Unknown, because the
//     lookup failed and nothing can be concluded
//   - complete with firstLoginTimestamp 0: known and zero, because nobody can
//     upload without authenticating, so the account exists and holds nothing
//   - otherwise: the reported used
//
// The configured quota is always read from quota.quota, never from
// quota.total, which is the effective space or a sentinel.
func decodeQuotaObject(raw json.RawMessage, firstLogin *int64) (configured, used core.Quota, neverUsed bool, err error) {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || trimmed == "null" || strings.HasPrefix(trimmed, "[") {
		// [] is what a storage error serializes to.
		return core.Unknown(), core.Unknown(), false, nil
	}

	var q wireQuota
	if err := json.Unmarshal(raw, &q); err != nil {
		return core.Unknown(), core.Unknown(), false, fmt.Errorf("quota object did not decode: %w", err)
	}

	configured, err = decodeQuotaValue(q.Quota)
	if err != nil {
		return core.Unknown(), core.Unknown(), false, err
	}

	if len(q.Total) == 0 || q.Relative == nil || q.Used == nil {
		return configured, core.Unknown(), false, nil
	}
	if firstLogin != nil && *firstLogin == 0 {
		zero, err := core.BytesQuota(0)
		if err != nil {
			return configured, core.Unknown(), false, err
		}
		return configured, zero, true, nil
	}
	if *q.Used < 0 {
		// Not a sentinel this adapter knows, and guessing would mean writing
		// against a number nobody read.
		return configured, core.Unknown(), false, nil
	}
	usedQuota, err := core.BytesQuota(*q.Used)
	if err != nil {
		return configured, core.Unknown(), false, err
	}
	return configured, usedQuota, false, nil
}

// subjectFrom returns the account id when it has the shape of a UUID, which is
// what an OIDC subject looks like on the stacks this was built for. The join
// is safe because it acts on two providers reporting the same value, not on a
// value looking like a subject. See ADR-0028.
func subjectFrom(id string) string {
	if !uuidLike(id) {
		return ""
	}
	return id
}

func uuidLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
