package studio

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"time"
)

// FakeSegmenter is a deterministic Segmenter for tests and the "fake" V2I_STUDIO_URL:
// the mask is an ellipse inscribed in the box, and the stats are computed from it.
//
// The ellipse has a soft edge about a pixel and a half wide (so the compositor's edge
// colour estimation runs), its stats come from ComputeStats with IoU 1 and nothing
// rescued, and it never needs the models. Delay simulates the sidecar's latency
// (context-aware).
type FakeSegmenter struct {
	Delay time.Duration
}

var _ Segmenter = FakeSegmenter{}

// FakeModels is the provenance the fake reports in MaskResult.Models.
var FakeModels = map[string]string{"fake": "ellipse"}

func (f FakeSegmenter) Mask(ctx context.Context, jpegData []byte, box Box) (*MaskResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validBox(box) {
		return nil, fmt.Errorf("%w: invalid box %+v", ErrSegmenterRejected, box)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpegData))
	if err != nil {
		return nil, fmt.Errorf("%w: not a JPEG: %w", ErrSegmenterRejected, err)
	}
	if f.Delay > 0 {
		t := time.NewTimer(f.Delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	start := time.Now()
	alpha := EllipseMask(cfg.Width, cfg.Height, box)
	st := ComputeStats(alpha, box)
	st.IoU = 1
	models := make(map[string]string, len(FakeModels))
	for k, v := range FakeModels {
		models[k] = v
	}
	return &MaskResult{
		Alpha:     alpha,
		Stats:     st,
		TimingsMS: map[string]int64{"total": time.Since(start).Milliseconds() + f.Delay.Milliseconds()},
		Models:    models,
	}, nil
}

// Ready is always nil: the fake needs no models.
func (FakeSegmenter) Ready(context.Context) error { return nil }

// EllipseMask is a w×h mask holding the ellipse inscribed in box (normalised), with a
// soft edge of about 1.5 px.
func EllipseMask(w, h int, box Box) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	cx, cy := (box.X0+box.X1)/2*float64(w), (box.Y0+box.Y1)/2*float64(h)
	rx, ry := (box.X1-box.X0)/2*float64(w), (box.Y1-box.Y0)/2*float64(h)
	if !(rx > 0) || !(ry > 0) {
		return g
	}
	const soft = 1.5 // px
	y0 := max(int(math.Floor(cy-ry-soft)), 0)
	y1 := min(int(math.Ceil(cy+ry+soft)), h-1)
	for y := y0; y <= y1; y++ {
		dy := (float64(y) + 0.5 - cy) / ry
		row := g.Pix[y*g.Stride : y*g.Stride+w]
		for x := range w {
			dx := (float64(x) + 0.5 - cx) / rx
			r := math.Sqrt(dx*dx + dy*dy)
			// signed distance to the outline in pixels, approximately
			d := (1 - r) * min(rx, ry)
			switch {
			case d >= soft/2:
				row[x] = 255
			case d > -soft/2:
				row[x] = to8(255 * (d + soft/2) / soft)
			}
		}
	}
	return g
}
