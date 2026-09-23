package review

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"log/slog"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/pipeline/analyze"
)

// Orientation is decided before the review, because models are unreliable at "how many
// degrees must this be rotated": on the user's phone video (rotated mid-recording)
// flash-lite answered 90 where 270 was right, and asking for "the edge nearest the top
// of the scene" got 2 of 4 frames right. Showing the same frame rotated four ways in a
// 2x2 grid and asking which copy looks upright got 4 of 4 right, at no thinking cost
// (measured 2026-09-23). Every shortlisted frame gets its own grid: the phone can turn
// at any moment, also inside one scene (on that video it turned at about 6 s, and the
// side-profile scene's shortlist had frames on both sides of the turn). A grid is one
// image, billed like one review image.

// gridTile is the side of one grid cell; the frame is fitted inside it.
const gridTile = 264

// orientAttempts bounds the orientation calls of one review. A bad answer or an
// unavailable model gets one more try: the call is cheap, and without it the frames
// are judged and exported as stored. Context errors and local problems are not retried.
const orientAttempts = 2

// gridCells lists the grid positions and the clockwise rotation shown in each.
var gridCells = []struct {
	name   string
	rotCW  int
	dx, dy int
}{
	{"top-left", 0, 0, 0},
	{"top-right", 90, 1, 0},
	{"bottom-left", 180, 0, 1},
	{"bottom-right", 270, 1, 1},
}

// orientPrompt is the instruction for the grid call.
const orientPrompt = "Each image is a 2x2 grid showing the SAME photo four times, each copy rotated " +
	"differently: top-left, top-right, bottom-left, bottom-right. For each image answer which copy " +
	"shows the scene upright like a normal photo taken by a person standing: ground and floor at the " +
	"bottom, sky, roof or ceiling at the top, the car's wheels below its body, text readable."

// gridFrame is one shortlisted frame's grid for the orientation call.
type gridFrame struct {
	cand string // candidate ID
	key  string // "img<n>", the id the model sees
	path string // the grid JPEG written for the call
}

// orientFrames returns the clockwise rotation that makes each shortlisted frame
// upright, by candidate ID (never nil). A frame the answer does not cover, or whose
// grid could not be written, is absent: it is judged as stored. The call is retried
// once (see orientAttempts); when it still fails, the error is the last failure and the
// map is empty. The usage of every call made is returned for billing.
func orientFrames(ctx context.Context, a analyzer.Analyzer, in Input, frames []frame, dir string) (map[string]int, []analyzer.Usage, error) {
	rot := map[string]int{}
	grids, err := writeGrids(ctx, dir, frames, in.JobID)
	if err != nil || len(grids) == 0 {
		return rot, nil, err
	}
	req := orientRequest(in, grids)
	var calls []analyzer.Usage
	for attempt := 1; ; attempt++ {
		answers, usage, err := askOrientation(ctx, a, in.JobID, req, grids)
		if usage != nil {
			calls = append(calls, *usage)
		}
		if err == nil {
			attrs := []any{"job", in.JobID, "domain", in.Domain.Key, "frames", len(grids),
				"answered", len(answers), "attempts", attempt, "rotations", answers}
			if len(answers) < len(grids) {
				slog.WarnContext(ctx, "review: incomplete orientation answer; the rest is judged as stored", attrs...)
			} else {
				slog.InfoContext(ctx, "review: orientation", attrs...)
			}
			return answers, calls, nil
		}
		retry := errors.Is(err, analyzer.ErrUnavailable) || errors.Is(err, analyze.ErrBadOutput)
		if attempt >= orientAttempts || !retry || ctx.Err() != nil {
			return rot, calls, err
		}
		slog.WarnContext(ctx, "review: orientation failed; retrying once", "job", in.JobID, "err", err)
	}
}

