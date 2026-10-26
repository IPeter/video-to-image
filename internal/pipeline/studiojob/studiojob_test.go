package studiojob_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/analyzer/fake"
	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/studiojob"
	"video-to-image/internal/pricing"
	"video-to-image/internal/queue"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
	"video-to-image/internal/studio"
)

var styles = []string{"grey", "light", "dark"}

func TestRunHappyPath(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	job, imgs := e.completedJob("walkaround.mp4",
		photoSpec{shot: "front_three_quarter", box: carBox()},
		photoSpec{shot: "interior_dashboard", box: carBox()}, // not an exterior view
		photoSpec{shot: "side_profile", box: carBox()},
	)
	require.Equal(t, store.StudioPending, job.StudioStatus)
	an := &qaAnalyzer{}
	seg := &segmenter{}

	require.NoError(t, e.runner(seg, an).Run(ctx, job.ID))

	got := e.job(job.ID)
	assert.Equal(t, store.StatusCompleted, got.Status, "the job's own status is never changed")
	assert.Equal(t, store.StudioDone, got.StudioStatus)
	assert.Equal(t, studiojob.ZipKey("cars", job.ID), got.StudioZipPath)
	assert.Equal(t, 2, seg.calls())
	require.Equal(t, 2, an.calls())

	after := e.images(job.ID)
	for i, im := range after {
		assert.Equal(t, imgs[i].ImagePath, im.ImagePath, "the original is untouched")
		assert.Equal(t, e.read(imgs[i].ImagePath), testPhoto(i), "the original's bytes are untouched")
	}
	assert.Equal(t, store.ImageStudioSkipped, after[1].Studio.Status)
	for _, im := range []store.SelectedImage{after[0], after[2]} {
		st := im.Studio
		require.NotNil(t, st, im.ShotID)
		require.Equal(t, store.ImageStudioReady, st.Status, "%s: %s", im.ShotID, st.Diagnostics)
		assert.Empty(t, st.Reason)
		require.Len(t, st.Styles, 3)
		for _, style := range styles {
			v := st.Styles[store.Style(style)]
			assert.Equal(t, studiojob.ImageKey("cars", job.ID, style, im.ID), v.Key)
			assert.Equal(t, studiojob.ThumbKey("cars", job.ID, style, im.ID), v.ThumbKey)
			assert.Equal(t, photoW, v.Width)
			assert.Equal(t, photoH, v.Height)
			full := e.read(v.Key)
			assert.Equal(t, int64(len(full)), v.Bytes)
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(full))
			require.NoError(t, err)
			assert.Equal(t, image.Pt(photoW, photoH), image.Pt(cfg.Width, cfg.Height), "same size and framing")
			cfg, err = jpeg.DecodeConfig(bytes.NewReader(e.read(v.ThumbKey)))
			require.NoError(t, err)
			assert.Equal(t, image.Pt(640, 480), image.Pt(cfg.Width, cfg.Height))
		}
		var diag struct {
			Stats   *studio.MaskStats `json:"stats"`
			BoxGate struct{ OK bool } `json:"boxGate"`
			QA      struct {
				Model string
				OK    bool
				Calls int
			} `json:"qa"`
			TimingsMS map[string]int64 `json:"timingsMs"`
		}
		require.NoError(t, json.Unmarshal(st.Diagnostics, &diag))
		require.NotNil(t, diag.Stats)
		assert.Equal(t, 1.0, diag.Stats.IoU)
		assert.True(t, diag.BoxGate.OK)
		assert.Equal(t, "gemini-3.8-flash", diag.QA.Model)
		assert.True(t, diag.QA.OK)
		assert.Equal(t, 1, diag.QA.Calls)
		for _, k := range []string{"mask", "render", "qa"} {
			assert.Contains(t, diag.TimingsMS, k)
		}
	}

	// Car pixels are the original's (up to the one JPEG re-encode); the background is new.
	orig, err := jpeg.Decode(bytes.NewReader(testPhoto(0)))
	require.NoError(t, err)
	grey, err := jpeg.Decode(bytes.NewReader(e.read(after[0].Studio.Styles["grey"].Key)))
	require.NoError(t, err)
	assert.Less(t, pixelDiff(orig, grey, photoW/2, photoH*3/5), 12, "a car pixel keeps its colour")
	assert.Greater(t, pixelDiff(orig, grey, 10, 10), 30, "a background pixel is the backdrop")

	// The QA calls: step studio, both images as files that existed during the call, the
	// upload's name as the fake analyzer's hint.
	for i, r := range an.reqs {
		assert.Equal(t, analyzer.StepStudio, r.Step)
		assert.Equal(t, "gemini-3.8-flash", r.Model)
		assert.Equal(t, job.ID, r.JobID)
		assert.Equal(t, "walkaround.mp4", r.SourceName)
		assert.NotEmpty(t, r.CacheKey)
		assert.True(t, an.filesOK[i], "QA image files exist while the model is called")
	}
	entries, err := os.ReadDir(filepath.Join(e.scratch, "jobs"))
	require.NoError(t, err)
	assert.Empty(t, entries, "the QA scratch files are removed")

	// Usage: one row per QA call, counted in the job's cost.
	assert.Equal(t, 2, e.usageRows(job.ID))
	perCall, known := pricing.Cost("gemini-3.8-flash", 2000, 50, 0)
	require.True(t, known)
	assert.InDelta(t, 2*perCall, got.CostUSD, 1e-5, "each event is stored with 6 decimals")
	usage, err := e.store.JobUsage(ctx, job.ID)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, store.UsageByStep{Step: "studio", Model: "gemini-3.8-flash", Calls: 2, InputTokens: 4000, OutputTokens: 100, CostUSD: usage[0].CostUSD}, usage[0])

	// studio.zip: a folder per style, manifest.json with every photo's outcome.
	zipBytes := e.read(got.StudioZipPath)
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	require.NoError(t, err)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	var want []string
	for _, im := range []store.SelectedImage{after[0], after[2]} {
		for _, style := range styles {
			want = append(want, style+"/"+strings.TrimSuffix(im.FileName, ".jpg")+"_studio-"+style+".jpg")
		}
	}
	want = append(want, "manifest.json")
	assert.Equal(t, want, names)
	for _, f := range zr.File {
		b := readZipEntry(t, f)
		if f.Name == "manifest.json" {
			var m studiojob.Manifest
			require.NoError(t, json.Unmarshal(b, &m))
			assert.Contains(t, m.Note, "car pixels are unchanged")
			assert.Equal(t, styles, m.Styles)
			assert.Equal(t, "grey", m.DefaultStyle)
			require.Len(t, m.Images, 3)
			assert.Equal(t, "ready", m.Images[0].Status)
			assert.Equal(t, "skipped", m.Images[1].Status)
			assert.Equal(t, studiojob.ReasonNotExterior, m.Images[1].Reason)
			assert.Equal(t, after[0].FileName, m.Images[0].Original)
			assert.Equal(t, "grey/"+strings.TrimSuffix(after[0].FileName, ".jpg")+"_studio-grey.jpg", m.Images[0].Files["grey"])
			continue
		}
		assert.Equal(t, zip.Store, f.Method)
		assert.Zero(t, f.Flags&0x8, "no data descriptor")
	}
	assert.Equal(t, e.read(after[0].Studio.Styles["light"].Key), readZipEntry(t, zr.File[1]))

	// Done is final: a re-delivery does nothing.
	require.NoError(t, e.runner(seg, an).Run(ctx, job.ID))
	assert.Equal(t, 2, seg.calls())
	assert.Equal(t, 2, an.calls())
}

