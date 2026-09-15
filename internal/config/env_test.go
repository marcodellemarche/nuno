// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

func TestParseEnv(t *testing.T) {
	in := `
# the admin surface
NUNO_ADDR=127.0.0.1:8080

  NUNO_DATA_DIR = /data
export NUNO_ADMIN_KEY="quoted value"
NUNO_ADMIN_PASSWORD='single quoted'
NUNO_PROVIDER_CLOUD_PASSWORD=has=equals=inside
NUNO_EMPTY=
NUNO_DOLLAR=$not_expanded
`
	env, err := ParseEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"NUNO_ADDR":                    "127.0.0.1:8080",
		"NUNO_DATA_DIR":                "/data",
		"NUNO_ADMIN_KEY":               "quoted value",
		"NUNO_ADMIN_PASSWORD":          "single quoted",
		"NUNO_PROVIDER_CLOUD_PASSWORD": "has=equals=inside",
		"NUNO_EMPTY":                   "",
		"NUNO_DOLLAR":                  "$not_expanded",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if len(env) != len(want) {
		t.Errorf("got %d keys, want %d: %v", len(env), len(want), env)
	}
}

func TestParseEnvRejectsAMalformedLine(t *testing.T) {
	for _, in := range []string{"NUNO_ADDR", "=value"} {
		if _, err := ParseEnv(strings.NewReader(in)); err == nil {
			t.Errorf("ParseEnv(%q) must fail rather than skip the line", in)
		}
	}
}

func TestMergeLetsTheProcessEnvironmentWin(t *testing.T) {
	file := Env{"NUNO_ADDR": "127.0.0.1:8080", "NUNO_DATA_DIR": "/data"}
	osEnv := Env{"NUNO_ADDR": "127.0.0.1:9999"}
	merged := Merge(file, osEnv)
	if merged["NUNO_ADDR"] != "127.0.0.1:9999" {
		t.Errorf("NUNO_ADDR = %q, want the process environment to override the file", merged["NUNO_ADDR"])
	}
	if merged["NUNO_DATA_DIR"] != "/data" {
		t.Errorf("NUNO_DATA_DIR = %q, want the file value kept", merged["NUNO_DATA_DIR"])
	}
}

func TestGetTreatsBlankAsUnset(t *testing.T) {
	env := Env{"NUNO_ADDR": ""}
	if got := env.get("NUNO_ADDR", DefaultAddr); got != DefaultAddr {
		t.Errorf("get() = %q, want the default: an empty value in compose means unset", got)
	}
}
