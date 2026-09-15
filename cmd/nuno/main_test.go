// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunDispatch(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{"version", []string{"version"}, ExitClean, version},
		{"help", []string{"help"}, ExitClean, "Usage:"},
		{"no arguments", nil, ExitConfig, "Usage:"},
		{"unknown command", []string{"frobnicate"}, ExitConfig, `unknown command "frobnicate"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(c.args, &stdout, &stderr)
			if code != c.wantCode {
				t.Errorf("code = %d, want %d", code, c.wantCode)
			}
			combined := stdout.String() + stderr.String()
			if !strings.Contains(combined, c.wantOut) {
				t.Errorf("output %q does not contain %q", combined, c.wantOut)
			}
		})
	}
}

// A typo must not silently look like success. FR-72 makes 3 the configuration
// failure, and an unknown command is one.
func TestUnknownCommandsAreNotSilentlyClean(t *testing.T) {
	for _, command := range []string{"frobnicate", "recncile", "tier"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{command}, &stdout, &stderr)
		if code != ExitConfig {
			t.Errorf("%q returned %d, want %d", command, code, ExitConfig)
		}
		if !strings.Contains(stderr.String(), "unknown command") {
			t.Errorf("%q must say what went wrong, got %q", command, stderr.String())
		}
	}
}

// Every command in the usage text is dispatched, so the two cannot drift.
func TestEveryDocumentedCommandIsDispatched(t *testing.T) {
	var documented []string
	for _, line := range strings.Split(usage, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.HasPrefix(line, "  ") || strings.HasPrefix(trimmed, "nuno ") {
			continue
		}
		name, _, ok := strings.Cut(trimmed, " ")
		if !ok || strings.Contains(name, "<") {
			continue
		}
		documented = append(documented, name)
	}
	if len(documented) < 10 {
		t.Fatalf("parsed %v out of the usage text, which does not look right", documented)
	}

	for _, command := range documented {
		var stdout, stderr bytes.Buffer
		// These reach configuration and stop there, because no data
		// directory is set up in a test. What matters is that none of them
		// falls through to "unknown command".
		run([]string{command}, &stdout, &stderr)
		if strings.Contains(stderr.String(), "unknown command") {
			t.Errorf("%q is documented but not dispatched", command)
		}
	}
}
