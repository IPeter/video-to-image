package studio

import (
	"image"
	"math"
	"sort"
)

// ComputeStats measures a mask the way the sidecar does (services/studio,
// mask_stats): the mask is alpha ≥ 128; box is the review box in the image's
// normalised coordinates (rounded to whole pixels); components are 8-connected; a frame
// edge counts as touched in a row (left/right) or column (top/bottom) when any of its
// outermost max(2, 0.3 % of the long side) pixels is in the mask. IoU and
// RescuedFraction describe the two models and are left 0; callers with a single mask set
// IoU to 1.
func ComputeStats(alpha *image.Gray, box Box) MaskStats {
	var st MaskStats
	if alpha == nil {
		return st
	}
	a := grayPlane(alpha)
	b := alpha.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return st
	}
	px := func(v float64, n int) int {
		if math.IsNaN(v) {
			return 0
		}
		return min(max(int(math.RoundToEven(v*float64(n))), 0), n)
	}
	bx0, by0, bx1, by1 := px(box.X0, w), px(box.Y0, h), px(box.X1, w), px(box.Y1, h)
	boxArea := max(0, bx1-bx0) * max(0, by1-by0)

	area, inBox, soft := 0, 0, 0
	for y := range h {
		row := a[y*w : (y+1)*w]
		inRows := y >= by0 && y < by1
		for x, v := range row {
			if v >= 128 {
				area++
				if inRows && x >= bx0 && x < bx1 {
					inBox++
				}
			}
			if v > 0 && v < 250 {
				soft++
			}
		}
	}
	n := float64(w * h)
	st.Coverage = float64(area) / n
	st.BoxFill = float64(inBox) / float64(max(1, boxArea))
	st.OutsideBox = float64(area-inBox) / float64(max(1, area))
	st.SemiTransparent = float64(soft) / float64(max(1, area))

	runs, roots := labelRuns(w, 0, 0, w-1, h-1, func(i int) bool { return a[i] >= 128 }, true)
	sizes := map[int32]int{}
	for _, r := range runs {
		sizes[roots[r.id]] += r.x1 - r.x0 + 1
	}
	areas := make([]int, 0, len(sizes))
	for _, s := range sizes {
		areas = append(areas, s)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(areas)))
	if len(areas) > 1 {
		st.SecondComponent = float64(areas[1]) / n
	}

	e := max(2, int(0.003*float64(max(w, h))))
	e = min(e, w, h)
	in := func(x, y int) bool { return a[y*w+x] >= 128 }
	var left, right, top, bottom int
	for y := range h {
		l, r := false, false
		for k := range e {
			l = l || in(k, y)
			r = r || in(w-1-k, y)
		}
		if l {
			left++
		}
		if r {
			right++
		}
	}
	for x := range w {
		t, bt := false, false
		for k := range e {
			t = t || in(x, k)
			bt = bt || in(x, h-1-k)
		}
		if t {
			top++
		}
		if bt {
			bottom++
		}
	}
	st.EdgeContact = EdgeContact{
		Left:   float64(left) / float64(h),
		Right:  float64(right) / float64(h),
		Top:    float64(top) / float64(w),
		Bottom: float64(bottom) / float64(w),
	}
	return st
}
