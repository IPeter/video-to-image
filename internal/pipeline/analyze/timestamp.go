package analyze

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseTimestamp accepts "MM:SS", "M:SS", "HH:MM:SS", "MM:SS.s" and bare seconds ("12", "12.5").
//
// Details: surrounding whitespace and one trailing "s" ("12.5s") are ignored; only the
// last component may carry a fraction ("00:04.25", "1:02.5"); every component after the
// first must be below 60 ("1:75" is rejected, "75:10" is 75 minutes). Negative values,
// signs, exponents, NaN/Inf, empty components and more than three components are errors.
func ParseTimestamp(s string) (float64, error) {
	t := strings.TrimSpace(s)
	if rest, ok := strings.CutSuffix(t, "s"); ok {
		t = strings.TrimSpace(rest)
	} else if rest, ok := strings.CutSuffix(t, "S"); ok {
		t = strings.TrimSpace(rest)
	}
	if t == "" {
		return 0, fmt.Errorf("analyze: timestamp %q: empty", s)
	}
	parts := strings.Split(t, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("analyze: timestamp %q: too many components", s)
	}
	total := 0.0
	for i, p := range parts {
		last := i == len(parts)-1
		v, err := parseComponent(p, last)
		if err != nil {
			return 0, fmt.Errorf("analyze: timestamp %q: %w", s, err)
		}
		if i > 0 && v >= 60 {
			return 0, fmt.Errorf("analyze: timestamp %q: component %q must be below 60", s, p)
		}
		total = total*60 + v
	}
	if math.IsNaN(total) || math.IsInf(total, 0) {
		return 0, fmt.Errorf("analyze: timestamp %q: out of range", s)
	}
	return total, nil
}

// parseComponent parses one ':'-separated component: ASCII digits, plus an optional
// ".digits" fraction when frac is true.
func parseComponent(p string, frac bool) (float64, error) {
	intPart, fracPart, hasDot := strings.Cut(p, ".")
	if hasDot && !frac {
		return 0, fmt.Errorf("fraction only allowed in the last component")
	}
	if !allDigits(intPart) || (hasDot && !allDigits(fracPart)) {
		return 0, fmt.Errorf("invalid component %q", p)
	}
	v, err := strconv.ParseFloat(p, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid component %q: %w", p, err)
	}
	return v, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
