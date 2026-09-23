package export

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"io"
	"math"
	"os"
	"path/filepath"

	"video-to-image/internal/storage"
)

// Encoding of edited images. The full image is re-encoded once, at a quality close to
// the extracted frame's (-qscale:v 2); the thumbnail matches the extracted 640 px copy.
const (
	editQuality  = 92
	thumbQuality = 85
	thumbWidth   = 640
)

// NewEdit returns the Edit that turns a frame rotateCW degrees clockwise and then crops
// the upright frame to crop ({X0, Y0, X1, Y1} normalised, crop.Result's shape; nil =
// no crop), or nil when it would change nothing.
func NewEdit(rotateCW int, crop *[4]float64) *Edit {
	e := &Edit{RotateCW: (rotateCW%360 + 360) % 360}
	if crop != nil {
		e.Crop = &NRect{X0: crop[0], Y0: crop[1], X1: crop[2], Y1: crop[3]}
	}
	if e.RotateCW == 0 && e.Crop == nil {
		return nil
	}
	return e
}

// editOp is an Edit resolved against the frame's pixel size.
type editOp struct {
	rotate  int             // 0, 90, 180 or 270
	upright image.Point     // frame size after the rotation
	crop    image.Rectangle // in the upright frame; the whole frame when not cropped
}

func (op editOp) cropped() bool { return op.crop != image.Rect(0, 0, op.upright.X, op.upright.Y) }

func (op editOp) noop() bool { return op.rotate == 0 && !op.cropped() }

// manifestCrop is the applied crop, normalised in the upright frame (nil if none).
func (op editOp) manifestCrop() *NRect {
	if !op.cropped() {
		return nil
	}
	w, h := float64(op.upright.X), float64(op.upright.Y)
	r6 := func(v float64) float64 { return math.Round(v*1e6) / 1e6 }
	return &NRect{
		X0: r6(float64(op.crop.Min.X) / w), Y0: r6(float64(op.crop.Min.Y) / h),
		X1: r6(float64(op.crop.Max.X) / w), Y1: r6(float64(op.crop.Max.Y) / h),
	}
}

// resolveEdit turns e into pixel operations for a w×h frame: the crop is clamped to
// the frame and its edges rounded to whole pixels. A crop of the whole frame is no
// crop, so a nil Edit and a full-frame crop without rotation are both a no-op.
func resolveEdit(e *Edit, w, h int) (editOp, error) {
	op := editOp{upright: image.Pt(w, h), crop: image.Rect(0, 0, w, h)}
	if e == nil {
		return op, nil
	}
	op.rotate = (e.RotateCW%360 + 360) % 360
	uw, uh := rotatedSize(w, h, op.rotate)
	op.upright = image.Pt(uw, uh)
	full := image.Rect(0, 0, uw, uh)
	op.crop = full
	if c := e.Crop; c != nil {
		px := func(v float64, n int) int { return int(math.Round(min(max(v, 0), 1) * float64(n))) }
		r := image.Rect(px(c.X0, uw), px(c.Y0, uh), px(c.X1, uw), px(c.Y1, uh)).Intersect(full)
		if r.Empty() {
			return op, fmt.Errorf("crop %+v is empty for a %dx%d frame", *c, uw, uh)
		}
		op.crop = r
	}
	return op, nil
}

// validEdit checks what can be checked without the frame: a rotation that is a
// multiple of 90 and a finite crop overlapping [0,1]² with X0 < X1 and Y0 < Y1.
func validEdit(e *Edit) error {
	if e == nil {
		return nil
	}
	if e.RotateCW%90 != 0 {
		return fmt.Errorf("rotation %d is not a multiple of 90", e.RotateCW)
	}
	if c := e.Crop; c != nil {
		for _, v := range []float64{c.X0, c.Y0, c.X1, c.Y1} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("invalid crop %+v", *c)
			}
		}
		if !(c.X0 < c.X1 && c.Y0 < c.Y1 && c.X0 < 1 && c.Y0 < 1 && c.X1 > 0 && c.Y1 > 0) {
			return fmt.Errorf("invalid crop %+v", *c)
		}
	}
	return nil
}

