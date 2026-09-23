package export

import (
	"image"
	"image/color"
	"image/draw"
	"math"
)

// rotatedSize is the size of a w×h image turned deg degrees (0, 90, 180 or 270).
func rotatedSize(w, h, deg int) (int, int) {
	if deg == 90 || deg == 270 {
		return h, w
	}
	return w, h
}

// rotate returns m turned clockwise by deg (0, 90, 180 or 270; 0 returns m as is). The
// result's origin is (0, 0).
//
// Colour JPEGs (*image.YCbCr) are rotated plane by plane with no colour conversion: the
// chroma planes turn with the image, so 4:2:0 and 4:4:4 stay as they are and 4:2:2
// becomes 4:4:0 at 90° and 270°. That needs the subsampled planes to line up with the
// luma after the turn, i.e. even sizes (every video frame has them); odd sizes, 4:1:1
// and 4:1:0, CMYK and other models go through *image.RGBA instead. *image.Gray and
// *image.RGBA are rotated directly.
func rotate(m image.Image, deg int) image.Image {
	if deg == 0 {
		return m
	}
	switch m := m.(type) {
	case *image.YCbCr:
		if r, ok := rotateYCbCr(m, deg); ok {
			return r
		}
	case *image.Gray:
		b := m.Bounds()
		w, h := b.Dx(), b.Dy()
		dw, dh := rotatedSize(w, h, deg)
		out := image.NewGray(image.Rect(0, 0, dw, dh))
		rotatePlane(out.Pix, out.Stride, m.Pix[m.PixOffset(b.Min.X, b.Min.Y):], m.Stride, w, h, 1, deg)
		return out
	case *image.RGBA:
		return rotateRGBA(m, deg)
	}
	return rotateRGBA(toRGBA(m), deg)
}

func rotateRGBA(m *image.RGBA, deg int) *image.RGBA {
	b := m.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := rotatedSize(w, h, deg)
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	rotatePlane(out.Pix, out.Stride, m.Pix[m.PixOffset(b.Min.X, b.Min.Y):], m.Stride, w, h, 4, deg)
	return out
}

// rotateYCbCr rotates the three planes; ok is false when the layout cannot be rotated
// exactly without a conversion (see rotate).
func rotateYCbCr(m *image.YCbCr, deg int) (*image.YCbCr, bool) {
	b := m.Bounds()
	w, h := b.Dx(), b.Dy()
	ratio, ok := rotatedRatio(m.SubsampleRatio, deg)
	if !ok {
		return nil, false
	}
	if m.SubsampleRatio != image.YCbCrSubsampleRatio444 &&
		(w%2 != 0 || h%2 != 0 || b.Min.X%2 != 0 || b.Min.Y%2 != 0) {
		return nil, false
	}
	dw, dh := rotatedSize(w, h, deg)
	out := image.NewYCbCr(image.Rect(0, 0, dw, dh), ratio)
	rotatePlane(out.Y, out.YStride, m.Y[m.YOffset(b.Min.X, b.Min.Y):], m.YStride, w, h, 1, deg)
	cw, ch := chromaSize(m.SubsampleRatio, w, h)
	co := m.COffset(b.Min.X, b.Min.Y)
	rotatePlane(out.Cb, out.CStride, m.Cb[co:], m.CStride, cw, ch, 1, deg)
	rotatePlane(out.Cr, out.CStride, m.Cr[co:], m.CStride, cw, ch, 1, deg)
	return out, true
}

// rotatedRatio is the subsampling of a YCbCr image after the turn: horizontal and
// vertical subsampling swap at 90° and 270°.
func rotatedRatio(r image.YCbCrSubsampleRatio, deg int) (image.YCbCrSubsampleRatio, bool) {
	switch r {
	case image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio420:
		return r, true
	case image.YCbCrSubsampleRatio422, image.YCbCrSubsampleRatio440:
		if deg == 180 {
			return r, true
		}
		if r == image.YCbCrSubsampleRatio422 {
			return image.YCbCrSubsampleRatio440, true
		}
		return image.YCbCrSubsampleRatio422, true
	}
	return r, false // 4:1:1 and 4:1:0 have no rotated counterpart
}

// chromaSize is the chroma plane size of a w×h image with even sides.
func chromaSize(r image.YCbCrSubsampleRatio, w, h int) (int, int) {
	switch r {
	case image.YCbCrSubsampleRatio422:
		return w / 2, h
	case image.YCbCrSubsampleRatio420:
		return w / 2, h / 2
	case image.YCbCrSubsampleRatio440:
		return w, h / 2
	}
	return w, h
}

// rotatePlane writes the w×h plane src (bpp bytes per pixel) turned clockwise by deg
// into dst, which holds the rotated size. It walks the source in tiles so that the
// transposed writes stay within a few cache lines.
func rotatePlane(dst []byte, dstStride int, src []byte, srcStride, w, h, bpp, deg int) {
	const tile = 64
	for ty := 0; ty < h; ty += tile {
		yEnd := min(ty+tile, h)
		for tx := 0; tx < w; tx += tile {
			xEnd := min(tx+tile, w)
			for sy := ty; sy < yEnd; sy++ {
				row := src[sy*srcStride : sy*srcStride+w*bpp]
				for sx := tx; sx < xEnd; sx++ {
					var dx, dy int
					switch deg {
					case 90: // top-left → top-right
						dx, dy = h-1-sy, sx
					case 180:
						dx, dy = w-1-sx, h-1-sy
					default: // 270: top-left → bottom-left
						dx, dy = sy, w-1-sx
					}
					o := dy*dstStride + dx*bpp
					if bpp == 1 {
						dst[o] = row[sx]
					} else {
						copy(dst[o:o+bpp], row[sx*bpp:sx*bpp+bpp])
					}
				}
			}
		}
	}
}

