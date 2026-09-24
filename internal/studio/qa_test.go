package studio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"video-to-image/internal/analyzer/fake"
)

func TestQAPreview(t *testing.T) {
	const w, h = 1024, 512
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 10, 200, 30, 255
	}
	alpha := image.NewGray(img.Rect)
	rect(alpha, 257, 128, 768, 384, 255) // the car: a block in the middle
	rect(alpha, 800, 450, 803, 453, 255) // a far speck the clean-up removes

	m := QAPreview(img, alpha, 512)
	require.NotNil(t, m)
	assert.Equal(t, image.Rect(0, 0, 512, 256), m.Bounds())
	at := func(x, y int) color.RGBA { return color.RGBAModel.Convert(m.At(x, y)).(color.RGBA) }
	assert.Equal(t, color.RGBA{10, 200, 30, 255}, at(256, 128), "car pixels are the original")
	assert.Equal(t, color.RGBA{255, 0, 255, 255}, at(10, 10), "background is magenta")
	assert.Equal(t, color.RGBA{255, 0, 255, 255}, at(400, 225), "the speck was cleaned up like in Render")
	edge := at(128, 100) // source columns 256 (magenta) and 257 (car) after the 2× box filter
	assert.InDelta(t, (10+255)/2, int(edge.R), 1)

	// Default size, no upscaling, and nil on a mismatched or missing mask.
	assert.Equal(t, image.Rect(0, 0, w, h), QAPreview(img, alpha, 0).Bounds(), "1024 px is the default")
	assert.Equal(t, image.Rect(0, 0, w, h), QAPreview(img, alpha, 4096).Bounds())
	assert.Nil(t, QAPreview(img, image.NewGray(image.Rect(0, 0, 10, 10)), 512))
	assert.Nil(t, QAPreview(img, nil, 512))
	assert.Nil(t, QAPreview(nil, alpha, 512))
	assert.Equal(t, uint8(255), alpha.GrayAt(801, 451).Y, "QAPreview must not modify the mask")
}

func TestQAImages(t *testing.T) {
	img, mask := loadFixture(t, "pub05_microvan_green_side_rusty")
	o, c, err := QAImages(img, mask)
	require.NoError(t, err)
	for _, b := range [][]byte{o, c} {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
		require.NoError(t, err)
		assert.Equal(t, img.Bounds().Dx(), cfg.Width, "800 px fixtures stay at their size (≤ 1024)")
		assert.Equal(t, img.Bounds().Dy(), cfg.Height)
	}
	cut, err := jpeg.Decode(bytes.NewReader(c))
	require.NoError(t, err)
	r, g, b, _ := cut.At(3, 3).RGBA()
	assert.Greater(t, r>>8, uint32(230), "background corner is magenta")
	assert.Less(t, g>>8, uint32(30))
	assert.Greater(t, b>>8, uint32(230))

	_, _, err = QAImages(img, image.NewGray(image.Rect(0, 0, 5, 5)))
	assert.ErrorIs(t, err, ErrMaskSize)
	_, _, err = QAImages(nil, mask)
	assert.Error(t, err)
}

