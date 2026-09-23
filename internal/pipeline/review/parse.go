package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/analyze"
)

// wireImage decodes one answer entry. Pointers tell a missing field from false/0;
// numbers are floats so 4.0 is accepted where an integer is asked for.
type wireImage struct {
	ID                  string    `json:"id"`
	Rotation            *float64  `json:"rotation"`
	View                string    `json:"view"`
	SubjectFullyVisible *bool     `json:"subjectFullyVisible"`
	Quality             *float64  `json:"quality"`
	Box2D               []float64 `json:"box_2d"`
	SameAs              string    `json:"sameAs"`
	Issues              string    `json:"issues"`
}

// parseAssessments decodes the answer (fence-tolerant) into verdicts by candidate ID.
// A missing, null or non-array "images" is invalid output. Entries are then checked one
// by one and a bad entry is ignored (counted), never fatal: an entry that does not
// decode, names no sent frame (see findFrame) or repeats a frame already answered (the
// first wins). Field rules: the rotation is the orientation call's (frame.rot) for a
// frame it oriented; for a frame sent as stored, the answer's rotation snaps to the
// nearest multiple of 90 (mod 360, missing → 0); view matches the domain's views case-insensitively, anything else →
// domain.OtherShotID; subjectFullyVisible missing → false; quality rounds and clamps to
// 1..5 (missing → 1); box_2d is converted by boxFrom and mapped from the image as sent
// to the frame as stored (toStored with frame.rot, never with the answer's rotation);
// issues is capped at MaxIssuesRunes. Frames without an entry are absent.
func parseAssessments(text string, frames []frame, d *domain.Config, minQ int) (map[string]Assessment, int, error) {
	payload := analyze.ExtractJSON(text)
	if payload == "" {
		return nil, 0, errors.New("empty response")
	}
	var w struct {
		Images *[]json.RawMessage `json:"images"`
	}
	if err := json.Unmarshal([]byte(payload), &w); err != nil {
		return nil, 0, fmt.Errorf("decode json: %w", err)
	}
	if w.Images == nil {
		return nil, 0, errors.New("missing images")
	}

	views := make(map[string]string)
	for _, v := range viewIDs(d) {
		views[strings.ToLower(v)] = v
	}

	out := make(map[string]Assessment, len(frames))
	ignored := 0
	for _, raw := range *w.Images {
		var e wireImage
		if err := json.Unmarshal(raw, &e); err != nil {
			ignored++
			continue
		}
		id, fr, ok := findFrame(frames, strings.TrimSpace(e.ID))
		if !ok {
			ignored++
			continue
		}
		if _, dup := out[id]; dup {
			ignored++
			continue
		}
		as := Assessment{
			CandidateID: id,
			RotateCW:    fr.rot,
			View:        domain.OtherShotID,
			Quality:     clampQuality(e.Quality),
			// The model measured the box on the copy sent (rotated by fr.rot); crop.Plan
			// wants it in the frame as stored.
			Box:    toStored(boxFrom(e.Box2D), fr.rot),
			Issues: analyze.Truncate(e.Issues, MaxIssuesRunes),
		}
		if v, ok := views[strings.ToLower(strings.TrimSpace(e.View))]; ok {
			as.View = v
		}
		if !fr.oriented && e.Rotation != nil {
			// Sent as stored (the orientation call failed or skipped it; old recordings):
			// only the export turns it upright, the box above stays as measured.
			as.RotateCW = snapRotation(e.Rotation)
		}
		if e.SubjectFullyVisible != nil {
			as.FullyVisible = *e.SubjectFullyVisible
		}
		as.Usable = as.FullyVisible && as.Quality >= minQ && as.View != ViewNone
		// Only a link to an EARLIER frame counts (the list is in shot-priority order, so the
		// kept one is the higher-priority view); self-links and unknown ids are ignored.
		if dup, dfr, ok := findFrame(frames, strings.TrimSpace(e.SameAs)); ok && dup != id && frameIndex(frames, dfr) < frameIndex(frames, fr) {
			as.DuplicateOf = dup
		}
		out[id] = as
	}
	return out, ignored, nil
}

// findFrame returns the candidate ID of the sent frame the model's id names: its key
// ("img3"), else its candidate ID, each matched exactly, else case-insensitively (ULIDs
// are case-insensitive).
func findFrame(frames []frame, id string) (string, frame, bool) {
	if id == "" {
		return "", frame{}, false
	}
	for _, match := range []func(frame) bool{
		func(f frame) bool { return f.key != "" && f.key == id },
		func(f frame) bool { return f.key != "" && strings.EqualFold(f.key, id) },
		func(f frame) bool { return f.cand.ID == id },
		func(f frame) bool { return strings.EqualFold(f.cand.ID, id) },
	} {
		for _, f := range frames {
			if match(f) {
				return f.cand.ID, f, true
			}
		}
	}
	return "", frame{}, false
}

// snapRotation returns r snapped to the nearest of 0, 90, 180, 270 (mod 360; halfway
// rounds away from zero). nil, NaN and ±Inf → 0.
func snapRotation(r *float64) int {
	if r == nil || math.IsNaN(*r) || math.IsInf(*r, 0) {
		return 0
	}
	q := int(math.Round(math.Mod(*r, 360) / 90))
	return ((q%4 + 4) % 4) * 90
}

// clampQuality rounds q and clamps it to 1..5; nil and NaN → 1.
func clampQuality(q *float64) int {
	if q == nil || math.IsNaN(*q) {
		return 1
	}
	return int(math.Max(1, math.Min(5, math.Round(*q))))
}

// boxFrom converts Gemini's box_2d [ymin, xmin, ymax, xmax] in 0-1000 to a normalised
// Box. Coordinates are clamped to [0,1] and swapped when inverted; anything that is not
// four finite numbers or does not make a Valid box gives the zero Box.
func boxFrom(v []float64) Box {
	if len(v) != 4 {
		return Box{}
	}
	n := make([]float64, 4)
	for i, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return Box{}
		}
		n[i] = math.Max(0, math.Min(1, x/1000))
	}
	b := Box{X0: n[1], Y0: n[0], X1: n[3], Y1: n[2]}
	if b.X0 > b.X1 {
		b.X0, b.X1 = b.X1, b.X0
	}
	if b.Y0 > b.Y1 {
		b.Y0, b.Y1 = b.Y1, b.Y0
	}
	if !b.Valid() {
		return Box{}
	}
	return b
}

// frameIndex is f's position in the shortlist (-1 if absent).
func frameIndex(frames []frame, f frame) int {
	for i := range frames {
		if frames[i].cand.ID == f.cand.ID {
			return i
		}
	}
	return -1
}
