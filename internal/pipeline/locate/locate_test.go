package locate

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/media"
	"video-to-image/internal/pipeline/analyze"
)

const (
	f3q  = "front_three_quarter"
	side = "side_profile"
	rear = "rear"
	dash = "interior_dashboard"
)

func testDomain() *domain.Config {
	return &domain.Config{
		Key:  "autos",
		Gate: domain.Gate{Subject: "a single passenger vehicle being shown, as for a sales listing."},
		Shots: []domain.Shot{
			{ID: f3q, Label: "Front 3/4", Required: true, Description: "Front and one side at roughly 45°, whole car in frame"},
			{ID: side, Label: "Side profile", Required: true, Description: "Full side of the car"},
			{ID: rear, Label: "Rear", Description: "Straight-on rear"},
			{ID: dash, Label: "Dashboard", Description: "Dashboard and steering wheel"},
		},
		Model:     domain.Model{Name: "test-model", Temperature: 0, MaxOutputTokens: 4096},
		Selection: domain.Selection{MaxScenes: 20},
		Locate:    domain.Locate{Enabled: true, Instructions: "  Exterior views need the WHOLE car.\n"},
	}
}

// writeFrame writes a w×h JPEG whose colour encodes n, so tiles can be told apart.
func writeFrame(t *testing.T, path string, w, h, n int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	c := color.RGBA{uint8(40 + n*7), uint8(200 - n*5), uint8(120), 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, jpeg.Encode(f, img, &jpeg.Options{Quality: 80}))
	require.NoError(t, f.Close())
}

func TestWriteSheetsLayoutAndLabels(t *testing.T) {
	dir := t.TempDir()
	var tiles []tile
	for i := 0; i < 16; i++ { // portrait preview frames, 2⅔ sheets of 6
		p := filepath.Join(dir, "f", fmt.Sprintf("f_%04d.jpg", i+1))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		writeFrame(t, p, 270, 480, i)
		tiles = append(tiles, tile{path: p, sec: float64(i)})
	}
	tiles[4].path = filepath.Join(dir, "missing.jpg") // one unreadable frame

	sheets, err := writeSheets(filepath.Join(dir, "sheets"), tiles, 1)
	require.NoError(t, err)
	require.Len(t, sheets, 3)
	assert.Equal(t, []int{6, 6, 4}, []int{sheets[0].tiles, sheets[1].tiles, sheets[2].tiles})
	assert.Equal(t, 0.0, sheets[0].first)
	assert.Equal(t, 5.0, sheets[0].last)
	assert.Equal(t, 12.0, sheets[2].first)
	assert.Equal(t, 15.0, sheets[2].last)
	assert.Equal(t, 1, sheets[0].unreadable)
	assert.Equal(t, 0, sheets[1].unreadable)

	f, err := os.Open(sheets[0].path)
	require.NoError(t, err)
	defer f.Close()
	img, err := jpeg.Decode(f)
	require.NoError(t, err)
	b := img.Bounds()
	assert.Equal(t, Cols*QuadSide+(Cols+1)*Gap, b.Dx())
	assert.Equal(t, Rows*QuadSide+(Rows+1)*Gap, b.Dy())
	// The last sheet has 4 quads: two rows.
	f2, err := os.Open(sheets[2].path)
	require.NoError(t, err)
	defer f2.Close()
	img2, err := jpeg.Decode(f2)
	require.NoError(t, err)
	assert.Equal(t, 2*QuadSide+3*Gap, img2.Bounds().Dy())

	sum := func(c color.Color) int {
		r, g, bl, _ := c.RGBA()
		return int(r>>8) + int(g>>8) + int(bl>>8)
	}
	// Quad 1 (second column): the as-recorded copy (top-left cell) is portrait, so it
	// leaves light cell background left and right and the frame colour in the middle;
	// the 90° copy (top-right cell) is landscape: frame colour at its left edge.
	quad := image.Rect(0, 0, QuadSide, QuadSide).Add(image.Pt(Gap+(QuadSide+Gap), Gap))
	tl := image.Rect(0, 0, CellSide, CellSide).Add(quad.Min)
	mid := img.At(tl.Min.X+CellSide/2, tl.Min.Y+CellSide/2)
	r, g, _, _ := mid.RGBA()
	assert.InDelta(t, 47, r>>8, 12, "frame 1 red")
	assert.InDelta(t, 195, g>>8, 12, "frame 1 green")
	assert.Greater(t, sum(img.At(tl.Min.X+4, tl.Min.Y+CellSide/2)), 3*200, "portrait copy: light letterbox at the left")
	tr := tl.Add(image.Pt(CellSide+InnerGap, 0))
	assert.InDelta(t, 47, func() uint32 { r, _, _, _ := img.At(tr.Min.X+4, tr.Min.Y+CellSide/2).RGBA(); return r >> 8 }(), 12,
		"landscape copy: frame colour reaches the left edge")
	assert.Greater(t, sum(img.At(tr.Min.X+CellSide/2, tr.Min.Y+4)), 3*200, "landscape copy: light band at the top")

	// The unreadable frame (quad 5, second row) stays dark.
	q5 := image.Rect(0, 0, QuadSide, QuadSide).Add(image.Pt(Gap+(QuadSide+Gap), Gap+QuadSide+Gap))
	assert.Less(t, sum(img.At(q5.Min.X+QuadSide/2, q5.Min.Y+QuadSide/2)), 3*70, "unreadable quad is dark")

	// The label box sits in the quad's top-left corner: black background, white glyphs.
	lab := image.Rect(quad.Min.X+InnerGap, quad.Min.Y+InnerGap, quad.Min.X+InnerGap+LabelScale*(7*2+4), quad.Min.Y+InnerGap+LabelScale*(13+4))
	dark, bright := 0, 0
	for y := lab.Min.Y; y < lab.Max.Y; y++ {
		for x := lab.Min.X; x < lab.Max.X; x++ {
			switch s := sum(img.At(x, y)); {
			case s < 3*60:
				dark++
			case s > 3*180:
				bright++
			}
		}
	}
	assert.Greater(t, dark, bright, "label background is black")
	assert.Greater(t, bright, 40, "label has white glyphs")
}

