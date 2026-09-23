package export

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/storage"
)

// Quadrant colours of the stored test frame.
var (
	red   = color.RGBA{220, 20, 20, 255}
	green = color.RGBA{20, 200, 40, 255}
	blue  = color.RGBA{30, 40, 220, 255}
	white = color.RGBA{240, 240, 240, 255}
)

// writeQuadrants writes a w×h JPEG: red top-left, green top-right, blue bottom-left,
// white bottom-right.
func writeQuadrants(t *testing.T, path string, w, h int, gray bool) {
	t.Helper()
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := red
			switch {
			case x >= w/2 && y < h/2:
				c = green
			case x < w/2 && y >= h/2:
				c = blue
			case x >= w/2 && y >= h/2:
				c = white
			}
			rgba.SetRGBA(x, y, c)
		}
	}
	var m image.Image = rgba
	if gray {
		g := image.NewGray(rgba.Bounds())
		for y := range h {
			for x := range w {
				g.Set(x, y, rgba.At(x, y))
			}
		}
		m = g
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, m, &jpeg.Options{Quality: 95}))
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

func decodeJPEG(t *testing.T, b []byte) image.Image {
	t.Helper()
	m, err := jpeg.Decode(bytes.NewReader(b))
	require.NoError(t, err)
	return m
}

// assertColour checks the colour at (fx, fy) of m's size (fractions), with JPEG slack.
func assertColour(t *testing.T, m image.Image, fx, fy float64, want color.RGBA, msg string) {
	t.Helper()
	b := m.Bounds()
	x, y := b.Min.X+int(fx*float64(b.Dx())), b.Min.Y+int(fy*float64(b.Dy()))
	got := color.RGBAModel.Convert(m.At(x, y)).(color.RGBA)
	near := func(a, b uint8) bool { return absDiff(a, b) <= 40 }
	assert.True(t, near(got.R, want.R) && near(got.G, want.G) && near(got.B, want.B),
		"%s: colour at (%d,%d) is %v, want about %v", msg, x, y, got, want)
}

// corners are the expected quadrant colours of an image: TL, TR, BL, BR.
type corners [4]color.RGBA

func assertCorners(t *testing.T, m image.Image, want corners, msg string) {
	t.Helper()
	assertColour(t, m, 0.25, 0.25, want[0], msg+" top-left")
	assertColour(t, m, 0.75, 0.25, want[1], msg+" top-right")
	assertColour(t, m, 0.25, 0.75, want[2], msg+" bottom-left")
	assertColour(t, m, 0.75, 0.75, want[3], msg+" bottom-right")
}

