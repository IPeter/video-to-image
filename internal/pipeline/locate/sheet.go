package locate

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// maxDecodePixels refuses to decode absurdly large sweep frames (they come from the
// 480p preview; this only guards against a bad file in the work dir).
const maxDecodePixels = 64 << 20

// tile is one sweep frame with its time.
type tile struct {
	path string
	sec  float64
}

// sheet is one contact sheet written to disk.
type sheet struct {
	path       string
	first      float64 // time of the first tile
	last       float64 // time of the last tile
	tiles      int
	unreadable int // frames that could not be decoded (their cell is left dark)
}

// writeSheets lays tiles out on Cols×Rows sheets in order, each tile as a quad of its
// four rotations (quadCells) fitted into CellSide squares, with its time printed in
// the quad's top-left corner, and writes them as JPEGs to dir/sheet-<n>.jpg. A frame
// that cannot be decoded leaves its quad dark and is counted; a sheet with no readable
// frame is not written, and an error is returned only when no sheet could be written.
func writeSheets(dir string, tiles []tile, fpsHint float64) ([]sheet, error) {
	if len(tiles) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("locate: create sheet dir: %w", err)
	}
	per := Cols * Rows
	var out []sheet
	var firstErr error
	for start := 0; start < len(tiles); start += per {
		chunk := tiles[start:min(start+per, len(tiles))]
		rows := (len(chunk) + Cols - 1) / Cols
		w := Cols*QuadSide + (Cols+1)*Gap
		h := rows*QuadSide + (rows+1)*Gap
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{40, 40, 40, 255}), image.Point{}, draw.Src)
		sh := sheet{first: chunk[0].sec, last: chunk[len(chunk)-1].sec, tiles: len(chunk)}
		for i, t := range chunk {
			quad := image.Rect(0, 0, QuadSide, QuadSide).Add(image.Pt(
				Gap+(i%Cols)*(QuadSide+Gap), Gap+(i/Cols)*(QuadSide+Gap)))
			fr, err := decodeJPEG(t.path)
			if err != nil {
				sh.unreadable++
				if firstErr == nil {
					firstErr = err
				}
			} else {
				drawQuad(img, quad, fr)
			}
			drawLabel(img, quad.Min.Add(image.Pt(InnerGap, InnerGap)), formatSec(t.sec, fpsHint))
		}
		if sh.unreadable == sh.tiles {
			continue // a sheet of dark cells would only cost tokens
		}
		sh.path = filepath.Join(dir, fmt.Sprintf("sheet-%02d.jpg", len(out)+1))
		if err := encodeJPEG(sh.path, img); err != nil {
			return out, err
		}
		out = append(out, sh)
	}
	if len(out) == 0 && firstErr != nil {
		return nil, fmt.Errorf("locate: no sheet could be written: %w", firstErr)
	}
	return out, nil
}

// drawQuad draws src four times into quad, one copy per quadCells entry, each fitted
// into a CellSide square (aspect kept, centred) on a white cell background.
func drawQuad(dst *image.RGBA, quad image.Rectangle, src image.Image) {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 {
		return
	}
	scale := math.Min(float64(CellSide)/float64(sw), float64(CellSide)/float64(sh))
	tw := max(1, int(math.Round(float64(sw)*scale)))
	th := max(1, int(math.Round(float64(sh)*scale)))
	small := resizeArea(src, tw, th)
	for _, c := range quadCells {
		cell := image.Rect(0, 0, CellSide, CellSide).Add(quad.Min.Add(image.Pt(c.dx*(CellSide+InnerGap), c.dy*(CellSide+InnerGap))))
		draw.Draw(dst, cell, image.NewUniform(color.RGBA{235, 235, 235, 255}), image.Point{}, draw.Src)
		r := rotateRGBA(small, c.rotCW)
		rb := r.Bounds()
		off := image.Pt(cell.Min.X+(CellSide-rb.Dx())/2, cell.Min.Y+(CellSide-rb.Dy())/2)
		draw.Draw(dst, rb.Add(off), r, rb.Min, draw.Src)
	}
}

