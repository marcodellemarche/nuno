// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"fmt"
	"strconv"
	"strings"
)

// Sizes a human types are IEC, and so are the ones Nuno prints. The SI
// spellings are accepted as aliases for the same powers of two, because that
// is what every provider in this space means by GB, and refusing them would
// be pedantry with a footgun attached (ADR-0011).
var sizeUnits = []struct {
	suffixes []string
	factor   int64
}{
	{[]string{"pib", "pb", "p"}, 1 << 50},
	{[]string{"tib", "tb", "t"}, 1 << 40},
	{[]string{"gib", "gb", "g"}, 1 << 30},
	{[]string{"mib", "mb", "m"}, 1 << 20},
	{[]string{"kib", "kb", "k"}, 1 << 10},
	{[]string{"b"}, 1},
}

// ParseSize reads a quota a person typed: "unlimited", "100GiB", "0", or a
// bare byte count. It never returns Unknown, because nobody types a value
// nobody knows.
func ParseSize(text string) (Quota, error) {
	value := strings.ToLower(strings.TrimSpace(text))
	switch value {
	case "":
		return Unknown(), fmt.Errorf("a size is required")
	case "unlimited", "none":
		return Unlimited(), nil
	case "unknown":
		return Unknown(), fmt.Errorf("unknown is not something you can ask for")
	}

	for _, unit := range sizeUnits {
		for _, suffix := range unit.suffixes {
			number, ok := strings.CutSuffix(value, suffix)
			if !ok {
				continue
			}
			number = strings.TrimSpace(number)
			amount, err := strconv.ParseFloat(number, 64)
			if err != nil {
				return Unknown(), fmt.Errorf("%q is not a size: %w", text, err)
			}
			if amount < 0 {
				return Unknown(), fmt.Errorf("%q is negative, and a negative is never a ceiling", text)
			}
			return BytesQuota(int64(amount * float64(unit.factor)))
		}
	}

	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return Unknown(), fmt.Errorf("%q is not a size: use bytes, or a unit like 100GiB, or unlimited", text)
	}
	return BytesQuota(amount)
}

// ParseSizeInGiB is ParseSize for a field that shows the unit outside the
// input: a bare number is read as GiB, so "50" and "50GiB" mean the same
// thing. Anything with an explicit unit, or "unlimited", is parsed exactly as
// before, which keeps one escape hatch for the states a number cannot express.
func ParseSizeInGiB(text string) (Quota, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed != "" {
		if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return ParseSize(trimmed + "GiB")
		}
	}
	return ParseSize(trimmed)
}

// ParseAllocationInGiB is ParseAllocation for the same kind of field. A bare
// number is GiB, because the tier editor no longer offers percentages: a
// ceiling a person reads should not move when somebody else's budget changes.
func ParseAllocationInGiB(text string) (Allocation, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed != "" {
		if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return ParseAllocation(trimmed + "GiB")
		}
	}
	return ParseAllocation(trimmed)
}

// FormatGiBNumber renders bytes as a bare number of GiB for an input field
// whose unit is shown next to it. It is display only: the value is parsed back
// with ParseSizeInGiB, so the two must round-trip.
func FormatGiBNumber(n int64) string {
	v := float64(n) / float64(int64(1)<<30)
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// ParseAllocation reads what a tier offers on one provider: "25%", "50GiB", or
// "unlimited". There is deliberately no spelling for absent: absence is the
// lack of an allocation, not a value (FR-17).
func ParseAllocation(text string) (Allocation, error) {
	value := strings.TrimSpace(text)
	if percent, ok := strings.CutSuffix(value, "%"); ok {
		share, err := strconv.ParseInt(strings.TrimSpace(percent), 10, 64)
		if err != nil {
			return Allocation{}, fmt.Errorf("%q is not a percentage: %w", text, err)
		}
		if share < 0 || share > 100 {
			return Allocation{}, fmt.Errorf("%q must be between 0%% and 100%%", text)
		}
		return Allocation{Mode: ModePercent, Value: share}, nil
	}

	quota, err := ParseSize(value)
	if err != nil {
		return Allocation{}, err
	}
	switch quota.Kind {
	case KindUnlimited:
		return Allocation{Mode: ModeUnlimited}, nil
	case KindBytes:
		return Allocation{Mode: ModeAbsolute, Value: quota.Bytes}, nil
	}
	return Allocation{}, fmt.Errorf("%q cannot be an allocation", text)
}

// Describe renders an allocation the way it was typed, for display.
func (a Allocation) Describe() string {
	switch a.Mode {
	case ModeUnlimited:
		return "unlimited"
	case ModePercent:
		return strconv.FormatInt(a.Value, 10) + "%"
	case ModeAbsolute:
		return FormatIEC(a.Value)
	}
	return string(a.Mode)
}

// DescribeGiB renders an absolute allocation as a bare number of GiB, which is
// what the editor's input holds when the unit sits outside the field. Any
// other mode is rendered exactly as Describe would.
func (a Allocation) DescribeGiB() string {
	if a.Mode == ModeAbsolute {
		return FormatGiBNumber(a.Value)
	}
	return a.Describe()
}