func TestRunEdits(t *testing.T) {
	const w, h = 1280, 720
	for _, tc := range []struct {
		name        string
		edit        *Edit
		gray        bool
		size, thumb image.Point
		corners     corners // of the stored image (and its thumbnail)
		rotation    int
		crop        *NRect // in the manifest
		check       func(t *testing.T, m image.Image)
	}{
		{
			name: "rotate 90", edit: &Edit{RotateCW: 90},
			size: image.Pt(720, 1280), thumb: image.Pt(640, 1138), rotation: 90,
			corners: corners{blue, red, white, green},
		},
		{
			name: "rotate 180", edit: &Edit{RotateCW: 180},
			size: image.Pt(1280, 720), thumb: image.Pt(640, 360), rotation: 180,
			corners: corners{white, blue, green, red},
		},
		{
			name: "rotate 270", edit: &Edit{RotateCW: 270},
			size: image.Pt(720, 1280), thumb: image.Pt(640, 1138), rotation: 270,
			corners: corners{green, white, red, blue},
		},
		{
			name: "rotate -90 is 270", edit: &Edit{RotateCW: -90},
			size: image.Pt(720, 1280), thumb: image.Pt(640, 1138), rotation: 270,
			corners: corners{green, white, red, blue},
		},
		{
			name: "crop only", edit: &Edit{Crop: &NRect{X0: 0.25, Y0: 0, X1: 1, Y1: 1}},
			size: image.Pt(960, 720), thumb: image.Pt(640, 480),
			crop: &NRect{X0: 0.25, Y0: 0, X1: 1, Y1: 1},
			check: func(t *testing.T, m image.Image) {
				// The quadrant boundary (x=640 in the frame) is at a third of the crop.
				assertColour(t, m, 0.15, 0.25, red, "left third, top")
				assertColour(t, m, 0.15, 0.75, blue, "left third, bottom")
				assertColour(t, m, 0.6, 0.25, green, "right, top")
				assertColour(t, m, 0.6, 0.75, white, "right, bottom")
			},
		},
		{
			name: "rotate 90 and crop the upright top-left", edit: &Edit{RotateCW: 90, Crop: &NRect{X0: 0, Y0: 0, X1: 0.75, Y1: 0.5}},
			// Upright 720x1280: blue top-left, red top-right. The crop is 540x640 and
			// keeps two thirds blue, one third red; the thumbnail is not upscaled.
			size: image.Pt(540, 640), thumb: image.Pt(540, 640), rotation: 90,
			crop: &NRect{X0: 0, Y0: 0, X1: 0.75, Y1: 0.5},
			check: func(t *testing.T, m image.Image) {
				assertColour(t, m, 0.3, 0.2, blue, "left")
				assertColour(t, m, 0.3, 0.9, blue, "left, bottom")
				assertColour(t, m, 0.85, 0.5, red, "right")
			},
		},
		{
			name: "crop is clamped to the frame", edit: &Edit{Crop: &NRect{X0: -0.2, Y0: 0.5, X1: 0.5, Y1: 1.3}},
			size: image.Pt(640, 360), thumb: image.Pt(640, 360),
			crop: &NRect{X0: 0, Y0: 0.5, X1: 0.5, Y1: 1},
			check: func(t *testing.T, m image.Image) {
				assertColour(t, m, 0.5, 0.5, blue, "bottom-left quadrant only")
				assertColour(t, m, 0.05, 0.05, blue, "near its corner")
				assertColour(t, m, 0.95, 0.95, blue, "far corner")
			},
		},
		{
			name: "greyscale frame rotated 90", edit: &Edit{RotateCW: 90}, gray: true,
			size: image.Pt(720, 1280), thumb: image.Pt(640, 1138), rotation: 90,
			check: func(t *testing.T, m image.Image) {
				// Luma of red, green, blue, white: about 80, 128, 57, 240; upright TL is
				// blue, TR red, BL white, BR green.
				lum := func(fx, fy float64) int {
					b := m.Bounds()
					r, _, _, _ := color.GrayModel.Convert(m.At(int(fx*float64(b.Dx())), int(fy*float64(b.Dy())))).RGBA()
					return int(r >> 8)
				}
				assert.InDelta(t, 57, lum(0.25, 0.25), 12)
				assert.InDelta(t, 80, lum(0.75, 0.25), 12)
				assert.InDelta(t, 240, lum(0.25, 0.75), 12)
				assert.InDelta(t, 128, lum(0.75, 0.75), 12)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := newStore(t)
			fx := newFixture(t, 2) // image 1 is edited, image 2 copied as before
			dir := filepath.Dir(fx.in.Images[0].FullPath)
			writeQuadrants(t, fx.in.Images[0].FullPath, w, h, tc.gray)
			fx.in.Images[0].Edit = tc.edit
			before, err := os.ReadDir(dir)
			require.NoError(t, err)

			out, err := Run(t.Context(), st, fx.in)
			require.NoError(t, err)
			require.Len(t, out.Images, 2)

			e := out.Images[0]
			assert.Equal(t, fx.in.Images[0], e.Image, "input carried through")
			assert.Equal(t, tc.size.X, e.Width)
			assert.Equal(t, tc.size.Y, e.Height)
			assert.Equal(t, tc.rotation, e.Rotation)
			assert.Equal(t, tc.crop != nil, e.Cropped)

			full := readObject(t, st, e.ImageKey)
			assert.EqualValues(t, len(full), e.Bytes)
			info, err := st.Head(t.Context(), e.ImageKey)
			require.NoError(t, err)
			assert.Equal(t, "image/jpeg", info.ContentType)
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(full))
			require.NoError(t, err)
			assert.Equal(t, tc.size, image.Pt(cfg.Width, cfg.Height), "stored image size")
			fullImg := decodeJPEG(t, full)
			thumb := decodeJPEG(t, readObject(t, st, e.ThumbKey))
			assert.Equal(t, tc.thumb, thumb.Bounds().Size(), "thumbnail size")
			if tc.check != nil {
				tc.check(t, fullImg)
				tc.check(t, thumb)
			} else {
				assertCorners(t, fullImg, tc.corners, "full")
				assertCorners(t, thumb, tc.corners, "thumbnail")
			}

			// The unedited image is still copied byte for byte.
			c := out.Images[1]
			assert.Zero(t, c.Rotation)
			assert.False(t, c.Cropped)
			src, err := os.ReadFile(fx.in.Images[1].FullPath)
			require.NoError(t, err)
			assert.Equal(t, src, readObject(t, st, c.ImageKey))
			small, err := os.ReadFile(fx.in.Images[1].SmallPath)
			require.NoError(t, err)
			assert.Equal(t, small, readObject(t, st, c.ThumbKey))

			// Manifest.
			var m Manifest
			require.NoError(t, json.Unmarshal(readObject(t, st, out.ManifestKey), &m))
			require.Len(t, m.Images, 2)
			assert.Equal(t, tc.rotation, m.Images[0].Rotation)
			assert.Equal(t, tc.crop, m.Images[0].Crop)
			assert.Equal(t, tc.size.X, m.Images[0].Width)
			assert.Equal(t, tc.size.Y, m.Images[0].Height)
			assert.Zero(t, m.Images[1].Rotation)
			assert.Nil(t, m.Images[1].Crop)

			// The ZIP carries the edited bytes.
			zipBytes := readObject(t, st, out.ZipKey)
			zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
			require.NoError(t, err)
			require.Len(t, zr.File, 3)
			assert.Equal(t, e.FileName, zr.File[0].Name)
			assert.Equal(t, zip.Store, zr.File[0].Method)
			assert.Zero(t, zr.File[0].Flags&0x8, "no data descriptor")
			assert.Equal(t, full, readZipEntry(t, zr.File[0]))
			assert.Equal(t, src, readZipEntry(t, zr.File[1]))

			// No temporary files are left next to the frames.
			after, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Equal(t, names(before), names(after))
		})
	}
}

