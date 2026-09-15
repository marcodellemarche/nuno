// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// QuotaKind tags what a Quota actually means. A provider reports a quota in
// states that are not numbers, and collapsing them into a nullable integer
// produces silent wrong writes. See ADR-0011.
type QuotaKind uint8

const (
	// KindUnknown means the value could not be read. It is never a write
	// target and never a write trigger.
	KindUnknown QuotaKind = iota
	KindUnlimited
	KindBytes
)

func (k QuotaKind) String() string {
	switch k {
	case KindUnknown:
		return "unknown"
	case KindUnlimited:
		return "unlimited"
	case KindBytes:
		return "bytes"
	}
	return fmt.Sprintf("QuotaKind(%d)", uint8(k))
}

// Quota is a tagged value. The zero value is Unknown, which is the safe
// default: nothing is written against it.
type Quota struct {
	Kind  QuotaKind
	Bytes int64
}

var ErrNegativeQuota = errors.New("quota in bytes cannot be negative")

func Unknown() Quota   { return Quota{Kind: KindUnknown} }
func Unlimited() Quota { return Quota{Kind: KindUnlimited} }

// BytesQuota rejects a negative count instead of carrying it. Nextcloud reads
// negative values as sentinels, so an unvalidated negative grants unlimited
// rather than failing. See ADR-0020.
func BytesQuota(n int64) (Quota, error) {
	if n < 0 {
		return Unknown(), fmt.Errorf("%w: %d", ErrNegativeQuota, n)
	}
	return Quota{Kind: KindBytes, Bytes: n}, nil
}

// MustBytes is for tests and for literals known at compile time.
func MustBytes(n int64) Quota {
	q, err := BytesQuota(n)
	if err != nil {
		panic(err)
	}
	return q
}

func (q Quota) IsKnown() bool     { return q.Kind != KindUnknown }
func (q Quota) IsUnlimited() bool { return q.Kind == KindUnlimited }
func (q Quota) IsBytes() bool     { return q.Kind == KindBytes }

// Valid catches a value that no adapter should have produced, including one
// read back from the database.
func (q Quota) Valid() error {
	switch q.Kind {
	case KindUnknown, KindUnlimited:
		if q.Bytes != 0 {
			return fmt.Errorf("%s quota carries %d bytes", q.Kind, q.Bytes)
		}
		return nil
	case KindBytes:
		if q.Bytes < 0 {
			return fmt.Errorf("%w: %d", ErrNegativeQuota, q.Bytes)
		}
		return nil
	}
	return fmt.Errorf("unknown quota kind %d", uint8(q.Kind))
}

// ValidAsUsage rejects Unlimited, which is meaningless for consumption.
// Account.Used reuses this type for convenience, so the restriction has to be
// checked rather than assumed. See ADR-0021 point 8.
func (q Quota) ValidAsUsage() error {
	if q.Kind == KindUnlimited {
		return errors.New("usage cannot be unlimited")
	}
	return q.Valid()
}

// Equal compares structurally. It does not treat Unknown specially: two
// unreadable values are equal as values, and that is not a reason to write.
// The planner excludes Unknown before it ever compares.
func (q Quota) Equal(other Quota) bool {
	if q.Kind != other.Kind {
		return false
	}
	return q.Kind != KindBytes || q.Bytes == other.Bytes
}

// String is human facing, so sizes are IEC. See ADR-0011.
func (q Quota) String() string {
	switch q.Kind {
	case KindUnknown:
		return "unknown"
	case KindUnlimited:
		return "unlimited"
	case KindBytes:
		return FormatIEC(q.Bytes)
	}
	return q.Kind.String()
}

// Encode is the stored and logged form. The audit log keeps the tag, or
// unlimited and unknown collide in the one table meant to explain what
// happened. See ADR-0011.
func (q Quota) Encode() string {
	if q.Kind == KindBytes {
		return "bytes:" + strconv.FormatInt(q.Bytes, 10)
	}
	return q.Kind.String()
}

func DecodeQuota(s string) (Quota, error) {
	switch s {
	case "unknown":
		return Unknown(), nil
	case "unlimited":
		return Unlimited(), nil
	}
	n, ok := strings.CutPrefix(s, "bytes:")
	if !ok {
		return Unknown(), fmt.Errorf("undecodable quota %q", s)
	}
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil {
		return Unknown(), fmt.Errorf("undecodable quota %q: %w", s, err)
	}
	return BytesQuota(v)
}

var iecUnits = [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// FormatIEC renders bytes in powers of two. The documented tiers of 100 GB and
// 200 GB mean GiB, and every size a human reads is IEC.
func FormatIEC(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	v := float64(n)
	unit := ""
	for _, u := range iecUnits {
		v /= 1024
		unit = u
		if v < 1024 {
			break
		}
	}
	if v >= 100 || v == float64(int64(v)) {
		return strconv.FormatFloat(v, 'f', 0, 64) + " " + unit
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + unit
}
