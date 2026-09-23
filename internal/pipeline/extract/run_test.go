package extract

import (
	"context"
	"fmt"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/media"
)

const (
	srcFPS             = 30
	srcW, srcH, barPx  = 1280, 720, 4
	srcDur             = 8.0
	oneSourceFrame     = 1.0 / srcFPS
	timestampTolerance = oneSourceFrame + 1e-3
)

// barSource is a 1280x720 30 fps clip whose frame k (at k/30 s) shows a white bar at
// x ∈ [4k, 4k+4), so every extracted JPG tells which source frame it is.
func barSource(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	out := filepath.Join(t.TempDir(), "source.mp4")
	graph := fmt.Sprintf("color=c=black:s=%dx%d:r=%d:d=%v[bg];color=c=white:s=%dx%d:r=%d[bar];"+
		"[bg][bar]overlay=x='%d*round(t*%d)':y=0:shortest=1,format=yuv420p",
		srcW, srcH, srcFPS, srcDur, barPx, srcH, srcFPS, barPx, srcFPS)
	cmd := exec.Command("ffmpeg", "-y", "-hide_banner", "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", graph, "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "30", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v\n%s", err, b)
	}
	return out
}

// sourceTime returns the timestamp of the source frame a JPG shows.
func sourceTime(t *testing.T, path string) float64 {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	img, err := jpeg.Decode(f)
	require.NoError(t, err)
	b := img.Bounds()
	y := b.Min.Y + b.Dy()/2
	sum, n := 0.0, 0
	for x := b.Min.X; x < b.Max.X; x++ {
		r, g, bl, _ := img.At(x, y).RGBA()
		if (r+g+bl)/3>>8 > 128 {
			sum += float64(x - b.Min.X)
			n++
		}
	}
	require.NotZero(t, n, "no bar in %s", path)
	cx := (sum/float64(n) + 0.5) * srcW / float64(b.Dx())
	return math.Round((cx-barPx/2.0)/barPx) / srcFPS
}

func jpegDims(t *testing.T, path string) [2]int {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	require.NoError(t, err)
	return [2]int{cfg.Width, cfg.Height}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return len(entries)
}

func sampleRanges() []Range {
	return []Range{
		{SceneIdx: 0, ShotID: "front_three_quarter", StartSec: 1.0, EndSec: 3.0, Relevance: 0.9, Reason: "front 3/4"},
		{SceneIdx: 1, ShotID: "side_profile", StartSec: 5.0, EndSec: 5.8, Relevance: 0.7, Reason: "side"}, // short → 8 fps
		{SceneIdx: 2, ShotID: "rear", StartSec: 7.8, EndSec: 9.0, Relevance: 0.6, Reason: "rear"},         // clamped at 8 s
	}
}

func TestRun(t *testing.T) {
	src := barSource(t)
	work := t.TempDir()
	ff := media.New("", "", nil)
	out, err := Run(context.Background(), ff, Input{
		SourcePath: src, WorkDir: work, DurationSec: srcDur,
		Ranges: sampleRanges(), SampleFPS: 4, MaxCandidates: 300,
	})
	require.NoError(t, err)
	require.Len(t, out.Plans, 3)
	assert.Equal(t, []int{12, 14, 5}, []int{out.Plans[0].Expected, out.Plans[1].Expected, out.Plans[2].Expected})
	assert.Equal(t, 8.0, out.Plans[1].FPS)
	assert.Equal(t, 8.0, out.Plans[2].PadEndSec)
	require.Len(t, out.Candidates, 31)

	seen := map[string]bool{}
	perScene := map[int][]Candidate{}
	for i, c := range out.Candidates {
		if i > 0 {
			prev := out.Candidates[i-1]
			require.True(t, prev.SceneIdx < c.SceneIdx || (prev.SceneIdx == c.SceneIdx && prev.TimestampSec < c.TimestampSec),
				"ordered by scene then time at %d", i)
		}
		assert.True(t, ids.Valid(ids.PrefixCandidate, c.ID), c.ID)
		assert.False(t, seen[c.ID], "unique id")
		seen[c.ID] = true
		perScene[c.SceneIdx] = append(perScene[c.SceneIdx], c)

		assert.FileExists(t, c.FullPath)
		assert.FileExists(t, c.SmallPath)
		assert.Equal(t, filepath.Join(work, "cand"), filepath.Dir(c.FullPath))
		assert.Equal(t, filepath.Join(work, "small"), filepath.Dir(c.SmallPath))
		assert.Equal(t, [2]int{srcW, srcH}, jpegDims(t, c.FullPath), "full frame at source resolution")
		assert.Equal(t, [2]int{SmallWidth, 360}, jpegDims(t, c.SmallPath), "small copy 640 wide")

		got := sourceTime(t, c.FullPath)
		assert.InDelta(t, c.TimestampSec, got, timestampTolerance, "candidate %d: timestamp %.4f shows frame at %.4f", i, c.TimestampSec, got)
		assert.InDelta(t, got, sourceTime(t, c.SmallPath), 1e-9, "small copy is the same frame")
	}
	assert.Equal(t, 31, countFiles(t, filepath.Join(work, "cand")), "no stray files")
	assert.Equal(t, 31, countFiles(t, filepath.Join(work, "small")))

	for k, p := range out.Plans {
		cs := perScene[p.SceneIdx]
		require.Len(t, cs, p.Expected, "scene %d", p.SceneIdx)
		for i, c := range cs {
			assert.Equal(t, p.ShotID, c.ShotID)
			assert.Equal(t, p.Relevance, c.Relevance)
			assert.Equal(t, p.Reason, c.SceneReason)
			assert.InDelta(t, p.PadStartSec+float64(i)/p.FPS, c.TimestampSec, 1e-9, "plan %d frame %d", k, i)
			assert.GreaterOrEqual(t, c.TimestampSec, p.PadStartSec)
			assert.Less(t, c.TimestampSec, p.PadEndSec)
		}
	}
}

