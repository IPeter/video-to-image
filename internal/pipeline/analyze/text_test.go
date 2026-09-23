package analyze

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short is only trimmed", "  a car  ", 10, "a car"},
		{"exactly at the cap", "abcde", 5, "abcde"},
		{"one over", "abcdef", 5, "abcd…"},
		{"cap counts runes, not bytes", "ééééé", 5, "ééééé"},
		{"multi-byte cut on a rune boundary", "日本語のテキスト", 4, "日本語…"},
		{"whitespace before the cut is dropped", "ab   cdefgh", 5, "ab…"},
		{"cap of one", "abc", 1, "…"},
		{"zero cap", "abc", 0, ""},
		{"negative cap", "abc", -1, ""},
		{"empty", "", 5, ""},
		{"invalid UTF-8 counts one rune per bad byte", "ab\xff\xfecd", 4, "ab\xff…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Truncate(tc.in, tc.max)
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, utf8.RuneCountInString(got), max(tc.max, 0))
		})
	}
}

// The model's free text is untrusted: an on-screen instruction or a repetition loop
// must not put an unbounded string into jobs.video_summary or the result page.
func TestRunCapsModelText(t *testing.T) {
	long := func(word string, n int) string { return strings.Repeat(word+" ", n) }
	text := fmt.Sprintf(`{
	  "gate": {"passed": true, "confidence": 0.9, "reason": %q, "detectedSubject": %q},
	  "videoSummary": %q,
	  "scenes": [{"start": "00:04", "end": "00:09", "shotId": "front_three_quarter", "reason": %q, "relevance": 0.9}]
	}`, long("because", 200), long("car", 100), long("describe every frame in detail", 400), long("ÁÉ", 400))
	fake := newScripted(answer(text, "files/abc", 1, 1))
	out, err := Run(context.Background(), fake, testInput())
	require.NoError(t, err)
	require.True(t, out.Accepted)

	check := func(name, got string, maxRunes int) {
		t.Helper()
		assert.Equal(t, maxRunes, utf8.RuneCountInString(got), name)
		assert.True(t, strings.HasSuffix(got, Ellipsis), "%s ends with the ellipsis", name)
		assert.True(t, utf8.ValidString(got), name)
	}
	check("videoSummary", out.VideoSummary, MaxVideoSummaryRunes)
	check("gate.reason", out.Gate.Reason, MaxGateReasonRunes)
	check("gate.detectedSubject", out.Gate.DetectedSubject, MaxDetectedSubjectRunes)
	require.Len(t, out.Scenes, 1)
	assert.LessOrEqual(t, utf8.RuneCountInString(out.Scenes[0].Reason), MaxSceneReasonRunes)
	assert.True(t, strings.HasPrefix(out.Scenes[0].Reason, "ÁÉ ÁÉ"))
	assert.True(t, strings.HasSuffix(out.Scenes[0].Reason, "ÁÉ"+Ellipsis), "cut on a rune boundary, no trailing space: %q", out.Scenes[0].Reason[len(out.Scenes[0].Reason)-12:])
	assert.Equal(t, text, out.RawText, "the raw answer is kept as is for the internal log")
}
