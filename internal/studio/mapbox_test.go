package studio

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/pipeline/crop"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/storage"
)

func assertBox(t *testing.T, want, got Box, msg string) {
	t.Helper()
	const eps = 1e-9
	assert.InDelta(t, want.X0, got.X0, eps, msg+" X0")
	assert.InDelta(t, want.Y0, got.Y0, eps, msg+" Y0")
	assert.InDelta(t, want.X1, got.X1, eps, msg+" X1")
	assert.InDelta(t, want.Y1, got.Y1, eps, msg+" Y1")
}

func TestMapBox(t *testing.T) {
	b := Box{0.1, 0.2, 0.4, 0.6}
	tests := []struct {
		name string
		rot  int
		crop *Box
		want Box
		ok   bool
	}{
		{"as stored", 0, nil, b, true},
		// 90° CW: (x, y) → (1-y, x)
		{"90", 90, nil, Box{0.4, 0.1, 0.8, 0.4}, true},
		{"180", 180, nil, Box{0.6, 0.4, 0.9, 0.8}, true},
		// 270° CW: (x, y) → (y, 1-x)
		{"270", 270, nil, Box{0.2, 0.6, 0.6, 0.9}, true},
		{"-90 is 270", -90, nil, Box{0.2, 0.6, 0.6, 0.9}, true},
		{"450 is 90", 450, nil, Box{0.4, 0.1, 0.8, 0.4}, true},
		{"crop left half", 0, &Box{0, 0, 0.5, 1}, Box{0.2, 0.2, 0.8, 0.6}, true},
		{"crop after a turn", 90, &Box{0.3, 0, 0.9, 0.5}, Box{1.0 / 6, 0.2, 5.0 / 6, 0.8}, true},
		{"crop clips the box", 0, &Box{0.2, 0, 1, 1}, Box{0, 0.2, 0.25, 0.6}, true},
		{"full-frame crop", 180, &Box{0, 0, 1, 1}, Box{0.6, 0.4, 0.9, 0.8}, true},
		{"box outside the crop", 0, &Box{0.5, 0, 1, 1}, Box{}, false},
		{"box touching the crop edge only", 0, &Box{0.4, 0, 1, 1}, Box{}, false},
		{"rotation not a multiple of 90", 45, nil, Box{}, false},
		{"inverted crop", 0, &Box{0.8, 0, 0.2, 1}, Box{}, false},
		{"NaN crop", 0, &Box{math.NaN(), 0, 1, 1}, Box{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MapBox(b, tt.rot, tt.crop)
			require.Equal(t, tt.ok, ok)
			assertBox(t, tt.want, got, tt.name)
		})
	}

	// The stored box is clamped to the frame first; empty or non-finite boxes fail.
	got, ok := MapBox(Box{-0.1, 0.2, 0.4, 1.2}, 0, nil)
	require.True(t, ok)
	assertBox(t, Box{0, 0.2, 0.4, 1}, got, "clamped")
	for _, bad := range []Box{{0.5, 0.2, 0.5, 0.6}, {0.4, 0.2, 0.1, 0.6}, {math.Inf(1), 0, 1, 1}, {1.2, 0.2, 1.5, 0.6}, {}} {
		_, ok := MapBox(bad, 90, nil)
		assert.False(t, ok, "%+v", bad)
	}
}

