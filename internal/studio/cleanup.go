package studio

import "math"

// cleanupOptions is the light alpha clean-up. It never lowers alpha inside the car:
// the only pixels that lose alpha are near-transparent haze (≤ lowSnap) and tiny islands
// far from the car.
type cleanupOptions struct {
	enabled bool
	// Alpha ≤ lowSnap becomes 0, alpha ≥ highSnap becomes 255 (fully original pixels).
	lowSnap, highSnap uint8
	// Groups of mask pixels that stay more than nearFrac × the long image side away from
	// the car (islands closer than that chain up, so a broken antenna stays) and cover
	// less than maxIslandFrac of the image are removed.
	maxIslandFrac, nearFrac float64
	// Enclosed not-fully-opaque regions smaller than maxHoleFrac of the image become
	// opaque (they show the original pixels).
	maxHoleFrac float64
}

var defaultCleanup = cleanupOptions{
	enabled:       true,
	lowSnap:       4,
	highSnap:      250,
	maxIslandFrac: 0.0005,
	nearFrac:      0.02,
	maxHoleFrac:   0.0003,
}

// CleanupStats reports what the clean-up changed.
type CleanupStats struct {
	IslandsRemoved  int `json:"islandsRemoved"`
	IslandPixels    int `json:"islandPixels"`
	HolesFilled     int `json:"holesFilled"`
	HolePixels      int `json:"holePixels"`
	LowSnapped      int `json:"lowSnapped"`
	HighSnapped     int `json:"highSnapped"`
	Components      int `json:"components"` // groups after bridging gaps ≤ nearFrac
	MainGroupPixels int `json:"mainGroupPixels"`
	OtherKeptPixels int `json:"otherKeptPixels"` // pixels in other groups too big to remove
}

// cleanupAlpha edits a (w×h) in place. Connected components are labelled on row runs
// with union-find (O(runs), not O(pixels) of stack work), so it stays fast at 4K.
func cleanupAlpha(a []uint8, w, h int, o cleanupOptions) CleanupStats {
	var st CleanupStats
	for i, v := range a {
		if v != 0 && v <= o.lowSnap {
			a[i] = 0
			st.LowSnapped++
		} else if v != 255 && v >= o.highSnap {
			a[i] = 255
			st.HighSnapped++
		}
	}
	n := w * h

	// Islands: group mask pixels whose gaps are at most `near` (dilate by near/2, label
	// the dilated mask, 8-connected), keep the group with the most mask pixels and every
	// group too big to count as a speck.
	near := max(2, int(math.Round(o.nearFrac*float64(max(w, h)))))
	d := dilateSquare(a, w, h, near/2)
	runs, roots := labelRuns(w, 0, 0, w-1, h-1, func(i int) bool { return d[i] != 0 }, true)
	fgCount := make(map[int32]int)
	for _, r := range runs {
		c := 0
		for _, v := range a[r.y*w+r.x0 : r.y*w+r.x1+1] {
			if v != 0 {
				c++
			}
		}
		fgCount[roots[r.id]] += c
	}
	st.Components = len(fgCount)
	var main int32 = -1
	for g, c := range fgCount {
		if main < 0 || c > fgCount[main] || (c == fgCount[main] && g < main) {
			main = g
		}
	}
	maxIsland := int(o.maxIslandFrac * float64(n))
	remove := map[int32]bool{}
	for g, c := range fgCount {
		switch {
		case g == main:
			st.MainGroupPixels = c
		case c < maxIsland:
			remove[g] = true
			st.IslandsRemoved++
			st.IslandPixels += c
		default:
			st.OtherKeptPixels += c
		}
	}
	if len(remove) > 0 {
		for _, r := range runs {
			if remove[roots[r.id]] {
				clear(a[r.y*w+r.x0 : r.y*w+r.x1+1])
			}
		}
	}

	// Holes: 4-connected regions of alpha < 255 inside the car's bounding box (grown by one
	// pixel) that do not reach its frame.
	x0, y0, x1, y1 := w, h, -1, -1
	for y := range h {
		row := a[y*w : (y+1)*w]
		for x, v := range row {
			if v != 0 {
				x0, x1 = min(x0, x), max(x1, x)
				y0, y1 = min(y0, y), max(y1, y)
			}
		}
	}
	if x1 < 0 {
		return st
	}
	bx0, by0, bx1, by1 := max(x0-1, 0), max(y0-1, 0), min(x1+1, w-1), min(y1+1, h-1)
	runs, roots = labelRuns(w, bx0, by0, bx1, by1, func(i int) bool { return a[i] != 255 }, false)
	area := map[int32]int{}
	touches := map[int32]bool{}
	for _, r := range runs {
		g := roots[r.id]
		area[g] += r.x1 - r.x0 + 1
		if r.y == by0 || r.y == by1 || r.x0 == bx0 || r.x1 == bx1 {
			touches[g] = true
		}
	}
	maxHole := int(o.maxHoleFrac * float64(n))
	for g, c := range area {
		if !touches[g] && c < maxHole {
			st.HolesFilled++
			st.HolePixels += c
		}
	}
	if st.HolesFilled > 0 {
		for _, r := range runs {
			g := roots[r.id]
			if !touches[g] && area[g] < maxHole {
				row := a[r.y*w+r.x0 : r.y*w+r.x1+1]
				for i := range row {
					row[i] = 255
				}
			}
		}
	}
	return st
}

