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

	Providers []Provider
}

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
	}

	var errs []error
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
