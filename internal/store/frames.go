package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"video-to-image/internal/store/sqlcdb"
)

// ReplaceScenes deletes the job's scenes (cascading to candidate_frames) and inserts these.
// All of the job's candidate_frames are deleted, including ones without a scene: they
// were derived from the old scenes. Scene.JobID may be empty (jobID is used). Missing
// job → ErrNotFound (when scenes is non-empty).
func (s *Store) ReplaceScenes(ctx context.Context, jobID string, scenes []Scene) error {
	if err := s.replaceScenes(ctx, jobID, scenes); err != nil {
		return wrapJobErr("replace scenes", jobID, err)
	}
	return nil
}

func (s *Store) replaceScenes(ctx context.Context, jobID string, scenes []Scene) error {
	rows, err := sceneRows(jobID, scenes)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *sqlcdb.Queries) error {
		return replaceSceneRows(ctx, q, jobID, rows)
	})
}

// sceneRows validates scenes and converts them to insert parameters for jobID.
func sceneRows(jobID string, scenes []Scene) ([]sqlcdb.InsertScenesParams, error) {
	rows := make([]sqlcdb.InsertScenesParams, 0, len(scenes))
	for _, sc := range scenes {
		if sc.JobID != "" && sc.JobID != jobID {
			return nil, fmt.Errorf("scene %d: %w (%s)", sc.Idx, errForeignJob, sc.JobID)
		}
		idx, err := int32Of("idx", int64(sc.Idx))
		if err != nil {
			return nil, fmt.Errorf("scene %d: %w", sc.Idx, err)
		}
		rows = append(rows, sqlcdb.InsertScenesParams{
			JobID:     jobID,
			Idx:       idx,
			StartSec:  f32(sc.StartSec),
			EndSec:    f32(sc.EndSec),
			ShotID:    sc.ShotID,
			Relevance: f32(sc.Relevance),
			Reason:    pgText(sc.Reason),
			RawStart:  pgText(sc.RawStart),
			RawEnd:    pgText(sc.RawEnd),
		})
	}
	return rows, nil
}

// replaceSceneRows deletes the job's candidates and scenes and inserts rows, inside
// the caller's transaction.
func replaceSceneRows(ctx context.Context, q *sqlcdb.Queries, jobID string, rows []sqlcdb.InsertScenesParams) error {
	if _, err := q.DeleteCandidates(ctx, jobID); err != nil {
		return fmt.Errorf("delete candidates: %w", err)
	}
	if _, err := q.DeleteScenes(ctx, jobID); err != nil {
		return fmt.Errorf("delete scenes: %w", err)
	}
	if len(rows) > 0 {
		if _, err := q.InsertScenes(ctx, rows); err != nil {
			return fmt.Errorf("insert scenes: %w", err)
		}
	}
	return nil
}

// ListScenes returns the job's scenes ordered by idx.
func (s *Store) ListScenes(ctx context.Context, jobID string) ([]Scene, error) {
	rows, err := s.q.ListScenes(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: list scenes %s: %w", jobID, err)
	}
	out := make([]Scene, 0, len(rows))
	for _, r := range rows {
		out = append(out, Scene{
			ID:        r.ID,
			JobID:     r.JobID,
			Idx:       int(r.Idx),
			StartSec:  f64(r.StartSec),
			EndSec:    f64(r.EndSec),
			ShotID:    r.ShotID,
			Relevance: f64(r.Relevance),
			Reason:    r.Reason,
			RawStart:  r.RawStart,
			RawEnd:    r.RawEnd,
		})
	}
	return out, nil
}

