package pipeline_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/analyzer/fake"
	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/pipeline"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
	"video-to-image/internal/testutil/pgtest"
)

// ---- fixtures ----

var (
	videoOnce sync.Once
	videoPath string
	videoErr  error
)

// sharedVideo renders a 12 s 1280x720 synthetic clip once per test binary.
func sharedVideo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	videoOnce.Do(func() {
		dir, err := os.MkdirTemp("", "v2i-runner-test")
		if err != nil {
			videoErr = err
			return
		}
		videoPath = filepath.Join(dir, "walkaround.mp4")
		// testsrc2 plus a moving "car body" (same recipe as scripts/gen-fixtures.sh), so
		// frames differ over time and exposure stays normal.
		out, err := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=12",
			"-f", "lavfi", "-i", "color=c=0xB8BCC2:size=520x200:rate=30:duration=12",
			"-filter_complex", "[0:v]hue=h='6*t'[bg];[bg][1:v]overlay=x='390+330*sin(2*PI*t/12)':y='380+12*sin(2*PI*t/4)':shortest=1,format=yuv420p",
			"-t", "12", "-c:v", "libx264", "-preset", "veryfast", videoPath).CombinedOutput()
		if err != nil {
			videoErr = errors.New(string(out))
		}
	})
	require.NoError(t, videoErr)
	return videoPath
}

type env struct {
	t       *testing.T
	store   *store.Store
	storage storage.Storage
	local   *storage.Local // the disk under storage (storage may be wrapped)
	ff      *media.FFmpeg
	reg     *domain.Registry
	scratch string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.New(t)
	st, err := storage.NewLocal(t.TempDir())
	require.NoError(t, err)
	reg, err := domain.LoadDir(filepath.Join("..", "..", "configs", "domains"))
	require.NoError(t, err)
	return &env{
		t: t, store: store.New(pool), storage: st, local: st,
		ff: media.New("", "", slog.Default()), reg: reg, scratch: t.TempDir(),
	}
}

// createJob stores the video as an uploaded original and inserts a queued job.
func (e *env) createJob(originalName string) *store.Job {
	e.t.Helper()
	return e.createJobInfo(originalName, nil)
}

// createJobInfo is createJob with the probed VideoInfo adjusted by mutate (e.g. a
// container header that lies about the duration).
func (e *env) createJobInfo(originalName string, mutate func(*media.VideoInfo)) *store.Job {
	e.t.Helper()
	ctx := context.Background()
	src := sharedVideo(e.t)
	info, err := e.ff.Probe(ctx, src)
	require.NoError(e.t, err)
	info.MIME = "video/mp4"
	info.SHA256 = "testsha-" + originalName
	info.OriginalName = originalName
	if mutate != nil {
		mutate(&info)
	}

	id := ids.NewJob()
	key := storage.OriginalKey("cars", id, "mp4")
	f, err := os.Open(src)
	require.NoError(e.t, err)
	defer f.Close()
	require.NoError(e.t, e.storage.Put(ctx, key, f, "video/mp4"))
	require.NoError(e.t, e.store.CreateJob(ctx, store.NewJob{
		ID: id, DomainKey: "cars", ExpiresAt: time.Now().Add(24 * time.Hour),
		VideoPath: key, VideoInfo: info, ModelName: "gemini-3.5-flash-lite",
	}, func(context.Context, pgx.Tx) error { return nil }))
	j, err := e.store.GetJob(ctx, id)
	require.NoError(e.t, err)
	return j
}

func (e *env) runner(a analyzer.Analyzer) *pipeline.Runner {
	return e.runnerWith(e.store, a)
}

// runnerWith uses st as the runner's store (e.g. a wrapper injecting failures).
func (e *env) runnerWith(st pipeline.Store, a analyzer.Analyzer) *pipeline.Runner {
	r := &pipeline.Runner{
		Store: st, Storage: e.storage, FFmpeg: e.ff, Analyzer: a,
		Registry: e.reg, ScratchDir: e.scratch, Logger: slog.Default(),
	}
	pipeline.SetPersistBackoff(r, []time.Duration{time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond})
	return r
}

func (e *env) steps(jobID string) map[string]store.StepStatus {
	e.t.Helper()
	steps, err := e.store.ListSteps(context.Background(), jobID)
	require.NoError(e.t, err)
	m := map[string]store.StepStatus{}
	for _, s := range steps {
		m[s.Name] = s.Status
	}
	return m
}

func (e *env) keysUnder(prefix string) []string {
	e.t.Helper()
	root := e.local.Root()
	var keys []string
	_ = filepath.Walk(filepath.Join(root, filepath.FromSlash(prefix)), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			keys = append(keys, filepath.ToSlash(rel))
		}
		return nil
	})
	return keys
}