func TestRotateRGBA(t *testing.T) {
	m := image.NewRGBA(image.Rect(0, 0, 3, 2))
	m.SetRGBA(0, 0, color.RGBA{255, 0, 0, 255}) // top-left red
	m.SetRGBA(2, 1, color.RGBA{0, 0, 255, 255}) // bottom-right blue
	r := rotateRGBA(m, 90)
	assert.Equal(t, image.Rect(0, 0, 2, 3), r.Bounds())
	assert.Equal(t, color.RGBA{255, 0, 0, 255}, r.RGBAAt(1, 0), "90° CW: top-left goes to top-right")
	assert.Equal(t, color.RGBA{0, 0, 255, 255}, r.RGBAAt(0, 2), "bottom-right goes to bottom-left")
	r = rotateRGBA(m, 180)
	assert.Equal(t, color.RGBA{255, 0, 0, 255}, r.RGBAAt(2, 1))
	r = rotateRGBA(m, 270)
	assert.Equal(t, color.RGBA{255, 0, 0, 255}, r.RGBAAt(0, 2), "270° CW: top-left goes to bottom-left")
	assert.Equal(t, m.Pix, rotateRGBA(m, 0).Pix)
}

func TestWriteSheetsNothingReadable(t *testing.T) {
	dir := t.TempDir()
	_, err := writeSheets(filepath.Join(dir, "s"), []tile{{path: filepath.Join(dir, "no.jpg"), sec: 0}}, 1)
	assert.ErrorContains(t, err, "no sheet could be written")
	assert.Empty(t, mustGlob(t, filepath.Join(dir, "s", "*.jpg")), "no sheet is left behind")
}

func mustGlob(t *testing.T, pattern string) []string {
	m, err := filepath.Glob(pattern)
	require.NoError(t, err)
	return m
}

func TestFormatSec(t *testing.T) {
	assert.Equal(t, "12s", formatSec(12, 1))
	assert.Equal(t, "0s", formatSec(0, 0.5))
	assert.Equal(t, "12.5s", formatSec(12.5, 2))
}