// ReplaceCandidates deletes the job's selected_images and candidate_frames and inserts
// cands in one transaction (bulk insert; ~300 rows). duplicate_of may reference a
// candidate later in the same batch (the FK is deferred).
//
// SceneIdx is resolved to scene_id through the job's current scenes; SceneIdx < 0, or
// an idx the job has no scene for, stores NULL (the frame is kept for evaluation
// rather than failing the job). DHash is stored bit-for-bit as bigint. A duplicate_of
// that names no candidate fails the commit. Missing job → ErrNotFound (when cands is
// non-empty).
func (s *Store) ReplaceCandidates(ctx context.Context, jobID string, cands []CandidateFrame) error {
	if err := s.replaceCandidates(ctx, jobID, cands); err != nil {
		return wrapJobErr("replace candidates", jobID, err)
	}
	return nil
}

func (s *Store) replaceCandidates(ctx context.Context, jobID string, cands []CandidateFrame) error {
	for _, c := range cands {
		switch {
		case c.ID == "":
			return errors.New("candidate with empty id")
		case c.JobID != "" && c.JobID != jobID:
			return fmt.Errorf("candidate %s: %w (%s)", c.ID, errForeignJob, c.JobID)
		}
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *sqlcdb.Queries) error {
		if _, err := q.DeleteSelectedImages(ctx, jobID); err != nil {
			return fmt.Errorf("delete images: %w", err)
		}
		if _, err := q.DeleteCandidates(ctx, jobID); err != nil {
			return fmt.Errorf("delete candidates: %w", err)
		}
		if len(cands) == 0 {
			return nil
		}
		sceneRows, err := q.SceneIDsByIdx(ctx, jobID)
		if err != nil {
			return fmt.Errorf("scene ids: %w", err)
		}
		sceneIDs := make(map[int]int64, len(sceneRows))
		for _, r := range sceneRows {
			sceneIDs[int(r.Idx)] = r.ID
		}
		rows := make([]sqlcdb.InsertCandidatesParams, 0, len(cands))
		for _, c := range cands {
			var sceneID *int64
			if id, ok := sceneIDs[c.SceneIdx]; ok && c.SceneIdx >= 0 {
				sceneID = &id
			}
			rows = append(rows, sqlcdb.InsertCandidatesParams{
				ID:              c.ID,
				JobID:           jobID,
				SceneID:         sceneID,
				ShotID:          c.ShotID,
				TimestampSec:    f32(c.TimestampSec),
				Sharpness:       f32(c.Sharpness),
				SharpnessZ:      f32(c.SharpnessZ),
				ExposurePenalty: f32(c.ExposurePenalty),
				Dhash:           int64(c.DHash),
				DuplicateOf:     nullText(c.DuplicateOf),
				RejectedReason:  c.RejectedReason,
				Score:           f32(c.Score),
				Selected:        c.Selected,
			})
		}
		if _, err := q.InsertCandidates(ctx, rows); err != nil {
			return fmt.Errorf("insert candidates: %w", err)
		}
		return nil
	})
}

// ListCandidates returns the job's candidates ordered by timestamp.
// Ties are broken by id; SceneIdx is the linked scene's idx, or -1 when scene_id is NULL.
func (s *Store) ListCandidates(ctx context.Context, jobID string) ([]CandidateFrame, error) {
	rows, err := s.q.ListCandidates(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: list candidates %s: %w", jobID, err)
	}
	out := make([]CandidateFrame, 0, len(rows))
	for _, r := range rows {
		c := r.CandidateFrame
		sceneIdx := -1
		if r.SceneIdx != nil {
			sceneIdx = int(*r.SceneIdx)
		}
		out = append(out, CandidateFrame{
			ID:              c.ID,
			JobID:           c.JobID,
			SceneIdx:        sceneIdx,
			SceneID:         c.SceneID,
			ShotID:          c.ShotID,
			TimestampSec:    f64(c.TimestampSec),
			Sharpness:       f64(c.Sharpness),
			SharpnessZ:      f64(c.SharpnessZ),
			ExposurePenalty: f64(c.ExposurePenalty),
			DHash:           uint64(c.Dhash),
			DuplicateOf:     textOrEmpty(c.DuplicateOf),
			RejectedReason:  c.RejectedReason,
			Score:           f64(c.Score),
			Selected:        c.Selected,
		})
	}
	return out, nil
}
