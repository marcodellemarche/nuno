// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads Nuno's configuration from the environment and from an
// optional env-style file. There is no interactive setup (NFR-11).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

const (
	DefaultAddr            = "127.0.0.1:8080"
	DefaultDataDir         = "/data"
	DefaultRefreshInterval = 15 * time.Minute
	DefaultTimeout         = 30 * time.Second

	providerPrefix = "NUNO_PROVIDER_"
)

// Config is the whole of Nuno's configuration. Every secret in it is a
// core.Secret, so printing the struct cannot leak one.
type Config struct {
	Addr            string
	UsageAddr       string // optional separate listener, so exposing usage never exposes the admin panel
	AllowPublicBind bool
	DataDir         string
	LogLevel        slog.Level
	RefreshInterval time.Duration

	AdminKey      core.Secret // machine, read-only, for /api/v1/usage (FR-46a)
	AdminPassword core.Secret // a human in the UI when no proxy provides auth

	// ProxySecret is a header Caddy injects on the way to the admin surface.
	// The admin panel has no password of its own when a proxy authenticates in
	// front of it, and the panel shares a Docker network with every other
	// service, so without this any container could reach it directly and skip
	// the proxy. Empty means no gate (NFR-14).
	ProxySecret core.Secret

	// TrustedProxy is the network the forward-auth headers may come from, for
	// the per-person /me page. Empty means no header is trusted (NFR-14).
	TrustedProxy string

	LDAP      LDAP
	Providers []Provider

	// WebhookURL receives failure and guardrail notifications. The scheduled
	// reconcile refuses to run without one: a timer that writes quotas
	// unattended and cannot tell anybody when it is blocked is worse than no
	// timer (FR-60, FR-62).
	WebhookURL     string
	WebhookTimeout time.Duration

	// ReconcileInterval drives the scheduled reconcile. Zero disables it,
	// which leaves Nuno reporting rather than controlling (FR-36).
	ReconcileInterval time.Duration

	// PublicURL names this instance in a notification.
	PublicURL string
}

// LDAP is the identity source. It is optional: manual users exist with no
// directory at all (FR-2), and Nuno reports rather than refusing to start.
type LDAP struct {
	URL          string
	BaseDN       string
	BindDN       string
	BindPassword core.Secret
	UserFilter   string
	GroupFilter  string
	Timeout      time.Duration
}

// Configured reports whether a directory was configured at all.
func (l LDAP) Configured() bool { return l.URL != "" }

// Provider is one configured provider instance. Its name comes from the env
// prefix it was read from, which is the ConfigRef in the model.
type Provider struct {
	Name        string
	Type        string
	BaseURL     string
	MatchKey    core.MatchKey
	Timeout     time.Duration
	Credentials map[string]core.Secret
}

// ConfigRef is the env prefix this instance was read from.
func (p Provider) ConfigRef() string { return providerPrefix + strings.ToUpper(p.Name) }

// Credential returns a named credential, treating a blank value as absent.
func (p Provider) Credential(name string) (core.Secret, bool) {
	v, ok := p.Credentials[name]
	if !ok || v.Empty() {
		return "", false
	}
	return v, true
}

// Settings converts to what the provider registry needs.
func (p Provider) Settings() core.ProviderSettings {
	return core.ProviderSettings{
		Instance: core.ProviderInstance{
			Type:      p.Type,
			Name:      p.Name,
			MatchKey:  p.MatchKey,
			ConfigRef: p.ConfigRef(),
		},
		BaseURL:     p.BaseURL,
		Credentials: p.Credentials,
		Timeout:     p.Timeout,
	}
}

// reservedProviderKeys are the settings of a provider instance. Everything
// else under its prefix is treated as a credential, so a new adapter with a
// new credential name needs no change here (FR-25).
var reservedProviderKeys = map[string]bool{
	"TYPE": true, "URL": true, "MATCH_KEY": true, "TIMEOUT": true,
}

