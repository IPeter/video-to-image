package studiojob_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/pipeline/studiojob"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
	"video-to-image/internal/studio"
	"video-to-image/internal/testutil/pgtest"
)

type env struct {
	t       *testing.T
	pool    *pgxpool.Pool
	store   *store.Store
	storage *storage.Local
	reg     *domain.Registry
	scratch string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.New(t)
	st, err := storage.NewLocal(t.TempDir())
	require.NoError(t, err)
	reg, err := domain.LoadDir(filepath.Join("..", "..", "..", "configs", "domains"))
	require.NoError(t, err)
	e := &env{t: t, pool: pool, store: store.New(pool), storage: st, reg: reg, scratch: t.TempDir()}
	// The tests below describe the drawn styles on the five whole-car views; the cars
	// domain now also has the generative style and every exterior shot. Tests of those
	// turn them back on with withDomain.
	e.withDomain(func(c *domain.Config) {
		c.Studio.Views = []string{"front_three_quarter", "side_profile", "rear_three_quarter", "front", "rear"}
		c.Studio.Generative = domain.StudioGenerative{}
	})
	return e
}

// withGenerative turns the generative style on (as cars.yaml ships it).
func (e *env) withGenerative() {
	e.withDomain(func(c *domain.Config) {
		c.Studio.Generative = domain.StudioGenerative{Enabled: true, Pad: 0.25, KeepPad: 0.1,
			InteriorViews: []string{"interior_dashboard", "interior_front_seats", "interior_rear_seats"}}
	})
}

// withDomain replaces the cars domain by a copy changed by mutate.
func (e *env) withDomain(mutate func(*domain.Config)) {
	cars, ok := e.reg.Get("cars")
	require.True(e.t, ok)
	c := *cars
	mutate(&c)
	e.reg = domain.NewRegistry(&c)
}

func (e *env) cars() *domain.Config {
	c, _ := e.reg.Get("cars")
	return c
}

func (e *env) runner(seg studio.Segmenter, an analyzer.Analyzer) *studiojob.Runner {
	r := &studiojob.Runner{
		Store: e.store, Storage: e.storage, Segmenter: seg, Analyzer: an, Registry: e.reg,
		Logger: slog.New(slog.DiscardHandler), ScratchDir: e.scratch, PhotoTimeout: 10 * time.Second,
	}
	studiojob.SetPersistBackoff(r, []time.Duration{time.Millisecond, 2 * time.Millisecond})
	return r
}

// photoSpec is one exported photo of a test job.
type photoSpec struct {
	shot string
	box  *store.NRect // nil: no review box
}

// carBox is a subject box well inside the frame.
func carBox() *store.NRect { return &store.NRect{X0: 0.15, Y0: 0.3, X1: 0.85, Y1: 0.85} }

const photoW, photoH = 800, 600 // 4:3; the thumbnails are 640x480