func TestRunRespectsCap(t *testing.T) {
	src := barSource(t)
	work := t.TempDir()
	out, err := Run(context.Background(), media.New("", "", nil), Input{
		SourcePath: src, WorkDir: work, DurationSec: srcDur,
		Ranges: sampleRanges(), SampleFPS: 4, MaxCandidates: 10,
	})
	require.NoError(t, err)
	require.LessOrEqual(t, len(out.Candidates), 10)
	require.GreaterOrEqual(t, len(out.Candidates), 10-len(out.Plans))
	scenes := map[int]int{}
	for _, c := range out.Candidates {
		scenes[c.SceneIdx]++
		// Fractional fps after scaling: the formula still holds.
		assert.InDelta(t, c.TimestampSec, sourceTime(t, c.FullPath), timestampTolerance)
	}
	assert.Len(t, scenes, 3, "every range keeps at least one frame")
	assert.Equal(t, len(out.Candidates), countFiles(t, filepath.Join(work, "cand")))
	assert.Equal(t, len(out.Candidates), countFiles(t, filepath.Join(work, "small")))
	for _, p := range out.Plans {
		assert.Less(t, p.FPS, 8.0)
	}
}

func TestRunSharedSceneIdx(t *testing.T) {
	src := barSource(t)
	work := t.TempDir()
	out, err := Run(context.Background(), media.New("", "", nil), Input{
		SourcePath: src, WorkDir: work, DurationSec: srcDur,
		Ranges: []Range{
			{SceneIdx: 4, ShotID: "wheel", StartSec: 1, EndSec: 2.5},
			{SceneIdx: 4, ShotID: "wheel", StartSec: 5, EndSec: 6.5},
		},
		SampleFPS: 2, MaxCandidates: 300,
	})
	require.NoError(t, err)
	require.Len(t, out.Candidates, 10) // 2 × 2.5 s × 2 fps
	paths := map[string]bool{}
	for i, c := range out.Candidates {
		assert.False(t, paths[c.FullPath], "file names must not collide")
		paths[c.FullPath] = true
		if i > 0 {
			assert.Greater(t, c.TimestampSec, out.Candidates[i-1].TimestampSec)
		}
		assert.InDelta(t, c.TimestampSec, sourceTime(t, c.FullPath), timestampTolerance)
	}
	assert.Equal(t, 10, countFiles(t, filepath.Join(work, "cand")))
}

