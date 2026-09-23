package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/store"
	"video-to-image/internal/testutil/pgtest"
)

// newStore returns a Store over a fresh, migrated database.
func newStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.New(t)
	return store.New(pool), pool
}

func testVideoInfo() media.VideoInfo {
	return media.VideoInfo{
		DurationSec:  42.5,
		Width:        1080,
		Height:       1920,
		FPS:          29.97,
		Codec:        "h264",
		Rotation:     -90,
		SizeBytes:    12_345_678,
		MIME:         "video/mp4",
		SHA256:       "ab12",
		OriginalName: "walkaround.mp4",
	}
}

type jobOpt func(*store.NewJob)

func withDomain(d string) jobOpt    { return func(j *store.NewJob) { j.DomainKey = d } }
func withExpiry(t time.Time) jobOpt { return func(j *store.NewJob) { j.ExpiresAt = t } }
func withVideoPath(p string) jobOpt { return func(j *store.NewJob) { j.VideoPath = p } }
func withModel(m string) jobOpt     { return func(j *store.NewJob) { j.ModelName = m } }

func newJobInput(opts ...jobOpt) store.NewJob {
	id := ids.NewJob()
	j := store.NewJob{
		ID:           id,
		DomainKey:    "cars",
		ExpiresAt:    time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond),
		ClientIPHash: "iphash",
		VideoPath:    "cars/jobs/" + id + "/original.mp4",
		VideoInfo:    testVideoInfo(),
		ModelName:    "gemini-3.5-flash-lite",
	}
	for _, o := range opts {
		o(&j)
	}
	return j
}

// createJob creates a queued job and returns its id.
func createJob(t *testing.T, s *store.Store, opts ...jobOpt) string {
	t.Helper()
	j := newJobInput(opts...)
	require.NoError(t, s.CreateJob(context.Background(), j, nil))
	return j.ID
}

// processingJob creates a job and moves it to processing.
func processingJob(t *testing.T, s *store.Store, opts ...jobOpt) string {
	t.Helper()
	id := createJob(t, s, opts...)
	require.NoError(t, s.StartProcessing(context.Background(), id))
	return id
}

func jobStatus(t *testing.T, s *store.Store, id string) store.JobStatus {
	t.Helper()
	j, err := s.GetJob(context.Background(), id)
	require.NoError(t, err)
	return j.Status
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}