func TestQARequest(t *testing.T) {
	p := QAParams{
		JobID: "job_1", DomainKey: "cars", ImageID: "img_1", View: "front_three_quarter",
		OriginalJPEG: []byte("orig"), CutoutJPEG: []byte("cut"),
		OriginalPath: "/tmp/a.jpg", CutoutPath: "/tmp/b.jpg", SourceName: "walk.mp4",
	}
	req := QARequest(p)
	assert.Equal(t, analyzer.StepStudio, req.Step)
	assert.Equal(t, "job_1", req.JobID)
	assert.Equal(t, "cars", req.DomainKey)
	assert.Equal(t, DefaultQAModel, req.Model)
	assert.Equal(t, "gemini-3.8-flash", req.Model)
	assert.Zero(t, req.Temperature)
	require.NotNil(t, req.ThinkingBudget)
	assert.Zero(t, *req.ThinkingBudget, "3.8-flash: thinkingBudget 0 (it rejects thinkingLevel minimal)")
	assert.Empty(t, req.ThinkingLevel)
	assert.Equal(t, int32(512), req.MaxOutputTokens)
	assert.Equal(t, "walk.mp4", req.SourceName)

	require.Len(t, req.Parts, 5)
	assert.Equal(t, "Image 1 (original):", req.Parts[0].Text)
	assert.Equal(t, analyzer.Part{FilePath: "/tmp/a.jpg", MIMEType: "image/jpeg"}, req.Parts[1])
	assert.Equal(t, "Image 2 (cut-out on magenta):", req.Parts[2].Text)
	assert.Equal(t, analyzer.Part{FilePath: "/tmp/b.jpg", MIMEType: "image/jpeg"}, req.Parts[3])
	prompt := req.Parts[4].Text
	for _, s := range []string{"(view: front three quarter)", "#FF00FF", "missingParts", "backgroundKept", "LARGE:",
		"antenna", "mirror", "gas struts", "damage", "THROUGH the car's windows"} {
		assert.Contains(t, prompt, s)
	}

	s := req.ResponseSchema
	require.NotNil(t, s)
	assert.Equal(t, "object", s.Type)
	assert.Equal(t, []string{"missingParts", "backgroundKept", "notes"}, s.PropertyOrdering)
	assert.ElementsMatch(t, []string{"missingParts", "backgroundKept", "notes"}, s.Required)
	assert.Equal(t, "array", s.Properties["missingParts"].Type)
	assert.Equal(t, "string", s.Properties["missingParts"].Items.Type)
	assert.Equal(t, "array", s.Properties["backgroundKept"].Type)
	assert.Equal(t, "string", s.Properties["notes"].Type)

	sum := sha256.Sum256([]byte("origcut"))
	assert.Equal(t, hex.EncodeToString(sum[:])+":cars:studio-qa", req.CacheKey)
	p.CutoutJPEG = []byte("other")
	assert.NotEqual(t, req.CacheKey, QARequest(p).CacheKey, "the cut-out is part of the key")
	p.OriginalJPEG = nil
	assert.Empty(t, QARequest(p).CacheKey, "no bytes, no recording key")

	// A flash-lite model gets the thinking level instead (it rejects budget 0).
	p.Model = "gemini-3.5-flash-lite"
	req = QARequest(p)
	assert.Equal(t, "gemini-3.5-flash-lite", req.Model)
	assert.Nil(t, req.ThinkingBudget)
	assert.Equal(t, "minimal", req.ThinkingLevel)
	p.View = ""
	assert.Contains(t, QARequest(p).Parts[4].Text, "(view: exterior)")
}

func TestWriteQAFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "qa")
	p := QAParams{ImageID: "img_7", OriginalJPEG: []byte("o"), CutoutJPEG: []byte("c")}
	require.NoError(t, WriteQAFiles(dir, &p))
	assert.Equal(t, filepath.Join(dir, "img_7_qa_original.jpg"), p.OriginalPath)
	assert.Equal(t, filepath.Join(dir, "img_7_qa_cutout.jpg"), p.CutoutPath)
	b, err := os.ReadFile(p.CutoutPath)
	require.NoError(t, err)
	assert.Equal(t, "c", string(b))
	req := QARequest(p)
	assert.Equal(t, p.OriginalPath, req.Parts[1].FilePath)

	assert.Error(t, WriteQAFiles(dir, &QAParams{ImageID: "../x", OriginalJPEG: []byte("o"), CutoutJPEG: []byte("c")}))
	assert.Error(t, WriteQAFiles(dir, &QAParams{ImageID: "img", CutoutJPEG: []byte("c")}))
}