func TestRunSkipsEmptyRanges(t *testing.T) {
	src := barSource(t)
	work := t.TempDir()
	// The container claims 9 s (e.g. a longer audio track); the video stops at 8 s.
	out, err := Run(context.Background(), media.New("", "", nil), Input{
		SourcePath: src, WorkDir: work, DurationSec: 9,
		Ranges: []Range{
			{SceneIdx: 0, StartSec: 2, EndSec: 3},
			{SceneIdx: 1, StartSec: 8.6, EndSec: 8.9},
		},
		SampleFPS: 4, MaxCandidates: 300,
	})
	require.NoError(t, err)
	require.Len(t, out.Plans, 2)
	require.NotEmpty(t, out.Candidates)
	for _, c := range out.Candidates {
		assert.Equal(t, 0, c.SceneIdx)
	}

	// Only empty ranges: the video is effectively undecodable there → error.
	_, err = Run(context.Background(), media.New("", "", nil), Input{
		SourcePath: src, WorkDir: t.TempDir(), DurationSec: 9,
		Ranges:    []Range{{SceneIdx: 1, StartSec: 8.6, EndSec: 8.9}},
		SampleFPS: 4, MaxCandidates: 300,
	})
	require.ErrorContains(t, err, "no frames")
}

func TestRunNoPlans(t *testing.T) {
	work := t.TempDir()
	out, err := Run(context.Background(), media.New("/nonexistent/ffmpeg", "", nil), Input{
		SourcePath: "/nonexistent.mp4", WorkDir: work, DurationSec: 10,
		Ranges: []Range{{StartSec: 12, EndSec: 14}},
	})
	require.NoError(t, err)
	assert.Empty(t, out.Candidates)
	assert.NotNil(t, out.Candidates)
	assert.Empty(t, out.Plans)
}

func TestRunCancelled(t *testing.T) {
	src := barSource(t)
	work := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, media.New("", "", nil), Input{
		SourcePath: src, WorkDir: work, DurationSec: srcDur, Ranges: sampleRanges(), SampleFPS: 4, MaxCandidates: 300,
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, countFiles(t, filepath.Join(work, "cand")))
	assert.Zero(t, countFiles(t, filepath.Join(work, "small")))
}

func TestRunFailureRemovesFiles(t *testing.T) {
	src := barSource(t)
	ff := media.New("", "", nil)

	t.Run("undecodable source", func(t *testing.T) {
		garbage := filepath.Join(t.TempDir(), "garbage.mp4")
		require.NoError(t, os.WriteFile(garbage, []byte("not a video at all"), 0o644))
		work := t.TempDir()
		_, err := Run(context.Background(), ff, Input{
			SourcePath: garbage, WorkDir: work, DurationSec: srcDur, Ranges: sampleRanges(), SampleFPS: 4, MaxCandidates: 300,
		})
		var ee *media.ExecError
		require.ErrorAs(t, err, &ee)
		assert.Zero(t, countFiles(t, filepath.Join(work, "cand")))
		assert.Zero(t, countFiles(t, filepath.Join(work, "small")))
	})

	t.Run("later range fails after earlier ranges wrote files", func(t *testing.T) {
		work := t.TempDir()
		// A directory where scene 1's first full frame must go makes that ffmpeg call fail.
		blocker := filepath.Join(work, "cand", "001_0001.jpg")
		require.NoError(t, os.MkdirAll(blocker, 0o755))
		_, err := Run(context.Background(), ff, Input{
			SourcePath: src, WorkDir: work, DurationSec: srcDur, Ranges: sampleRanges(), SampleFPS: 4, MaxCandidates: 300,
		})
		var ee *media.ExecError
		require.ErrorAs(t, err, &ee)
		assert.Contains(t, err.Error(), "scene 1")
		assert.Equal(t, 1, countFiles(t, filepath.Join(work, "cand")), "only the blocker remains; scene 0 frames removed")
		assert.Zero(t, countFiles(t, filepath.Join(work, "small")))
	})
}

func TestRunValidation(t *testing.T) {
	ff := media.New("", "", nil)
	_, err := Run(context.Background(), nil, Input{SourcePath: "a.mp4", WorkDir: t.TempDir()})
	assert.Error(t, err)
	_, err = Run(context.Background(), ff, Input{WorkDir: t.TempDir()})
	assert.Error(t, err)
	_, err = Run(context.Background(), ff, Input{SourcePath: "a.mp4"})
	assert.Error(t, err)
}