// toStoredMirror is internal/pipeline/review's toStored (orient.go): the box as sent
// (the frame turned rotCW clockwise) back to the frame as stored.
func toStoredMirror(b Box, rotCW int) Box {
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

func randomBox(r *rand.Rand) Box {
	x0, y0 := r.Float64()*0.8, r.Float64()*0.8
	return Box{x0, y0, x0 + 0.02 + r.Float64()*(0.98-x0-0.02), y0 + 0.02 + r.Float64()*(0.98-y0-0.02)}
}

// TestMapBoxInvertsTheReview: the review measures the box in the upright copy it sent
// and stores it with toStored; mapping it back through the same rotation (no crop)
// gives the upright box again, for every rotation.
func TestMapBoxInvertsTheReview(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	for i := range 2000 {
		sent := randomBox(r)
		rot := 90 * (i % 4)
		got, ok := MapBox(toStoredMirror(sent, rot), rot, nil)
		require.True(t, ok)
		assertBox(t, sent, got, fmt.Sprintf("rot %d", rot))
	}
}

// TestMapBoxWithCropPlan: for crops planned by crop.Plan (which never cuts the
// subject), the mapped box lies inside the exported image without clipping and keeps
// its size relative to the crop.
func TestMapBoxWithCropPlan(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	sizes := [][2]int{{3840, 2160}, {2160, 3840}, {1920, 1080}, {1080, 1920}}
	params := crop.Params{Aspect: 4.0 / 3, MarginPct: 6, MinShortSidePx: 480}
	planned := 0
	for i := range 3000 {
		sz := sizes[i%len(sizes)]
		rot := 90 * r.IntN(4)
		stored := randomBox(r)
		res := crop.Plan(crop.Frame{Width: sz[0], Height: sz[1], RotateCW: rot,
			Box: [4]float64{stored.X0, stored.Y0, stored.X1, stored.Y1}}, params)
		if res.Crop == nil {
			continue
		}
		planned++
		c := Box{res.Crop[0], res.Crop[1], res.Crop[2], res.Crop[3]}
		got, ok := MapBox(stored, rot, &c)
		require.True(t, ok)
		up, _ := MapBox(stored, rot, nil)
		cw, ch := c.X1-c.X0, c.Y1-c.Y0
		// No clipping happened: the unclipped mapping equals the result.
		assertBox(t, Box{(up.X0 - c.X0) / cw, (up.Y0 - c.Y0) / ch, (up.X1 - c.X0) / cw, (up.Y1 - c.Y0) / ch}, got, "crop")
		// In exported pixels the box keeps its upright pixel size.
		uw, uh := sz[0], sz[1]
		if rot == 90 || rot == 270 {
			uw, uh = uh, uw
		}
		assert.InDelta(t, (up.X1-up.X0)*float64(uw), (got.X1-got.X0)*float64(res.Width), 1.5)
		assert.InDelta(t, (up.Y1-up.Y0)*float64(uh), (got.Y1-got.Y0)*float64(res.Height), 1.5)
	}
	require.Greater(t, planned, 300, "too few crops planned to mean anything")
}

// TestMapBoxThroughExport renders a white box into a black stored frame, exports it
// with the real export step (rotate, crop, JPEG), finds the white box in the exported
// image and compares it with MapBox.
func TestMapBoxThroughExport(t *testing.T) {
	if testing.Short() {
		t.Skip("encodes and exports frames")
	}
	st, err := storage.NewLocal(t.TempDir())
	require.NoError(t, err)
	dir := t.TempDir()
	stored := Box{0.15, 0.3, 0.55, 0.8}
	crops := []*Box{nil, {0.1, 0.05, 0.9, 0.95}, {0.2, 0.1, 0.8, 0.7}, {0, 0.25, 0.6, 1}}
	sizes := [][2]int{{320, 240}, {240, 320}}
	n := 0
	for _, sz := range sizes {
		for _, rot := range []int{0, 90, 180, 270} {
			for _, c := range crops {
				n++
				w, h := sz[0], sz[1]
				frame := image.NewRGBA(image.Rect(0, 0, w, h))
				for y := range h {
					for x := range w {
						fx, fy := (float64(x)+0.5)/float64(w), (float64(y)+0.5)/float64(h)
						v := uint8(0)
						if fx >= stored.X0 && fx < stored.X1 && fy >= stored.Y0 && fy < stored.Y1 {
							v = 255
						}
						frame.SetRGBA(x, y, color.RGBA{v, v, v, 255})
					}
				}
				full := filepath.Join(dir, fmt.Sprintf("f%d.jpg", n))
				var buf bytes.Buffer
				require.NoError(t, jpeg.Encode(&buf, frame, &jpeg.Options{Quality: 95}))
				require.NoError(t, os.WriteFile(full, buf.Bytes(), 0o600))
				var ec *[4]float64
				if c != nil {
					ec = &[4]float64{c.X0, c.Y0, c.X1, c.Y1}
				}
				edit := export.NewEdit(rot, ec)
				if edit == nil {
					edit = &export.Edit{} // copied as is
				}
				imgID := fmt.Sprintf("img_01J0000000000000000000%04d", n)
				out, err := export.Run(context.Background(), st, export.Input{
					DomainKey: "cars", JobID: "job_01J0000000000000007K2M9QXA",
					Images: []export.Image{{Index: 1, ImageID: imgID, ShotID: "side_profile", FullPath: full, SmallPath: full, Edit: edit}},
				})
				require.NoError(t, err)
				rc, err := st.Get(context.Background(), out.Images[0].ImageKey)
				require.NoError(t, err)
				data, err := io.ReadAll(rc)
				rc.Close()
				require.NoError(t, err)
				img, err := jpeg.Decode(bytes.NewReader(data))
				require.NoError(t, err)
				ew, eh := img.Bounds().Dx(), img.Bounds().Dy()
				// the white box's pixel bounds in the export
				x0, y0, x1, y1 := ew, eh, -1, -1
				for y := range eh {
					for x := range ew {
						if r, _, _, _ := img.At(x, y).RGBA(); r > 0x8000 {
							x0, x1 = min(x0, x), max(x1, x)
							y0, y1 = min(y0, y), max(y1, y)
						}
					}
				}
				want, ok := MapBox(stored, rot, c)
				msg := fmt.Sprintf("%dx%d rot %d crop %v", w, h, rot, c)
				require.True(t, ok, msg)
				require.GreaterOrEqual(t, x1, 0, msg+": white box not found")
				assert.InDelta(t, want.X0*float64(ew), float64(x0), 2, msg+" x0")
				assert.InDelta(t, want.Y0*float64(eh), float64(y0), 2, msg+" y0")
				assert.InDelta(t, want.X1*float64(ew), float64(x1+1), 2, msg+" x1")
				assert.InDelta(t, want.Y1*float64(eh), float64(y1+1), 2, msg+" y1")
			}
		}
	}
}