func Load(env Env) (*Config, error) {
	cfg := &Config{
		Addr:            env.get("NUNO_ADDR", DefaultAddr),
		UsageAddr:       env.get("NUNO_USAGE_ADDR", ""),
		DataDir:         env.get("NUNO_DATA_DIR", DefaultDataDir),
		RefreshInterval: DefaultRefreshInterval,
		AdminKey:        core.Secret(env.get("NUNO_ADMIN_KEY", "")),
		AdminPassword:   core.Secret(env.get("NUNO_ADMIN_PASSWORD", "")),
		ProxySecret:     core.Secret(env.get("NUNO_PROXY_SECRET", "")),
		TrustedProxy:    env.get("NUNO_TRUSTED_PROXY", ""),
	}

	var errs []error
	var err error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if v := env.get("NUNO_ALLOW_PUBLIC_BIND", ""); v != "" {
		b, err := parseBool(v)
		if err != nil {
			fail("NUNO_ALLOW_PUBLIC_BIND: %w", err)
		}
		cfg.AllowPublicBind = b
	}
	if v := env.get("NUNO_LOG_LEVEL", "info"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			fail("NUNO_LOG_LEVEL %q: want debug, info, warn or error", v)
		}
	}
	if v := env.get("NUNO_REFRESH_INTERVAL", ""); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			fail("NUNO_REFRESH_INTERVAL %q: %w", v, err)
		case d <= 0:
			fail("NUNO_REFRESH_INTERVAL must be positive, got %s", d)
		default:
			cfg.RefreshInterval = d
		}
	}

	cfg.LDAP, err = loadLDAP(env)
	if err != nil {
		errs = append(errs, err)
	}

	cfg.WebhookURL = env.get("NUNO_WEBHOOK_URL", "")
	cfg.PublicURL = env.get("NUNO_PUBLIC_URL", "")
	for _, d := range []struct {
		key    string
		target *time.Duration
	}{
		{"NUNO_WEBHOOK_TIMEOUT", &cfg.WebhookTimeout},
		{"NUNO_RECONCILE_INTERVAL", &cfg.ReconcileInterval},
	} {
		if v := env.get(d.key, ""); v != "" {
			parsed, err := time.ParseDuration(v)
			switch {
			case err != nil:
				fail("%s %q: %w", d.key, v, err)
			case parsed < 0:
				fail("%s cannot be negative, got %s", d.key, parsed)
			default:
				*d.target = parsed
			}
		}
	}

	providers, perrs := loadProviders(env)
	cfg.Providers = providers
	errs = append(errs, perrs...)

	errs = append(errs, cfg.validate()...)
	return cfg, errors.Join(errs...)
}

func (c *Config) validate() []error {
	var errs []error
	for _, listener := range []struct{ name, addr string }{
		{"NUNO_ADDR", c.Addr},
		{"NUNO_USAGE_ADDR", c.UsageAddr},
	} {
		if listener.addr == "" {
			continue
		}
		if err := checkBind(listener.addr, c.AllowPublicBind); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", listener.name, err))
		}
	}
	if c.DataDir == "" {
		errs = append(errs, errors.New("NUNO_DATA_DIR cannot be empty"))
	}
	return errs
}

// checkBind holds NFR-14: the admin surface is on localhost unless an
// acknowledgement says otherwise. Refusing at startup beats discovering it
// from a log line.
func checkBind(addr string, allowPublic bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("%q has no port", addr)
	}
	if allowPublic || host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	switch {
	case host == "":
		return fmt.Errorf("%q listens on every interface: set NUNO_ALLOW_PUBLIC_BIND=true to mean it", addr)
	case ip == nil:
		return fmt.Errorf("%q is not an IP address: set NUNO_ALLOW_PUBLIC_BIND=true if you mean to listen wider than localhost", addr)
	case ip.IsLoopback():
		return nil
	case ip.IsUnspecified():
		return fmt.Errorf("%q listens on every interface: set NUNO_ALLOW_PUBLIC_BIND=true to mean it", addr)
	}
	return fmt.Errorf("%q is not a loopback address: set NUNO_ALLOW_PUBLIC_BIND=true to mean it", addr)
}

