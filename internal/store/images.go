package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"video-to-image/internal/store/sqlcdb"
)

// ListSelectedImages returns the job's images ordered by idx.
func (s *Store) ListSelectedImages(ctx context.Context, jobID string) ([]SelectedImage, error) {
	rows, err := s.q.ListSelectedImages(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: list selected images %s: %w", jobID, err)
	}
	out := make([]SelectedImage, 0, len(rows))
	for _, r := range rows {
		im, err := imageFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("store: list selected images %s: %w", jobID, err)
		}
		out = append(out, im)
	}
	return out, nil
}

// GetSelectedImage returns one image of the job or ErrNotFound.
func (s *Store) GetSelectedImage(ctx context.Context, jobID, imageID string) (*SelectedImage, error) {
	r, err := s.q.GetSelectedImage(ctx, sqlcdb.GetSelectedImageParams{JobID: jobID, ID: imageID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: get selected image %s/%s: %w", jobID, imageID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get selected image %s/%s: %w", jobID, imageID, err)
	}
	im, err := imageFromRow(r)
	if err != nil {
		return nil, fmt.Errorf("store: get selected image %s/%s: %w", jobID, imageID, err)
	}
	return &im, nil
}

func imageFromRow(r sqlcdb.SelectedImage) (SelectedImage, error) {
	crop, err := decodeCrop(r.Crop)
	if err != nil {
		return SelectedImage{}, fmt.Errorf("image %s: crop: %w", r.ID, err)
	}
	return SelectedImage{
		ID:           r.ID,
		JobID:        r.JobID,
		CandidateID:  textOrEmpty(r.CandidateID),
		Idx:          int(r.Idx),
		ShotID:       r.ShotID,
		TimestampSec: f64(r.TimestampSec),
		Width:        int(r.Width),
		Height:       int(r.Height),
		Reason:       r.Reason,
		FileName:     r.FileName,
		ImagePath:    textOrEmpty(r.ImagePath),
		ThumbPath:    textOrEmpty(r.ThumbPath),
		Bytes:        int64(r.Bytes),
		Rotation:     int(r.Rotation),
		Crop:         crop,
	}, nil
}

// decodeCrop decodes selected_images.crop; NULL (and JSON null) → nil.
func decodeCrop(b []byte) (*NRect, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var r *NRect
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return r, nil
}

// encodeCrop encodes a crop for the jsonb column; nil → NULL. The rectangle must be
// normalised: finite, inside [0,1] and with X0 < X1 and Y0 < Y1.
func encodeCrop(r *NRect) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	for _, v := range []float64{r.X0, r.Y0, r.X1, r.Y1} {
		if math.IsNaN(v) || v < 0 || v > 1 {
			return nil, fmt.Errorf("crop %+v is not normalised to [0,1]", *r)
		}
	}
	if r.X0 >= r.X1 || r.Y0 >= r.Y1 {
		return nil, fmt.Errorf("crop %+v is empty", *r)
	}
	return jsonb(r)
}

// validRotation reports whether deg is a quarter turn clockwise (0, 90, 180, 270).
func validRotation(deg int) bool {
	switch deg {
	case 0, 90, 180, 270:
		return true
	}
	return false
}

// imageParams validates im for insertion under jobID.
func imageParams(jobID string, im SelectedImage) (sqlcdb.InsertSelectedImagesParams, error) {
	var p sqlcdb.InsertSelectedImagesParams
	switch {
	case im.ID == "":
		return p, errors.New("image with empty id")
	case im.JobID != "" && im.JobID != jobID:
		return p, fmt.Errorf("image %s: %w (%s)", im.ID, errForeignJob, im.JobID)
	case im.Idx < 1:
		return p, fmt.Errorf("image %s: idx %d is not 1-based", im.ID, im.Idx)
	}
	idx, err := int32Of("idx", int64(im.Idx))
	if err != nil {
		return p, fmt.Errorf("image %s: %w", im.ID, err)
	}
	w, err := int32Of("width", int64(im.Width))
	if err != nil {
		return p, fmt.Errorf("image %s: %w", im.ID, err)
	}
	h, err := int32Of("height", int64(im.Height))
	if err != nil {
		return p, fmt.Errorf("image %s: %w", im.ID, err)
	}
	size, err := int32Of("bytes", im.Bytes)
	if err != nil {
		return p, fmt.Errorf("image %s: %w", im.ID, err)
	}
	if !validRotation(im.Rotation) {
		return p, fmt.Errorf("image %s: rotation %d is not 0, 90, 180 or 270", im.ID, im.Rotation)
	}
	crop, err := encodeCrop(im.Crop)
	if err != nil {
		return p, fmt.Errorf("image %s: %w", im.ID, err)
	}
	return sqlcdb.InsertSelectedImagesParams{
		ID:           im.ID,
		JobID:        jobID,
		CandidateID:  nullText(im.CandidateID),
		Idx:          idx,
		ShotID:       im.ShotID,
		TimestampSec: f32(im.TimestampSec),
		Width:        w,
		Height:       h,
		Reason:       pgText(im.Reason),
		FileName:     im.FileName,
		ImagePath:    nullText(im.ImagePath),
		ThumbPath:    nullText(im.ThumbPath),
		Bytes:        size,
		Rotation:     int32(im.Rotation),
		Crop:         crop,
	}, nil
}