// rendered is an edited frame written to a temporary file, plus its thumbnail.
type rendered struct {
	path          string // temporary JPEG, next to the source frame
	size          int64
	crc           uint32
	width, height int
	thumb         []byte
}

// editImage decodes the frame, applies op, writes the result and its thumbnail, and
// uploads both. Only one frame is decoded at a time across the whole export (x.editSem):
// a 4K frame is ~12 MB decoded and the rotation holds a second copy, so concurrent
// edits would multiply the peak. Uploads run outside that section.
func (x *exporter) editImage(ctx context.Context, img Image, f *os.File, cfg image.Config, op editOp) (item, error) {
	select {
	case x.editSem <- struct{}{}:
	case <-ctx.Done():
		return item{}, ctx.Err()
	}
	r, err := render(f, img.FullPath, cfg, op)
	<-x.editSem
	if err != nil {
		return item{}, err
	}
	keep := false
	defer func() {
		if !keep {
			os.Remove(r.path)
		}
	}()

	imageKey := storage.ImageKey(x.domainKey, x.jobID, img.ImageID)
	if err := putFile(ctx, x.st, imageKey, r.path); err != nil {
		return item{}, fmt.Errorf("store image: %w", err)
	}
	thumbKey := storage.ThumbKey(x.domainKey, x.jobID, img.ImageID)
	if err := x.st.Put(ctx, thumbKey, bytes.NewReader(r.thumb), "image/jpeg"); err != nil {
		return item{}, fmt.Errorf("store thumbnail: %w", err)
	}
	keep = true
	return item{
		Exported: Exported{
			Image:    img,
			FileName: FileName(x.domainKey, x.jobID, img.Index, img.ShotID, img.TimestampSec),
			ImageKey: imageKey,
			ThumbKey: thumbKey,
			Width:    r.width,
			Height:   r.height,
			Bytes:    r.size,
			Rotation: op.rotate,
			Cropped:  op.cropped(),
		},
		crc:  r.crc,
		path: r.path,
		tmp:  true,
		crop: op.manifestCrop(),
	}, nil
}

// render decodes f (whose header said cfg), rotates and crops it, encodes the result
// into a temporary file next to the source and the thumbnail into memory.
func render(f *os.File, srcPath string, cfg image.Config, op editOp) (rendered, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return rendered{}, err
	}
	src, err := jpeg.Decode(f)
	if err != nil {
		return rendered{}, fmt.Errorf("decode %s: %w", srcPath, err)
	}
	if b := src.Bounds(); b.Dx() != cfg.Width || b.Dy() != cfg.Height {
		return rendered{}, fmt.Errorf("%s changed while exporting", srcPath)
	}
	// src is dead after the turn, so the GC can take it while the rotated copy is
	// encoded. rotate returns images with their origin at (0, 0), and so does the JPEG
	// decoder; the Add is for safety only.
	up := rotate(src, op.rotate)
	edited := subImage(up, op.crop.Add(up.Bounds().Min))

	out, err := os.CreateTemp(filepath.Dir(srcPath), ".export-*.jpg")
	if err != nil {
		return rendered{}, fmt.Errorf("edited image: %w", err)
	}
	r := rendered{path: out.Name(), width: edited.Bounds().Dx(), height: edited.Bounds().Dy()}
	h := crc32.NewIEEE()
	cw := &countingWriter{w: io.MultiWriter(out, h)}
	err = jpeg.Encode(cw, edited, &jpeg.Options{Quality: editQuality})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(r.path)
		return rendered{}, fmt.Errorf("encode edited image: %w", err)
	}
	r.size, r.crc = cw.n, h.Sum32()

	var thumb bytes.Buffer
	if err := jpeg.Encode(&thumb, scaleToWidth(edited, thumbWidth), &jpeg.Options{Quality: thumbQuality}); err != nil {
		os.Remove(r.path)
		return rendered{}, fmt.Errorf("encode thumbnail: %w", err)
	}
	r.thumb = thumb.Bytes()
	return r, nil
}

func putFile(ctx context.Context, st storage.Storage, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return st.Put(ctx, key, f, "image/jpeg")
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