// diagValue is the value at path in an outcome's diagnostics (nil when missing).
func diagValue(t *testing.T, st *store.ImageStudio, path ...string) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(st.Diagnostics, &v))
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func diagString(t *testing.T, st *store.ImageStudio, path ...string) string {
	t.Helper()
	s, _ := diagValue(t, st, path...).(string)
	return s
}

func pixelDiff(a, b image.Image, x, y int) int {
	r1, g1, b1, _ := a.At(x, y).RGBA()
	r2, g2, b2, _ := b.At(x, y).RGBA()
	d := func(p, q uint32) int { return max(int(p>>8)-int(q>>8), int(q>>8)-int(p>>8)) }
	return max(d(r1, r2), d(g1, g2), d(b1, b2))
}

func readZipEntry(t *testing.T, f *zip.File) []byte {
	t.Helper()
	rc, err := f.Open()
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	return b
}

// With the fake analyzer (V2I_ANALYZER=fake), studio versions are made for free, and a
// file name containing "missingpart" withholds them.
func TestRunWithFakeAnalyzer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	an, err := fake.New(fake.Options{})
	require.NoError(t, err)

	job, _ := e.completedJob("car.mp4", photoSpec{shot: "rear_three_quarter", box: carBox()})
	require.NoError(t, e.runner(studio.FakeSegmenter{}, an).Run(ctx, job.ID))
	got := e.images(job.ID)
	assert.Equal(t, store.ImageStudioReady, got[0].Studio.Status, "%s", got[0].Studio.Diagnostics)
	assert.Zero(t, e.job(job.ID).CostUSD)

	job, _ = e.completedJob("car-missingpart.mp4", photoSpec{shot: "rear_three_quarter", box: carBox()})
	require.NoError(t, e.runner(studio.FakeSegmenter{}, an).Run(ctx, job.ID))
	got = e.images(job.ID)
	assert.Equal(t, store.ImageStudioWithheld, got[0].Studio.Status)
	assert.Empty(t, e.job(job.ID).StudioZipPath, "no ready photo, no studio.zip")
	assert.Equal(t, store.StudioDone, e.job(job.ID).StudioStatus)
}

