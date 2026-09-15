// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Env is a flat set of configuration keys. Taking it as a value rather than
// reading os.Getenv directly is what makes the loader testable without
// mutating the process environment.
type Env map[string]string

func (e Env) get(key, fallback string) string {
	if v, ok := e[key]; ok && v != "" {
		return v
	}
	return fallback
}

// FromOS reads the process environment.
func FromOS() Env {
	env := Env{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// FromFile reads an env-style file: KEY=value, one per line, # for comments.
// There is no interpolation and no shell evaluation, so a value containing a
// dollar sign means what it says.
func FromFile(path string) (Env, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	env, err := ParseEnv(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return env, nil
}

func ParseEnv(r io.Reader) (Env, error) {
	env := Env{}
	scanner := bufio.NewScanner(r)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: want KEY=value, got %q", line, text)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", line)
		}
		env[key] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return env, nil
}

func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// Merge layers env sets, later ones winning. The process environment is
// applied last, so an operator can always override the file from compose.
func Merge(sets ...Env) Env {
	merged := Env{}
	for _, set := range sets {
		for k, v := range set {
			merged[k] = v
		}
	}
	return merged
}

// Resolve is the standard order: the file named by NUNO_CONFIG, then the
// process environment. A configured path that cannot be read is an error, not
// a warning: continuing would silently run on defaults.
func Resolve() (Env, error) {
	osEnv := FromOS()
	path := osEnv.get("NUNO_CONFIG", "")
	if path == "" {
		return osEnv, nil
	}
	fileEnv, err := FromFile(path)
	if err != nil {
		return nil, fmt.Errorf("NUNO_CONFIG: %w", err)
	}
	return Merge(fileEnv, osEnv), nil
}
