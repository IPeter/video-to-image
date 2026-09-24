package studiojob

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/store"
)

// ManifestNote is the manifest's statement about what the studio versions are.
const ManifestNote = "Studio backgrounds: only the background was replaced (backdrop and floor shadow); " +
	"the car pixels are unchanged from the original photo: no retouching, no generative redraw. " +
	"The originals are in the job's image ZIP."

// Manifest is manifest.json inside studio.zip.
type Manifest struct {
	JobID        string          `json:"jobId"`
	DomainKey    string          `json:"domainKey"`
	CreatedAt    time.Time       `json:"createdAt"`
	Note         string          `json:"note"`
	Styles       []string        `json:"styles"` // display order, the default first
	DefaultStyle string          `json:"defaultStyle"`
	Images       []ManifestImage `json:"images"`
}

// ManifestImage is one photo of the job and its studio outcome.
type ManifestImage struct {
	Index    int               `json:"index"`
	ImageID  string            `json:"imageId"`
	Original string            `json:"original"` // the original's file name (export.zip)
	ShotID   string            `json:"shotId"`
	Status   string            `json:"status"` // ready, withheld, skipped, failed
	Reason   string            `json:"reason,omitempty"`
	Files    map[string]string `json:"files,omitempty"` // style → path inside studio.zip
}

// errReaderGone is what the ZIP writer sees once the storage Put returned.
var errReaderGone = errors.New("studiojob: zip upload stopped reading")

// putZip streams studio.zip into storage: a folder per style with every ready photo's
// variant (<style>/<file name>_studio-<style>.jpg, stored, read back from storage one at
// a time) and manifest.json (deflated) with every photo's outcome.
func (rn *run) putZip(ctx context.Context, key string, images []store.SelectedImage) error {
	pr, pw := io.Pipe()
	stop := context.AfterFunc(ctx, func() { pw.CloseWithError(ctx.Err()) })
	defer stop()
	done := make(chan error, 1)
	go func() {
		err := rn.writeZip(ctx, pw, images)
		pw.CloseWithError(err) // nil: the reader sees io.EOF
		done <- err
	}()
	putErr := rn.Storage.Put(ctx, key, pr, "application/zip")
	pr.CloseWithError(errReaderGone)
	writeErr := <-done
	switch {
	case putErr == nil && writeErr == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("studiojob: zip: %w", ctx.Err())
	case writeErr != nil && !errors.Is(writeErr, errReaderGone):
		return fmt.Errorf("studiojob: write zip: %w", writeErr)
	case putErr != nil:
		return fmt.Errorf("studiojob: store zip: %w", putErr)
	default:
		return errors.New("studiojob: store zip: storage returned before reading the whole archive")
	}
}

func (rn *run) writeZip(ctx context.Context, w io.Writer, images []store.SelectedImage) error {
	modified := time.Now().UTC().Truncate(time.Second)
	m := Manifest{
		JobID: rn.job.ID, DomainKey: rn.job.DomainKey, CreatedAt: rn.job.CreatedAt.UTC(), Note: ManifestNote,
		Styles: rn.styles, DefaultStyle: rn.dom.Studio.EffectiveDefaultStyle(),
		Images: make([]ManifestImage, 0, len(images)),
	}
	zw := zip.NewWriter(w)
	for _, im := range images {
		mi := ManifestImage{Index: im.Idx, ImageID: im.ID, Original: im.FileName, ShotID: im.ShotID}
		if im.Studio != nil {
			mi.Status, mi.Reason = im.Studio.Status, im.Studio.Reason
		}
		if im.Studio != nil && im.Studio.Status == store.ImageStudioReady {
			base := im.FileName
			if base == "" {
				base = im.ID + ".jpg"
			}
			for _, style := range rn.styles {
				v, ok := im.Studio.Styles[store.Style(style)]
				if !ok {
					continue
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				data, err := rn.readObject(ctx, v.Key)
				if err != nil {
					return err
				}
				name := style + "/" + FileName(base, style)
				if err := export.WriteStoredEntry(zw, name, data, modified); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				if mi.Files == nil {
					mi.Files = map[string]string{}
				}
				mi.Files[style] = name
			}
		}
		m.Images = append(m.Images, mi)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	fh := &zip.FileHeader{Name: export.ManifestFile, Method: zip.Deflate, Modified: modified}
	fh.SetMode(0o644)
	mw, err := zw.CreateHeader(fh)
	if err != nil {
		return err
	}
	if _, err := mw.Write(append(b, '\n')); err != nil {
		return err
	}
	return zw.Close()
}

func (rn *run) readObject(ctx context.Context, key string) ([]byte, error) {
	rc, err := rn.Storage.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	return b, nil
}