func TestParseMoments(t *testing.T) {
	d := testDomain()
	got, dropped, err := parseMoments("```json\n"+`{"moments":[
	  {"sec":8,"shotId":"side_profile","wholeSubject":true,"upright":"Bottom-Right","confidence":0.9},
	  {"sec":7.2,"shotId":"SIDE_PROFILE ","wholeSubject":true,"confidence":1.4},
	  {"sec":7,"shotId":"side_profile","wholeSubject":true,"confidence":0.8},
	  {"sec":4,"shotId":"side_profile","wholeSubject":false,"confidence":0.9},
	  {"sec":61,"shotId":"rear","wholeSubject":true,"confidence":0.9},
	  {"sec":-1,"shotId":"rear","wholeSubject":true,"confidence":0.9},
	  {"sec":20,"shotId":"wheel","wholeSubject":true,"confidence":0.9},
	  {"sec":20,"shotId":"rear","wholeSubject":true},
	  {"shotId":"rear","wholeSubject":true,"confidence":0.9},
	  {"sec":0,"shotId":"front_three_quarter","wholeSubject":true,"confidence":0.7},
	  {"sec":0,"shotId":"interior_dashboard","wholeSubject":true,"confidence":0.7}
	]}`+"\n```", d, 60, 1)
	require.NoError(t, err)
	assert.Equal(t, 5, dropped, "out of range ×2, unknown shot, repeat of 7 s, no sec")
	assert.Equal(t, []Moment{
		{Sec: 0, ShotID: f3q, Confidence: 0.7, Whole: true, RotateCW: -1},
		{Sec: 0, ShotID: dash, Confidence: 0.7, Whole: true, RotateCW: -1},
		{Sec: 4, ShotID: side, Confidence: 0.9, Whole: false, RotateCW: -1},
		{Sec: 7, ShotID: side, Confidence: 1, Whole: true, RotateCW: -1},
		{Sec: 8, ShotID: side, Confidence: 0.9, Whole: true, RotateCW: 270},
		{Sec: 20, ShotID: rear, Confidence: 0.5, Whole: true, RotateCW: -1},
	}, got, "snapped to the grid, ordered by second then shot priority, missing confidence 0.5, missing wholeSubject false, upright → rotation")

	for name, text := range map[string]string{
		"empty":      "",
		"not json":   "sure!",
		"no moments": `{"images":[]}`,
		"null":       `{"moments":null}`,
		"not a list": `{"moments":{"sec":1}}`,
	} {
		_, _, err := parseMoments(text, d, 60, 1)
		assert.Error(t, err, name)
	}
	got, dropped, err = parseMoments(`{"moments":[]}`, d, 60, 1)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 0, dropped)

	// A finer grid snaps to halves.
	got, _, err = parseMoments(`{"moments":[{"sec":7.3,"shotId":"rear","wholeSubject":true,"confidence":0.9}]}`, d, 60, 2)
	require.NoError(t, err)
	assert.Equal(t, 7.5, got[0].Sec)
}

func TestScenesAndMerge(t *testing.T) {
	d := testDomain()
	moments := []Moment{
		{Sec: 6, ShotID: side, Confidence: 0.9, Whole: true},
		{Sec: 7, ShotID: side, Confidence: 0.9, Whole: true},
		{Sec: 8, ShotID: side, Confidence: 0.8, Whole: false},
		{Sec: 22, ShotID: rear, Confidence: 0.85, Whole: true},
		{Sec: 0, ShotID: f3q, Confidence: 0.6, Whole: true},
		{Sec: 59, ShotID: dash, Confidence: 0.6, Whole: true},
	}
	extra := Scenes(moments, d, 59.09)
	require.Len(t, extra, 6)
	assert.Equal(t, analyze.Scene{Idx: 0, StartSec: 5.5, EndSec: 6.5, ShotID: side, Relevance: 0.9,
		Reason: "Side profile view", RawStart: "sheet 6s", RawEnd: "sheet 6s"}, extra[0])
	assert.InDelta(t, 0.8*PartialRelevance, extra[2].Relevance, 1e-9, "not seen whole: lower relevance")
	assert.Equal(t, 0.0, extra[4].StartSec, "clamped at the start")
	assert.Equal(t, 59.09, extra[5].EndSec, "clamped at the end")

	base := []analyze.Scene{
		{Idx: 0, StartSec: 0, EndSec: 2, ShotID: "front", Relevance: 0.95, Reason: "Straight-on front"},
		{Idx: 1, StartSec: 4, EndSec: 7, ShotID: side, Relevance: 0.95, Reason: "The side profile captures the full length"},
		{Idx: 2, StartSec: 30, EndSec: 33, ShotID: dash, Relevance: 0.95, Reason: "Dashboard"},
	}
	merged, drops := analyze.MergeScenes(base, extra, d)
	var got []string
	for _, s := range merged {
		got = append(got, s.ShotID)
	}
	assert.Equal(t, []string{f3q, "front", side, rear, dash, dash}, got)
	sideScene := merged[2]
	assert.Equal(t, 4.0, sideScene.StartSec)
	assert.Equal(t, 8.5, sideScene.EndSec, "the analyze range and the three located seconds became one")
	assert.Equal(t, "The side profile captures the full length", sideScene.Reason, "the more relevant reason wins")
	assert.Equal(t, 0.95, sideScene.Relevance)
	assert.Equal(t, 2, merged[2].Idx)
	assert.Len(t, drops, 3, "the three absorbed side ranges")
	for _, dr := range drops {
		assert.Equal(t, analyze.DropMerged, dr.Reason)
	}

	// MaxScenes keeps the most relevant.
	d.Selection.MaxScenes = 3
	merged, drops = analyze.MergeScenes(base, extra, d)
	assert.Len(t, merged, 3)
	over := 0
	for _, dr := range drops {
		if dr.Reason == analyze.DropOverMaxScenes {
			over++
		}
	}
	assert.Equal(t, 3, over)

	assert.Nil(t, Scenes(nil, d, 60))
	m, dr := analyze.MergeScenes(nil, nil, d)
	assert.Nil(t, m)
	assert.Nil(t, dr)
}

