package analyze

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseTimestamp(t *testing.T) {
	ok := []struct {
		in   string
		want float64
	}{
		{"00:04", 4},
		{"00:00", 0},
		{"0:09", 9},
		{"01:30", 90},
		{"1:02", 62},
		{"75:10", 75*60 + 10}, // minutes may exceed 59 without an hour component
		{"01:02:03", 3723},
		{"1:02:03", 3723},
		{"0:00:59", 59},
		{"100:00:00", 360000},
		{"00:04.25", 4.25},
		{"1:02.5", 62.5},
		{"0:01:02.125", 62.125},
		{"12", 12},
		{"0", 0},
		{"12.5", 12.5},
		{"007", 7},
		{"  00:04 \n", 4},
		{"\t12.5\t", 12.5},
		{"12.5s", 12.5},
		{"12s", 12},
		{"12 s", 12},
		{"12S", 12},
		{" 00:07s ", 7},
		{"59.999", 59.999},
		{"00:59.999", 59.999},
	}
	for _, c := range ok {
		t.Run("ok/"+c.in, func(t *testing.T) {
			got, err := ParseTimestamp(c.in)
			if assert.NoError(t, err) {
				assert.InDelta(t, c.want, got, 1e-9)
			}
		})
	}

	bad := []string{
		"",
		"   ",
		"s",
		"-1",
		"-00:04",
		"00:-04",
		"+5",
		"00:60",    // seconds ≥ 60 with a minutes component
		"1:75",     // same
		"01:60:00", // minutes ≥ 60 with an hours component
		"01:02:60",
		"1:02:03:04",
		"::",
		":30",
		"1:",
		"1::2",
		"00:04.",
		".5",
		"1.5:00", // fraction only on the last component
		"00:04.2.5",
		"1e3",
		"NaN",
		"nan",
		"Inf",
		"+Inf",
		"0x10",
		"12,5",
		"four",
		"00:04 ss",
		"12ss",
		"١٢", // non-ASCII digits
	}
	bad = append(bad,
		strings.Repeat("9", 400),          // overflows float64
		strings.Repeat("9", 306)+":00:00", // finite components, infinite total
	)
	for _, in := range bad {
		name := in
		if len(name) > 20 {
			name = name[:20] + "…"
		}
		t.Run("bad/"+name, func(t *testing.T) {
			_, err := ParseTimestamp(in)
			assert.Error(t, err)
		})
	}
}
