package app

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer/fake"
	"video-to-image/internal/config"
	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
	"video-to-image/internal/testutil/pgtest"
)

// The worker NewWorker builds (work, serve --embed-worker) with V2I_STUDIO_URL=fake
// completes a job, then makes its studio backgrounds in the studio queue: the whole
// flow with the fake analyzer and the fake segmenter.
func TestWorkerMakesStudioBackgroundsAfterTheJob(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	pool := pgtest.New(t)
	ctx := context.Background()
	video := filepath.Join(t.TempDir(), "walkaround.mp4")
	out, err := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=12",
		"-f", "lavfi", "-i", "color=c=0xB8BCC2:size=520x200:rate=30:duration=12",
		"-filter_complex", "[0:v]hue=h='6*t'[bg];[bg][1:v]overlay=x='390+330*sin(2*PI*t/12)':y='380+12*sin(2*PI*t/4)':shortest=1,format=yuv420p",
		"-t", "12", "-c:v", "libx264", "-preset", "veryfast", video).CombinedOutput()
	require.NoError(t, err, "%s", out)

	log := slog.New(slog.DiscardHandler)
	st, err := storage.NewLocal(t.TempDir())
	require.NoError(t, err)
	reg, err := domain.LoadDir(filepath.Join("..", "..", "configs", "domains"))
	require.NoError(t, err)
	a := &App{
		Cfg: &config.Config{
			StudioURL: config.StudioFake, StudioTimeout: 30 * time.Second, StudioWorkers: 1,
			PipelineWorkers: 1, ScratchDir: t.TempDir(), DailyCostCeilingUSD: 5,
		},
		Log: log, Pool: pool, Store: store.New(pool), Storage: st,
		FFmpeg: media.New("", "", log), Registry: reg,
	}
	an, err := fake.New(fake.Options{})
	require.NoError(t, err)
	w, err := a.NewWorker(an)
	require.NoError(t, err)
	require.NoError(t, w.Start(ctx))
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		assert.NoError(t, w.Stop(sctx))
	})

	info, err := a.FFmpeg.Probe(ctx, video)
	require.NoError(t, err)
	info.MIME, info.SHA256, info.OriginalName = "video/mp4", "sha-app-studio", "walkaround.mp4"
	id := ids.NewJob()
	key := storage.OriginalKey("cars", id, "mp4")
	f, err := os.Open(video)
	require.NoError(t, err)
	require.NoError(t, st.Put(ctx, key, f, "video/mp4"))
	require.NoError(t, f.Close())
	require.NoError(t, a.Store.CreateJob(ctx, store.NewJob{
		ID: id, DomainKey: "cars", ExpiresAt: time.Now().Add(time.Hour), VideoPath: key, VideoInfo: info,
		ModelName: "gemini-3.5-flash-lite",
	}, func(ctx context.Context, tx pgx.Tx) error { return w.Client().EnqueuePipelineTx(ctx, tx, id) }))

	var job *store.Job
	require.Eventually(t, func() bool {
		job, err = a.Store.GetJob(ctx, id)
		require.NoError(t, err)
		return job.Status.Terminal() && !job.StudioStatus.Active()
	}, 3*time.Minute, 200*time.Millisecond, "the job and its studio work finish")
	require.Equal(t, store.StatusCompleted, job.Status, job.InternalLog)
	assert.Equal(t, store.StudioDone, job.StudioStatus)
	require.NotEmpty(t, job.StudioZipPath)
	_, err = st.Head(ctx, job.StudioZipPath)
	require.NoError(t, err)

	imgs, err := a.Store.ListSelectedImages(ctx, id)
	require.NoError(t, err)
	ready := 0
	for _, im := range imgs {
		require.NotNil(t, im.Studio, "%s has an outcome", im.ShotID)
		if im.Studio.Status != store.ImageStudioReady {
			continue
		}
		ready++
		assert.Len(t, im.Studio.Styles, 4, "the generative style and the three drawn ones: %s", im.Studio.Diagnostics)
		assert.Contains(t, im.Studio.Styles, store.Style("studio"))
		for _, v := range im.Studio.Styles {
			_, err := st.Head(ctx, v.Key)
			assert.NoError(t, err)
		}
	}
	assert.Positive(t, ready)

	// River records the finished job right after Work returns.
	require.Eventually(t, func() bool {
		var state string
		err := pool.QueryRow(ctx, `SELECT state::text FROM river_job WHERE kind = 'studio' AND args->>'jobId' = $1`, id).Scan(&state)
		return err == nil && state == "completed"
	}, 30*time.Second, 100*time.Millisecond, "the River studio job completes")
}