func loadProviders(env Env) ([]Provider, []error) {
	var errs []error
	names := providerNames(env)
	providers := make([]Provider, 0, len(names))

	for _, name := range names {
		prefix := providerPrefix + name + "_"
		p := Provider{
			Name:        strings.ToLower(name),
			Type:        strings.ToLower(env.get(prefix+"TYPE", "")),
			BaseURL:     env.get(prefix+"URL", ""),
			Timeout:     DefaultTimeout,
			Credentials: map[string]core.Secret{},
		}
		if p.Type == "" {
			errs = append(errs, fmt.Errorf("%sTYPE is required, or %s%s_* configures nothing", prefix, providerPrefix, name))
		}
		if p.BaseURL == "" {
			errs = append(errs, fmt.Errorf("%sURL is required", prefix))
		}
		if v := env.get(prefix+"MATCH_KEY", ""); v != "" {
			p.MatchKey = core.MatchKey(strings.ToLower(v))
			if !p.MatchKey.Valid() {
				errs = append(errs, fmt.Errorf("%sMATCH_KEY %q: want subject, email or username", prefix, v))
			}
		}
		if v := env.get(prefix+"TIMEOUT", ""); v != "" {
			d, err := time.ParseDuration(v)
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("%sTIMEOUT %q: %w", prefix, v, err))
			case d <= 0:
				errs = append(errs, fmt.Errorf("%sTIMEOUT must be positive, got %s", prefix, d))
			default:
				p.Timeout = d
			}
		}
		for key, value := range env {
			suffix, ok := strings.CutPrefix(key, prefix)
			if !ok || reservedProviderKeys[suffix] {
				continue
			}
			p.Credentials[strings.ToLower(suffix)] = core.Secret(value)
		}
		providers = append(providers, p)
	}
	return providers, errs
}

// providerNames finds the instance names from the keys under the provider
// prefix. A name is the segment before the first underscore that follows the
// prefix.
func providerNames(env Env) []string {
	seen := map[string]bool{}
	var names []string
	for key := range env {
		rest, ok := strings.CutPrefix(key, providerPrefix)
		if !ok {
			continue
		}
		name, _, ok := strings.Cut(rest, "_")
		if !ok || name == "" {
			continue
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a boolean", v)
}

// loadLDAP reads the identity source. A URL with no base DN is a
// misconfiguration worth refusing: searching from an empty base would either
// fail or return the whole tree.
func loadLDAP(env Env) (LDAP, error) {
	ldap := LDAP{
		URL:          env.get("NUNO_LDAP_URL", ""),
		BaseDN:       env.get("NUNO_LDAP_BASE_DN", ""),
		BindDN:       env.get("NUNO_LDAP_BIND_DN", ""),
		BindPassword: core.Secret(env.get("NUNO_LDAP_BIND_PASSWORD", "")),
		UserFilter:   env.get("NUNO_LDAP_USER_FILTER", ""),
		GroupFilter:  env.get("NUNO_LDAP_GROUP_FILTER", ""),
	}
	if !ldap.Configured() {
		return ldap, nil
	}
	if ldap.BaseDN == "" {
		return ldap, errors.New("NUNO_LDAP_BASE_DN is required when NUNO_LDAP_URL is set")
	}
	if v := env.get("NUNO_LDAP_TIMEOUT", ""); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			return ldap, fmt.Errorf("NUNO_LDAP_TIMEOUT %q: %w", v, err)
		case d <= 0:
			return ldap, fmt.Errorf("NUNO_LDAP_TIMEOUT must be positive, got %s", d)
		default:
			ldap.Timeout = d
		}
	}
	return ldap, nil
}
