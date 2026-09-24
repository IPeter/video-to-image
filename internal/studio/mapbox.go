package studio

import "math"

// MapBox maps a review box measured in the frame AS STORED (before rotation) through
// the export's clockwise rotation and its crop (normalised in the upright frame; nil =
// uncropped) into the exported image. ok is false when the box does not survive (empty
// after clipping).
//
// The conventions are the pipeline's: a clockwise turn by 90° maps a stored point
// (x, y) to (1-y, x), 180° to (1-x, 1-y) and 270° to (y, 1-x) (crop.Plan,
// export.rotate); the crop is the rectangle of the upright frame that becomes the
// exported image, so an upright point u lands at (u - crop.X0) / (crop.X1 - crop.X0).
// The export rounds the crop to whole pixels; with the crop it records (manifest /
// selected_images.crop, snapped to its pixel edges) the mapping is exact. The input box
// is clamped to [0,1]² first; a rotation that is not a multiple of 90, a non-finite
// value or an empty crop gives ok false.
func MapBox(stored Box, rotateCW int, crop *Box) (Box, bool) {
	if !finiteBox(stored) || rotateCW%90 != 0 {
		return Box{}, false
	}
	b := Box{X0: clamp01(stored.X0), Y0: clamp01(stored.Y0), X1: clamp01(stored.X1), Y1: clamp01(stored.Y1)}
	if !(b.X1 > b.X0) || !(b.Y1 > b.Y0) {
		return Box{}, false
	}
	rot := (rotateCW%360 + 360) % 360
	point := func(x, y float64) (float64, float64) {
		switch rot {
		case 90:
			return 1 - y, x
		case 180:
			return 1 - x, 1 - y
		case 270:
			return y, 1 - x
		}
		return x, y
	}
	ax, ay := point(b.X0, b.Y0)
	bx, by := point(b.X1, b.Y1)
	u := Box{X0: min(ax, bx), Y0: min(ay, by), X1: max(ax, bx), Y1: max(ay, by)}
	if crop != nil {
		if !finiteBox(*crop) {
			return Box{}, false
		}
		c := Box{X0: clamp01(crop.X0), Y0: clamp01(crop.Y0), X1: clamp01(crop.X1), Y1: clamp01(crop.Y1)}
		cw, ch := c.X1-c.X0, c.Y1-c.Y0
		if !(cw > 0) || !(ch > 0) {
			return Box{}, false
		}
		u = Box{
			X0: clamp01((u.X0 - c.X0) / cw), Y0: clamp01((u.Y0 - c.Y0) / ch),
			X1: clamp01((u.X1 - c.X0) / cw), Y1: clamp01((u.Y1 - c.Y0) / ch),
		}
	}
	if !(u.X1 > u.X0) || !(u.Y1 > u.Y0) {
		return Box{}, false
	}
	return u, true
}

func finiteBox(b Box) bool {
	for _, v := range []float64{b.X0, b.Y0, b.X1, b.Y1} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