type run struct {
	y, x0, x1 int
	id        int32
}

// labelRuns finds the runs of pixels with in(i) in the box [bx0,bx1]×[by0,by1] of a
// plane w pixels wide and unions touching runs of neighbouring rows (8- or
// 4-connected). roots[id] is the component of run id.
func labelRuns(w, bx0, by0, bx1, by1 int, in func(int) bool, eight bool) ([]run, []int32) {
	var runs []run
	var parent []int32
	find := func(x int32) int32 {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int32) {
		ra, rb := find(a), find(b)
		if ra != rb {
			if ra < rb {
				parent[rb] = ra
			} else {
				parent[ra] = rb
			}
		}
	}
	slack := 0
	if eight {
		slack = 1
	}
	prevStart, prevEnd := 0, 0 // runs of the previous row: runs[prevStart:prevEnd]
	for y := by0; y <= by1; y++ {
		curStart := len(runs)
		base := y * w
		for x := bx0; x <= bx1; {
			if !in(base + x) {
				x++
				continue
			}
			s := x
			for x <= bx1 && in(base+x) {
				x++
			}
			id := int32(len(runs))
			runs = append(runs, run{y: y, x0: s, x1: x - 1, id: id})
			parent = append(parent, id)
		}
		// merge with the previous row (both lists are sorted by x)
		j := prevStart
		for i := curStart; i < len(runs); i++ {
			r := runs[i]
			for j < prevEnd && runs[j].x1+slack < r.x0 {
				j++
			}
			for k := j; k < prevEnd && runs[k].x0-slack <= r.x1; k++ {
				union(r.id, runs[k].id)
			}
		}
		prevStart, prevEnd = curStart, len(runs)
	}
	roots := make([]int32, len(parent))
	for i := range parent {
		roots[i] = find(int32(i))
	}
	return runs, roots
}

// dilateSquare returns 1 where any pixel of a within the (2r+1)² square is non-zero.
func dilateSquare(a []uint8, w, h, r int) []uint8 {
	n := w * h
	hz := make([]uint8, n)
	for y := range h {
		row := a[y*w : (y+1)*w]
		out := hz[y*w : (y+1)*w]
		last := -1 << 30 // x of the last set pixel to the left
		for x := range w {
			if row[x] != 0 {
				last = x
			}
			if x-last <= r {
				out[x] = 1
			}
		}
		last = 1 << 30
		for x := w - 1; x >= 0; x-- {
			if row[x] != 0 {
				last = x
			}
			if last-x <= r {
				out[x] = 1
			}
		}
	}
	d := make([]uint8, n)
	cnt := make([]int32, w)
	for y := 0; y <= min(r, h-1); y++ {
		for x, v := range hz[y*w : (y+1)*w] {
			cnt[x] += int32(v)
		}
	}
	for y := range h {
		out := d[y*w : (y+1)*w]
		for x, c := range cnt {
			if c > 0 {
				out[x] = 1
			}
		}
		if y+r+1 < h {
			for x, v := range hz[(y+r+1)*w : (y+r+2)*w] {
				cnt[x] += int32(v)
			}
		}
		if y-r >= 0 {
			for x, v := range hz[(y-r)*w : (y-r+1)*w] {
				cnt[x] -= int32(v)
			}
		}
	}
	return d
}