func toRGBA(m image.Image) *image.RGBA {
	b := m.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), m, b.Min, draw.Src)
	return out
}

// subImage crops m to r (in m's coordinates).
func subImage(m image.Image, r image.Rectangle) image.Image {
	if r == m.Bounds() {
		return m
	}
	if s, ok := m.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return s.SubImage(r)
	}
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Bounds(), m, r.Min, draw.Src)
	return out
}

// scaleToWidth returns m scaled to width w, keeping the aspect ratio; an image that is
// not wider than w is returned as is (thumbnails are never upscaled).
func scaleToWidth(m image.Image, w int) image.Image {
	b := m.Bounds()
	if b.Dx() <= w {
		return m
	}
	h := max(1, int(math.Round(float64(b.Dy())*float64(w)/float64(b.Dx()))))
	return resizeArea(m, w, h)
}

// span is one output pixel's run of source pixels and their weights (summing to 1).
type span struct {
	start int
	w     []float32
}

// boxSpans maps n source pixels onto dn output pixels: output i averages the source
// interval [i·n/dn, (i+1)·n/dn), partly covered pixels at either end weighted by
// their coverage.
func boxSpans(n, dn int) []span {
	scale := float64(n) / float64(dn)
	spans := make([]span, dn)
	for i := range spans {
		lo, hi := float64(i)*scale, float64(i+1)*scale
		first := min(int(lo), n-1)
		last := min(int(math.Ceil(hi))-1, n-1)
		ws := make([]float32, 0, last-first+1)
		for s := first; s <= last; s++ {
			c := math.Min(float64(s+1), hi) - math.Max(float64(s), lo)
			ws = append(ws, float32(max(c, 0)/scale))
		}
		spans[i] = span{start: first, w: ws}
	}
	return spans
}

// resizeArea scales m to dw×dh by area averaging (a box filter with exact fractional
// coverage, separable), which is the right filter for the large downscales of
// thumbnails and needs only a few rows of floats.
func resizeArea(m image.Image, dw, dh int) *image.RGBA {
	b := m.Bounds()
	xs, ys := boxSpans(b.Dx(), dw), boxSpans(b.Dy(), dh)
	read := rowReader(m)
	srcRow := make([]float32, b.Dx()*3)
	hRow := make([]float32, dw*3) // srcRow after the horizontal pass
	acc := make([]float32, dw*3)
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	cached := -1
	for dy, ysp := range ys {
		clear(acc)
		for k, wy := range ysp.w {
			sy := ysp.start + k
			if sy != cached { // neighbouring output rows share their boundary row
				read(b.Min.Y+sy, srcRow)
				for dx, xsp := range xs {
					var r, g, bl float32
					for j, wx := range xsp.w {
						p := srcRow[(xsp.start+j)*3:]
						r += wx * p[0]
						g += wx * p[1]
						bl += wx * p[2]
					}
					hRow[dx*3], hRow[dx*3+1], hRow[dx*3+2] = r, g, bl
				}
				cached = sy
			}
			for i, v := range hRow {
				acc[i] += wy * v
			}
		}
		o := out.Pix[dy*out.Stride : dy*out.Stride+dw*4]
		for dx := range dw {
			o[dx*4] = to8(acc[dx*3])
			o[dx*4+1] = to8(acc[dx*3+1])
			o[dx*4+2] = to8(acc[dx*3+2])
			o[dx*4+3] = 0xff
		}
	}
	return out
}

// rowReader returns a function filling dst with the RGB values of row y of m.
func rowReader(m image.Image) func(y int, dst []float32) {
	b := m.Bounds()
	switch m := m.(type) {
	case *image.YCbCr:
		return func(y int, dst []float32) {
			for x := b.Min.X; x < b.Max.X; x++ {
				yi, ci := m.YOffset(x, y), m.COffset(x, y)
				r, g, bl := color.YCbCrToRGB(m.Y[yi], m.Cb[ci], m.Cr[ci])
				i := (x - b.Min.X) * 3
				dst[i], dst[i+1], dst[i+2] = float32(r), float32(g), float32(bl)
			}
		}
	case *image.RGBA:
		return func(y int, dst []float32) {
			p := m.Pix[m.PixOffset(b.Min.X, y):]
			for i := range b.Dx() {
				dst[i*3], dst[i*3+1], dst[i*3+2] = float32(p[i*4]), float32(p[i*4+1]), float32(p[i*4+2])
			}
		}
	case *image.Gray:
		return func(y int, dst []float32) {
			p := m.Pix[m.PixOffset(b.Min.X, y):]
			for i := range b.Dx() {
				v := float32(p[i])
				dst[i*3], dst[i*3+1], dst[i*3+2] = v, v, v
			}
		}
	}
	return func(y int, dst []float32) {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := m.At(x, y).RGBA()
			i := (x - b.Min.X) * 3
			dst[i], dst[i+1], dst[i+2] = float32(r>>8), float32(g>>8), float32(bl>>8)
		}
	}
}

func to8(v float32) uint8 {
	return uint8(min(max(v+0.5, 0), 255))
}