func TestBuildRequest(t *testing.T) {
	d := testDomain()
	in := Input{Domain: d, JobID: "job_1", CacheKey: "sha", DurationSec: 59}
	sheets := []sheet{{path: "/tmp/a.jpg", first: 0, last: 11, tiles: 12}, {path: "/tmp/b.jpg", first: 12, last: 20, tiles: 9}}
	req := buildRequest(in, sheets, 1)
	assert.Equal(t, analyzer.StepLocate, req.Step)
	assert.Equal(t, "sha:autos:locate", req.CacheKey)
	assert.Equal(t, "test-model", req.Model)
	require.Len(t, req.Parts, 6)
	intro := req.Parts[0].Text
	assert.Contains(t, intro, "The video shows a single passenger vehicle being shown, as for a sales listing.")
	assert.Contains(t, intro, "2 contact sheet(s). Every numbered square on a sheet is ONE frame of the video (one frame per second")
	assert.Contains(t, intro, "as recorded (top-left), turned 90° clockwise (top-right), 180° (bottom-left) and 270° (bottom-right)")
	assert.Contains(t, intro, "judge every frame by its upright copy and report which copy that is")
	assert.Contains(t, intro, "Exterior views need the WHOLE car.")
	assert.Contains(t, intro, "- side_profile: Side profile — Full side of the car")
	assert.Contains(t, intro, "list EVERY second whose frame shows that view recognisably")
	assert.Contains(t, intro, "when in doubt list the second with wholeSubject false")
	assert.Equal(t, "Sheet 1 of 2: 0s to 11s", req.Parts[1].Text)
	assert.Equal(t, analyzer.Part{FilePath: "/tmp/a.jpg", MIMEType: "image/jpeg"}, req.Parts[2])
	assert.Equal(t, "Sheet 2 of 2: 12s to 20s", req.Parts[3].Text)
	assert.Contains(t, req.Parts[5].Text, `{"moments":[{"sec":12,"shotId":"...","wholeSubject":true,"upright":"top-left","confidence":0.9}]}`)
	assert.Contains(t, req.Parts[5].Text, "every qualifying second up to 4 per view")
	item := req.ResponseSchema.Properties["moments"].Items
	assert.Equal(t, []string{f3q, side, rear, dash}, item.Properties["shotId"].Enum)
	assert.Equal(t, []string{"sec", "shotId", "wholeSubject", "upright", "confidence"}, item.Required)
	assert.Equal(t, []string{"top-left", "top-right", "bottom-left", "bottom-right"}, item.Properties["upright"].Enum)
	assert.Equal(t, int32(4096), req.MaxOutputTokens, "the domain cap is enough for 4 shots")
	assert.Equal(t, []string{f3q, side, rear, dash}, req.ShotIDs)
	assert.Equal(t, 59.0, req.DurationSec)

	d.Locate.Instructions = ""
	req = buildRequest(in, sheets, 2)
	assert.Contains(t, req.Parts[0].Text, "2 frames per second")
	assert.NotContains(t, req.Parts[0].Text, "WHOLE car")
	assert.Equal(t, "Sheet 1 of 2: 0.0s to 11.0s", req.Parts[1].Text)
}

// scripted is a minimal analyzer: one canned answer, the requests recorded.
type scripted struct {
	text     string
	err      error
	requests []*analyzer.Request
}