// testPhoto is a photo with a textured "car" inside carBox on a gradient background.
func testPhoto(seed int) []byte {
	m := image.NewRGBA(image.Rect(0, 0, photoW, photoH))
	for y := range photoH {
		for x := range photoW {
			c := color.RGBA{uint8(40 + x*120/photoW), uint8(90 + y*80/photoH), 60, 255}
			fx, fy := float64(x)/photoW, float64(y)/photoH
			if fx > 0.2 && fx < 0.8 && fy > 0.35 && fy < 0.8 {
				c = color.RGBA{uint8(150 + (x*7+seed*13)%60), uint8(30 + (y*5)%40), uint8(40 + (x+y)%50), 255}
			}
			m.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, &jpeg.Options{Quality: 90}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// completedJob creates a job completed like the pipeline does it: exported photos in
// storage, SubjectBox set, photos that are not eligible skipped, and studio_status
// pending when any photo is eligible.
func (e *env) completedJob(originalName string, specs ...photoSpec) (*store.Job, []store.SelectedImage) {
	e.t.Helper()
	ctx := context.Background()
	id := ids.NewJob()
	require.NoError(e.t, e.store.CreateJob(ctx, store.NewJob{
		ID: id, DomainKey: "cars", ExpiresAt: time.Now().Add(24 * time.Hour),
		VideoInfo: media.VideoInfo{OriginalName: originalName, DurationSec: 30},
	}, nil))
	require.NoError(e.t, e.store.StartProcessing(ctx, id))
	c := store.Completion{Explanation: "ok", ZipPath: storage.ZipKey("cars", id)}
	eligible := 0
	for i, sp := range specs {
		imgID := ids.NewImage()
		key := storage.ImageKey("cars", id, imgID)
		require.NoError(e.t, e.storage.Put(ctx, key, bytes.NewReader(testPhoto(i)), "image/jpeg"))
		im := store.SelectedImage{
			ID: imgID, JobID: id, Idx: i + 1, ShotID: sp.shot, TimestampSec: float64(2 * i),
			Width: photoW, Height: photoH, FileName: export.FileName("cars", id, i+1, sp.shot, float64(2*i)),
			ImagePath: key, ThumbPath: storage.ThumbKey("cars", id, imgID), SubjectBox: sp.box,
		}
		if ok, reason := studiojob.Eligible(e.cars(), im); ok {
			eligible++
		} else {
			im.Studio = &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: reason}
		}
		c.Images = append(c.Images, im)
	}
	if eligible > 0 {
		c.Studio = store.StudioPending
	}
	require.NoError(e.t, e.store.CompleteJob(ctx, id, c))
	return e.job(id), e.images(id)
}

func (e *env) job(id string) *store.Job {
	e.t.Helper()
	j, err := e.store.GetJob(context.Background(), id)
	require.NoError(e.t, err)
	return j
}

func (e *env) images(jobID string) []store.SelectedImage {
	e.t.Helper()
	imgs, err := e.store.ListSelectedImages(context.Background(), jobID)
	require.NoError(e.t, err)
	return imgs
}

func (e *env) keysUnder(prefix string) []string {
	e.t.Helper()
	root := e.storage.Root()
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

func (e *env) read(key string) []byte {
	e.t.Helper()
	rc, err := e.storage.Get(context.Background(), key)
	require.NoError(e.t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(e.t, err)
	return b
}

func (e *env) usageRows(jobID string) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM usage_events WHERE job_id = $1 AND step IN ('studio', 'studio-gen')`, jobID).Scan(&n))
	return n
}

// ---- fakes ----

// qaAnswerOK is a QA answer that finds nothing missing.
const qaAnswerOK = `{"missingParts":[],"backgroundKept":[],"notes":""}`

// qaAnalyzer answers QA calls with answer(n, req) (n counts from 0), recording the
// requests and whether their image files existed during the call.
type qaAnalyzer struct {
	mu      sync.Mutex
	answer  func(n int, r *analyzer.Request) (*analyzer.Response, error)
	reqs    []*analyzer.Request
	filesOK []bool
}

func (a *qaAnalyzer) Name() string                              { return "scripted" }
func (a *qaAnalyzer) ReleaseFile(context.Context, string) error { return nil }

func (a *qaAnalyzer) Generate(ctx context.Context, r *analyzer.Request) (*analyzer.Response, error) {
	a.mu.Lock()
	n := len(a.reqs)
	a.reqs = append(a.reqs, r)
	ok := true
	for _, p := range r.Parts {
		if p.FilePath != "" {
			if _, err := os.Stat(p.FilePath); err != nil {
				ok = false
			}
		}
	}
	a.filesOK = append(a.filesOK, ok)
	answer := a.answer
	a.mu.Unlock()
	if answer == nil {
		return qaText(qaAnswerOK)(n, r)
	}
	return answer(n, r)
}

func (a *qaAnalyzer) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.reqs)
}

// qaText answers with text and 2000 input / 50 output tokens of the requested model.
func qaText(text string) func(int, *analyzer.Request) (*analyzer.Response, error) {
	return func(_ int, r *analyzer.Request) (*analyzer.Response, error) {
		return &analyzer.Response{Text: text, Usage: analyzer.Usage{Model: r.Model, InputTokens: 2000, OutputTokens: 50}}, nil
	}
}

// segmenter wraps a Segmenter (default studio.FakeSegmenter), counting calls; before
// runs first (an error is returned as the call's error) and mutate edits the result.
type segmenter struct {
	inner  studio.Segmenter
	mu     sync.Mutex
	n      int
	before func(n int, ctx context.Context) error
	mutate func(n int, r *studio.MaskResult)
}

func (s *segmenter) Mask(ctx context.Context, jpegData []byte, box studio.Box) (*studio.MaskResult, error) {
	s.mu.Lock()
	n := s.n
	s.n++
	before, mutate := s.before, s.mutate
	s.mu.Unlock()
	if before != nil {
		if err := before(n, ctx); err != nil {
			return nil, err
		}
	}
	inner := s.inner
	if inner == nil {
		inner = studio.FakeSegmenter{}
	}
	res, err := inner.Mask(ctx, jpegData, box)
	if err == nil && mutate != nil {
		mutate(n, res)
	}
	return res, err
}

func (s *segmenter) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// failingStore fails SetImageStudio for the images in failFor (a database outage).
type failingStore struct {
	*store.Store
	mu      sync.Mutex
	failFor map[string]bool
}

var errDBDown = errors.New("db down")

func (f *failingStore) SetImageStudio(ctx context.Context, jobID, imageID string, st store.ImageStudio) error {
	f.mu.Lock()
	fail := f.failFor[imageID]
	f.mu.Unlock()
	if fail {
		return errDBDown
	}
	return f.Store.SetImageStudio(ctx, jobID, imageID, st)
}
