package extract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/media"
)

// Tests for Run's concurrent range extraction.

// concurrencyRanges are six ranges over barSource, one sharing a SceneIdx and one
// clamped at the end of the video.
func concurrencyRanges() []Range {
	return []Range{
		{SceneIdx: 0, ShotID: "front_three_quarter", StartSec: 0.2, EndSec: 1.4, Relevance: 0.9},
		{SceneIdx: 1, ShotID: "side_profile", StartSec: 1.5, EndSec: 2.5, Relevance: 0.8},
		{SceneIdx: 2, ShotID: "rear", StartSec: 3, EndSec: 4.2, Relevance: 0.7},
		{SceneIdx: 3, ShotID: "wheel", StartSec: 4.5, EndSec: 5.0, Relevance: 0.6},
		{SceneIdx: 3, ShotID: "wheel", StartSec: 6, EndSec: 6.8, Relevance: 0.6},
		{SceneIdx: 4, ShotID: "interior", StartSec: 7.2, EndSec: 9, Relevance: 0.5},
	}
}

type candidateView struct {
	SceneIdx          int
	ShotID            string
	TimestampSec      float64
	Full, Small       string // base names
	FullSum, SmallSum string
}

func viewCandidates(t *testing.T, cs []Candidate) []candidateView {
	t.Helper()
	sum := func(p string) string {
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		h := sha256.Sum256(data)
		return hex.EncodeToString(h[:])
	}
	out := make([]candidateView, len(cs))
	for i, c := range cs {
		out[i] = candidateView{
			SceneIdx: c.SceneIdx, ShotID: c.ShotID, TimestampSec: c.TimestampSec,
			Full: filepath.Base(c.FullPath), Small: filepath.Base(c.SmallPath),
			FullSum: sum(c.FullPath), SmallSum: sum(c.SmallPath),
		}
	}
	return out
}

// TestRunConcurrentMatchesSequential: the candidates (order, timestamps, file names
// and bytes) and plans do not depend on the concurrency.
func TestRunConcurrentMatchesSequential(t *testing.T) {
	src := barSource(t)
	ff := media.New("", "", nil)
	ff.Threads = 2
	run := func(conc int) (*Output, string) {
		work := t.TempDir()
		out, err := Run(context.Background(), ff, Input{
			SourcePath: src, WorkDir: work, DurationSec: srcDur,
			Ranges: concurrencyRanges(), SampleFPS: 4, MaxCandidates: 300, Concurrency: conc,
		})
		require.NoError(t, err, "concurrency %d", conc)
		return out, work
	}
	seq, seqWork := run(1)
	require.Len(t, seq.Plans, 6)
	want := viewCandidates(t, seq.Candidates)
	require.NotEmpty(t, want)
	for _, c := range seq.Candidates {
		assert.InDelta(t, c.TimestampSec, sourceTime(t, c.FullPath), timestampTolerance)
	}
	for _, conc := range []int{0, 3, 6, 16} {
		got, work := run(conc)
		assert.Equal(t, seq.Plans, got.Plans, "concurrency %d", conc)
		assert.Equal(t, want, viewCandidates(t, got.Candidates), "concurrency %d", conc)
		assert.Equal(t, countFiles(t, filepath.Join(seqWork, "cand")), countFiles(t, filepath.Join(work, "cand")))
		assert.Equal(t, countFiles(t, filepath.Join(seqWork, "small")), countFiles(t, filepath.Join(work, "small")))
	}
}

// fakeFFmpeg writes a /bin/sh stand-in for ffmpeg's extract invocation. It appends
// "start <pattern>" / "end <pattern>" to log (O_APPEND, one short line per write).
// When the output pattern's base name is fail it exits 1 after 0.2 s; otherwise it
// runs behaviour, writes -frames:v empty files for each %04d.jpg output and then runs
// block (e.g. "exec sleep 30": exec, so the context kill reaches the sleep directly).
func fakeFFmpeg(t *testing.T, log, fail, behaviour, block string) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	script := fmt.Sprintf(`#!/bin/sh
log=%q
n=1; prev=""; name=""
for a in "$@"; do
  [ "$prev" = "-frames:v" ] && n=$a
  case "$a" in *%%04d.jpg) name=$(basename "$a");; esac
  prev=$a
done
echo "start $name" >> "$log"
if [ "$name" = %q ]; then
  sleep 0.2
  echo "fake decode failure" >&2
  echo "end $name" >> "$log"
  exit 1
fi
%s
for a in "$@"; do
  case "$a" in *%%04d.jpg)
    i=1; while [ $i -le $n ]; do : > "$(printf "$a" $i)"; i=$((i+1)); done;;
  esac
done
%s
echo "end $name" >> "$log"
`, log, fail, behaviour, block)
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	return bin
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Fields(strings.ReplaceAll(string(data), "\n", " "))
}

// maxInFlight replays a start/end log and returns the most processes alive at once.
func maxInFlight(t *testing.T, log []string) int {
	t.Helper()
	require.Zero(t, len(log)%2, "%q", log)
	cur, peak := 0, 0
	for i := 0; i < len(log); i += 2 {
		switch log[i] {
		case "start":
			cur++
		case "end":
			cur--
		default:
			t.Fatalf("bad log entry %q", log[i])
		}
		peak = max(peak, cur)
	}
	require.Zero(t, cur, "every process ended")
	return peak
}