func (s *scripted) Generate(_ context.Context, req *analyzer.Request) (*analyzer.Response, error) {
	s.requests = append(s.requests, req)
	if s.err != nil {
		return nil, s.err
	}
	return &analyzer.Response{Text: s.text, Usage: analyzer.Usage{Model: req.Model, InputTokens: 5000, OutputTokens: 60},
		FileHandles: make([]string, len(req.Parts))}, nil
}
func (s *scripted) ReleaseFile(context.Context, string) error { return nil }
func (s *scripted) Name() string                              { return "scripted" }

// testPreview writes a 270×480, 2 fps, sec-second synthetic preview (like a portrait
// phone preview) with ffmpeg, or skips the test when ffmpeg is not installed.
func testPreview(t *testing.T, sec int) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	out := filepath.Join(t.TempDir(), "preview.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi",
		"-i", fmt.Sprintf("testsrc=size=270x480:rate=2:duration=%d", sec), "-pix_fmt", "yuv420p", out)
	require.NoError(t, cmd.Run(), "ffmpeg testsrc")
	return out
}

// Run end to end on a synthetic preview: 26 s → 26 sweep frames on 5 sheets, one
// call, moments parsed and usage reported.
func TestRunOnPreview(t *testing.T) {
	preview := testPreview(t, 26)
	fa := &scripted{text: `{"moments":[
	  {"sec":6,"shotId":"side_profile","wholeSubject":true,"confidence":0.9},
	  {"sec":7,"shotId":"side_profile","wholeSubject":true,"confidence":0.9},
	  {"sec":3,"shotId":"side_profile","wholeSubject":false,"confidence":0.9},
	  {"sec":22,"shotId":"rear","wholeSubject":true,"confidence":0.8}]}`}
	in := Input{Domain: testDomain(), JobID: "job_1", PreviewPath: preview, DurationSec: 26.04,
		WorkDir: t.TempDir(), CacheKey: "sha", FFmpeg: media.New("", "", nil)}
	out, err := Run(context.Background(), fa, in)
	require.NoError(t, err)
	assert.Equal(t, 26, out.Frames)
	assert.Equal(t, 5, out.Sheets)
	assert.Len(t, out.Calls, 1)
	assert.Equal(t, 0, out.Dropped)
	assert.Equal(t, []Moment{{3, side, 0.9, false, -1}, {6, side, 0.9, true, -1}, {7, side, 0.9, true, -1}, {22, rear, 0.8, true, -1}}, out.Moments)
	require.Len(t, fa.requests, 1)
	assert.Len(t, fa.requests[0].Parts, 2+2*5)
	for _, p := range fa.requests[0].Parts {
		if p.FilePath != "" {
			_, err := os.Stat(p.FilePath)
			assert.NoError(t, err, "sheet exists when the call is made")
		}
	}
	assert.Len(t, mustGlob(t, filepath.Join(in.WorkDir, "locate", "sweep", "f_*.jpg")), 26)
}

func TestRunErrors(t *testing.T) {
	d := testDomain()
	ff := media.New("", "", nil)
	for name, in := range map[string]Input{
		"nil domain":  {JobID: "j", PreviewPath: "x", WorkDir: "y", DurationSec: 1, FFmpeg: ff},
		"no preview":  {Domain: d, WorkDir: "y", DurationSec: 1, FFmpeg: ff},
		"no duration": {Domain: d, PreviewPath: "x", WorkDir: "y", FFmpeg: ff},
		"nil ffmpeg":  {Domain: d, PreviewPath: "x", WorkDir: "y", DurationSec: 1},
	} {
		out, err := Run(context.Background(), &scripted{text: "{}"}, in)
		assert.Error(t, err, name)
		assert.NotNil(t, out, name)
	}
	_, err := Run(context.Background(), nil, Input{Domain: d})
	assert.Error(t, err)

	preview := testPreview(t, 3)
	in := Input{Domain: d, JobID: "job_1", PreviewPath: preview, DurationSec: 3, WorkDir: t.TempDir(), FFmpeg: ff}
	out, err := Run(context.Background(), &scripted{text: "not json"}, in)
	assert.ErrorIs(t, err, analyze.ErrBadOutput)
	assert.Len(t, out.Calls, 1, "usage is reported even for a bad answer")
	assert.Equal(t, "not json", out.RawText)

	out, err = Run(context.Background(), &scripted{err: fmt.Errorf("gemini: 503: %w", analyzer.ErrUnavailable)}, in)
	assert.ErrorIs(t, err, analyzer.ErrUnavailable)
	assert.Empty(t, out.Calls)
}