func names(entries []os.DirEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestRunEditNoOp: an edit that changes nothing keeps the byte-for-byte copy.
func TestRunEditNoOp(t *testing.T) {
	for name, edit := range map[string]*Edit{
		"zero edit":                     {},
		"full-frame crop":               {Crop: &NRect{X0: 0, Y0: 0, X1: 1, Y1: 1}},
		"full turn":                     {RotateCW: 360},
		"crop rounding to the frame":    {Crop: &NRect{X0: 0.0001, Y0: 0, X1: 1, Y1: 0.9999}},
		"crop overhanging on all sides": {Crop: &NRect{X0: -1, Y0: -1, X1: 2, Y1: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			st := newStore(t)
			fx := newFixture(t, 1)
			fx.in.Images[0].Edit = edit
			out, err := Run(t.Context(), st, fx.in)
			require.NoError(t, err)
			e := out.Images[0]
			assert.Zero(t, e.Rotation)
			assert.False(t, e.Cropped)
			assert.Equal(t, image.Pt(1920, 1080), image.Pt(e.Width, e.Height))
			src, err := os.ReadFile(fx.in.Images[0].FullPath)
			require.NoError(t, err)
			assert.Equal(t, src, readObject(t, st, e.ImageKey), "byte for byte")
			small, err := os.ReadFile(fx.in.Images[0].SmallPath)
			require.NoError(t, err)
			assert.Equal(t, small, readObject(t, st, e.ThumbKey), "extracted thumbnail")
		})
	}
}

func TestRunEditValidation(t *testing.T) {
	st := newStore(t)
	fx := newFixture(t, 1)
	for _, tc := range []struct {
		name string
		edit *Edit
		want string
	}{
		{"odd rotation", &Edit{RotateCW: 45}, "rotation 45 is not a multiple of 90"},
		{"NaN crop", &Edit{Crop: &NRect{X0: 0.1, Y0: 0.1, X1: math.NaN(), Y1: 0.9}}, "invalid crop"},
		{"reversed crop", &Edit{Crop: &NRect{X0: 0.8, Y0: 0.1, X1: 0.2, Y1: 0.9}}, "invalid crop"},
		{"crop outside", &Edit{Crop: &NRect{X0: 1.1, Y0: 0.1, X1: 1.5, Y1: 0.9}}, "invalid crop"},
		{"crop thinner than a pixel", &Edit{Crop: &NRect{X0: 0.5, Y0: 0.1, X1: 0.5001, Y1: 0.9}}, "is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fx.in
			in.Images = []Image{fx.in.Images[0]}
			in.Images[0].Edit = tc.edit
			_, err := Run(t.Context(), st, in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), in.Images[0].ImageID)
		})
	}
}