func TestRunGatesWithhold(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	job, _ := e.completedJob("car.mp4",
		photoSpec{shot: "front", box: &store.NRect{X0: 0.2, Y0: 0.3, X1: 1, Y1: 0.8}}, // touches the right edge
		photoSpec{shot: "rear", box: carBox()},                                        // the models disagree
		photoSpec{shot: "front_three_quarter", box: carBox()},                         // passes
	)
	seg := &segmenter{mutate: func(n int, r *studio.MaskResult) {
		if n == 0 {
			r.Stats.IoU = 0.7
		}
	}}
	an := &qaAnalyzer{}

	require.NoError(t, e.runner(seg, an).Run(ctx, job.ID))

	got := e.images(job.ID)
	assert.Equal(t, 2, seg.calls(), "the box gate decides before the sidecar is called")
	assert.Equal(t, 1, an.calls(), "only the photo that passed the gates is checked by the model")
	edge := got[0].Studio
	assert.Equal(t, store.ImageStudioWithheld, edge.Status)
	assert.Contains(t, edge.Reason, "right edge")
	assert.Equal(t, "cut_off", diagString(t, edge, "boxGate", "code"))
	assert.Empty(t, edge.Styles)
	disagree := got[1].Studio
	assert.Equal(t, store.ImageStudioWithheld, disagree.Status)
	assert.Contains(t, disagree.Reason, "disagreed")
	assert.Equal(t, "models_disagree", diagString(t, disagree, "maskGate", "code"))
	assert.Equal(t, store.ImageStudioReady, got[2].Studio.Status)
	for _, im := range got[:2] {
		for _, style := range styles {
			_, err := e.storage.Head(ctx, studiojob.ImageKey("cars", job.ID, style, im.ID))
			assert.ErrorIs(t, err, storage.ErrNotFound, "no variant is stored for a withheld photo")
		}
	}
}

// A hard case (the models agreed less than HardIoU) gets the QA check even when the
// domain turns it off; an easy one does not.
func TestRunHardCaseNeedsQA(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withDomain(func(c *domain.Config) { c.Studio.QA = false })
	ctx := context.Background()
	job, _ := e.completedJob("car.mp4",
		photoSpec{shot: "front", box: carBox()},
		photoSpec{shot: "rear", box: carBox()},
	)
	seg := &segmenter{mutate: func(n int, r *studio.MaskResult) {
		if n == 1 {
			r.Stats.IoU = 0.9 // above MinIoU, below HardIoU
		}
	}}
	an := &qaAnalyzer{}
	require.NoError(t, e.runner(seg, an).Run(ctx, job.ID))

	got := e.images(job.ID)
	assert.Equal(t, 1, an.calls())
	assert.Equal(t, store.ImageStudioReady, got[0].Studio.Status)
	assert.Nil(t, diagValue(t, got[0].Studio, "qa"), "no QA without a hard case")
	assert.Equal(t, store.ImageStudioReady, got[1].Studio.Status)
	assert.Equal(t, true, diagValue(t, got[1].Studio, "qa", "required"))
	assert.Equal(t, "hard_case", diagString(t, got[1].Studio, "maskGate", "code"))
	assert.Equal(t, 1, e.usageRows(job.ID))
}

func TestRunQAWithholds(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	job, _ := e.completedJob("car.mp4", photoSpec{shot: "side_profile", box: carBox()})
	an := &qaAnalyzer{answer: func(_ int, r *analyzer.Request) (*analyzer.Response, error) {
		return qaText("```json\n"+`{"missingParts":["left mirror cap","roof antenna tip"],"backgroundKept":[],"notes":"mirror cut"}`+"\n```")(0, r)
	}}
	require.NoError(t, e.runner(&segmenter{}, an).Run(ctx, job.ID))

	st := e.images(job.ID)[0].Studio
	assert.Equal(t, store.ImageStudioWithheld, st.Status)
	assert.Contains(t, st.Reason, "left mirror cap")
	assert.NotContains(t, st.Reason, "antenna", "a lost antenna tip is minor and not a reason")
	assert.Equal(t, []any{"left mirror cap"}, diagValue(t, st, "qa", "missingParts"))
	assert.Equal(t, []any{"roof antenna tip"}, diagValue(t, st, "qa", "minorParts"))
	assert.Empty(t, e.keysUnder(studiojob.Prefix("cars", job.ID)), "nothing withheld is stored")
	assert.Equal(t, 1, e.usageRows(job.ID), "the check is paid for either way")
	j := e.job(job.ID)
	assert.Equal(t, store.StudioDone, j.StudioStatus)
	assert.Empty(t, j.StudioZipPath)
}