func (e *env) readObject(key string) []byte {
	e.t.Helper()
	rc, err := e.storage.Get(context.Background(), key)
	require.NoError(e.t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(e.t, err)
	return b
}

func readManifest(t *testing.T, b []byte) *export.Manifest {
	t.Helper()
	var m export.Manifest
	require.NoError(t, json.Unmarshal(b, &m))
	return &m
}

// scripted is an analyzer returning canned answers in order, counting calls.
type scripted struct {
	answers []func(*analyzer.Request) (*analyzer.Response, error)
	calls   atomic.Int32
	onCall  func()
}

func (s *scripted) Name() string                              { return "scripted" }
func (s *scripted) ReleaseFile(context.Context, string) error { return nil }
func (s *scripted) Generate(_ context.Context, r *analyzer.Request) (*analyzer.Response, error) {
	n := int(s.calls.Add(1)) - 1
	if s.onCall != nil {
		s.onCall()
	}
	if n >= len(s.answers) {
		return nil, errors.New("scripted: no more answers")
	}
	return s.answers[n](r)
}

func text(t string) func(*analyzer.Request) (*analyzer.Response, error) {
	return func(r *analyzer.Request) (*analyzer.Response, error) {
		return &analyzer.Response{Text: t, Usage: analyzer.Usage{Model: r.Model, InputTokens: 1000, OutputTokens: 100}}, nil
	}
}

func newFake(t *testing.T) analyzer.Analyzer {
	t.Helper()
	a, err := fake.New(fake.Options{})
	require.NoError(t, err)
	return a
}

// ---- tests ----

func TestRunCompletes(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()

	require.NoError(t, e.runner(newFake(t)).Run(ctx, job.ID))

	got, err := e.store.GetJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	require.NotNil(t, got.Gate)
	assert.True(t, got.Gate.Passed)
	assert.NotEmpty(t, got.ZipPath)
	assert.NotEmpty(t, got.ResultExplanation)
	assert.Zero(t, got.CostUSD, "the fake analyzer is free")

	imgs, err := e.store.ListSelectedImages(ctx, job.ID)
	require.NoError(t, err)
	require.NotEmpty(t, imgs)
	assert.LessOrEqual(t, len(imgs), 8)
	for i, im := range imgs {
		assert.Equal(t, i+1, im.Idx)
		assert.Equal(t, 1280, im.Width)
		assert.Equal(t, 720, im.Height)
		_, err := e.storage.Head(ctx, im.ImagePath)
		assert.NoError(t, err)
		_, err = e.storage.Head(ctx, im.ThumbPath)
		assert.NoError(t, err)
		assert.True(t, strings.HasPrefix(im.FileName, "cars_"), im.FileName)
	}
	_, err = e.storage.Head(ctx, got.ZipPath)
	assert.NoError(t, err)

	// manifest.json (in storage and inside the ZIP) carries the model's summary, which
	// is only known after the job row was loaded.
	require.NotEmpty(t, got.VideoSummary)
	stored := readManifest(t, e.readObject(storage.ManifestKey("cars", job.ID)))
	assert.Equal(t, got.VideoSummary, stored.Summary)
	zipBytes := e.readObject(got.ZipPath)
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	require.NoError(t, err)
	var inZip *export.Manifest
	for _, f := range zr.File {
		if f.Name == "manifest.json" {
			rc, err := f.Open()
			require.NoError(t, err)
			b, err := io.ReadAll(rc)
			require.NoError(t, err)
			require.NoError(t, rc.Close())
			inZip = readManifest(t, b)
		}
	}
	require.NotNil(t, inZip, "export.zip holds manifest.json")
	assert.Equal(t, got.VideoSummary, inZip.Summary)
	assert.Equal(t, got.ResultExplanation, inZip.Explanation)
	assert.Len(t, inZip.Images, len(imgs))

	_, err = e.storage.Head(ctx, storage.PreviewKey("cars", job.ID))
	assert.ErrorIs(t, err, storage.ErrNotFound, "preview is deleted after analyze")

	for name, status := range e.steps(job.ID) {
		assert.Equal(t, store.StepDone, status, name)
	}
	cands, err := e.store.ListCandidates(ctx, job.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, cands)
	selected := 0
	for _, c := range cands {
		if c.Selected {
			selected++
		}
	}
	assert.Equal(t, len(imgs), selected)

	entries, _ := os.ReadDir(filepath.Join(e.scratch, "jobs"))
	assert.Empty(t, entries, "scratch dir is removed")

	// Re-running a terminal job is a no-op.
	require.NoError(t, e.runner(&scripted{}).Run(ctx, job.ID))
}

func TestRunRejectedByGate(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("reject-this-kitchen.mp4")
	ctx := context.Background()

	require.NoError(t, e.runner(newFake(t)).Run(ctx, job.ID))

	got, err := e.store.GetJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusRejected, got.Status)
	require.NotNil(t, got.Gate)
	assert.False(t, got.Gate.Passed)
	assert.NotEmpty(t, got.Gate.Reason)
	assert.Empty(t, got.VideoPath)
	assert.Empty(t, e.keysUnder(storage.JobPrefix("cars", job.ID)), "rejected jobs keep no files")
	st := e.steps(job.ID)
	assert.Equal(t, store.StepDone, st["analyze"])
	assert.Equal(t, store.StepSkipped, st["extract"])
	assert.Equal(t, store.StepSkipped, st["export"])
}

