package review

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/analyze"
)

// upright applies a clockwise rotation to a stored-frame box (the forward transform
// crop.Plan uses), so toStored can be checked as its inverse.
func upright(b Box, rotCW int) Box {
	p := func(x, y float64) (float64, float64) {
		switch rotCW {
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

func TestToStoredInvertsTheRotation(t *testing.T) {
	stored := Box{X0: 0.1, Y0: 0.2, X1: 0.5, Y1: 0.9}
	for _, rot := range []int{0, 90, 180, 270} {
		got := toStored(upright(stored, rot), rot)
		assert.InDelta(t, stored.X0, got.X0, 1e-9, "rot %d", rot)
		assert.InDelta(t, stored.Y0, got.Y0, 1e-9, "rot %d", rot)
		assert.InDelta(t, stored.X1, got.X1, 1e-9, "rot %d", rot)
		assert.InDelta(t, stored.Y1, got.Y1, 1e-9, "rot %d", rot)
	}
	assert.Equal(t, Box{}, toStored(Box{}, 90), "an invalid box stays invalid")
}

// The grid shows the frame as stored top-left and rotated 90/180/270° clockwise in the
// other cells; a marker at the stored frame's top-left corner reveals each rotation.
func TestWriteGridLayout(t *testing.T) {
	dir := t.TempDir()
	// Landscape, larger than a grid tile (the grid only downscales), red marker top-left.
	src := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for y := range 200 {
		for x := range 400 {
			c := color.RGBA{120, 120, 120, 255}
			if x < 60 && y < 60 {
				c = color.RGBA{255, 0, 0, 255}
			}
			src.SetRGBA(x, y, c)
		}
	}
	srcPath := filepath.Join(dir, "src.jpg")
	f, err := os.Create(srcPath)
	require.NoError(t, err)
	require.NoError(t, jpeg.Encode(f, src, &jpeg.Options{Quality: 95}))
	require.NoError(t, f.Close())

	out := filepath.Join(dir, "grid.jpg")
	require.NoError(t, writeGrid(srcPath, out))
	gf, err := os.Open(out)
	require.NoError(t, err)
	defer gf.Close()
	grid, err := jpeg.Decode(gf)
	require.NoError(t, err)
	require.Equal(t, image.Rect(0, 0, 2*gridTile, 2*gridTile), grid.Bounds())

	red := func(x, y int) bool {
		r, g, b, _ := grid.At(x, y).RGBA()
		return r > 0xc000 && g < 0x6000 && b < 0x6000
	}
	// Where the marker lands in each cell (cell-relative quadrant of the fitted image).
	cellHasRedNear := func(cx, cy int, fx, fy float64) bool {
		x := cx*gridTile + int(fx*float64(gridTile))
		y := cy*gridTile + int(fy*float64(gridTile))
		return red(x, y)
	}
	// 0°: marker top-left of the (wide) image, which is centred vertically.
	assert.True(t, cellHasRedNear(0, 0, 0.06, 0.30), "top-left cell: as stored")
	// 90° CW: top-left corner → top-right.
	assert.True(t, cellHasRedNear(1, 0, 0.70, 0.06), "top-right cell: rotated 90° clockwise")
	// 180°: → bottom-right.
	assert.True(t, cellHasRedNear(0, 1, 0.94, 0.70), "bottom-left cell: rotated 180°")
	// 270° CW: → bottom-left.
	assert.True(t, cellHasRedNear(1, 1, 0.30, 0.94), "bottom-right cell: rotated 270° clockwise")
}

func TestOrientationDrivesRotationAndBox(t *testing.T) {
	in := withFiles(t)
	fa := newScripted(reply(`{"images":[{"id":"img1","view":"front_three_quarter","subjectFullyVisible":true,"quality":4,"box_2d":[375,100,625,900],"issues":""}]}`))
	fa.orient = map[string]string{"img1": "bottom-right"} // cf_F2 needs 270° CW
	out, err := Run(context.Background(), fa, in)
	require.NoError(t, err)
	require.Len(t, fa.orientRequests(), 1, "one orientation call")
	got, ok := out.Assessments["cf_F2"]
	require.True(t, ok)
	assert.Equal(t, 270, got.RotateCW)
	assert.Equal(t, 270, out.Rotations["cf_F2"])
	// The box was given on the upright copy; it is returned in stored coordinates.
	want := toStored(Box{X0: 0.1, Y0: 0.375, X1: 0.9, Y1: 0.625}, 270)
	assert.InDelta(t, want.X0, got.Box.X0, 1e-9)
	assert.InDelta(t, want.Y1, got.Box.Y1, 1e-9)
	assert.True(t, got.Usable)
	// The copy sent was turned upright: 640×360 stored → 288×512.
	w, h := imageSize(t, filepath.Join(in.WorkDir, "review", "cf_F2.jpg"))
	assert.Equal(t, [2]int{288, 512}, [2]int{w, h})
}

// The phone can turn inside one scene (on the user's video it turned at ~6 s, inside
// the side-profile scene), so every frame gets its own grid and its own rotation.
func TestOrientationPerFrame(t *testing.T) {
	in := withFiles(t)
	for i := range in.Candidates {
		in.Candidates[i].SceneIdx = 2 // one scene for the whole shortlist
	}
	fa := newScripted(reply(`{"images":[]}`))
	fa.orient = map[string]string{"img2": "top-right", "img3": "bottom-right"} // cf_F4, cf_F7: turned after cf_F2
	out, err := Run(context.Background(), fa, in)
	require.NoError(t, err)
	orq := fa.orientRequests()
	require.Len(t, orq, 1)
	assert.Len(t, orq[0].ResponseSchema.Properties["images"].Items.Properties["id"].Enum, len(wantOrder), "one grid per frame, not per scene")
	assert.Equal(t, 0, out.Rotations["cf_F2"])
	assert.Equal(t, 90, out.Rotations["cf_F4"])
	assert.Equal(t, 270, out.Rotations["cf_F7"])
	assert.Len(t, out.Rotations, len(wantOrder))
	for id, want := range map[string][2]int{"cf_F2": {512, 288}, "cf_F4": {288, 512}, "cf_F7": {288, 512}} {
		w, h := imageSize(t, filepath.Join(in.WorkDir, "review", id+".jpg"))
		assert.Equal(t, want, [2]int{w, h}, id)
	}
}

// The rotations are known before the judgement call, so a failed judgement still hands
// them to the caller (the export can straighten the frames without a verdict).
func TestRotationsSurviveFailedJudgement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for name, tc := range map[string]struct {
		ctx  context.Context
		step func(context.Context, *analyzer.Request) (*analyzer.Response, error)
	}{
		"unavailable": {context.Background(), fail(fmt.Errorf("gemini: 503: %w", analyzer.ErrUnavailable))},
		"bad output":  {context.Background(), reply(`{"images":[{"id":"img1"`)},
		"canceled": {ctx, func(context.Context, *analyzer.Request) (*analyzer.Response, error) {
			cancel()
			return nil, context.Canceled
		}},
	} {
		t.Run(name, func(t *testing.T) {
			in := withFiles(t)
			fa := newScripted(tc.step)
			fa.orient = map[string]string{"img4": "top-right"} // cf_S2
			out, err := Run(tc.ctx, fa, in)
			require.Error(t, err)
			require.NotNil(t, out)
			assert.Empty(t, out.Assessments)
			assert.NoError(t, out.OrientationErr)
			assert.Len(t, out.Rotations, len(wantOrder))
			assert.Equal(t, 90, out.Rotations["cf_S2"])
			assert.Equal(t, 0, out.Rotations["cf_F2"])
		})
	}
}

// A bad answer or an unavailable model gets one more orientation call; a context error
// or a local problem does not.
func TestOrientationRetry(t *testing.T) {
	upright := `{"images":[{"id":"img1","upright":"top-right"}]}`
	for name, tc := range map[string]struct {
		first     func(context.Context, *analyzer.Request) (*analyzer.Response, error)
		wantCalls []analyzer.Usage // billed calls: orientation attempts, then the judgement
	}{
		"bad json":     {reply("not json"), []analyzer.Usage{testUsage, testUsage, testUsage}},
		"empty answer": {reply(`{"images":[]}`), []analyzer.Usage{testUsage, testUsage, testUsage}},
		"unknown ids":  {reply(`{"images":[{"id":"scene1","upright":"top-left"}]}`), []analyzer.Usage{testUsage, testUsage, testUsage}},
		"unavailable":  {fail(fmt.Errorf("gemini: 503: %w", analyzer.ErrUnavailable)), []analyzer.Usage{testUsage, testUsage}},
		"call timeout": {fail(fmt.Errorf("analyze: review call timed out after 2m0s: %w", analyzer.ErrUnavailable)), []analyzer.Usage{testUsage, testUsage}},
	} {
		t.Run(name, func(t *testing.T) {
			in := withFiles(t)
			fa := newScripted(reply(`{"images":[]}`))
			fa.orientSteps = append(fa.orientSteps, tc.first, reply(upright))
			out, err := Run(context.Background(), fa, in)
			require.NoError(t, err)
			assert.Len(t, fa.orientRequests(), 2, "retried once")
			assert.NoError(t, out.OrientationErr)
			assert.Equal(t, map[string]int{"cf_F2": 90}, out.Rotations)
			assert.Equal(t, tc.wantCalls, out.Calls)
		})
	}

	t.Run("local error", func(t *testing.T) {
		in := withFiles(t)
		fa := newScripted(reply(`{"images":[]}`))
		fa.orientSteps = orientFails(fail(errors.New("gemini: response schema: boom")))
		out, err := Run(context.Background(), fa, in)
		require.NoError(t, err, "the judgement still runs")
		assert.Len(t, fa.orientRequests(), 1, "not retried")
		assert.ErrorContains(t, out.OrientationErr, "boom")
		assert.Len(t, fa.requests(), 1)
	})

	t.Run("canceled", func(t *testing.T) {
		in := withFiles(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fa := newScripted()
		fa.orientSteps = orientFails(func(context.Context, *analyzer.Request) (*analyzer.Response, error) {
			cancel()
			return nil, context.Canceled
		})
		out, err := Run(ctx, fa, in)
		require.ErrorIs(t, err, context.Canceled)
		assert.Len(t, fa.orientRequests(), 1, "not retried")
		assert.ErrorIs(t, out.OrientationErr, context.Canceled)
		assert.Empty(t, fa.requests(), "no judgement call")
	})
}

// When the orientation stays unknown, the frames are sent as stored and the prompt says
// so; the judgement then reports the rotation, and the box stays in stored coordinates
// (it was measured on the frame as stored).
func TestOrientationFailedJudgesAsStored(t *testing.T) {
	in := withFiles(t)
	fa := newScripted(reply(`{"images":[{"id":"img1","rotation":270,"view":"front_three_quarter","subjectFullyVisible":true,"quality":4,"box_2d":[100,375,900,625],"issues":""}]}`))
	fa.orientSteps = orientFails(fail(fmt.Errorf("gemini: 503: %w", analyzer.ErrUnavailable)))
	out, err := Run(context.Background(), fa, in)
	require.NoError(t, err)
	assert.Len(t, fa.orientRequests(), orientAttempts)
	require.ErrorIs(t, out.OrientationErr, analyzer.ErrUnavailable)
	assert.NotNil(t, out.Rotations)
	assert.Empty(t, out.Rotations)

	req := fa.requests()[0]
	intro := req.Parts[0].Text
	assert.NotContains(t, intro, "already been turned upright")
	assert.Contains(t, intro, "The images are shown as recorded and may be sideways or upside down")
	assert.Contains(t, intro, "- rotation: the clockwise rotation in degrees (0, 90, 180 or 270) that makes the image upright")
	assert.Contains(t, intro, "relative to the image as given (as shown, also when it is sideways).")
	assert.NotContains(t, req.Parts[1].Text, "as recorded", "no per-image mark when no image was turned")
	assert.Contains(t, req.Parts[len(req.Parts)-1].Text, `{"id":"...","rotation":0,"view":"..."`)
	item := req.ResponseSchema.Properties["images"].Items
	require.Contains(t, item.Properties, "rotation")
	assert.Equal(t, "integer", item.Properties["rotation"].Type)
	assert.Equal(t, []string{"id", "rotation", "view", "subjectFullyVisible", "quality", "box_2d", "sameAs", "issues"}, item.PropertyOrdering)
	assert.Equal(t, item.PropertyOrdering, item.Required)

	w, h := imageSize(t, filepath.Join(in.WorkDir, "review", "cf_S1.jpg"))
	assert.Equal(t, [2]int{288, 512}, [2]int{w, h}, "sent as stored")
	got := out.Assessments["cf_F2"]
	assert.Equal(t, 270, got.RotateCW, "the judgement's rotation")
	assert.Equal(t, Box{X0: 0.375, Y0: 0.1, X1: 0.625, Y1: 0.9}, got.Box, "box as measured: already stored coordinates")
}

// No grid could be written (here: every grid path is taken by a directory): no
// orientation call, the frames are judged as stored.
func TestOrientationNoGrid(t *testing.T) {
	in := withFiles(t)
	for _, id := range wantOrder {
		require.NoError(t, os.MkdirAll(filepath.Join(in.WorkDir, "review", "orient-"+id+".jpg", "x"), 0o755))
	}
	fa := newScripted(reply(`{"images":[]}`))
	out, err := Run(context.Background(), fa, in)
	require.NoError(t, err)
	assert.Empty(t, fa.orientRequests())
	assert.ErrorContains(t, out.OrientationErr, "no grid could be written")
	assert.Contains(t, fa.requests()[0].ResponseSchema.Properties["images"].Items.Properties, "rotation")
}

// A partial orientation answer: the frames it covers are turned upright, the others
// are sent as stored and marked, and only for those does the answer's rotation count.
func TestOrientationPartialAnswer(t *testing.T) {
	in := withFiles(t)
	var ans []string
	for i := 1; i < len(wantOrder); i++ { // img9 (cf_O2) is missing
		ans = append(ans, fmt.Sprintf(`{"id":"img%d","upright":"top-left"}`, i))
	}
	fa := newScripted(reply(`{"images":[
	  {"id":"img1","rotation":90,"view":"front_three_quarter","subjectFullyVisible":true,"quality":4,"box_2d":[100,100,900,900],"issues":""},
	  {"id":"img9","rotation":90,"view":"other","subjectFullyVisible":true,"quality":4,"box_2d":[100,100,900,900],"issues":""}
	]}`))
	fa.orientSteps = append(fa.orientSteps, reply(`{"images":[`+strings.Join(ans, ",")+`]}`))
	out, err := Run(context.Background(), fa, in)
	require.NoError(t, err)
	assert.Len(t, fa.orientRequests(), 1, "a partial answer is not retried")
	assert.NoError(t, out.OrientationErr)
	assert.Len(t, out.Rotations, len(wantOrder)-1)
	assert.NotContains(t, out.Rotations, "cf_O2")

	req := fa.requests()[0]
	assert.Contains(t, req.Parts[0].Text, `Images marked "as recorded" are shown as recorded and may be sideways or upside down`)
	assert.Contains(t, req.Parts[0].Text, "- rotation:")
	assert.Equal(t, "Image 1 — id img1, proposed view: front_three_quarter", req.Parts[1].Text)
	assert.Equal(t, "Image 9 (as recorded) — id img9, proposed view: other", req.Parts[1+2*8].Text)
	assert.Contains(t, req.ResponseSchema.Properties["images"].Items.Properties, "rotation")

	assert.Equal(t, 0, out.Assessments["cf_F2"].RotateCW, "oriented: the grid's answer counts")
	assert.Equal(t, 90, out.Assessments["cf_O2"].RotateCW, "as stored: the judgement's rotation counts")
}

func TestParseOrientation(t *testing.T) {
	got, err := parseOrientation("```json\n" + `{"images":[
	  {"id":" IMG1 ","upright":"Top-Right"},
	  {"id":"img1","upright":"bottom-left"},
	  {"id":"img2","upright":"sideways"},
	  {"id":"img2","upright":"bottom-right"}
	]}` + "\n```")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"img1": 90, "img2": 270}, got, "trimmed and lower-cased; the first valid answer wins")
	for _, bad := range []string{"", "nope", `{"picks":[]}`, `{"images":null}`} {
		_, err := parseOrientation(bad)
		assert.Error(t, err, "%q", bad)
	}
}

func TestOutputBudget(t *testing.T) {
	assert.Equal(t, int32(4096), outputBudget(4096, reviewBaseTokens+reviewTokensPerFrame*9), "the domain cap when it suffices")
	assert.Equal(t, int32(512+160*MaxImages), outputBudget(4096, reviewBaseTokens+reviewTokensPerFrame*MaxImages), "raised for a full shortlist")
	assert.Equal(t, int32(50000), outputBudget(50000, 1000), "a larger domain cap is kept")
	assert.Equal(t, int32(maxOutputCeiling), outputBudget(4096, 1<<20), "never above the ceiling")

	in := fixture("/w")
	frames := make([]frame, MaxImages)
	for i := range frames {
		frames[i] = frame{cand: cand("/w", fmt.Sprintf("cf_%02d", i), f3q, float64(i), ""), shot: f3q, key: fmt.Sprintf("img%d", i+1), oriented: true}
	}
	assert.Equal(t, int32(512+160*MaxImages), buildRequest(in, frames).MaxOutputTokens)
	grids := make([]gridFrame, MaxImages)
	for i := range grids {
		grids[i] = gridFrame{cand: frames[i].cand.ID, key: frames[i].key}
	}
	assert.Equal(t, domain.DefaultMaxOutputTokens, orientRequest(in, grids).MaxOutputTokens, "40 grids fit in the default cap")
}

// A bad answer that used the whole output budget says so in the error.
func TestRunReportsOutputLimit(t *testing.T) {
	cut := func(out int) func(context.Context, *analyzer.Request) (*analyzer.Response, error) {
		return func(context.Context, *analyzer.Request) (*analyzer.Response, error) {
			return &analyzer.Response{Text: `{"images":[{"id":"img1"`, Usage: analyzer.Usage{Model: "m", OutputTokens: out, ThinkingTokens: 6}}, nil
		}
	}
	in := withFiles(t)
	_, err := Run(context.Background(), newScripted(cut(4090)), in)
	require.ErrorIs(t, err, analyze.ErrBadOutput)
	assert.ErrorContains(t, err, "answer probably cut off at the 4096-token output limit")

	_, err = Run(context.Background(), newScripted(cut(1000)), withFiles(t))
	require.ErrorIs(t, err, analyze.ErrBadOutput)
	assert.NotContains(t, err.Error(), "output limit")
}

// sameAs links a frame to an EARLIER one; links forward, to itself or to unknown ids
// are ignored.
func TestSameAsLinksOnlyToEarlierFrames(t *testing.T) {
	in := withFiles(t)
	fa := newScripted(reply(`{"images":[
	  {"id":"img1","view":"front_three_quarter","subjectFullyVisible":true,"quality":4,"box_2d":[100,100,900,900],"sameAs":"img3","issues":""},
	  {"id":"img2","view":"front_three_quarter","subjectFullyVisible":true,"quality":4,"box_2d":[100,100,900,900],"sameAs":"img2","issues":""},
	  {"id":"img3","view":"side_profile","subjectFullyVisible":true,"quality":4,"box_2d":[100,100,900,900],"sameAs":"img1","issues":""},
	  {"id":"img4","view":"side_profile","subjectFullyVisible":true,"quality":4,"box_2d":[100,100,900,900],"sameAs":"none","issues":""}
	]}`))
	out, err := Run(context.Background(), fa, in)
	require.NoError(t, err)
	byKey := map[string]Assessment{}
	for _, a := range out.Assessments {
		byKey[a.CandidateID] = a
	}
	var linked, self, none int
	for _, a := range out.Assessments {
		switch a.DuplicateOf {
		case "":
			none++
		case a.CandidateID:
			self++
		default:
			linked++
			// the target must be a frame the model also answered for
			_, ok := byKey[a.DuplicateOf]
			assert.True(t, ok)
		}
	}
	assert.Equal(t, 1, linked, "only img3 → img1 (earlier) is kept")
	assert.Zero(t, self, "self-links are ignored")
	assert.Equal(t, 3, none)
}