func TestParseQA(t *testing.T) {
	tests := []struct {
		name string
		text string
		want QAVerdict
	}{
		{"nothing missing", `{"missingParts":[],"backgroundKept":[],"notes":""}`,
			QAVerdict{OK: true, MissingParts: []string{}, BackgroundKept: []string{}}},
		{"antenna missing (spike pub01)", `{"missingParts":["thin roof antenna"],"backgroundKept":[],"notes":""}`,
			QAVerdict{MissingParts: []string{"thin roof antenna"}, BackgroundKept: []string{}}},
		{"two parts (spike user02)", `{"missingParts":["portion of the left side mirror"," left tailgate gas strut "],"backgroundKept":[],"notes":"Mirror cap lost."}`,
			QAVerdict{MissingParts: []string{"portion of the left side mirror", "left tailgate gas strut"}, BackgroundKept: []string{}, Notes: "Mirror cap lost."}},
		{"small leftovers are fine", `{"missingParts":[],"backgroundKept":["ground shadow under the bumper"],"notes":""}`,
			QAVerdict{OK: true, MissingParts: []string{}, BackgroundKept: []string{"ground shadow under the bumper"}}},
		{"large leftover withholds", `{"missingParts":[],"backgroundKept":["LARGE: part of a silver car","gravel"],"notes":""}`,
			QAVerdict{MissingParts: []string{}, BackgroundKept: []string{"part of a silver car", "gravel"}, LargeBackground: true}},
		{"large, lower case, no text", `{"missingParts":[],"backgroundKept":["large:"]}`,
			QAVerdict{MissingParts: []string{}, BackgroundKept: []string{"large leftover background"}, LargeBackground: true}},
		{"fenced", "```json\n{\"missingParts\": [], \"backgroundKept\": [], \"notes\": \"ok\"}\n```",
			QAVerdict{OK: true, MissingParts: []string{}, BackgroundKept: []string{}, Notes: "ok"}},
		{"fence without language", "```\n{\"missingParts\": [\"wheel\"]}\n```",
			QAVerdict{MissingParts: []string{"wheel"}, BackgroundKept: []string{}}},
		{"prose around it", "Here is the result:\n{\"missingParts\":[\"mirror\"],\"backgroundKept\":[]}\nDone.",
			QAVerdict{MissingParts: []string{"mirror"}, BackgroundKept: []string{}}},
		{"placeholders dropped", `{"missingParts":["None", " ", "n/a."],"backgroundKept":["nothing"]}`,
			QAVerdict{OK: true, MissingParts: []string{}, BackgroundKept: []string{}}},
		{"no background key", `{"missingParts":[]}`,
			QAVerdict{OK: true, MissingParts: []string{}, BackgroundKept: []string{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := ParseQA(tt.text)
			require.NoError(t, err)
			assert.Equal(t, tt.want, v)
		})
	}

	for _, bad := range []string{
		"", "no json here", `{"backgroundKept":[],"notes":"x"}`, `{"missingParts":null}`,
		`{"missingParts":"mirror"}`, `{"missingParts":[1]}`, `{"missingParts":[`, "[]",
	} {
		_, err := ParseQA(bad)
		assert.ErrorIs(t, err, ErrQABadOutput, "%q", bad)
	}

	// Caps: 12 items of at most 120 characters, notes 300.
	long := strings.Repeat("é", 200)
	items := `"` + strings.Repeat(long+`","`, 19) + long + `"`
	v, err := ParseQA(`{"missingParts":[` + items + `],"notes":"` + strings.Repeat("n", 500) + `"}`)
	require.NoError(t, err)
	assert.Len(t, v.MissingParts, 12)
	assert.Equal(t, 120, len([]rune(v.MissingParts[0])))
	assert.Len(t, v.Notes, 300)
	assert.False(t, v.OK)
}

func TestQAVerdictReason(t *testing.T) {
	assert.Empty(t, QAVerdict{OK: true}.Reason())
	assert.Equal(t, "The cut-out may have removed part of the car: roof antenna, left mirror.",
		QAVerdict{MissingParts: []string{"roof antenna", "left mirror"}}.Reason())
	assert.Equal(t, "The cut-out kept a large piece of the old background, so it was not used.",
		QAVerdict{LargeBackground: true, BackgroundKept: []string{"a car"}}.Reason())
}

// TestFakeAnalyzerQA: the fake analyzer's studio answer is a valid QA answer with
// nothing missing (the default e2e path), and "missingpart" in the source name makes
// it report a missing part (the withheld path).
func TestFakeAnalyzerQA(t *testing.T) {
	a, err := fake.New(fake.Options{})
	require.NoError(t, err)
	p := QAParams{JobID: "job_1", DomainKey: "cars", ImageID: "img_1", View: "side_profile", SourceName: "walkaround.mp4"}
	resp, err := a.Generate(context.Background(), QARequest(p))
	require.NoError(t, err)
	v, err := ParseQA(resp.Text)
	require.NoError(t, err)
	assert.True(t, v.OK)
	assert.Equal(t, fake.ModelPrefix+DefaultQAModel, resp.Usage.Model, "priced at $0")

	p.SourceName = "walkaround-MissingPart.mp4"
	resp, err = a.Generate(context.Background(), QARequest(p))
	require.NoError(t, err)
	v, err = ParseQA(resp.Text)
	require.NoError(t, err)
	assert.False(t, v.OK)
	assert.NotEmpty(t, v.MissingParts)
}
