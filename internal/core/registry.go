// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

// ProviderSettings is everything an adapter needs to exist, already resolved
// from configuration. Core holds the shape so that registering a provider
// needs no import from core into the config package.
type ProviderSettings struct {
	Instance    ProviderInstance
	BaseURL     string
	Credentials map[string]Secret
	Timeout     time.Duration
}

// Credential returns a named credential, reporting whether it was set. An
// empty value counts as absent: a blank password is a misconfiguration, not a
// credential.
func (s ProviderSettings) Credential(name string) (Secret, bool) {
	v, ok := s.Credentials[name]
	if !ok || v.Empty() {
		return "", false
	}
	return v, true
}

type Factory func(ProviderSettings) (Provider, error)

// Registry maps a provider type to its constructor. It is populated by the
// composition root, which is the only place that imports the adapters.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

func (r *Registry) Register(typ string, f Factory) error {
	if typ == "" {
		return fmt.Errorf("provider type cannot be empty")
	}
	if f == nil {
		return fmt.Errorf("provider %q: factory cannot be nil", typ)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[typ]; exists {
		return fmt.Errorf("provider type %q is already registered", typ)
	}
	r.factories[typ] = f
	return nil
}

func (r *Registry) Build(s ProviderSettings) (Provider, error) {
	r.mu.RLock()
	f, ok := r.factories[s.Instance.Type]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown provider type %q (registered: %v)", s.Instance.Type, r.Types())
	}
	p, err := f(s)
	if err != nil {
		return nil, fmt.Errorf("provider %q (%s): %w", s.Instance.Name, s.Instance.Type, err)
	}
	return p, nil
}

func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	types := make([]string, 0, len(r.factories))
	for t := range r.factories {
		types = append(types, t)
	}
	slices.Sort(types)
	return types
}
