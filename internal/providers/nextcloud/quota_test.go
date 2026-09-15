// SPDX-License-Identifier: AGPL-3.0-or-later

package nextcloud

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/marcodellemarche/nuno/internal/core"
)

const fixtureDir = "../../../tests/fixtures"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The captured round trip is the test vector, not an illustration. If this
// fails, NormalizeQuota is wrong and the planner will emit the same change
// forever while passing every test against a mock.
func TestNormalizeAgainstTheCapturedRoundTrip(t *testing.T) {
	var capture struct {
		OriginalQuotaBytes int64             `json:"original_quota_bytes"`
		WrittenBytes       int64             `json:"written_bytes"`
		ReadBackBytes      int64             `json:"read_back_bytes"`
		Transform          map[string]string `json:"humanFileSize_transform_observed_in_container"`
	}
	if err := json.Unmarshal(readFixture(t, "nextcloud-roundtrip.json"), &capture); err != nil {
		t.Fatal(err)
	}
	if capture.WrittenBytes == 0 || capture.ReadBackBytes == 0 || len(capture.Transform) == 0 {
		t.Fatal("the fixture did not decode: this test would then prove nothing")
	}

	got, err := normalizeBytes(capture.WrittenBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got != capture.ReadBackBytes {
		t.Errorf("normalizeBytes(%d) = %d, want %d as captured from the instance",
			capture.WrittenBytes, got, capture.ReadBackBytes)
	}

	// The instance's own humanFileSize output, observed in the container.
	for raw, want := range capture.Transform {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if got := humanString(n); got != want {
			t.Errorf("humanString(%d) = %q, want %q", n, got, want)
		}
	}

	// A whole number of units survives untouched, which is why 50 GB and
	// 150 GB pass through while 25.0009 GB does not.
	for _, n := range []int64{capture.OriginalQuotaBytes, capture.ReadBackBytes} {
		got, err := normalizeBytes(n)
		if err != nil {
			t.Fatal(err)
		}
		if got != n {
			t.Errorf("normalizeBytes(%d) = %d, want it unchanged", n, got)
		}
	}
}

// ARCHITECTURE section 11 requires Normalize(Normalize(x)) == Normalize(x) for
// every adapter. Without it the planner never converges.
func TestNormalizeIsIdempotent(t *testing.T) {
	values := []int64{0, 1, 1023, 1024, 1025, 1536, 26844594176, 53687091200, 161061273600,
		27487790694, 1<<40 + 12345, 1<<50 + 1}
	for i := 0; i < 2000; i++ {
		values = append(values, rand.Int64N(1<<53))
	}
	for _, n := range values {
		once, err := normalizeBytes(n)
		if err != nil {
			t.Fatalf("normalizeBytes(%d): %v", n, err)
		}
		twice, err := normalizeBytes(once)
		if err != nil {
			t.Fatalf("normalizeBytes(%d): %v", once, err)
		}
		if once != twice {
			t.Fatalf("not idempotent: %d -> %d -> %d (stores as %q then %q)",
				n, once, twice, humanString(n), humanString(once))
		}
	}
}

func TestFormatQuotaValue(t *testing.T) {
	// Unlimited is "none". A numeric -3 would be read as a byte count, which
	// is the most destructive value the API accepts.
	got, err := formatQuotaValue(core.Unlimited())
	if err != nil || got != "none" {
		t.Errorf("unlimited = %q, %v, want \"none\"", got, err)
	}
	if got, err := formatQuotaValue(core.MustBytes(53687091200)); err != nil || got != "53687091200" {
		t.Errorf("bytes = %q, %v", got, err)
	}
	if _, err := formatQuotaValue(core.Unknown()); err == nil {
		t.Error("writing an unknown quota must be refused: it is never a write target")
	}
}

func TestDecodeQuotaValue(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want core.Quota
		ok   bool
	}{
		{"the captured sentinel", float64(SpaceUnlimited), core.Unlimited(), true},
		{"the string none", "none", core.Unlimited(), true},
		{"the string NONE", "NONE", core.Unlimited(), true},
		{"a byte count", float64(53687091200), core.MustBytes(53687091200), true},
		{"zero", float64(0), core.MustBytes(0), true},
		{"absent", nil, core.Unknown(), true},
		{"empty string", "", core.Unknown(), true},
		{"a human size set by hand", "25 GB", core.MustBytes(26843545600), true},
		{"an unknown sentinel", float64(-1), core.Unknown(), false},
		{"nonsense", true, core.Unknown(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decodeQuotaValue(c.raw)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
			if !got.Equal(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