func TestRunQAFailures(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(int, *analyzer.Request) (*analyzer.Response, error){
		"model unavailable": func(int, *analyzer.Request) (*analyzer.Response, error) {
			return nil, fmt.Errorf("gemini: %w", analyzer.ErrUnavailable)
		},
		"unreadable answer": qaText("I think the car looks fine."),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			job, _ := e.completedJob("car.mp4", photoSpec{shot: "front", box: carBox()}, photoSpec{shot: "rear", box: carBox()})
			an := &qaAnalyzer{answer: func(n int, r *analyzer.Request) (*analyzer.Response, error) {
				if n >= 2 { // the second photo's check works
					return qaText(qaAnswerOK)(n, r)
				}
				return answer(n, r)
			}}
			require.NoError(t, e.runner(&segmenter{}, an).Run(context.Background(), job.ID))
			got := e.images(job.ID)
			assert.Equal(t, 3, an.calls(), "a failed check is asked once more")
			assert.Equal(t, store.ImageStudioFailed, got[0].Studio.Status)
			assert.Equal(t, studiojob.ReasonQAFailed, got[0].Studio.Reason)
			assert.Equal(t, 2.0, diagValue(t, got[0].Studio, "qa", "calls"))
			assert.NotEmpty(t, diagString(t, got[0].Studio, "qa", "error"))
			assert.Equal(t, store.ImageStudioReady, got[1].Studio.Status, "one photo's failure does not stop the others")
			assert.Equal(t, store.StudioDone, e.job(job.ID).StudioStatus)
		})
	}
}

func TestRunDailyCeilingSkips(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	job, _ := e.completedJob("car.mp4", photoSpec{shot: "front", box: carBox()}, photoSpec{shot: "rear", box: carBox()})
	// Today's spend (another job's usage) is already at the ceiling.
	require.NoError(t, e.store.InsertUsage(ctx, store.UsageEvent{DomainKey: "cars", Step: "analyze", Model: "m", CostUSD: 0.5}))
	an := &qaAnalyzer{}
	r := e.runner(&segmenter{}, an)
	r.DailyCostCeilingUSD = 0.5

	require.NoError(t, r.Run(ctx, job.ID))

	assert.Zero(t, an.calls(), "no QA call above the ceiling")
	j := e.job(job.ID)
	assert.Equal(t, store.StudioSkipped, j.StudioStatus)
	assert.Empty(t, j.StudioZipPath)
	for _, im := range e.images(job.ID) {
		assert.Equal(t, &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: studiojob.ReasonCeiling}, im.Studio)
	}
	assert.Empty(t, e.keysUnder(studiojob.Prefix("cars", job.ID)))

	// Below the ceiling the work goes on.
	job, _ = e.completedJob("car.mp4", photoSpec{shot: "front", box: carBox()})
	r.DailyCostCeilingUSD = 5
	require.NoError(t, r.Run(ctx, job.ID))
	assert.Equal(t, store.StudioDone, e.job(job.ID).StudioStatus)
}

// A crash between two photos (here: the database stays down while the second outcome
// is recorded) keeps the first photo's outcome; the retry does only the second photo
// and pays for nothing twice.
func TestRunResumesAfterCrash(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	job, imgs := e.completedJob("car.mp4",
		photoSpec{shot: "front_three_quarter", box: carBox()},
		photoSpec{shot: "side_profile", box: carBox()},
	)
	fs := &failingStore{Store: e.store, failFor: map[string]bool{imgs[1].ID: true}}
	an := &qaAnalyzer{}
	seg := &segmenter{}
	r := e.runner(seg, an)
	r.Store = fs

	err := r.Run(ctx, job.ID)
	require.ErrorIs(t, err, errDBDown, "River retries the job")
	got := e.images(job.ID)
	assert.Equal(t, store.ImageStudioReady, got[0].Studio.Status)
	assert.Nil(t, got[1].Studio)
	assert.Equal(t, store.StudioRunning, e.job(job.ID).StudioStatus)
	assert.Equal(t, 2, an.calls())

	fs.mu.Lock()
	fs.failFor = nil
	fs.mu.Unlock()
	require.NoError(t, r.Run(ctx, job.ID))
	assert.Equal(t, 3, seg.calls(), "only the second photo is segmented again")
	assert.Equal(t, 3, an.calls(), "only the second photo is checked again")
	got = e.images(job.ID)
	assert.Equal(t, store.ImageStudioReady, got[1].Studio.Status)
	j := e.job(job.ID)
	assert.Equal(t, store.StudioDone, j.StudioStatus)
	zipBytes := e.read(j.StudioZipPath)
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	require.NoError(t, err)
	assert.Len(t, zr.File, 7, "both photos in three styles, plus the manifest")
}