func TestRunConcurrencyBound(t *testing.T) {
	for _, tt := range []struct{ conc, want int }{{1, 1}, {2, 2}, {0, DefaultConcurrency}} {
		t.Run(fmt.Sprintf("concurrency %d", tt.conc), func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "calls.log")
			ff := media.New(fakeFFmpeg(t, log, "none", "sleep 0.5", ""), "", nil)
			work := t.TempDir()
			out, err := Run(context.Background(), ff, Input{
				SourcePath: "/in.mp4", WorkDir: work, DurationSec: srcDur,
				Ranges: concurrencyRanges(), SampleFPS: 4, MaxCandidates: 300, Concurrency: tt.conc,
			})
			require.NoError(t, err)
			entries := readLog(t, log)
			assert.Len(t, entries, 4*len(out.Plans), "one start and one end per range")
			assert.Equal(t, tt.want, maxInFlight(t, entries))

			total := 0
			for _, p := range out.Plans {
				total += p.Expected
			}
			require.Len(t, out.Candidates, total)
			for i := 1; i < len(out.Candidates); i++ {
				a, b := out.Candidates[i-1], out.Candidates[i]
				require.True(t, a.SceneIdx < b.SceneIdx || (a.SceneIdx == b.SceneIdx && a.TimestampSec < b.TimestampSec), "ordered at %d", i)
			}
			assert.Equal(t, total, countFiles(t, filepath.Join(work, "cand")))
		})
	}
}

// TestRunFailureCancelsOtherRanges: when one range fails, the ranges still running are
// killed (not waited for), the ones not started never start, and every frame file is
// removed.
func TestRunFailureCancelsOtherRanges(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls.log")
	ff := media.New(fakeFFmpeg(t, log, "001_%04d.jpg", "", "exec sleep 30"), "", nil)
	work := t.TempDir()
	start := time.Now()
	_, err := Run(context.Background(), ff, Input{
		SourcePath: "/in.mp4", WorkDir: work, DurationSec: srcDur,
		Ranges: concurrencyRanges(), SampleFPS: 4, MaxCandidates: 300, Concurrency: 2,
	})
	took := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "scene 1 ", "the failing range's error, not a sibling's cancellation")
	var ee *media.ExecError
	require.ErrorAs(t, err, &ee)
	assert.Contains(t, ee.StderrTail, "fake decode failure")
	assert.NotErrorIs(t, err, context.Canceled)
	assert.Less(t, took, 10*time.Second, "scene 0 was killed, not waited for")

	assert.ElementsMatch(t, []string{"start", "000_%04d.jpg", "start", "001_%04d.jpg", "end", "001_%04d.jpg"}, readLog(t, log),
		"scenes 2+ never start once scene 1 failed")
	assert.Zero(t, countFiles(t, filepath.Join(work, "cand")), "scene 0's partial frames removed")
	assert.Zero(t, countFiles(t, filepath.Join(work, "small")))
}

// TestRunFailureRemovesFinishedRanges: frames of ranges that completed before another
// range failed are removed too.
func TestRunFailureRemovesFinishedRanges(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls.log")
	// The last range fails after the others finished (Concurrency 1 runs in order).
	ff := media.New(fakeFFmpeg(t, log, "004_%04d.jpg", "", ""), "", nil)
	work := t.TempDir()
	_, err := Run(context.Background(), ff, Input{
		SourcePath: "/in.mp4", WorkDir: work, DurationSec: srcDur,
		Ranges: concurrencyRanges(), SampleFPS: 4, MaxCandidates: 300, Concurrency: 1,
	})
	require.ErrorContains(t, err, "scene 4 ")
	assert.Len(t, readLog(t, log), 4*6, "every range ran")
	assert.Zero(t, countFiles(t, filepath.Join(work, "cand")))
	assert.Zero(t, countFiles(t, filepath.Join(work, "small")))
}

// TestRunCancelledWhileRunning: cancelling ctx kills the running ranges and removes
// their frames; the error wraps context.Canceled.
func TestRunCancelledWhileRunning(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls.log")
	ff := media.New(fakeFFmpeg(t, log, "none", "", "exec sleep 30"), "", nil)
	work := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel once the first ranges are running and have written frames (no
		// require here: this is not the test goroutine).
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(log)
			small, _ := os.ReadDir(filepath.Join(work, "small"))
			if strings.Count(string(data), "start") == DefaultConcurrency && len(small) > 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := Run(ctx, ff, Input{
		SourcePath: "/in.mp4", WorkDir: work, DurationSec: srcDur,
		Ranges: concurrencyRanges(), SampleFPS: 4, MaxCandidates: 300,
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 10*time.Second)
	assert.Len(t, readLog(t, log), 2*DefaultConcurrency, "only the first ranges started")
	assert.Zero(t, countFiles(t, filepath.Join(work, "cand")))
	assert.Zero(t, countFiles(t, filepath.Join(work, "small")))
}

// Fixture sanity for the fake: it really is picked up as the ffmpeg binary.
func TestFakeFFmpegRuns(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls.log")
	bin := fakeFFmpeg(t, log, "none", "", "")
	dir := t.TempDir()
	out, err := exec.Command(bin, "-frames:v", "2", filepath.Join(dir, "a_%04d.jpg")).CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.Equal(t, 2, countFiles(t, dir))
	assert.Equal(t, []string{"start", "a_%04d.jpg", "end", "a_%04d.jpg"}, readLog(t, log))
}
