package review

import (
	"cmp"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sync/errgroup"
)

// JPEGQuality is the encoder quality of the frames sent to the model.
const JPEGQuality = 85

// maxDecodePixels refuses to decode absurdly large "scoring copies" (they are 640 px
// wide; this only guards against a decompression bomb in the work dir).
const maxDecodePixels = 64 << 20

// writeImages writes a copy of every frame's scoring image, downscaled so its long side
// is at most maxSide (never upscaled, aspect ratio kept) and then rotated clockwise by
// the frame's rot (0 for a frame whose orientation is unknown: the model then judges it
// as stored), to dir/<candidateID>.jpg, and returns the frames that were
// written with their path set, in input order. A frame that cannot be read or written
// is logged and left out; when none is left the first error is returned. Context
// errors are returned as-is.
func writeImages(ctx context.Context, dir string, frames []frame, maxSide int, jobID string) ([]frame, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("review: create image dir: %w", err)
	}
	errs := make([]error, len(frames))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(runtime.GOMAXPROCS(0))
	for i := range frames {
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			// A bad frame is recorded, not returned: it must not cancel the others.
			path := filepath.Join(dir, frames[i].cand.ID+".jpg")
			errs[i] = downscaleRotated(frames[i].cand.SmallPath, path, maxSide, frames[i].rot)
			if errs[i] == nil {
				frames[i].path = path
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	out := make([]frame, 0, len(frames))
	var first error
	for i, f := range frames {
		if errs[i] != nil {
			slog.WarnContext(ctx, "review: frame left out", "job", jobID, "candidate", f.cand.ID, "err", errs[i])
			first = cmp.Or(first, errs[i])
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 && first != nil {
		return nil, fmt.Errorf("review: no frame could be prepared: %w", first)
	}
	return out, nil
}

// Downscale reads the JPEG at src and writes it to dst as a JPEG (quality JPEGQuality)
// whose long side is at most maxSide px (≤ 0 → DefaultMaxImageSide), keeping the aspect
// ratio (each side rounded, at least 1 px). Smaller images are re-encoded at their own
// size. Downscaling averages the covered source pixels (box filter), which does not
// alias however large the ratio.
func Downscale(src, dst string, maxSide int) error { return downscaleRotated(src, dst, maxSide, 0) }

// downscaleRotated is Downscale followed by a clockwise rotation (0, 90, 180, 270).
func downscaleRotated(src, dst string, maxSide, rotCW int) error {
	if maxSide <= 0 {
		maxSide = DefaultMaxImageSide
	}
	img, err := decodeJPEG(src)
	if err != nil {
		return err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if long := max(w, h); long > maxSide {
		scale := float64(maxSide) / float64(long)
		img = resizeArea(img, max(1, roundInt(float64(w)*scale)), max(1, roundInt(float64(h)*scale)))
	}
	if rotCW%360 != 0 {
		rgba, ok := img.(*image.RGBA)
		if !ok {
			rgba = image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
			draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
		}
		img = rotateRGBA(rgba, rotCW%360)
	}
	return encodeJPEG(dst, img)
}

func roundInt(v float64) int { return int(v + 0.5) }

func decodeJPEG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("review: open frame: %w", err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		return nil, fmt.Errorf("review: decode %s: %w", filepath.Base(path), err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return nil, fmt.Errorf("review: decode %s: unsupported size %dx%d", filepath.Base(path), cfg.Width, cfg.Height)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("review: rewind %s: %w", filepath.Base(path), err)
	}
	img, err := jpeg.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("review: decode %s: %w", filepath.Base(path), err)
	}
	return img, nil
}

// encodeJPEG writes img to path via a temporary file, so a failed write never leaves a
// truncated image behind.
func encodeJPEG(path string, img image.Image) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".review-*.jpg")
	if err != nil {
		return fmt.Errorf("review: create image: %w", err)
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := jpeg.Encode(tmp, img, &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return fmt.Errorf("review: encode image: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("review: write image: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("review: write image: %w", err)
	}
	return nil
}

// tap is one source pixel's share of a destination pixel.
type tap struct {
	idx int
	w   float32
}

// areaTaps returns, for each of dst output pixels along one axis of src input pixels
// (dst ≤ src), the input pixels it covers and their weights (summing to 1).
func areaTaps(src, dst int) [][]tap {
	scale := float64(src) / float64(dst)
	out := make([][]tap, dst)
	for i := range out {
		lo, hi := float64(i)*scale, float64(i+1)*scale
		for j := int(lo); j < src && float64(j) < hi; j++ {
			a, b := max(lo, float64(j)), min(hi, float64(j+1))
			if b > a {
				out[i] = append(out[i], tap{idx: j, w: float32((b - a) / scale)})
			}
		}
	}
	return out
}

// resizeArea downscales img to w×h (both ≤ the source size) with a separable box
// filter. The result is opaque RGBA.
func resizeArea(img image.Image, w, h int) *image.RGBA {
	b := img.Bounds()
	src := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)
	sw, sh := b.Dx(), b.Dy()
	w, h = min(w, sw), min(h, sh)

	// Horizontal pass: sw×sh → w×sh, 3 channels (JPEG has no alpha).
	xt := areaTaps(sw, w)
	mid := make([]float32, w*sh*3)
	for y := 0; y < sh; y++ {
		row := src.Pix[y*src.Stride:]
		for x, taps := range xt {
			var r, g, bl float32
			for _, t := range taps {
				p := row[t.idx*4:]
				r += t.w * float32(p[0])
				g += t.w * float32(p[1])
				bl += t.w * float32(p[2])
			}
			o := (y*w + x) * 3
			mid[o], mid[o+1], mid[o+2] = r, g, bl
		}
	}

	// Vertical pass: w×sh → w×h.
	yt := areaTaps(sh, h)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y, taps := range yt {
		for x := 0; x < w; x++ {
			var r, g, bl float32
			for _, t := range taps {
				o := (t.idx*w + x) * 3
				r += t.w * mid[o]
				g += t.w * mid[o+1]
				bl += t.w * mid[o+2]
			}
			p := dst.Pix[y*dst.Stride+x*4:]
			p[0], p[1], p[2], p[3] = toU8(r), toU8(g), toU8(bl), 0xff
		}
	}
	return dst
}

func toU8(v float32) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(v + 0.5)
	}
}