// A shutdown during a QA call leaves that photo for the retry instead of recording a
// failure.
func TestRunInterruptedQAIsRedone(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	job, _ := e.completedJob("car.mp4", photoSpec{shot: "front", box: carBox()})
	ctx, cancel := context.WithCancel(context.Background())
	an := &qaAnalyzer{answer: func(n int, r *analyzer.Request) (*analyzer.Response, error) {
		cancel()
		return nil, context.Canceled
	}}
	err := e.runner(&segmenter{}, an).Run(ctx, job.ID)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, e.images(job.ID)[0].Studio)
	assert.Equal(t, store.StudioRunning, e.job(job.ID).StudioStatus)

	require.NoError(t, e.runner(&segmenter{}, &qaAnalyzer{}).Run(context.Background(), job.ID))
	assert.Equal(t, store.ImageStudioReady, e.images(job.ID)[0].Studio.Status)
}

func TestRunSegmenterUnavailable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	job, _ := e.completedJob("car.mp4", photoSpec{shot: "front", box: carBox()}, photoSpec{shot: "rear", box: carBox()})

	// The real client against a sidecar that is not listening.
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close()
	down := studio.NewHTTPSegmenter(url, 2*time.Second, nil)
	an := &qaAnalyzer{}
	err := e.runner(down, an).Run(ctx, job.ID)
	require.ErrorIs(t, err, studio.ErrSegmenterUnavailable)
	for _, im := range e.images(job.ID) {
		assert.Nil(t, im.Studio, "nothing is decided while the sidecar is down")
	}
	assert.Equal(t, store.StudioRunning, e.job(job.ID).StudioStatus)
	assert.Zero(t, an.calls())

	// River's final attempt marks the studio work failed; the job is untouched.
	w := &queue.StudioWorker{Runner: e.runner(down, an), Store: e.store}
	err = w.Work(ctx, &river.Job[queue.StudioArgs]{
		JobRow: &rivertype.JobRow{ID: 1, Attempt: queue.StudioMaxAttempts, MaxAttempts: queue.StudioMaxAttempts},
		Args:   queue.StudioArgs{JobID: job.ID},
	})
	require.ErrorIs(t, err, studio.ErrSegmenterUnavailable)
	j := e.job(job.ID)
	assert.Equal(t, store.StudioFailed, j.StudioStatus)
	assert.Equal(t, store.StatusCompleted, j.Status)
	assert.NotEmpty(t, j.ZipPath, "the originals' ZIP is kept")
	for _, im := range e.images(job.ID) {
		assert.Equal(t, &store.ImageStudio{Status: store.ImageStudioFailed, Reason: queue.StudioFailReason}, im.Studio)
	}
	assert.Contains(t, j.InternalLog, "studio: river attempt 3/3 failed")
}

// A segmentation that times out, or that the sidecar refuses, fails only that photo.
func TestRunPhotoFailuresDoNotStopTheJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	job, _ := e.completedJob("car.mp4",
		photoSpec{shot: "front", box: carBox()},
		photoSpec{shot: "rear", box: carBox()},
		photoSpec{shot: "side_profile", box: carBox()},
	)
	seg := &segmenter{before: func(n int, ctx context.Context) error {
		switch n {
		case 0:
			<-ctx.Done() // a hung sidecar: the photo timeout ends the call
			return fmt.Errorf("%w: %w", studio.ErrSegmenterTimeout, ctx.Err())
		case 1:
			return fmt.Errorf("%w: bad image", studio.ErrSegmenterRejected)
		}
		return nil
	}}
	r := e.runner(seg, &qaAnalyzer{})
	r.PhotoTimeout = 50 * time.Millisecond
	require.NoError(t, r.Run(context.Background(), job.ID))
	got := e.images(job.ID)
	for _, im := range got[:2] {
		assert.Equal(t, store.ImageStudioFailed, im.Studio.Status)
		assert.Equal(t, studiojob.ReasonFailed, im.Studio.Reason)
		assert.Contains(t, diagString(t, im.Studio, "error"), "segmentation")
	}
	assert.Equal(t, store.ImageStudioReady, got[2].Studio.Status)
	j := e.job(job.ID)
	assert.Equal(t, store.StudioDone, j.StudioStatus)
	assert.NotEmpty(t, j.StudioZipPath)
}

