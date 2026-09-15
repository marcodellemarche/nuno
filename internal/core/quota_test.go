// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"errors"
	"testing"
)

func TestZeroValueIsUnknown(t *testing.T) {
	var q Quota
	if q.IsKnown() {
		t.Fatal("the zero value must be Unknown, so nothing is written against it by default")
	}
	if q.Kind != KindUnknown {
		t.Fatalf("kind = %v", q.Kind)
	}
}

func TestBytesQuotaRejectsNegative(t *testing.T) {
	if _, err := BytesQuota(-3); !errors.Is(err, ErrNegativeQuota) {
		t.Fatalf("err = %v, want ErrNegativeQuota: -3 is a Nextcloud sentinel, not a byte count", err)
	}
	if _, err := BytesQuota(0); err != nil {
		t.Fatalf("zero is a legal quota that blocks uploads: %v", err)
	}
}

func TestValid(t *testing.T) {
	cases := []struct {
		name string
		q    Quota
		ok   bool
	}{
		{"unknown", Unknown(), true},
		{"unlimited", Unlimited(), true},
		{"bytes", MustBytes(1024), true},
		{"zero bytes", MustBytes(0), true},
		{"unlimited carrying bytes", Quota{Kind: KindUnlimited, Bytes: 5}, false},
		{"unknown carrying bytes", Quota{Kind: KindUnknown, Bytes: 5}, false},
		{"negative bytes", Quota{Kind: KindBytes, Bytes: -3}, false},
		{"undefined kind", Quota{Kind: QuotaKind(9)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.q.Valid(); (err == nil) != c.ok {
				t.Fatalf("Valid() = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

func TestValidAsUsageRejectsUnlimited(t *testing.T) {
	if err := Unlimited().ValidAsUsage(); err == nil {
		t.Fatal("usage cannot be unlimited, per ADR-0021 point 8")
	}
	for _, q := range []Quota{Unknown(), MustBytes(0), MustBytes(94422551)} {
		if err := q.ValidAsUsage(); err != nil {
			t.Fatalf("%v: %v", q, err)
		}
	}
}

func TestEqualDistinguishesTheKinds(t *testing.T) {
	if Unlimited().Equal(Unknown()) {
		t.Fatal("unlimited and unknown must never compare equal")
	}
	if Unlimited().Equal(MustBytes(0)) {
		t.Fatal("unlimited and zero must never compare equal")
	}
	if !MustBytes(53687091200).Equal(MustBytes(53687091200)) {
		t.Fatal("equal byte counts must compare equal")
	}
	if MustBytes(26844594176).Equal(MustBytes(26843545600)) {
		t.Fatal("the two sides of the Nextcloud round trip are different values")
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	for _, q := range []Quota{Unknown(), Unlimited(), MustBytes(0), MustBytes(161061273600)} {
		got, err := DecodeQuota(q.Encode())
		if err != nil {
			t.Fatalf("%v: %v", q, err)
		}
		if !got.Equal(q) {
			t.Fatalf("round trip: %v -> %q -> %v", q, q.Encode(), got)
		}
	}
}

func TestDecodeQuotaRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "none", "-3", "bytes:", "bytes:-3", "bytes:abc", "50 GB"} {
		if _, err := DecodeQuota(s); err == nil {
			t.Fatalf("DecodeQuota(%q) must fail", s)
		}
	}
}

func TestFormatIEC(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1 KiB"},
		{53687091200, "50 GiB"},
		{161061273600, "150 GiB"},
		{26843545600, "25 GiB"},
		{9864872002, "9.2 GiB"},
		{94422551, "90.0 MiB"},
	}
	for _, c := range cases {
		if got := FormatIEC(c.n); got != c.want {
			t.Errorf("FormatIEC(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
