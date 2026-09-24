package studio

import (
	"image"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rect(g *image.Gray, x0, y0, x1, y1 int, v uint8) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			g.Pix[y*g.Stride+x] = v
		}
	}
}

func TestComputeStatsExact(t *testing.T) {
	const w, h = 200, 100
	g := image.NewGray(image.Rect(0, 0, w, h))
	rect(g, 40, 20, 140, 80, 255)                  // 100×60 = 6000 px car, all inside the box
	rect(g, 40, 20, 140, 21, 200)                  // its top row semi-transparent (still ≥ 128)
	rect(g, 150, 85, 160, 95, 255)                 // a 10×10 second component, outside the box
	rect(g, 0, 0, 5, 5, 100)                       // faint haze: < 128, not mask, but semi-transparent
	box := Box{X0: 0.2, Y0: 0.2, X1: 0.7, Y1: 0.8} // pixels 40..140 × 20..80 = 6000
	s := ComputeStats(g, box)
	area := 6000.0 + 100
	assert.InDelta(t, area/(w*h), s.Coverage, 1e-12)
	assert.InDelta(t, 1.0, s.BoxFill, 1e-12)
	assert.InDelta(t, 100/area, s.OutsideBox, 1e-12)
	assert.InDelta(t, 100.0/(w*h), s.SecondComponent, 1e-12)
	assert.InDelta(t, (100+25)/area, s.SemiTransparent, 1e-12)
	assert.Equal(t, EdgeContact{}, s.EdgeContact)
	assert.Zero(t, s.IoU, "IoU is the sidecar's")
	assert.Zero(t, s.RescuedFraction)

	// Edge contact: share of the rows (left/right) or columns (top/bottom) that the mask
	// reaches within the outermost 2 px.
	e := image.NewGray(image.Rect(0, 0, w, h))
	rect(e, 0, 10, 50, 60, 255)   // left: rows 10..59 → 0.5
	rect(e, 190, 0, 199, 25, 255) // right: ends 1 px before the edge, still within 2 px → rows 0..24
	rect(e, 100, 97, 150, 100, 255)
	s = ComputeStats(e, Box{0, 0, 1, 1})
	assert.InDelta(t, 0.5, s.EdgeContact.Left, 1e-12)
	assert.InDelta(t, 0.25, s.EdgeContact.Right, 1e-12)
	assert.InDelta(t, 9.0/w, s.EdgeContact.Top, 1e-12, "the right block reaches the top: columns 190..198")
	assert.InDelta(t, 50.0/w, s.EdgeContact.Bottom, 1e-12)

	assert.Equal(t, MaskStats{}, ComputeStats(nil, box))
	empty := ComputeStats(image.NewGray(image.Rect(0, 0, 10, 10)), box)
	assert.Equal(t, MaskStats{}, empty, "an empty mask measures zero everywhere")
}

// TestComputeStatsMatchesTheSidecar: on the spike's masks (downscaled to the testdata
// size) the Go statistics agree with what the spike's sidecar measured at full size.
func TestComputeStatsMatchesTheSidecar(t *testing.T) {
	want := map[string]MaskStats{
		"pub03_chevrolet_sonic_white_rear34":   {Coverage: 0.5539, BoxFill: 0.6614, OutsideBox: 0.0003},
		"pub04_mazda_silver_rear34_vandalised": {Coverage: 0.423, BoxFill: 0.586, OutsideBox: 0.0006},
		"pub05_microvan_green_side_rusty":      {Coverage: 0.3331, BoxFill: 0.7148, OutsideBox: 0},
	}
	for _, fx := range fixtures {
		_, mask := loadFixture(t, fx.name)
		s := ComputeStats(mask, fx.box)
		w := want[fx.name]
		require.NotZero(t, w.Coverage)
		assert.InDelta(t, w.Coverage, s.Coverage, 0.005, fx.name)
		assert.InDelta(t, w.BoxFill, s.BoxFill, 0.005, fx.name)
		assert.InDelta(t, w.OutsideBox, s.OutsideBox, 0.002, fx.name)
		assert.Zero(t, s.SecondComponent, fx.name)
		assert.Equal(t, EdgeContact{}, s.EdgeContact, fx.name)
		assert.Less(t, s.SemiTransparent, DefaultGates.MaxSemiTransparent, fx.name)
		s.IoU = 0.99
		assert.True(t, CheckMask(s, DefaultGates).OK, fx.name)
	}
}
