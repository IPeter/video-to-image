package analyze

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Caps on the free text the model writes. It is untrusted (on-screen text in an upload
// can steer the model) and ends up in jobs.video_summary, job_steps.meta, the candidate
// reasons and the result page, so it is bounded however long the answer was.
const (
	MaxVideoSummaryRunes    = 500
	MaxGateReasonRunes      = 300
	MaxDetectedSubjectRunes = 80
	MaxSceneReasonRunes     = 300
)

// Ellipsis ends a string that Truncate shortened.
const Ellipsis = "…"

// Truncate trims surrounding whitespace from s and caps it at maxRunes runes. A longer
// string is cut on a rune boundary, loses the whitespace before the cut and gets
// Ellipsis appended; the result, ellipsis included, is at most maxRunes runes.
// maxRunes ≤ 0 returns "". Shared with the rerank step.
func Truncate(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return ""
	}
	if len(s) <= maxRunes || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxRunes-1 {
			return strings.TrimRightFunc(s[:i], unicode.IsSpace) + Ellipsis
		}
		n++
	}
	return s // unreachable: s has more than maxRunes runes
}