func TestAllocate(t *testing.T) {
	tests := []struct {
		name      string
		counts    []int
		relevance []float64
		max       int
		want      []int
	}{
		{"under the cap", []int{3, 4}, []float64{0, 0}, 10, []int{3, 4}},
		{"exactly the cap", []int{5, 5}, []float64{0, 0}, 10, []int{5, 5}},
		{"proportional", []int{10, 10, 10}, []float64{0, 0, 0}, 15, []int{5, 5, 5}},
		{"proportional uneven", []int{20, 1, 9}, []float64{0, 0, 0}, 10, []int{6, 1, 3}},
		{"largest remainder", []int{5, 5, 5}, []float64{0, 0, 0}, 7, []int{3, 2, 2}},
		{"one-frame minimum takes from the biggest", []int{1, 1, 1, 97}, []float64{0, 0, 0, 0}, 4, []int{1, 1, 1, 1}},
		{"empty ranges get nothing", []int{0, 5}, []float64{0, 0}, 3, []int{0, 3}},
		{"more ranges than the cap: most relevant win", []int{3, 3, 3}, []float64{0.2, 0.9, 0.5}, 2, []int{0, 1, 1}},
		{"zero cap", []int{3}, []float64{0}, 0, []int{0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := allocate(tt.counts, tt.relevance, tt.max)
			assert.Equal(t, tt.want, got)
			sum := 0
			for i, q := range got {
				sum += q
				assert.LessOrEqual(t, q, tt.counts[i])
			}
			assert.LessOrEqual(t, sum, max(tt.max, 0))
		})
	}
}

func TestEvenIndices(t *testing.T) {
	assert.Equal(t, []int{1, 5, 8}, evenIndices(10, 3))
	assert.Equal(t, []int{0, 1, 2, 3}, evenIndices(4, 4))
	assert.Equal(t, []int{0, 1, 2}, evenIndices(3, 7))
	assert.Equal(t, []int{2}, evenIndices(5, 1), "a single survivor is the middle frame, not the first")
	assert.Equal(t, []int{}, evenIndices(4, 0))
	for n := 1; n < 40; n++ {
		for q := 1; q <= n; q++ {
			idx := evenIndices(n, q)
			require.Len(t, idx, q)
			for i := 1; i < len(idx); i++ {
				require.Greater(t, idx[i], idx[i-1], "strictly increasing n=%d q=%d", n, q)
			}
			require.GreaterOrEqual(t, idx[0], 0)
			require.Less(t, idx[q-1], n)
		}
	}
}

func TestEnforceCapDropsEvenlyAndDeletes(t *testing.T) {
	dir := t.TempDir()
	mk := func(prefix string, n int) (full, small []string) {
		for i := range n {
			f := filepath.Join(dir, fmt.Sprintf("%s_full_%d.jpg", prefix, i))
			s := filepath.Join(dir, fmt.Sprintf("%s_small_%d.jpg", prefix, i))
			require.NoError(t, os.WriteFile(f, nil, 0o644))
			require.NoError(t, os.WriteFile(s, nil, 0o644))
			full, small = append(full, f), append(small, s)
		}
		return full, small
	}
	af, as := mk("a", 6)
	bf, bs := mk("b", 4)
	got := []rangeFrames{{plan: 0, full: af, small: as}, {plan: 1, full: bf, small: bs}}
	plans := []RangePlan{{Range: Range{Relevance: 0.5}}, {Range: Range{Relevance: 0.5}}}

	dropped := enforceCap(got, plans, 5)
	assert.Equal(t, 5, dropped)
	assert.Equal(t, []int{1, 3, 5}, got[0].keep)
	assert.Equal(t, []int{1, 3}, got[1].keep)
	for i, p := range af {
		kept := i == 1 || i == 3 || i == 5
		assert.Equal(t, kept, fileExists(p), p)
		assert.Equal(t, kept, fileExists(as[i]), as[i])
	}
	for i, p := range bf {
		kept := i == 1 || i == 3
		assert.Equal(t, kept, fileExists(p), p)
		assert.Equal(t, kept, fileExists(bs[i]), bs[i])
	}

	// Under the cap nothing is dropped.
	got2 := []rangeFrames{{plan: 0, full: af[:1], small: as[:1]}}
	assert.Zero(t, enforceCap(got2, plans, 5))
	assert.Equal(t, []int{0}, got2[0].keep)
}

func TestFileStems(t *testing.T) {
	plans := []RangePlan{
		{Range: Range{SceneIdx: 3}},
		{Range: Range{SceneIdx: 12}},
		{Range: Range{SceneIdx: 3}},
		{Range: Range{SceneIdx: 1234}},
	}
	assert.Equal(t, []string{"003-00", "012", "003-02", "1234"}, fileStems(plans))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
