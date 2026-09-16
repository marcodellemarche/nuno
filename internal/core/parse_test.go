// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "testing"

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want Quota
		ok   bool
	}{
		{"unlimited", Unlimited(), true},
		{"none", Unlimited(), true},
		{"100GiB", MustBytes(100 << 30), true},
		// SI spellings mean the same powers of two, because that is what
		// every provider here means by GB.
		{"100GB", MustBytes(100 << 30), true},
		{"100 gb", MustBytes(100 << 30), true},
		{"50G", MustBytes(50 << 30), true},
		{"1TiB", MustBytes(1 << 40), true},
		{"1.5GiB", MustBytes(1610612736), true},
		{"1024", MustBytes(1024), true},
		{"0", MustBytes(0), true},
		{"", Unknown(), false},
		{"unknown", Unknown(), false},
		{"-5GiB", Unknown(), false},
		{"lots", Unknown(), false},
		{"GiB", Unknown(), false},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if (err == nil) != c.ok {
			t.Errorf("ParseSize(%q) = %v, %v", c.in, got, err)
			continue
		}
		if c.ok && !got.Equal(c.want) {
			t.Errorf("ParseSize(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseAllocation(t *testing.T) {
	cases := []struct {
		in   string
		mode AllocationMode
		val  int64
		ok   bool
	}{
		{"25%", ModePercent, 25, true},
		{"100%", ModePercent, 100, true},
		{"0%", ModePercent, 0, true},
		{"50GiB", ModeAbsolute, 50 << 30, true},
		{"unlimited", ModeUnlimited, 0, true},
		{"101%", "", 0, false},
		{"-1%", "", 0, false},
		{"absent", "", 0, false},
		{"", "", 0, false},
	}
	for _, c := range cases {
		got, err := ParseAllocation(c.in)
		if (err == nil) != c.ok {
			t.Errorf("ParseAllocation(%q) = %+v, %v", c.in, got, err)
			continue
		}
		if c.ok && (got.Mode != c.mode || got.Value != c.val) {
			t.Errorf("ParseAllocation(%q) = %+v, want %s %d", c.in, got, c.mode, c.val)
		}
	}
}

func TestAllocationDescribeRoundTrips(t *testing.T) {
	for _, text := range []string{"25%", "unlimited", "50 GiB"} {
		parsed, err := ParseAllocation(text)
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseAllocation(parsed.Describe())
		if err != nil {
			t.Fatalf("Describe() produced %q, which does not parse back: %v", parsed.Describe(), err)
		}
		if again.Mode != parsed.Mode || again.Value != parsed.Value {
			t.Errorf("%q round tripped to %+v, want %+v", text, again, parsed)
		}
	}
}

// The editor shows the unit outside the field, so a bare number there is GiB.
// Anything with an explicit unit, or "unlimited", still means what it says.
func TestParseInGiB(t *testing.T) {
	if got, err := ParseSizeInGiB("50"); err != nil || !got.Equal(MustBytes(50<<30)) {
		t.Errorf("ParseSizeInGiB(50) = %v, %v, want 50 GiB", got, err)
	}
	if got, err := ParseSizeInGiB("1.5"); err != nil || !got.Equal(MustBytes(1610612736)) {
		t.Errorf("ParseSizeInGiB(1.5) = %v, %v, want 1.5 GiB", got, err)
	}
	if got, err := ParseSizeInGiB("unlimited"); err != nil || !got.IsUnlimited() {
		t.Errorf("ParseSizeInGiB(unlimited) = %v, %v, want unlimited", got, err)
	}
	if got, err := ParseAllocationInGiB("50"); err != nil || got.Mode != ModeAbsolute || got.Value != 50<<30 {
		t.Errorf("ParseAllocationInGiB(50) = %+v, %v, want absolute 50 GiB", got, err)
	}
	if got, err := ParseAllocationInGiB("25%"); err != nil || got.Mode != ModePercent || got.Value != 25 {
		t.Errorf("ParseAllocationInGiB(25%%) = %+v, %v, want the explicit percentage kept", got, err)
	}
}

func TestFormatGiBNumberRoundTrips(t *testing.T) {
	for _, n := range []int64{0, 1 << 30, 25 << 30, 150 << 30, 1610612736} {
		text := FormatGiBNumber(n)
		back, err := ParseSizeInGiB(text)
		if err != nil || !back.IsBytes() || back.Bytes != n {
			t.Errorf("FormatGiBNumber(%d) = %q, which parses to %v, %v", n, text, back, err)
		}
	}
}