// askOrientation makes one orientation call and maps the answer to rotations by
// candidate ID. An answer that covers none of the grids is bad output. The usage is
// returned whenever the call was made, also with a bad answer.
func askOrientation(ctx context.Context, a analyzer.Analyzer, jobID string, req *analyzer.Request, grids []gridFrame) (map[string]int, *analyzer.Usage, error) {
	resp, err := analyze.Generate(ctx, a, req, analyze.CallTimeout)
	if err != nil {
		return nil, nil, err
	}
	releaseFiles(ctx, a, jobID, resp.FileHandles)
	answers, err := parseOrientation(resp.Text)
	rot := make(map[string]int, len(grids))
	if err == nil {
		for _, g := range grids {
			if deg, ok := answers[strings.ToLower(g.key)]; ok {
				rot[g.cand] = deg
			}
		}
		if len(rot) == 0 {
			err = errors.New("no image answered")
		}
	}
	if err != nil {
		err = limitHint(err, req, resp)
		return nil, &resp.Usage, fmt.Errorf("review: orientation: %w", &analyze.BadOutputError{Raw: resp.Text, Err: err})
	}
	return rot, &resp.Usage, nil
}

// writeGrids writes the orientation grid of every frame to dir/orient-<candidateID>.jpg
// (in parallel) and returns the grids that were written, keyed "img1".."imgN" in
// shortlist order: the keys of the review call whenever every frame could be prepared.
// A frame whose grid cannot be written is logged and left out; when none is left the
// first error is returned. Context errors are returned as-is.
func writeGrids(ctx context.Context, dir string, frames []frame, jobID string) ([]gridFrame, error) {
	paths := make([]string, len(frames))
	errs := make([]error, len(frames))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(runtime.GOMAXPROCS(0))
	for i := range frames {
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			// A bad frame is recorded, not returned: it must not cancel the others.
			paths[i] = filepath.Join(dir, "orient-"+frames[i].cand.ID+".jpg")
			errs[i] = writeGrid(frames[i].cand.SmallPath, paths[i])
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var grids []gridFrame
	var first error
	for i, f := range frames {
		if errs[i] != nil {
			slog.WarnContext(ctx, "review: orientation grid left out", "job", jobID, "candidate", f.cand.ID, "err", errs[i])
			first = cmp.Or(first, errs[i])
			continue
		}
		grids = append(grids, gridFrame{cand: f.cand.ID, key: "img" + strconv.Itoa(len(grids)+1), path: paths[i]})
	}
	if len(grids) == 0 && first != nil {
		return nil, fmt.Errorf("review: orientation: no grid could be written: %w", first)
	}
	return grids, nil
}

func orientRequest(in Input, grids []gridFrame) *analyzer.Request {
	d := in.Domain
	keys := make([]string, len(grids))
	parts := []analyzer.Part{{Text: orientPrompt}}
	for i, g := range grids {
		keys[i] = g.key
		parts = append(parts,
			analyzer.Part{Text: "Image " + g.key + ":"},
			analyzer.Part{FilePath: g.path, MIMEType: "image/jpeg"})
	}
	parts = append(parts, analyzer.Part{Text: `Answer with {"images":[{"id","upright"}]}, one entry per image, in order.`})
	cells := make([]string, len(gridCells))
	for i, c := range gridCells {
		cells[i] = c.name
	}
	schema := &analyzer.Schema{
		Type: "object",
		Properties: map[string]*analyzer.Schema{
			"images": {Type: "array", Items: &analyzer.Schema{
				Type: "object",
				Properties: map[string]*analyzer.Schema{
					"id":      {Type: "string", Enum: keys},
					"upright": {Type: "string", Enum: cells, Description: "the grid position of the upright copy"},
				},
				Required:         []string{"id", "upright"},
				PropertyOrdering: []string{"id", "upright"},
			}},
		},
		Required: []string{"images"},
	}
	req := &analyzer.Request{
		Step:            analyzer.StepReview,
		JobID:           in.JobID,
		DomainKey:       d.Key,
		Model:           d.Model.Name,
		Temperature:     d.Model.Temperature,
		ThinkingBudget:  d.Model.ThinkingBudget,
		ThinkingLevel:   d.Model.ThinkingLevel,
		MaxOutputTokens: outputBudget(d.Model.EffectiveMaxOutputTokens(), orientBaseTokens+orientTokensPerFrame*len(grids)),
		SystemPrompt:    "You judge the orientation of photos and return structured JSON only.",
		Parts:           parts,
		ResponseSchema:  schema,
		ShotIDs:         d.ShotIDs(),
	}
	if in.CacheKey != "" {
		req.CacheKey = in.CacheKey + ":" + d.Key + ":review-orient"
	}
	return req
}

// parseOrientation maps the answer's ids (trimmed, lower-cased) to clockwise rotations.
// Unknown positions are skipped (that frame stays as stored); for an id answered twice
// the first valid answer wins.
func parseOrientation(text string) (map[string]int, error) {
	payload := analyze.ExtractJSON(text)
	if payload == "" {
		return nil, errors.New("empty response")
	}
	var w struct {
		Images *[]struct {
			ID      string `json:"id"`
			Upright string `json:"upright"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(payload), &w); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	if w.Images == nil {
		return nil, errors.New("missing images")
	}
	out := map[string]int{}
	for _, e := range *w.Images {
		id := strings.ToLower(strings.TrimSpace(e.ID))
		if _, dup := out[id]; dup {
			continue
		}
		pos := strings.ToLower(strings.TrimSpace(e.Upright))
		for _, c := range gridCells {
			if c.name == pos {
				out[id] = c.rotCW
			}
		}
	}
	return out, nil
}

// writeGrid renders the 2x2 orientation grid for one frame: the frame fitted into a
// gridTile square four times, rotated per gridCells, on white.
func writeGrid(src, dst string) error {
	img, err := decodeJPEG(src)
	if err != nil {
		return err
	}
	b := img.Bounds()
	scale := float64(gridTile-8) / float64(max(b.Dx(), b.Dy()))
	small := resizeArea(img, max(1, roundInt(float64(b.Dx())*scale)), max(1, roundInt(float64(b.Dy())*scale)))
	grid := image.NewRGBA(image.Rect(0, 0, 2*gridTile, 2*gridTile))
	draw.Draw(grid, grid.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	for _, c := range gridCells {
		r := rotateRGBA(small, c.rotCW)
		rb := r.Bounds()
		off := image.Pt(c.dx*gridTile+(gridTile-rb.Dx())/2, c.dy*gridTile+(gridTile-rb.Dy())/2)
		draw.Draw(grid, rb.Add(off), r, rb.Min, draw.Src)
	}
	return encodeJPEG(dst, grid)
}

// rotateRGBA returns m rotated clockwise by deg (0, 90, 180, 270).
func rotateRGBA(m *image.RGBA, deg int) *image.RGBA {
	b := m.Bounds()
	w, h := b.Dx(), b.Dy()
	if deg%180 != 0 {
		w, h = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			var nx, ny int
			switch deg {
			case 90:
				nx, ny = b.Dy()-1-y, x
			case 180:
				nx, ny = b.Dx()-1-x, b.Dy()-1-y
			case 270:
				nx, ny = y, b.Dx()-1-x
			default:
				nx, ny = x, y
			}
			out.SetRGBA(nx, ny, m.RGBAAt(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}

// toStored converts a box measured in the image AS SENT (the frame rotated clockwise by
// rotCW, the rotation applied before sending) back to the frame as stored, which is
// what crop.Plan expects. A frame sent as stored (rotCW 0) keeps its box, even when the
// model reports that it needs a rotation.
func toStored(b Box, rotCW int) Box {
	if !b.Valid() || rotCW%360 == 0 {
		return b
	}
	// Undo a clockwise rotation by rotCW = rotate clockwise by 360-rotCW.
	inv := (360 - rotCW%360) % 360
	p := func(x, y float64) (float64, float64) {
		switch inv {
		case 90:
			return 1 - y, x
		case 180:
			return 1 - x, 1 - y
		case 270:
			return y, 1 - x
		}
		return x, y
	}
	x0, y0 := p(b.X0, b.Y0)
	x1, y1 := p(b.X1, b.Y1)
	return Box{X0: min(x0, x1), Y0: min(y0, y1), X1: max(x0, x1), Y1: max(y0, y1)}
}
