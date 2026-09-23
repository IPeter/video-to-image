package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	_ "image/jpeg" // DecodeConfig of the exported frames
	"io"
	"math"
	"os"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"video-to-image/internal/storage"
)

// uploadConcurrency bounds parallel image/thumbnail uploads (two Puts each).
const uploadConcurrency = 4

// item is an Exported image plus what the ZIP writer needs to write it as a stored
// entry with sizes and CRC in the local header (no data descriptor).
type item struct {
	Exported
	crc uint32
}

// Run uploads images, thumbnails, manifest.json and export.zip (streamed through an
// io.Pipe, no temp file) using the storage key helpers.
//
// Full-resolution frames are stored byte for byte (image/jpeg, never re-encoded) under
// storage.ImageKey and thumbnails under storage.ThumbKey; Width/Height come from the
// JPEG header and Bytes is the file size. manifest.json is always written. The ZIP holds
// every image under its FileName (stored, JPEG is already compressed) followed by
// manifest.json (deflated); with no images there is no ZIP and Output.ZipKey and
// Output.ZipName are empty. Output.Images keeps the order of in.Images.
//
// Run is idempotent: a retry overwrites the same keys. On error, objects already
// uploaded are left for the job's retention cleanup.
func Run(ctx context.Context, st storage.Storage, in Input) (*Output, error) {
	if err := validate(st, in); err != nil {
		return nil, err
	}
	exportedAt := time.Now().UTC().Truncate(time.Second)

	items := make([]item, len(in.Images))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(uploadConcurrency)
	for i, img := range in.Images {
		g.Go(func() error {
			it, err := exportImage(gctx, st, in.DomainKey, in.JobID, img)
			if err != nil {
				return fmt.Errorf("export: image %d (%s): %w", img.Index, img.ImageID, err)
			}
			items[i] = it
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	out := &Output{
		Images:      make([]Exported, len(items)),
		ManifestKey: storage.ManifestKey(in.DomainKey, in.JobID),
	}
	for i, it := range items {
		out.Images[i] = it.Exported
	}

	manifest, err := json.MarshalIndent(buildManifest(in, out.Images, exportedAt), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("export: encode manifest: %w", err)
	}
	manifest = append(manifest, '\n')
	if err := st.Put(ctx, out.ManifestKey, bytes.NewReader(manifest), "application/json"); err != nil {
		return nil, fmt.Errorf("export: store manifest: %w", err)
	}

	if len(items) > 0 {
		zipKey := storage.ZipKey(in.DomainKey, in.JobID)
		if err := putZip(ctx, st, zipKey, items, manifest, exportedAt); err != nil {
			return nil, err
		}
		out.ZipKey = zipKey
		out.ZipName = ZipName(in.DomainKey, in.JobID)
	}
	return out, nil
}

func validate(st storage.Storage, in Input) error {
	if st == nil {
		return errors.New("export: storage is nil")
	}
	if in.DomainKey == "" || in.JobID == "" {
		return errors.New("export: domain key and job ID are required")
	}
	imageIDs := make(map[string]bool, len(in.Images))
	names := make(map[string]bool, len(in.Images))
	for _, img := range in.Images {
		switch {
		case img.ImageID == "" || strings.ContainsAny(img.ImageID, `/\`):
			return fmt.Errorf("export: image %d: invalid image ID %q", img.Index, img.ImageID)
		case img.Index < 1:
			return fmt.Errorf("export: image %s: index %d is not 1-based", img.ImageID, img.Index)
		case img.FullPath == "" || img.SmallPath == "":
			return fmt.Errorf("export: image %s: full and small paths are required", img.ImageID)
		case imageIDs[img.ImageID]:
			return fmt.Errorf("export: duplicate image ID %s", img.ImageID)
		}
		imageIDs[img.ImageID] = true
		name := FileName(in.DomainKey, in.JobID, img.Index, img.ShotID, img.TimestampSec)
		if names[name] {
			return fmt.Errorf("export: duplicate file name %s", name)
		}
		names[name] = true
	}
	return nil
}

// exportImage uploads one frame and its thumbnail.
func exportImage(ctx context.Context, st storage.Storage, domainKey, jobID string, img Image) (item, error) {
	f, err := os.Open(img.FullPath)
	if err != nil {
		return item{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return item{}, err
	}
	cfg, err := jpegConfig(f)
	if err != nil {
		return item{}, fmt.Errorf("%s: %w", img.FullPath, err)
	}
	// CRC pass for the ZIP's stored entry (the file is in the page cache right after).
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return item{}, err
	}
	h := crc32.NewIEEE()
	n, err := io.Copy(h, f)
	if err != nil {
		return item{}, fmt.Errorf("read %s: %w", img.FullPath, err)
	}
	if n != fi.Size() {
		return item{}, fmt.Errorf("%s changed while exporting", img.FullPath)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return item{}, err
	}
	imageKey := storage.ImageKey(domainKey, jobID, img.ImageID)
	if err := st.Put(ctx, imageKey, f, "image/jpeg"); err != nil {
		return item{}, fmt.Errorf("store image: %w", err)
	}

	thumbKey := storage.ThumbKey(domainKey, jobID, img.ImageID)
	if err := putThumb(ctx, st, thumbKey, img.SmallPath); err != nil {
		return item{}, err
	}

	return item{
		Exported: Exported{
			Image:    img,
			FileName: FileName(domainKey, jobID, img.Index, img.ShotID, img.TimestampSec),
			ImageKey: imageKey,
			ThumbKey: thumbKey,
			Width:    cfg.Width,
			Height:   cfg.Height,
			Bytes:    n,
		},
		crc: h.Sum32(),
	}, nil
}

func putThumb(ctx context.Context, st storage.Storage, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("thumbnail: %w", err)
	}
	defer f.Close()
	if _, err := jpegConfig(f); err != nil {
		return fmt.Errorf("thumbnail %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("thumbnail: %w", err)
	}
	if err := st.Put(ctx, key, f, "image/jpeg"); err != nil {
		return fmt.Errorf("store thumbnail: %w", err)
	}
	return nil
}

// jpegConfig reads the image header and insists on JPEG, since the object is served
// as image/jpeg.
func jpegConfig(r io.Reader) (image.Config, error) {
	cfg, format, err := image.DecodeConfig(r)
	if err != nil {
		return image.Config{}, fmt.Errorf("read image header: %w", err)
	}
	if format != "jpeg" {
		return image.Config{}, fmt.Errorf("want a JPEG, got %s", format)
	}
	return cfg, nil
}

func buildManifest(in Input, images []Exported, exportedAt time.Time) Manifest {
	createdAt := in.CreatedAt.UTC()
	if in.CreatedAt.IsZero() {
		createdAt = exportedAt
	}
	m := Manifest{
		JobID:        in.JobID,
		DomainKey:    in.DomainKey,
		CreatedAt:    createdAt,
		Summary:      in.Summary,
		Explanation:  in.Explanation,
		Images:       make([]ManifestImage, 0, len(images)),
		MissingShots: make([]MissingShot, 0, len(in.MissingShots)),
	}
	for _, e := range images {
		m.Images = append(m.Images, ManifestImage{
			Index:          e.Index,
			File:           e.FileName,
			ShotID:         e.ShotID,
			ShotLabel:      e.ShotLabel,
			TimestampSec:   math.Round(finite(e.TimestampSec)*1000) / 1000,
			TimestampLabel: TimestampLabel(e.TimestampSec),
			Width:          e.Width,
			Height:         e.Height,
			Reason:         e.Reason,
			Scores: Scores{
				Relevance:       finite(e.Scores.Relevance),
				Sharpness:       finite(e.Scores.Sharpness),
				SharpnessZ:      finite(e.Scores.SharpnessZ),
				ExposurePenalty: finite(e.Scores.ExposurePenalty),
				Score:           finite(e.Scores.Score),
			},
		})
	}
	m.MissingShots = append(m.MissingShots, in.MissingShots...)
	return m
}

// finite maps NaN and ±Inf (which encoding/json rejects) to 0.
func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}