// The user deletes the job while a QA call runs: the run stops quietly and leaves no
// studio files behind.
func TestRunJobDeletedMeanwhile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	job, _ := e.completedJob("car.mp4", photoSpec{shot: "front", box: carBox()}, photoSpec{shot: "rear", box: carBox()})
	an := &qaAnalyzer{answer: func(n int, r *analyzer.Request) (*analyzer.Response, error) {
		if n == 1 {
			// Deleted like httpapi does it: files, then the row.
			require.NoError(t, e.storage.DeletePrefix(ctx, storage.JobPrefix("cars", job.ID)))
			require.NoError(t, e.store.ExpireJob(ctx, job.ID))
		}
		return qaText(qaAnswerOK)(n, r)
	}}
	require.NoError(t, e.runner(&segmenter{}, an).Run(ctx, job.ID))

	j := e.job(job.ID)
	assert.Equal(t, store.StatusExpired, j.Status)
	assert.Equal(t, store.StudioSkipped, j.StudioStatus)
	assert.Empty(t, j.StudioZipPath)
	assert.Empty(t, e.keysUnder(storage.JobPrefix("cars", job.ID)), "uploads after the sweep are removed by the studio job")

	// Deleted before the studio job starts: nothing happens.
	job, _ = e.completedJob("car.mp4", photoSpec{shot: "front", box: carBox()})
	require.NoError(t, e.store.ExpireJob(ctx, job.ID))
	seg := &segmenter{}
	require.NoError(t, e.runner(seg, &qaAnalyzer{}).Run(ctx, job.ID))
	assert.Zero(t, seg.calls())
	require.NoError(t, e.runner(seg, &qaAnalyzer{}).Run(ctx, "job_missing"))
}

// Retention and user deletion sweep the job prefix: it holds every studio file.
func TestJobPrefixCoversStudioFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	job, _ := e.completedJob("car.mp4", photoSpec{shot: "front", box: carBox()})
	require.NoError(t, e.runner(&segmenter{}, &qaAnalyzer{}).Run(ctx, job.ID))
	j := e.job(job.ID)
	require.NotEmpty(t, j.StudioZipPath)
	prefix := storage.JobPrefix("cars", job.ID)
	keys := e.keysUnder(prefix)
	assert.Len(t, keys, 2*3+1+1, "variants and thumbnails, studio.zip, the original photo")
	for _, k := range keys {
		assert.True(t, strings.HasPrefix(k, prefix), k)
	}
	require.NoError(t, e.storage.DeletePrefix(ctx, prefix))
	assert.Empty(t, e.keysUnder(prefix))
	require.NoError(t, e.store.ExpireJob(ctx, job.ID))
	j = e.job(job.ID)
	assert.Empty(t, j.StudioZipPath)
	assert.Nil(t, e.images(job.ID)[0].Studio.Styles)
}

// The QA check of photo i runs while photo i+1 is segmented, but never more than one
// photo ahead.
func TestRunOverlapsQAWithTheNextSegmentation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	job, _ := e.completedJob("car.mp4",
		photoSpec{shot: "front", box: carBox()},
		photoSpec{shot: "rear", box: carBox()},
		photoSpec{shot: "side_profile", box: carBox()},
	)
	var qaDone atomic.Int32
	segStarted := make(chan int, 3)
	var overlapped atomic.Bool
	var violations atomic.Int32
	seg := &segmenter{before: func(n int, _ context.Context) error {
		// Photo n is segmented only after photo n-2's check has finished.
		if n >= 2 && int(qaDone.Load()) < n-1 {
			violations.Add(1)
		}
		segStarted <- n
		return nil
	}}
	an := &qaAnalyzer{answer: func(n int, r *analyzer.Request) (*analyzer.Response, error) {
		if n == 0 {
			// Photo 1's segmentation starts while this check is still running.
			select {
			case got := <-segStarted:
				for got != 1 {
					got = <-segStarted
				}
				overlapped.Store(true)
			case <-time.After(5 * time.Second):
			}
		}
		defer qaDone.Add(1)
		return qaText(qaAnswerOK)(n, r)
	}}
	require.NoError(t, e.runner(seg, an).Run(context.Background(), job.ID))
	assert.True(t, overlapped.Load(), "the next photo is segmented during the QA call")
	assert.Zero(t, violations.Load(), "at most one photo ahead")
	for _, im := range e.images(job.ID) {
		assert.Equal(t, store.ImageStudioReady, im.Studio.Status)
	}
}

func TestRunDomainRemoved(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	job, _ := e.completedJob("car.mp4", photoSpec{shot: "front", box: carBox()})
	r := e.runner(&segmenter{}, &qaAnalyzer{})
	r.Registry = domain.NewRegistry()
	require.NoError(t, r.Run(context.Background(), job.ID))
	assert.Equal(t, store.StudioFailed, e.job(job.ID).StudioStatus)
	assert.Equal(t, store.ImageStudioFailed, e.images(job.ID)[0].Studio.Status)
}

// A photo that became ineligible (the domain dropped its view) is skipped, not rendered.
func TestRunDomainChangedSinceCompletion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	job, _ := e.completedJob("car.mp4", photoSpec{shot: "front", box: carBox()}, photoSpec{shot: "rear", box: carBox()})
	e.withDomain(func(c *domain.Config) { c.Studio.Views = []string{"rear"} })
	seg := &segmenter{}
	require.NoError(t, e.runner(seg, &qaAnalyzer{}).Run(context.Background(), job.ID))
	got := e.images(job.ID)
	assert.Equal(t, 1, seg.calls())
	assert.Equal(t, &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: studiojob.ReasonNotExterior}, got[0].Studio)
	assert.Equal(t, store.ImageStudioReady, got[1].Studio.Status)
}

