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
		{"unknown command", []string{"reconcile"}, ExitConfig, `unknown command "reconcile"`},
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

// A command that does not exist yet must not silently look like success. FR-72
// makes 3 the configuration failure, and a typo is one.
func TestUnimplementedCommandsAreNotSilentlyClean(t *testing.T) {
	for _, command := range []string{"doctor", "plan", "usage", "link"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{command}, &stdout, &stderr); code == ExitClean {
			t.Errorf("%q returned ExitClean before it is implemented", command)
		}
	}
}
