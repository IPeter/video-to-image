package analyze

import (
	"fmt"
	"strings"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
)

// SystemPrompt is the system instruction of the analyze call.
const SystemPrompt = "You analyze short videos and return structured JSON only."

// RetryNudge is appended as an extra text part when the first answer was not valid JSON
// matching the schema (plan/03-pipeline.md "Error handling and retries").
const RetryNudge = "Your previous answer was not valid JSON matching the required schema. Return valid JSON only."

// BuildPrompt composes the system and user prompt from the domain config
// (template in plan/03-pipeline.md step 2). YAML block scalars are normalised: trailing
// whitespace and blank edge lines are removed, common indentation is stripped and runs
// of blank lines collapse to one; one-line values (subject, shot descriptions) are
// reflowed to single spaces. The result depends only on d, so it is stable per domain.
func BuildPrompt(d *domain.Config) (system, user string) {
	var b strings.Builder

	subject := strings.TrimRight(cleanInline(d.Gate.Subject), ". ")
	fmt.Fprintf(&b, "Step 1 — Gate. Decide whether this video primarily shows: %s.\n", subject)
	writeBlock(&b, d.Gate.Instructions)
	b.WriteString("If it does not, set gate.passed=false, explain briefly in gate.reason, and return an empty scenes array.\n")
	b.WriteString("In every case, give a brief explanation of the verdict in gate.reason, set gate.confidence to how " +
		"sure you are of the verdict (0–1), set gate.detectedSubject to a short noun phrase naming what the video " +
		"mainly shows, and set videoSummary to one sentence describing the whole video.\n")

	b.WriteString("\nStep 2 — Scenes. Only if the gate passed.\n")
	writeBlock(&b, d.Selection.Instructions)

	b.WriteString("\nShot list (find the best moment for each, if present):\n")
	for _, s := range d.Shots {
		fmt.Fprintf(&b, "- %s: %s\n", strings.TrimSpace(s.ID), cleanInline(s.Description))
	}

	fmt.Fprintf(&b, "\nReturn up to %d scenes. For each: start and end time as \"MM:SS\", the shotId it best "+
		"fulfils (or %q), a one-sentence reason, and a relevance score 0–1. Prefer moments where the camera "+
		"is steady and the subject fills the frame. Avoid near-duplicate moments.",
		d.Selection.MaxScenes, domain.OtherShotID)

	return SystemPrompt, b.String()
}

// ResponseSchema is the structured-output schema; shotId is an enum of the domain's
// shot IDs plus domain.OtherShotID. Every field is required and PropertyOrdering makes
// the model emit the gate first, then the summary, then the scenes.
func ResponseSchema(d *domain.Config) *analyzer.Schema {
	gateOrder := []string{"passed", "confidence", "reason", "detectedSubject"}
	sceneOrder := []string{"start", "end", "shotId", "reason", "relevance"}
	topOrder := []string{"gate", "videoSummary", "scenes"}
	shotEnum := append(d.ShotIDs(), domain.OtherShotID)

	return &analyzer.Schema{
		Type: "object",
		Properties: map[string]*analyzer.Schema{
			"gate": {
				Type:        "object",
				Description: "Whether the video primarily shows the required subject. Decided before the scenes.",
				Properties: map[string]*analyzer.Schema{
					"passed":          {Type: "boolean", Description: "true if the video primarily shows the required subject"},
					"confidence":      unitNumber("how sure you are of the verdict, 0 to 1"),
					"reason":          {Type: "string", Description: "brief explanation of the verdict"},
					"detectedSubject": {Type: "string", Description: "short noun phrase naming what the video mainly shows"},
				},
				PropertyOrdering: gateOrder,
				Required:         gateOrder,
			},
			"videoSummary": {Type: "string", Description: "one sentence describing the whole video"},
			"scenes": {
				Type:        "array",
				Description: "relevant moments; empty when the gate failed",
				Items: &analyzer.Schema{
					Type: "object",
					Properties: map[string]*analyzer.Schema{
						"start":     {Type: "string", Description: `start time as "MM:SS"`},
						"end":       {Type: "string", Description: `end time as "MM:SS"`},
						"shotId":    {Type: "string", Description: "the shot this moment best fulfils", Enum: shotEnum},
						"reason":    {Type: "string", Description: "one sentence on why this moment fits the shot"},
						"relevance": unitNumber("how well the moment fulfils the shot, 0 to 1"),
					},
					PropertyOrdering: sceneOrder,
					Required:         sceneOrder,
				},
			},
		},
		PropertyOrdering: topOrder,
		Required:         topOrder,
	}
}

func unitNumber(desc string) *analyzer.Schema {
	lo, hi := 0.0, 1.0
	return &analyzer.Schema{Type: "number", Description: desc, Minimum: &lo, Maximum: &hi}
}

// writeBlock appends a cleaned multi-line block followed by a newline; empty blocks
// write nothing.
func writeBlock(b *strings.Builder, s string) {
	if c := cleanBlock(s); c != "" {
		b.WriteString(c)
		b.WriteByte('\n')
	}
}

// cleanInline collapses all whitespace (including line breaks) to single spaces.
func cleanInline(s string) string { return strings.Join(strings.Fields(s), " ") }

// cleanBlock normalises a YAML block scalar: CRLF → LF, trailing whitespace per line
// removed, leading/trailing blank lines dropped, the common indentation of non-blank
// lines stripped and runs of blank lines collapsed to a single blank line.
func cleanBlock(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	indent := -1
	for i, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		lines[i] = l
		if l == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l == "" {
			if len(out) == 0 || out[len(out)-1] == "" {
				continue
			}
			out = append(out, "")
			continue
		}
		out = append(out, l[indent:])
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}