// rotateRGBA returns m rotated clockwise by deg (0, 90, 180, 270).
func rotateRGBA(m *image.RGBA, deg int) *image.RGBA {
	b := m.Bounds()
	w, h := b.Dx(), b.Dy()
	if deg%180 != 0 {
		w, h = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			var nx, ny int
			switch deg {
			case 90:
				nx, ny = b.Dy()-1-y, x
			case 180:
				nx, ny = b.Dx()-1-x, b.Dy()-1-y
			case 270:
				nx, ny = y, b.Dx()-1-x
			default:
				nx, ny = x, y
			}
			out.SetRGBA(nx, ny, m.RGBAAt(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}

// resizeArea downsamples src to w×h by averaging the covered source pixels (box
// filter); it never aliases however large the ratio. Upscaling is nearest neighbour.
func resizeArea(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0 := b.Min.Y + y*sh/h
		y1 := b.Min.Y + max((y+1)*sh/h, y*sh/h+1)
		for x := 0; x < w; x++ {
			x0 := b.Min.X + x*sw/w
			x1 := b.Min.X + max((x+1)*sw/w, x*sw/w+1)
			var r, g, bl, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, _ := src.At(xx, yy).RGBA()
					r += uint64(cr >> 8)
					g += uint64(cg >> 8)
					bl += uint64(cb >> 8)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			i := out.PixOffset(x, y)
			out.Pix[i+0] = uint8(r / n)
			out.Pix[i+1] = uint8(g / n)
			out.Pix[i+2] = uint8(bl / n)
			out.Pix[i+3] = 255
		}
	}
	return out
}

// drawLabel prints text at pos: white 7×13 bitmap glyphs scaled LabelScale times on
// a black box, big enough to be read after the model downsizes the sheet.
func drawLabel(dst *image.RGBA, pos image.Point, text string) {
	face := basicfont.Face7x13
	adv := font.MeasureString(face, text).Ceil()
	pad := 2
	small := image.NewRGBA(image.Rect(0, 0, adv+2*pad, face.Height+2*pad))
	draw.Draw(small, small.Bounds(), image.Black, image.Point{}, draw.Src)
	d := &font.Drawer{
		Dst:  small,
		Src:  image.White,
		Face: face,
		Dot:  fixed.P(pad, pad+face.Ascent),
	}
	d.DrawString(text)
	// Nearest-neighbour upscale keeps the glyph edges crisp.
	sb := small.Bounds()
	for y := 0; y < sb.Dy()*LabelScale; y++ {
		for x := 0; x < sb.Dx()*LabelScale; x++ {
			px, py := pos.X+x, pos.Y+y
			if !(image.Point{px, py}.In(dst.Bounds())) {
				continue
			}
			dst.Set(px, py, small.At(x/LabelScale, y/LabelScale))
		}
	}
}

// formatSec prints a time label: whole seconds at 1 fps and below ("12s"), one
// decimal when the sampling is finer ("12.5s").
func formatSec(sec, fps float64) string {
	if fps > 1 {
		return fmt.Sprintf("%.1fs", sec)
	}
	return fmt.Sprintf("%ds", int(math.Round(sec)))
}

func decodeJPEG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("locate: open frame: %w", err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		return nil, fmt.Errorf("locate: decode %s: %w", filepath.Base(path), err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return nil, fmt.Errorf("locate: decode %s: unsupported size %dx%d", filepath.Base(path), cfg.Width, cfg.Height)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("locate: rewind %s: %w", filepath.Base(path), err)
	}
	img, err := jpeg.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("locate: decode %s: %w", filepath.Base(path), err)
	}
	return img, nil
}

// encodeJPEG writes img to path via a temporary file, so a failed write never leaves
// a truncated sheet behind.
func encodeJPEG(path string, img image.Image) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sheet-*.jpg")
	if err != nil {
		return fmt.Errorf("locate: create sheet: %w", err)
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := jpeg.Encode(tmp, img, &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return fmt.Errorf("locate: encode sheet: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("locate: close sheet: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("locate: rename sheet: %w", err)
	}
	return nil
}
