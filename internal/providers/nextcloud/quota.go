// SPDX-License-Identifier: AGPL-3.0-or-later

package nextcloud

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/marcodellemarche/nuno/internal/core"
)

// SpaceUnlimited is FileInfo::SPACE_UNLIMITED on the wire. It is a sentinel,
// never a byte count, and writing it back as a number is the most destructive
// value the API accepts. See ADR-0021 point 6.
const SpaceUnlimited = -3

// Nextcloud writes powers of two under SI names. The names are what the round
// trip produces and parses, so they are kept as they are rather than
// corrected to IEC.
var unitFactors = []struct {
	name   string
	factor float64
}{
	{"B", 1},
	{"KB", 1 << 10},
	{"MB", 1 << 20},
	{"GB", 1 << 30},
	{"TB", 1 << 40},
	{"PB", 1 << 50},
}

// humanFileSize reproduces Util::humanFileSize. The first step rounds to zero
// decimals and every step after it to one, which is the detail that decides
// whether the planner converges. See ADR-0021 point 9.
func humanFileSize(n int64) (float64, string) {
	if n < 1024 {
		return float64(n), "B"
	}
	v := math.Round(float64(n) / 1024)
	if v < 1024 {
		return v, "KB"
	}
	for _, unit := range []string{"MB", "GB", "TB"} {
		v = round1(v / 1024)
		if v < 1024 {
			return v, unit
		}
	}
	return round1(v / 1024), "PB"
}

// round1 is PHP's round($v, 1): half away from zero, which is what math.Round
// does too.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// computerFileSize is the other half of the trip, Util::computerFileSize: the
// stored string parsed back into bytes.
func computerFileSize(value float64, unit string) (int64, error) {
	for _, u := range unitFactors {
		if u.name == unit {
			return int64(math.Round(value * u.factor)), nil
		}
	}
	return 0, fmt.Errorf("unknown unit %q", unit)
}

// humanString is the value as the instance stores it, which is worth showing
// in a plan or in doctor: "stores as 25 GB" explains a normalized write that
// otherwise looks like Nuno changing the number it was given.
func humanString(n int64) string {
	v, unit := humanFileSize(n)
	return strconv.FormatFloat(v, 'f', -1, 64) + " " + unit
}

// normalizeBytes is what the instance will really store when asked for n.
func normalizeBytes(n int64) (int64, error) {
	value, unit := humanFileSize(n)
	return computerFileSize(value, unit)
}

// formatQuotaValue is what goes on the wire for a write. Unlimited is the
// string "none": a numeric -3 would be parsed as a byte count.
func formatQuotaValue(q core.Quota) (string, error) {
	switch q.Kind {
	case core.KindUnlimited:
		return "none", nil
	case core.KindBytes:
		return strconv.FormatInt(q.Bytes, 10), nil
	}
	return "", fmt.Errorf("refusing to write a %s quota", q.Kind)
}

// decodeQuotaValue reads ocs.data.users.<id>.quota.quota, which is a number, a
// sentinel, or the string "none". Every other shape is a provider error rather
// than a guess.
func decodeQuotaValue(raw any) (core.Quota, error) {
	switch v := raw.(type) {
	case nil:
		return core.Unknown(), nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "none":
			return core.Unlimited(), nil
		case "":
			return core.Unknown(), nil
		}
		// A human size can appear here on instances configured by hand.
		n, err := parseHumanSize(v)
		if err != nil {
			return core.Unknown(), fmt.Errorf("undecodable quota %q: %w", v, err)
		}
		return quotaFromWire(n)
	case float64:
		return quotaFromWire(int64(v))
	case int64:
		return quotaFromWire(v)
	}
	return core.Unknown(), fmt.Errorf("undecodable quota of type %T", raw)
}

func quotaFromWire(n int64) (core.Quota, error) {
	if n == SpaceUnlimited {
		return core.Unlimited(), nil
	}
	if n < 0 {
		// Another negative is a sentinel this adapter does not know. Guessing
		// would mean writing against a value nobody read.
		return core.Unknown(), fmt.Errorf("unexpected negative quota %d", n)
	}
	return core.BytesQuota(n)
}

// parseHumanSize accepts what Nextcloud itself stores, for example "25 GB".
func parseHumanSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	cut := strings.LastIndexFunc(s, func(r rune) bool {
		return r >= '0' && r <= '9'
	})
	if cut < 0 {
		return 0, fmt.Errorf("no number in %q", s)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(s[:cut+1]), 64)
	if err != nil {
		return 0, err
	}
	unit := strings.ToUpper(strings.TrimSpace(s[cut+1:]))
	if unit == "" {
		unit = "B"
	}
	// Nextcloud accepts "G" as well as "GB", and treats "GiB" the same way.
	unit = strings.ReplaceAll(unit, "I", "")
	if !strings.HasSuffix(unit, "B") {
		unit += "B"
	}
	return computerFileSize(value, unit)
}