// TestRunEditUploadFails removes the temporary file when the upload fails.
func TestRunEditUploadFails(t *testing.T) {
	local := newStore(t)
	fx := newFixture(t, 2)
	for i := range fx.in.Images {
		writeQuadrants(t, fx.in.Images[i].FullPath, 320, 240, false) // small: fast under -race
		fx.in.Images[i].Edit = &Edit{RotateCW: 90}
	}
	dir := filepath.Dir(fx.in.Images[0].FullPath)
	before, err := os.ReadDir(dir)
	require.NoError(t, err)
	badKey := storage.ThumbKey(fx.in.DomainKey, fx.in.JobID, fx.in.Images[1].ImageID)
	st := &hookStorage{Storage: local, put: func(_ context.Context, key string, _ io.Reader) (bool, error) {
		return key == badKey, os.ErrPermission
	}}
	_, err = Run(t.Context(), st, fx.in)
	require.ErrorIs(t, err, os.ErrPermission)
	after, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Equal(t, names(before), names(after), "temporary files removed")
}

// TestRunEditIsIdempotent: re-encoding is deterministic, so a retry stores the same bytes.
func TestRunEditIsIdempotent(t *testing.T) {
	st := newStore(t)
	fx := newFixture(t, 1)
	writeQuadrants(t, fx.in.Images[0].FullPath, 320, 240, false)
	fx.in.Images[0].Edit = &Edit{RotateCW: 270, Crop: &NRect{X0: 0.1, Y0: 0.1, X1: 0.9, Y1: 0.6}}
	first, err := Run(t.Context(), st, fx.in)
	require.NoError(t, err)
	a := readObject(t, st, first.Images[0].ImageKey)
	second, err := Run(t.Context(), st, fx.in)
	require.NoError(t, err)
	assert.Equal(t, first.Images, second.Images)
	assert.Equal(t, a, readObject(t, st, second.Images[0].ImageKey))
}

func TestNewEdit(t *testing.T) {
	assert.Nil(t, NewEdit(0, nil))
	assert.Nil(t, NewEdit(360, nil))
	assert.Equal(t, &Edit{RotateCW: 270}, NewEdit(-90, nil))
	assert.Equal(t, &Edit{Crop: &NRect{X0: 0.1, Y0: 0.2, X1: 0.3, Y1: 0.4}}, NewEdit(0, &[4]float64{0.1, 0.2, 0.3, 0.4}))
}
