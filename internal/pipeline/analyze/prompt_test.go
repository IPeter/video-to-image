package analyze

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

func TestBuildPromptGolden(t *testing.T) {
	system, user := BuildPrompt(testDomain())
	assert.Equal(t, "You analyze short videos and return structured JSON only.", system)

	golden := filepath.Join("testdata", "prompt_autos.golden")
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(golden, []byte(user), 0o644))
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err, "run: go test ./internal/pipeline/analyze -run TestBuildPromptGolden -update")
	assert.Equal(t, string(want), user)
}

func TestBuildPromptStructure(t *testing.T) {
	d := testDomain()
	_, user := BuildPrompt(d)

	// Sections appear in template order.
	order := []string{
		"Step 1 — Gate. Decide whether this video primarily shows: a single car, shown for a sales listing.\n",
		"Pass if one car is the main subject.\nFail if it is mainly people or scenery.\n\nSeveral different cars also fail.\n",
		"If it does not, set gate.passed=false, explain briefly in gate.reason, and return an empty scenes array.",
		"gate.confidence",
		"gate.detectedSubject",
		"videoSummary",
		"Step 2 — Scenes. Only if the gate passed.\n",
		"Prefer moments where the whole car is in frame.\nAvoid motion blur.\n",
		"Shot list (list up to 2 distinct moments for each shot that is present; a walkaround usually shows an exterior view more than once, and the later take is often the better one):\n",
		"- front_three_quarter: Front and one side at roughly 45°, whole car in frame\n",
		"- side_profile: Full side of the car, perpendicular\n",
		"- interior_dashboard: Dashboard, steering wheel and center console\n",
		`Return up to 5 scenes. For each: start and end time as "MM:SS", the shotId it best fulfils (or "other")`,
		"a one-sentence reason, and a relevance score 0–1.",
		"Prefer moments where the camera is steady and the subject fills the frame. Avoid near-duplicate moments.",
	}
	pos := 0
	for _, s := range order {
		i := strings.Index(user[pos:], s)
		require.GreaterOrEqual(t, i, 0, "missing or out of order: %q", s)
		pos += i + len(s)
	}

	assertTidy(t, user)
}

// The shipped car domain must produce a tidy prompt that mentions every shot in
// priority order.
func TestBuildPromptShippedCars(t *testing.T) {
	d, err := domain.LoadFile(filepath.Join("..", "..", "..", "configs", "domains", "cars.yaml"))
	require.NoError(t, err)
	_, user := BuildPrompt(d)

	assert.Contains(t, user, "Decide whether this video primarily shows: a single passenger vehicle (car, SUV, van or pickup) being shown or walked around, as for a sales listing.\n")
	assert.Contains(t, user, "recording, or if multiple different vehicles are shown with no clear single subject.\nIf it does not,")
	assert.Contains(t, user, "Return up to 30 scenes.")
	pos := 0
	for _, s := range d.Shots {
		line := "- " + s.ID + ": " + s.Description + "\n"
		i := strings.Index(user[pos:], line)
		require.GreaterOrEqual(t, i, 0, "shot line missing or out of order: %q", line)
		pos += i + len(line)
	}
	assertTidy(t, user)
}

func assertTidy(t *testing.T, s string) {
	t.Helper()
	assert.NotContains(t, s, "\n\n\n", "no runs of blank lines")
	assert.NotContains(t, s, "..", "no doubled periods")
	assert.False(t, strings.HasPrefix(s, "\n") || strings.HasSuffix(s, "\n"), "no blank edges")
	for i, line := range strings.Split(s, "\n") {
		assert.Equal(t, strings.TrimRight(line, " \t"), line, "trailing whitespace on line %d", i+1)
	}
}

func TestCleanBlock(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  \n\t\n", ""},
		{"one line\n", "one line"},
		{"a  \r\nb\t\r\n", "a\nb"},
		{"\n\na\n\n\n\nb\n\n", "a\n\nb"},
		{"    indented\n      more\n    back\n", "indented\n  more\nback"},
		{"a\n  b", "a\n  b"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, cleanBlock(c.in), "cleanBlock(%q)", c.in)
	}
}

func TestResponseSchema(t *testing.T) {
	d := testDomain()
	s := ResponseSchema(d)
	require.NotNil(t, s)

	assert.Equal(t, "object", s.Type)
	assert.Equal(t, []string{"gate", "videoSummary", "scenes"}, s.PropertyOrdering)
	assert.ElementsMatch(t, []string{"gate", "videoSummary", "scenes"}, s.Required)

	gate := s.Properties["gate"]
	require.NotNil(t, gate)
	assert.Equal(t, "object", gate.Type)
	assert.Equal(t, []string{"passed", "confidence", "reason", "detectedSubject"}, gate.PropertyOrdering)
	assert.ElementsMatch(t, gate.PropertyOrdering, gate.Required)
	assert.Equal(t, "boolean", gate.Properties["passed"].Type)
	assertUnit(t, gate.Properties["confidence"])
	assert.Equal(t, "string", gate.Properties["reason"].Type)
	assert.Equal(t, "string", gate.Properties["detectedSubject"].Type)

	assert.Equal(t, "string", s.Properties["videoSummary"].Type)

	scenes := s.Properties["scenes"]
	require.NotNil(t, scenes)
	assert.Equal(t, "array", scenes.Type)
	item := scenes.Items
	require.NotNil(t, item)
	assert.Equal(t, "object", item.Type)
	assert.Equal(t, []string{"start", "end", "shotId", "reason", "relevance"}, item.PropertyOrdering)
	assert.ElementsMatch(t, item.PropertyOrdering, item.Required)
	assert.Equal(t, "string", item.Properties["start"].Type)
	assert.Equal(t, "string", item.Properties["end"].Type)
	assert.Equal(t, "string", item.Properties["reason"].Type)
	assertUnit(t, item.Properties["relevance"])
	shot := item.Properties["shotId"]
	assert.Equal(t, "string", shot.Type)
	assert.Equal(t, []string{"front_three_quarter", "side_profile", "interior_dashboard", domain.OtherShotID}, shot.Enum)

	// Nothing is left out of the orderings.
	assert.Len(t, s.Properties, len(s.PropertyOrdering))
	assert.Len(t, gate.Properties, len(gate.PropertyOrdering))
	assert.Len(t, item.Properties, len(item.PropertyOrdering))

	// Each call returns an independent schema (callers may mutate it).
	s2 := ResponseSchema(d)
	s2.Properties["gate"].Properties["confidence"].Maximum = nil
	assert.NotNil(t, s.Properties["gate"].Properties["confidence"].Maximum)
}

func assertUnit(t *testing.T, s *analyzer.Schema) {
	t.Helper()
	require.NotNil(t, s)
	assert.Equal(t, "number", s.Type)
	require.NotNil(t, s.Minimum)
	require.NotNil(t, s.Maximum)
	assert.Equal(t, 0.0, *s.Minimum)
	assert.Equal(t, 1.0, *s.Maximum)
}
