// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const leak = "hunter2-should-never-appear"

func TestSecretIsRedactedByEveryVerb(t *testing.T) {
	s := Secret(leak)
	for _, format := range []string{"%v", "%s", "%q", "%#v", "%+v", "%d", "%x"} {
		if got := fmt.Sprintf(format, s); strings.Contains(got, leak) {
			t.Errorf("fmt %s leaked: %s", format, got)
		}
	}
	if strings.Contains(s.String(), leak) || strings.Contains(s.GoString(), leak) {
		t.Error("String or GoString leaked")
	}
}

func TestSecretIsRedactedInJSONAndLogs(t *testing.T) {
	body, err := json.Marshal(struct {
		Password Secret `json:"password"`
	}{Secret(leak)})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(leak)) {
		t.Errorf("JSON leaked: %s", body)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("connecting", "password", Secret(leak))
	if strings.Contains(buf.String(), leak) {
		t.Errorf("slog leaked: %s", buf.String())
	}
}

func TestSecretRevealIsTheOnlyWayOut(t *testing.T) {
	if got := Secret(leak).Reveal(); got != leak {
		t.Fatalf("Reveal() = %q", got)
	}
	if !Secret("").Empty() || Secret("x").Empty() {
		t.Fatal("Empty is wrong")
	}
}

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://cloud.example.org/ocs/v2.php", "https://cloud.example.org/ocs/v2.php"},
		{"https://admin:hunter2@cloud.example.org/", "https://redacted@cloud.example.org/"},
		{"://nonsense", redacted},
	}
	for _, c := range cases {
		if got := RedactURL(c.in); got != c.want {
			t.Errorf("RedactURL(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(RedactURL(c.in), "hunter2") {
			t.Errorf("RedactURL(%q) leaked the password", c.in)
		}
	}
}