func TestRunLowConfidenceGateRejects(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	a := &scripted{answers: []func(*analyzer.Request) (*analyzer.Response, error){
		text(`{"gate":{"passed":true,"confidence":0.4,"reason":"blurry, maybe a car","detectedSubject":"car?"},"videoSummary":"unclear","scenes":[{"start":"00:01","end":"00:03","shotId":"side_profile","reason":"r","relevance":0.9}]}`),
	}}
	require.NoError(t, e.runner(a).Run(context.Background(), job.ID))
	got, _ := e.store.GetJob(context.Background(), job.ID)
	assert.Equal(t, store.StatusRejected, got.Status)
}

func TestRunNoScenes(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	a := &scripted{answers: []func(*analyzer.Request) (*analyzer.Response, error){
		text(`{"gate":{"passed":true,"confidence":0.9,"reason":"a car","detectedSubject":"car"},"videoSummary":"A car far away.","scenes":[]}`),
	}}
	ctx := context.Background()
	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusCompleted, got.Status)
	assert.Equal(t, pipeline.NoScenesExplanation, got.ResultExplanation)
	assert.Empty(t, got.ZipPath)
	assert.Contains(t, got.MissingShots, "front_three_quarter")
	imgs, _ := e.store.ListSelectedImages(ctx, job.ID)
	assert.Empty(t, imgs)
}

func TestRunBadOutputFailsAfterNudge(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	a := &scripted{answers: []func(*analyzer.Request) (*analyzer.Response, error){text("not json"), text("still {not json")}}
	ctx := context.Background()
	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusFailed, got.Status)
	assert.Equal(t, "MODEL_BAD_OUTPUT", got.ErrorCode)
	assert.NotContains(t, got.ErrorMessage, "json", "user message stays generic")
	assert.Contains(t, got.InternalLog, "still {not json")
	assert.Equal(t, int32(2), a.calls.Load())
	assert.Empty(t, e.keysUnder(storage.JobPrefix("cars", job.ID)), "failed jobs keep no files")
}

func TestRunModelUnavailable(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	a := &scripted{answers: []func(*analyzer.Request) (*analyzer.Response, error){
		func(*analyzer.Request) (*analyzer.Response, error) { return nil, analyzer.ErrUnavailable },
	}}
	ctx := context.Background()
	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusFailed, got.Status)
	assert.Equal(t, "MODEL_UNAVAILABLE", got.ErrorCode)
	st := e.steps(job.ID)
	assert.Equal(t, store.StepFailed, st["analyze"])
	assert.Equal(t, store.StepSkipped, st["export"])
}

// flakyStorage fails the first Put of export.zip, like a storage hiccup.
type flakyStorage struct {
	storage.Storage
	failed atomic.Bool
}

func (f *flakyStorage) Put(ctx context.Context, key string, r io.Reader, ct string) error {
	if strings.HasSuffix(key, "/export.zip") && f.failed.CompareAndSwap(false, true) {
		_, _ = io.Copy(io.Discard, r)
		return errors.New("storage hiccup")
	}
	return f.Storage.Put(ctx, key, r, ct)
}

func TestRunResumesWithoutPayingTwice(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	e.storage = &flakyStorage{Storage: e.storage}

	first := &countingAnalyzer{Analyzer: newFake(t)}
	err := e.runner(first).Run(ctx, job.ID)
	require.Error(t, err, "infrastructure error is returned so River retries")
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusProcessing, got.Status)
	assert.Equal(t, store.StepDone, e.steps(job.ID)["analyze"])
	assert.Equal(t, int32(3), first.calls.Load(), "analyze + frame review (orientation, judgement)")

	second := &countingAnalyzer{Analyzer: newFake(t)}
	require.NoError(t, e.runner(second).Run(ctx, job.ID))
	assert.Zero(t, second.calls.Load(), "neither analyze nor the review is paid again on retry")
	got, _ = e.store.GetJob(ctx, job.ID)
	assert.Equal(t, store.StatusCompleted, got.Status)
}

type countingAnalyzer struct {
	analyzer.Analyzer
	calls        atomic.Int32 // every model call
	analyzeCalls atomic.Int32 // the (video) analyze step only
}

func (c *countingAnalyzer) Generate(ctx context.Context, r *analyzer.Request) (*analyzer.Response, error) {
	c.calls.Add(1)
	if r.Step == analyzer.StepAnalyze {
		c.analyzeCalls.Add(1)
	}
	return c.Analyzer.Generate(ctx, r)
}

func TestRunStopsWhenUserDeletesJob(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	inner := newFake(t)
	a := &scripted{onCall: func() {
		// The user presses "Delete now" while the model is thinking.
		require.NoError(t, e.store.ExpireJob(ctx, job.ID))
	}, answers: []func(*analyzer.Request) (*analyzer.Response, error){
		func(r *analyzer.Request) (*analyzer.Response, error) { return inner.Generate(ctx, r) },
	}}
	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	got, _ := e.store.GetJob(ctx, job.ID)
	assert.Equal(t, store.StatusExpired, got.Status)
	assert.Empty(t, e.keysUnder(storage.JobPrefix("cars", job.ID)))
}
