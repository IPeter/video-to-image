package store

import (
	"context"
	"errors"
	"fmt"

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
		out = append(out, imageFromRow(r))
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
	im := imageFromRow(r)
	return &im, nil
}

func imageFromRow(r sqlcdb.SelectedImage) SelectedImage {
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
	}
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
	}, nil
}
