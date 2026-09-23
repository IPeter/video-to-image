package selectr

import (
	"fmt"
	"strings"

	"video-to-image/internal/domain"
)

// NoScenesExplanation is the message for a result with no images (the brief prefers it
// over returning unrelated frames).
const NoScenesExplanation = "No suitable scenes found."

// explain builds the user-facing summary. Shots are named by their labels.
func explain(n, imageCount int, missing []domain.Shot) string {
	if n == 0 {
		return NoScenesExplanation
	}
	var b strings.Builder
	if n >= imageCount {
		b.WriteString(plural(n, "image", "images") + " selected.")
		if len(missing) > 0 {
			fmt.Fprintf(&b, " Not clearly visible in the video: %s.", labels(missing))
		}
		return b.String()
	}
	if n == 1 {
		b.WriteString("Only 1 distinct high-quality view was found.")
	} else {
		fmt.Fprintf(&b, "Only %d distinct high-quality views were found.", n)
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, " Missing: %s.", labels(missing))
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// labels joins the shot labels with ", " (the ID stands in for an empty label).
func labels(shots []domain.Shot) string {
	parts := make([]string, len(shots))
	for i, s := range shots {
		parts[i] = s.Label
		if parts[i] == "" {
			parts[i] = s.ID
		}
	}
	return strings.Join(parts, ", ")
}