func TestNothingToDo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	// Studio not requested (no eligible photo).
	job, _ := e.completedJob("car.mp4", photoSpec{shot: "interior_dashboard", box: carBox()})
	require.Equal(t, store.StudioNone, job.StudioStatus)
	seg := &segmenter{}
	require.NoError(t, e.runner(seg, &qaAnalyzer{}).Run(ctx, job.ID))
	assert.Zero(t, seg.calls())
	assert.Equal(t, store.StudioNone, e.job(job.ID).StudioStatus)
}

func TestEligibleAndNames(t *testing.T) {
	t.Parallel()
	reg, err := domain.LoadDir(filepath.Join("..", "..", "..", "configs", "domains"))
	require.NoError(t, err)
	cars, _ := reg.Get("cars")
	box := &store.NRect{X0: 0.1, Y0: 0.1, X1: 0.9, Y1: 0.9}
	for shot, want := range map[string]bool{
		"front_three_quarter": true, "side_profile": true, "rear_three_quarter": true, "front": true, "rear": true,
		"trunk": true, "wheel": true, "detail": true, // every exterior shot since the generative style
		"interior_dashboard": true, "interior_front_seats": true, // the studio through the glass
		"odometer": false, "other": false,
	} {
		ok, reason := studiojob.Eligible(cars, store.SelectedImage{ShotID: shot, SubjectBox: box})
		assert.Equal(t, want, ok, shot)
		if !ok {
			assert.Equal(t, studiojob.ReasonNotExterior, reason)
		}
	}
	ok, reason := studiojob.Eligible(cars, store.SelectedImage{ShotID: "front"})
	assert.False(t, ok)
	assert.Equal(t, studiojob.ReasonNoBox, reason)
	ok, _ = studiojob.Eligible(cars, store.SelectedImage{ShotID: "interior_dashboard"})
	assert.True(t, ok, "an interior needs no box")
	noGen := *cars
	noGen.Studio.Generative = domain.StudioGenerative{}
	ok, reason = studiojob.Eligible(&noGen, store.SelectedImage{ShotID: "interior_dashboard", SubjectBox: box})
	assert.False(t, ok)
	assert.Equal(t, studiojob.ReasonNotExterior, reason)
	off := *cars
	off.Studio.Enabled = false
	ok, _ = studiojob.Eligible(&off, store.SelectedImage{ShotID: "front", SubjectBox: box})
	assert.False(t, ok)

	assert.Equal(t, "cars/jobs/job_1/studio/grey/img_1.jpg", studiojob.ImageKey("cars", "job_1", "grey", "img_1"))
	assert.Equal(t, "cars/jobs/job_1/studio/dark/thumbs/img_1.jpg", studiojob.ThumbKey("cars", "job_1", "dark", "img_1"))
	assert.Equal(t, "cars/jobs/job_1/studio.zip", studiojob.ZipKey("cars", "job_1"))
	assert.True(t, strings.HasPrefix(studiojob.ImageKey("cars", "job_1", "grey", "img_1"), storage.JobPrefix("cars", "job_1")))
	assert.Equal(t, "cars_7K2M9QXA_1_front_three_quarter_00-06_studio-grey.jpg",
		studiojob.FileName("cars_7K2M9QXA_1_front_three_quarter_00-06.jpg", "grey"))
	assert.True(t, strings.HasSuffix(studiojob.ZipName("cars", "job_01J9Z3K7K2M9QXAB5V1Y0Z2C3D"), "_studio.zip"))
	assert.False(t, strings.Contains(studiojob.ZipName("cars", "job_01J9Z3K7K2M9QXAB5V1Y0Z2C3D"), "images"))

	// The default QA model has a price (its cost counts against the daily ceiling).
	_, known := pricing.Lookup(domain.DefaultStudioQAModel)
	assert.True(t, known)
}

func TestUnavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmt.Errorf("x: %w", studio.ErrSegmenterUnavailable), true},
		{fmt.Errorf("x: %w", studio.ErrSegmenterTimeout), false},
		{fmt.Errorf("x: %w", studio.ErrSegmenterRejected), false},
		{fmt.Errorf("x: %w", studio.ErrSegmenterBadResponse), false},
		{fmt.Errorf("post: %w", io.EOF), true},
		{errors.New("mask has the wrong size"), false},
		{context.DeadlineExceeded, false},
	} {
		assert.Equal(t, tc.want, studiojob.Unavailable(tc.err), "%v", tc.err)
	}
	assert.True(t, slices.Equal(styles, []string{"grey", "light", "dark"}))
}

// The generative style on an exterior: the fake image model fills the magenta and keeps
// the car in place, so the photo gets a "studio" variant first, then the drawn ones;
// the call is paid for next to the QA check.
func TestRunGenerativeExterior(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withGenerative()
	ctx := context.Background()
	job, _ := e.completedJob("walkaround.mp4", photoSpec{shot: "front_three_quarter", box: carBox()})
	an, err := fake.New(fake.Options{})
	require.NoError(t, err)
	seg := &segmenter{}
	require.NoError(t, e.runner(seg, an).Run(ctx, job.ID))

	im := e.images(job.ID)[0]
	st := im.Studio
	require.NotNil(t, st)
	require.Equal(t, store.ImageStudioReady, st.Status, "%s", st.Diagnostics)
	require.Contains(t, st.Styles, store.Style("studio"))
	for _, s := range styles {
		assert.Contains(t, st.Styles, store.Style(s))
	}
	v := st.Styles["studio"]
	// The kept margin widens the frame; the result is then scaled back to the photo's
	// long side (the generated background was upscaled anyway).
	assert.Equal(t, [2]int{photoW, photoH}, [2]int{v.Width, v.Height})
	assert.NotEmpty(t, e.read(v.Key))
	assert.NotEmpty(t, e.read(v.ThumbKey))
	assert.Equal(t, true, diagValue(t, st, "gen", "ok"))
	assert.Less(t, diagValue(t, st, "gen", "alignment").(float64), studio.MaxAlignment)
	assert.Equal(t, float64(1), diagValue(t, st, "gen", "calls"))
	assert.Equal(t, 2, e.usageRows(job.ID), "the image call and the QA check")
	assert.Equal(t, []string{"studio", "grey", "light", "dark"}, e.cars().Studio.EffectiveStyles())
}

// A model that moves the car: the answer is rejected twice (the second time with a
// wider margin) and the photo keeps its drawn styles only.
func TestRunGenerativeMisaligned(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withGenerative()
	job, _ := e.completedJob("walkaround-misaligned.mp4", photoSpec{shot: "front", box: carBox()})
	an, err := fake.New(fake.Options{})
	require.NoError(t, err)
	require.NoError(t, e.runner(&segmenter{}, an).Run(context.Background(), job.ID))

	st := e.images(job.ID)[0].Studio
	require.Equal(t, store.ImageStudioReady, st.Status, "%s", st.Diagnostics)
	assert.NotContains(t, st.Styles, store.Style("studio"))
	assert.Contains(t, st.Styles, store.Style("grey"))
	assert.Equal(t, false, diagValue(t, st, "gen", "ok"))
	assert.Equal(t, float64(2), diagValue(t, st, "gen", "calls"))
	assert.Contains(t, diagValue(t, st, "gen", "error"), "moved the car")
	assert.InDelta(t, 0.4, diagValue(t, st, "gen", "pad").(float64), 1e-9, "the retry widened the margin")
	assert.Equal(t, 3, e.usageRows(job.ID), "two image calls and the QA check")
}

// An interior: no segmentation, no QA; the model's window treatment becomes the only
// variant, and a photo without a box is eligible.
func TestRunGenerativeInterior(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withGenerative()
	job, _ := e.completedJob("walkaround.mp4",
		photoSpec{shot: "interior_dashboard"}, photoSpec{shot: "odometer", box: carBox()})
	require.Equal(t, store.StudioPending, job.StudioStatus)
	an, err := fake.New(fake.Options{})
	require.NoError(t, err)
	seg := &segmenter{}
	require.NoError(t, e.runner(seg, an).Run(context.Background(), job.ID))

	imgs := e.images(job.ID)
	st := imgs[0].Studio
	require.Equal(t, store.ImageStudioReady, st.Status, "%s", st.Diagnostics)
	assert.Equal(t, []store.Style{"studio"}, slices.Collect(maps.Keys(st.Styles)))
	assert.Equal(t, photoW, st.Styles["studio"].Width, "an interior keeps its frame")
	assert.Equal(t, true, diagValue(t, st, "gen", "interior"))
	assert.Equal(t, float64(1), diagValue(t, st, "gen", "windows"), "the fake names one glass box")
	assert.Greater(t, diagValue(t, st, "gen", "changed").(float64), studio.MinInteriorChange)
	assert.Zero(t, seg.calls(), "no cut-out for an interior")
	assert.Equal(t, store.ImageStudioSkipped, imgs[1].Studio.Status, "the odometer is neither exterior nor interior view")
	assert.Equal(t, 2, e.usageRows(job.ID), "the window boxes and the image call")
	assert.Equal(t, store.StudioDone, e.job(job.ID).StudioStatus)
}
