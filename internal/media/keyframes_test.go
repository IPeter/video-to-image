package media

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for BuildPreview's decode plan: the keyframe probe, the keyframes-only fast
// path, the noref path, and the read-back check that falls back from one to the next.

func TestPlanPreview(t *testing.T) {
	tests := []struct {
		name   string
		maxGap float64
		fps    int
		want   previewPlan
	}{
		{"iPhone HEVC: a keyframe every 0.93 s", 0.935, 2, previewPlan{fps: 1, skip: skipNonKey}},
		{"GOP 15 at 30 fps", 0.5, 2, previewPlan{fps: 2, skip: skipNonKey}},
		{"GOP 15 at 29.97 fps (rounding slack)", 0.5005, 2, previewPlan{fps: 2, skip: skipNonKey}},
		{"never above the requested fps", 1.0 / 30, 2, previewPlan{fps: 2, skip: skipNonKey}},
		{"never above the keyframe rate", 0.5, 4, previewPlan{fps: 2, skip: skipNonKey}},
		{"a keyframe every 0.3 s at 4 fps", 0.3, 4, previewPlan{fps: 3, skip: skipNonKey}},
		{"GOP 30 at 30 fps", 1.0, 2, previewPlan{fps: 1, skip: skipNonKey}},
		{"largest accepted gap (0.8 keyframes/s): 1 fps", 1.25, 2, previewPlan{fps: 1, skip: skipNonKey}},
		{"requested 1 fps", 1.25, 1, previewPlan{fps: 1, skip: skipNonKey}},
		{"gap too long", 1.26, 2, previewPlan{fps: 2, skip: skipNonRef}},
		{"Auto-FPS stretched GOP (56 frames at 30 fps)", 1.87, 2, previewPlan{fps: 2, skip: skipNonRef}},
		{"one long take after a dense intro", 10, 2, previewPlan{fps: 2, skip: skipNonRef}},
		{"degenerate: zero gap", 0, 2, previewPlan{fps: 2, skip: skipNonRef}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planPreview(keyframeStats{Packets: 100, Keyframes: 10, EndSec: 10, MaxGapSec: tt.maxGap}, tt.fps)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, PreviewInfo{FPS: float64(tt.want.fps), KeyframesOnly: tt.want.skip == skipNonKey}, got.info())
		})
	}
	assert.Equal(t, previewPlan{fps: 2, skip: skipNonRef}, planPreview(keyframeStats{Packets: 10, EndSec: 1, MaxGapSec: 1}, 2),
		"no keyframe at all")
}

func TestParseKeyframePackets(t *testing.T) {
	// B-frames: packets in decode order, so pts is not monotonic.
	out := []byte(`{"packets": [
		{ "pts_time": "-0.033333", "dts_time": "-0.066667", "duration_time": "0.033333", "flags": "KD_" },
		{ "pts_time": "0.000000", "dts_time": "-0.033333", "duration_time": "0.033333", "flags": "K__" },
		{ "pts_time": "0.100000", "dts_time": "0.000000", "duration_time": "0.033333", "flags": "___" },
		{ "pts_time": "N/A", "dts_time": "0.500000", "duration_time": "N/A", "flags": "K__" },
		{ "pts_time": "N/A", "dts_time": "N/A", "flags": "K__" },
		{ "pts_time": "1.300000", "dts_time": "1.200000", "duration_time": "0.033333", "flags": "K__" },
		{ "pts_time": "1.100000", "dts_time": "1.233333", "duration_time": "0.033333", "flags": "__C" },
		{ "pts_time": "2.966667", "dts_time": "2.933333", "duration_time": "0.033333", "flags": "___" }
	]}`)
	st, err := parseKeyframePackets(out)
	require.NoError(t, err)
	assert.Equal(t, 6, st.Packets, "discarded and untimed packets are ignored")
	assert.Equal(t, 3, st.Keyframes)
	assert.InDelta(t, 0.0, st.StartSec, 1e-9)
	assert.InDelta(t, 3.0, st.EndSec, 1e-9, "the end of the last packet")
	assert.InDelta(t, 3.0, st.DurationSec(), 1e-9)
	assert.InDelta(t, 1.7, st.MaxGapSec, 1e-9, "from the last keyframe (1.3 s) to the end (3.0 s)")

	for _, tt := range []struct {
		name    string
		packets string
		maxGap  float64
	}{
		{"gap between keyframes", `{"pts_time": "0", "duration_time": "0.5", "flags": "K__"},
			{"pts_time": "0.5", "duration_time": "0.5", "flags": "___"},
			{"pts_time": "3.0", "duration_time": "0.5", "flags": "K__"}`, 3.0},
		{"stream starts before the first keyframe", `{"pts_time": "0", "duration_time": "0.5", "flags": "___"},
			{"pts_time": "2.0", "duration_time": "0.5", "flags": "K__"}`, 2.0},
		{"no keyframe: the whole stream", `{"pts_time": "1", "duration_time": "0.5", "flags": "___"},
			{"pts_time": "2", "duration_time": "0.5", "flags": "___"}`, 1.5},
		{"keyframe at the very end", `{"pts_time": "0", "duration_time": "0.25", "flags": "K__"},
			{"pts_time": "0.25", "duration_time": "0.25", "flags": "K__"}`, 0.25},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, err := parseKeyframePackets([]byte(`{"packets": [` + tt.packets + `]}`))
			require.NoError(t, err)
			assert.InDelta(t, tt.maxGap, st.MaxGapSec, 1e-9)
		})
	}

	_, err = parseKeyframePackets([]byte(`{"packets": []}`))
	require.ErrorContains(t, err, "no timed video packets")
	_, err = parseKeyframePackets([]byte(`{}`))
	require.Error(t, err)
	_, err = parseKeyframePackets([]byte(`not json`))
	require.ErrorContains(t, err, "parse keyframe probe")
}

func TestKeyframeProbeArgs(t *testing.T) {
	args := (&FFmpeg{MaxPixels: 8912896, Threads: 2}).keyframeProbeArgs("/v.mp4")
	assert.Equal(t, "/v.mp4", args[len(args)-1])
	assert.GreaterOrEqual(t, argIndex(args, 0, "-max_pixels", strconv.Itoa(4*8912896)), 0, "the stream analysis is capped like Probe: %q", args)
	assert.NotContains(t, args, "-read_intervals", "the whole stream is read")
	assert.GreaterOrEqual(t, argIndex(args, 0, "-select_streams", "V:0"), 0, "%q", args)
	assert.GreaterOrEqual(t, argIndex(args, 0, "-show_entries", "packet=pts_time,dts_time,duration_time,flags"), 0, "%q", args)
	assert.NotContains(t, args, "-skip_frame", "packets only: nothing is decoded")
	assert.NotContains(t, args, "-show_frames")
	assert.NotContains(t, (&FFmpeg{}).keyframeProbeArgs("/v.mp4"), "-max_pixels")
}

func TestProbeKeyframes(t *testing.T) {
	needFFmpeg(t)
	f := New("", "", nil)
	ctx := context.Background()

	st, err := f.probeKeyframes(ctx, gopVideo(t, 12, 15))
	require.NoError(t, err)
	assert.Equal(t, 12*fixtureFPS, st.Packets, "the whole stream is read")
	assert.Equal(t, 24, st.Keyframes, "keyframes at 0, 0.5, ..., 11.5 s")
	assert.InDelta(t, 12.0, st.DurationSec(), 0.01)
	assert.InDelta(t, 0.5, st.MaxGapSec, 0.01)

	st, err = f.probeKeyframes(ctx, gopVideo(t, 12, 300))
	require.NoError(t, err)
	assert.Equal(t, 2, st.Keyframes, "keyframes at 0 and 10 s")
	assert.InDelta(t, 10.0, st.MaxGapSec, 0.01)

	// The last keyframe (10 s) is 5 s before the end: that tail counts as a gap.
	st, err = f.probeKeyframes(ctx, barVideoKeyframes(t, 480, 32, 1, 15, []float64{0, 5, 10}))
	require.NoError(t, err)
	assert.Equal(t, 3, st.Keyframes)
	assert.InDelta(t, 5.0, st.MaxGapSec, 0.01)

	garbage := filepath.Join(t.TempDir(), "garbage.mp4")
	require.NoError(t, os.WriteFile(garbage, []byte("not a video"), 0o644))
	_, err = f.probeKeyframes(ctx, garbage)
	var ee *ExecError
	require.ErrorAs(t, err, &ee)
}

func TestPreviewAttempts(t *testing.T) {
	assert.Equal(t, []previewPlan{{fps: 1, skip: skipNonKey}, {fps: 2, skip: skipNonRef}, {fps: 2}},
		previewAttempts(previewPlan{fps: 1, skip: skipNonKey}, 2), "keyframes only, then noref and a full decode at the requested fps")
	assert.Equal(t, []previewPlan{{fps: 2, skip: skipNonRef}, {fps: 2}}, previewAttempts(previewPlan{fps: 2, skip: skipNonRef}, 2))
	assert.Equal(t, []previewPlan{{fps: 2}}, previewAttempts(previewPlan{fps: 2}, 2))
}

func TestPreviewShortfall(t *testing.T) {
	for _, tt := range []struct {
		name           string
		frames         int
		dur            float64
		fps            int
		source, maxSec float64
		ok             bool
	}{
		{"complete", 24, 12, 2, 12, 0, true},
		{"unknown source length", 1, 0.5, 2, 0, 0, true},
		{"80% of the frames", 20, 12, 2, 12, 0, true},
		{"fewer than 80% of the frames", 19, 12, 2, 12, 0, false},
		{"one frame of a 12 s source (intra refresh under nokey)", 1, 1, 1, 12, 13, false},
		{"ends one frame interval plus slack early", 23, 11.25, 2, 12, 0, true},
		{"ends too early (frozen, cut tail)", 41, 20.5, 2, 30, 0, false},
		{"cap longer than the source: the end is checked", 22, 11, 2, 12, 13, false},
		{"capped: only the frame count", 10, 5, 2, 30, 5, true},
		{"capped: too few frames", 7, 5, 2, 30, 5, false},
		{"the iPhone preview", 59, 59, 1, 59.088, 60.088, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			why := previewShortfall(previewOutput{Frames: tt.frames, DurationSec: tt.dur}, tt.fps, tt.source, tt.maxSec)
			if tt.ok {
				assert.Empty(t, why)
			} else {
				assert.NotEmpty(t, why)
			}
		})
	}
}

// TestProbePreviewOutput: an encode that exits 0 without a single frame leaves an MP4
// without a video stream, which is an error like an unreadable file.
func TestProbePreviewOutput(t *testing.T) {
	needFFmpeg(t)
	f := New("", "", nil)
	ctx := context.Background()
	dir := t.TempDir()

	ok := filepath.Join(dir, "ok.mp4")
	_, err := f.encodePreview(ctx, gopVideo(t, 3, 15), filepath.Join(dir, ".ok.partial"), ok, 480, 0, 0, []previewPlan{{fps: 2}})
	require.NoError(t, err)
	got, err := f.probePreviewOutput(ctx, ok, 2)
	require.NoError(t, err)
	assert.Equal(t, previewOutput{Frames: 6, DurationSec: 3}, got)

	// H.264 open GOP under nokey: ffmpeg exits 0 and writes an MP4 without a stream.
	empty := filepath.Join(dir, "empty.mp4")
	src := x264Video(t, "opengop.mp4", 3, "keyint=30:min-keyint=30:scenecut=0:open-gop=1")
	_, err = f.run(ctx, 0, f.ffmpegBin(), f.previewArgs(src, empty, 480, 0, previewPlan{fps: 1, skip: skipNonKey})...)
	require.NoError(t, err, "fixture: ffmpeg does not fail")
	_, err = f.probePreviewOutput(ctx, empty, 1)
	require.ErrorContains(t, err, "no video stream")

	garbage := filepath.Join(dir, "garbage.mp4")
	require.NoError(t, os.WriteFile(garbage, []byte("not a video"), 0o644))
	_, err = f.probePreviewOutput(ctx, garbage, 2)
	require.Error(t, err)
	assert.True(t, isExitFailure(err), "%v", err)
}

// previewFrames decodes every frame of a preview to JPEG, in order.
func previewFrames(t *testing.T, preview string) []string {
	t.Helper()
	dir := t.TempDir()
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-loglevel", "error",
		"-i", preview, "-fps_mode", "passthrough", "-q:v", "2", filepath.Join(dir, "%04d.jpg")).CombinedOutput(); err != nil {
		t.Fatalf("dump preview frames: %v\n%s", err, b)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = filepath.Join(dir, e.Name())
	}
	sort.Strings(paths)
	return paths
}

// TestBuildPreviewKeyframesOnly: with a keyframe every 15 frames (2 per second) only
// keyframes are decoded; the preview runs at min(requested, 2) fps and frame k shows
// the source at k/FPS (on a bar video, frame k is exactly keyframe 15k/(2/FPS)).
func TestBuildPreviewKeyframesOnly(t *testing.T) {
	const w, h, px, dur = 640, 360, 3, 6.0
	src := barVideoGOP(t, w, h, px, dur, 15)
	f := New("", "", nil)
	f.Threads = 2

	for _, tt := range []struct {
		requested int
		want      PreviewInfo
	}{
		{2, PreviewInfo{FPS: 2, KeyframesOnly: true}},
		{4, PreviewInfo{FPS: 2, KeyframesOnly: true}}, // never faster than the keyframes
		{1, PreviewInfo{FPS: 1, KeyframesOnly: true}},
	} {
		t.Run(fmt.Sprintf("requested %d fps", tt.requested), func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "preview.mp4")
			info, err := f.BuildPreview(context.Background(), src, out, 480, tt.requested, 0)
			require.NoError(t, err)
			require.Equal(t, tt.want, info)

			streams := probeStreams(t, out)
			require.Len(t, streams, 1)
			assert.Equal(t, fmt.Sprintf("%v/1", info.FPS), streams[0].AvgFrameRate)
			p := probeFile(t, out)
			assert.InDelta(t, dur, p.duration(t), 1e-6, "no keyframe is lost at the end")
			wantFrames := int(dur * info.FPS)
			assert.Equal(t, strconv.Itoa(wantFrames), p.Streams[0].NbFrames)

			frames := previewFrames(t, out)
			require.Len(t, frames, wantFrames)
			step := int(fixtureFPS / info.FPS)
			for k, fr := range frames {
				assert.Equal(t, k*step, barFrame(t, fr, w, px), "preview frame %d shows the source at %v s", k, float64(k)/info.FPS)
			}
		})
	}

	t.Run("duration cap uses the real fps", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "preview.mp4")
		info, err := f.BuildPreview(context.Background(), src, out, 480, 2, 2.2)
		require.NoError(t, err)
		require.True(t, info.KeyframesOnly)
		p := probeFile(t, out)
		assert.Equal(t, strconv.Itoa(previewFrameCap(2.2, 2)), p.Streams[0].NbFrames)
		assert.LessOrEqual(t, p.duration(t), 2.2+1/info.FPS)
	})
}

// TestBuildPreviewSparseKeyframes: with one keyframe per 10 s the noref path keeps the
// requested fps, and its frames are within a few source frames of a full decode's.
func TestBuildPreviewSparseKeyframes(t *testing.T) {
	const w, h, px, dur = 640, 360, 3, 6.0
	src := barVideoGOP(t, w, h, px, dur, 300)
	f := New("", "", nil)
	f.Threads = 2

	out := filepath.Join(t.TempDir(), "preview.mp4")
	info, err := f.BuildPreview(context.Background(), src, out, 480, 2, 0)
	require.NoError(t, err)
	require.Equal(t, PreviewInfo{FPS: 2}, info)
	streams := probeStreams(t, out)
	require.Len(t, streams, 1)
	assert.Equal(t, "2/1", streams[0].AvgFrameRate)
	p := probeFile(t, out)
	assert.InDelta(t, dur, p.duration(t), 1e-6)
	assert.Equal(t, "12", p.Streams[0].NbFrames)

	// Reference: the same preview from a full decode.
	dir := t.TempDir()
	full := filepath.Join(dir, "full.mp4")
	ref, err := f.encodePreview(context.Background(), src, filepath.Join(dir, ".full.partial"), full, 480, 0, 0, []previewPlan{{fps: 2}})
	require.NoError(t, err)
	require.Equal(t, PreviewInfo{FPS: 2}, ref)

	got, want := previewFrames(t, out), previewFrames(t, full)
	require.Len(t, got, len(want))
	for k := range got {
		g, r := barFrame(t, got[k], w, px), barFrame(t, want[k], w, px)
		assert.LessOrEqual(t, math.Abs(float64(g-r)), 3.0, "preview frame %d: source frame %d, full decode %d", k, g, r)
	}
}

// TestBuildPreviewProbeFailureFallsBack: when the keyframe probe fails, non-reference
// frames are skipped at the requested fps; with the source length unknown the preview
// only has to have frames.
func TestBuildPreviewProbeFailureFallsBack(t *testing.T) {
	src := gopVideo(t, 4, 15) // would take the keyframes-only path
	var logs bytes.Buffer
	f := New("", failingPacketProbe(t), slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	out := filepath.Join(t.TempDir(), "preview.mp4")
	info, err := f.BuildPreview(context.Background(), src, out, 480, 2, 0)
	require.NoError(t, err)
	assert.Equal(t, PreviewInfo{FPS: 2}, info)
	assert.Equal(t, "8", probeFile(t, out).Streams[0].NbFrames)
	assert.Contains(t, logs.String(), "keyframe probe failed")
	assert.NotContains(t, logs.String(), "retrying")
}

// failingPacketProbe returns an ffprobe wrapper that fails the keyframe (packet) probe
// and runs the real ffprobe for everything else.
func failingPacketProbe(t *testing.T) string {
	t.Helper()
	needFFmpeg(t)
	real, err := exec.LookPath("ffprobe")
	require.NoError(t, err)
	script := filepath.Join(t.TempDir(), "ffprobe")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\n"+
		"case \"$*\" in *packet=*) echo 'fake: packet probe refused' >&2; exit 1;; esac\n"+
		"exec "+strconv.Quote(real)+" \"$@\"\n"), 0o755))
	return script
}

// TestEncodePreviewFallbackChain: an unusable attempt (ffmpeg fails, or exits 0 with a
// preview that misses frames) moves on to the next decode mode; the last attempt only
// needs a frame, and the returned PreviewInfo describes the attempt that was kept.
func TestEncodePreviewFallbackChain(t *testing.T) {
	src := gopVideo(t, 3, 15)
	ctx := context.Background()
	run := func(t *testing.T, sourceSec float64, attempts ...previewPlan) (PreviewInfo, string, string, error) {
		t.Helper()
		var logs bytes.Buffer
		f := New("", "", slog.New(slog.NewTextHandler(&logs, nil)))
		dir := t.TempDir()
		tmp, out := filepath.Join(dir, ".p.partial"), filepath.Join(dir, "p.mp4")
		info, err := f.encodePreview(ctx, src, tmp, out, 480, 0, sourceSec, attempts)
		if err == nil {
			assert.NoFileExists(t, tmp, "renamed")
		}
		return info, out, logs.String(), err
	}

	t.Run("ffmpeg fails", func(t *testing.T) {
		// Fixture sanity: under "all" (a decoder that treats no frame as a keyframe)
		// ffmpeg exits with a failure.
		f := New("", "", nil)
		tmp := filepath.Join(t.TempDir(), "p.mp4")
		_, err := f.run(ctx, 0, f.ffmpegBin(), f.previewArgs(src, tmp, 480, 0, previewPlan{fps: 1, skip: "all"})...)
		require.True(t, isNoFramesFailure(err), "%v", err)

		info, out, logs, err := run(t, 3, previewPlan{fps: 1, skip: "all"}, previewPlan{fps: 2, skip: "all"}, previewPlan{fps: 2})
		require.NoError(t, err)
		assert.Equal(t, PreviewInfo{FPS: 2}, info, "the full decode runs at the requested fps")
		assert.Equal(t, "6", probeFile(t, out).Streams[0].NbFrames)
		assert.Equal(t, 2, strings.Count(logs, "retrying"), logs)
	})
	t.Run("incomplete preview", func(t *testing.T) {
		// The source "should" be 60 s long: the 3 s noref preview misses frames.
		info, out, logs, err := run(t, 60, previewPlan{fps: 2, skip: skipNonRef}, previewPlan{fps: 2})
		require.NoError(t, err)
		assert.Equal(t, PreviewInfo{FPS: 2}, info)
		assert.Equal(t, "6", probeFile(t, out).Streams[0].NbFrames, "the last attempt is kept as it is")
		assert.Contains(t, logs, "is incomplete")
		assert.Contains(t, logs, "retrySkipFrame=\"\"")
	})
	t.Run("usable first attempt", func(t *testing.T) {
		info, _, logs, err := run(t, 3, previewPlan{fps: 2, skip: skipNonKey}, previewPlan{fps: 2, skip: skipNonRef}, previewPlan{fps: 2})
		require.NoError(t, err)
		assert.Equal(t, PreviewInfo{FPS: 2, KeyframesOnly: true}, info)
		assert.NotContains(t, logs, "retrying")
	})
	t.Run("the last attempt needs a frame", func(t *testing.T) {
		info, out, _, err := run(t, 3, previewPlan{fps: 1, skip: "all"})
		require.Error(t, err)
		assert.Zero(t, info)
		assert.NoFileExists(t, out)

		og := x264Video(t, "opengop.mp4", 3, "keyint=30:min-keyint=30:scenecut=0:open-gop=1")
		dir := t.TempDir()
		_, err = New("", "", nil).encodePreview(ctx, og, filepath.Join(dir, ".p.partial"), filepath.Join(dir, "p.mp4"),
			480, 0, 0, []previewPlan{{fps: 1, skip: skipNonKey}})
		require.ErrorContains(t, err, "no video stream", "ffmpeg exited 0 without writing a frame")
		assert.NoFileExists(t, filepath.Join(dir, "p.mp4"))
	})
	t.Run("ffmpeg does not run", func(t *testing.T) {
		var logs bytes.Buffer
		f := New(filepath.Join(t.TempDir(), "no-ffmpeg"), "", slog.New(slog.NewTextHandler(&logs, nil)))
		dir := t.TempDir()
		_, err := f.encodePreview(ctx, src, filepath.Join(dir, ".p.partial"), filepath.Join(dir, "p.mp4"),
			480, 0, 3, previewAttempts(previewPlan{fps: 2, skip: skipNonKey}, 2))
		var ee *ExecError
		require.ErrorAs(t, err, &ee)
		assert.NotContains(t, logs.String(), "retrying", "another decode mode would not help")
	})
}

// TestBuildPreviewBrokenKeyframes: H.264 open GOP and intra refresh flag non-IDR
// recovery points as keyframes, so the probe picks keyframes only, but the H.264
// decoder decodes only IDR pictures under -skip_frame nokey and ffmpeg still exits 0
// (open GOP: no frame at all; intra refresh: one). The preview is read back, rejected
// and rebuilt with noref at the requested fps.
func TestBuildPreviewBrokenKeyframes(t *testing.T) {
	const dur = 12.0
	for _, tt := range []struct{ name, params string }{
		{"open GOP", "keyint=30:min-keyint=30:scenecut=0:open-gop=1"},
		{"intra refresh", "keyint=30:intra-refresh=1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := x264Video(t, "src.mp4", dur, tt.params)
			var logs bytes.Buffer
			f := New("", "", slog.New(slog.NewTextHandler(&logs, nil)))
			f.Threads = 2
			ctx := context.Background()

			st, err := f.probeKeyframes(ctx, src)
			require.NoError(t, err)
			require.Equal(t, previewPlan{fps: 1, skip: skipNonKey}, planPreview(st, 2), "fixture: a keyframe about every second")
			// Fixture sanity: the keyframes-only encode "succeeds" but is unusable.
			dir := t.TempDir()
			retry, err := f.tryPreview(ctx, src, filepath.Join(dir, "p.mp4"), 480, dur+1, st.DurationSec(), planPreview(st, 2), false)
			require.Error(t, err)
			require.True(t, retry)

			out := filepath.Join(t.TempDir(), "preview.mp4")
			info, err := f.BuildPreview(ctx, src, out, 480, 2, dur+1)
			require.NoError(t, err)
			assert.Equal(t, PreviewInfo{FPS: 2}, info, "the preview that was kept: noref at the requested fps")
			assert.Contains(t, logs.String(), "retrySkipFrame=noref")
			p := probeFile(t, out)
			assert.InDelta(t, dur, p.duration(t), 1e-6)
			assert.Equal(t, "24", p.Streams[0].NbFrames)
			assert.Len(t, previewFrames(t, out), 24, "every frame decodes")
		})
	}
}

func TestParseDecodedFrames(t *testing.T) {
	out := []byte("frame:0    pts:0       pts_time:0\nv2i.decoded=1\n" +
		"frame:1    pts:560     pts_time:0.933333\nv2i.decoded=1\n" +
		"frame:2    pts:NOPTS   pts_time:NOPTS\nv2i.decoded=1\n" +
		"frame:3    pts:35304   pts_time:58.84\r\nv2i.decoded=1\n" +
		"stray output\nframe:4 without a time\n" +
		"frame:5    pts:36000   pts_time:60") // no final newline
	assert.Equal(t, []float64{0, 0.933333, 58.84, 60}, parseDecodedFrames(out))
	assert.Empty(t, parseDecodedFrames(nil))
}

func TestDecodedGapShortfall(t *testing.T) {
	seq := func(from, to, step float64) []float64 {
		var ts []float64
		for v := from; v <= to+1e-9; v += step {
			ts = append(ts, v)
		}
		return ts
	}
	for _, tt := range []struct {
		name  string
		times []float64
		fps   int
		ok    bool
	}{
		{"iPhone: every keyframe decoded", seq(0, 58.84, 0.933333), 1, true},
		{"largest planned gap (1.25 s at 1 fps)", seq(0, 30, 1.25), 1, true},
		{"within the timestamp slack", []float64{0, 1.34, 2.5}, 1, true},
		{"one keyframe skipped", []float64{0, 1, 3, 4}, 1, false},
		{"IDR only (intra refresh with an IDR every 10 s)", []float64{0, 10, 20, 29.5}, 1, false},
		{"2 fps: a keyframe every 0.5 s", seq(0, 10, 0.5), 2, true},
		{"2 fps: one keyframe skipped", []float64{0, 0.5, 1.5, 2}, 2, false},
		{"order does not matter", []float64{2, 0, 1}, 1, true},
		{"a single frame", []float64{3}, 1, true}, // the frame count and length checks catch this
		{"nothing reported", nil, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			why := decodedGapShortfall(slices.Clone(tt.times), tt.fps)
			if tt.ok {
				assert.Empty(t, why)
			} else {
				assert.NotEmpty(t, why)
			}
		})
	}
}

// TestDecodedFramesFilter: in a keyframes-only encode the frames the decoder produced
// are printed on stdout, and the filter leaves the preview byte for byte unchanged.
func TestDecodedFramesFilter(t *testing.T) {
	needFFmpeg(t)
	src := gopVideo(t, 3, 15)
	f := New("", "", nil)
	f.Threads = 2
	dir := t.TempDir()
	plan := previewPlan{fps: 2, skip: skipNonKey}

	withFilter := filepath.Join(dir, "with.mp4")
	stdout, err := f.run(context.Background(), 0, f.ffmpegBin(), f.previewArgs(src, withFilter, 480, 0, plan)...)
	require.NoError(t, err)
	assert.Equal(t, []float64{0, 0.5, 1, 1.5, 2, 2.5}, parseDecodedFrames(stdout), "one line per decoded keyframe:\n%s", stdout)

	args := f.previewArgs(src, filepath.Join(dir, "without.mp4"), 480, 0, plan)
	vf := slices.Index(args, "-vf") + 1
	require.Contains(t, args[vf], decodedFramesFilter+",")
	args[vf] = strings.Replace(args[vf], decodedFramesFilter+",", "", 1)
	stdout, err = f.run(context.Background(), 0, f.ffmpegBin(), args...)
	require.NoError(t, err)
	require.Empty(t, stdout)

	a, err := os.ReadFile(withFilter)
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(dir, "without.mp4"))
	require.NoError(t, err)
	assert.True(t, bytes.Equal(a, b), "the preview must not change (%d vs %d bytes)", len(a), len(b))
}

// TestBuildPreviewSkippedKeyframes: intra refresh (recovery points flagged as keyframes
// about every second) with an IDR picture forced at 0, 10, 20 and 29.5 s. The H.264
// decoder decodes only the IDR pictures under -skip_frame nokey, so the keyframes-only
// preview shows three pictures for 30 s, yet it has every frame and reaches the end:
// only the decoded-frame timestamps give it away, and the preview is rebuilt with noref.
func TestBuildPreviewSkippedKeyframes(t *testing.T) {
	const w, h, px, dur = 1800, 32, 2, 30.0
	src := barVideoX264(t, w, h, px, dur, "keyint=30:intra-refresh=1", []float64{0, 10, 20, 29.5})
	var logs bytes.Buffer
	f := New("", "", slog.New(slog.NewTextHandler(&logs, nil)))
	f.Threads = 2
	ctx := context.Background()

	st, err := f.probeKeyframes(ctx, src)
	require.NoError(t, err)
	plan := planPreview(st, 2)
	require.Equal(t, previewPlan{fps: 1, skip: skipNonKey}, plan, "fixture: a keyframe about every second")

	// Fixture sanity: the keyframes-only preview passes the frame count and length
	// checks; only the decoded frames show that it froze.
	dir := t.TempDir()
	tmp := filepath.Join(dir, "p.mp4")
	stdout, err := f.run(ctx, 0, f.ffmpegBin(), f.previewArgs(src, tmp, 480, dur+1, plan)...)
	require.NoError(t, err)
	got, err := f.probePreviewOutput(ctx, tmp, plan.fps)
	require.NoError(t, err)
	require.Empty(t, previewShortfall(got, plan.fps, st.DurationSec(), dur+1), "fixture: %+v", got)
	require.Contains(t, decodedGapShortfall(parseDecodedFrames(stdout), plan.fps), "skipped keyframes")
	frames := previewFrames(t, tmp)
	require.Len(t, frames, 30)
	assert.Equal(t, 0, barFrame(t, frames[9], w, px), "fixture: preview frame 9 (9 s) still shows the first IDR")

	out := filepath.Join(t.TempDir(), "preview.mp4")
	info, err := f.BuildPreview(ctx, src, out, 480, 2, dur+1)
	require.NoError(t, err)
	assert.Equal(t, PreviewInfo{FPS: 2}, info, "rebuilt with noref at the requested fps")
	assert.Contains(t, logs.String(), "skipped keyframes")
	assert.Contains(t, logs.String(), "retrySkipFrame=noref")
	frames = previewFrames(t, out)
	full := filepath.Join(dir, "full.mp4")
	_, err = f.encodePreview(ctx, src, filepath.Join(dir, ".full.partial"), full, 480, 0, 0, []previewPlan{{fps: 2}})
	require.NoError(t, err)
	want := previewFrames(t, full)
	require.Len(t, frames, int(dur*2))
	require.Len(t, want, len(frames))
	for k := range frames {
		assert.InDelta(t, barFrame(t, want[k], w, px), barFrame(t, frames[k], w, px), 3, "preview frame %d (%v s)", k, float64(k)/2)
	}
}

// TestBuildPreviewKeyframeLayouts: the largest keyframe gap over the whole stream, not
// the density of its first seconds, decides the decode mode. Each source is a 30 s bar
// video, so every preview frame can be mapped back to the source frame it shows.
func TestBuildPreviewKeyframeLayouts(t *testing.T) {
	const w, h, px, dur = 1800, 32, 2, 30.0
	every := func(from, to, step float64) []float64 {
		var ts []float64
		for i := 0; from+float64(i)*step < to-1e-9; i++ {
			ts = append(ts, from+float64(i)*step)
		}
		return ts
	}
	for _, tt := range []struct {
		name   string
		keys   []float64
		maxGap float64
		want   PreviewInfo
	}{
		// Keyframes every 0.5 s for 10 s, then at 10 and 20 s: nokey at 2 fps would
		// freeze 10–20 s and lose 20–30 s.
		{"dense first 10 s", append(every(0, 10, 0.5), 10, 20), 10, PreviewInfo{FPS: 2}},
		// 10 keyframes in the first second, then one every 10 s.
		{"clustered", append(every(0, 1, 0.1), 10, 20), 10, PreviewInfo{FPS: 2}},
		// Keyframes every 0.5 s up to 25 s, none in the last 5 s.
		{"sparse tail", every(0, 25.5, 0.5), 5, PreviewInfo{FPS: 2}},
		// Control: a keyframe every second.
		{"uniform", every(0, 30, 1), 1, PreviewInfo{FPS: 1, KeyframesOnly: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := barVideoKeyframes(t, w, h, px, dur, tt.keys)
			var logs bytes.Buffer
			f := New("", "", slog.New(slog.NewTextHandler(&logs, nil)))
			st, err := f.probeKeyframes(context.Background(), src)
			require.NoError(t, err)
			require.Equal(t, len(tt.keys), st.Keyframes, "fixture: keyframes exactly where forced")
			require.InDelta(t, tt.maxGap, st.MaxGapSec, 0.01)

			out := filepath.Join(t.TempDir(), "preview.mp4")
			info, err := f.BuildPreview(context.Background(), src, out, 480, 2, dur+1)
			require.NoError(t, err)
			require.Equal(t, tt.want, info)
			assert.NotContains(t, logs.String(), "retrying", "the planned mode is usable")

			p := probeFile(t, out)
			assert.InDelta(t, dur, p.duration(t), 1e-6, "the whole source is covered")
			frames := previewFrames(t, out)
			require.Len(t, frames, int(dur*info.FPS))
			if info.KeyframesOnly {
				// Preview frame k is exactly keyframe k.
				step := int(fixtureFPS / info.FPS)
				for k, fr := range frames {
					assert.Equal(t, k*step, barFrame(t, fr, w, px), "preview frame %d", k)
				}
				return
			}
			// noref: within a few source frames of what a full decode shows.
			dir := t.TempDir()
			full := filepath.Join(dir, "full.mp4")
			_, err = f.encodePreview(context.Background(), src, filepath.Join(dir, ".full.partial"), full, 480, 0, 0, []previewPlan{{fps: 2}})
			require.NoError(t, err)
			want := previewFrames(t, full)
			require.Len(t, want, len(frames))
			for k := range frames {
				assert.InDelta(t, barFrame(t, want[k], w, px), barFrame(t, frames[k], w, px), 3, "preview frame %d (%v s)", k, float64(k)/info.FPS)
			}
		})
	}
}

// TestBuildPreviewPixelLimitNotRetried: a frame above MaxPixels fails the preview
// without the full-decode retry (it would fail the same way).
func TestBuildPreviewPixelLimitNotRetried(t *testing.T) {
	for name, src := range map[string]string{
		"keyframes only": gopVideo(t, 3, 15),
		"noref":          gopVideo(t, 3, 300),
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			f := New("", "", slog.New(slog.NewTextHandler(&logs, nil)))
			f.MaxPixels = 320*240 - 1
			out := filepath.Join(t.TempDir(), "preview.mp4")
			_, err := f.BuildPreview(context.Background(), src, out, 480, 2, 0)
			require.ErrorIs(t, err, ErrPixelLimit)
			var ee *ExecError
			require.ErrorAs(t, err, &ee)
			assert.Contains(t, ee.Cmd, "-skip_frame:v")
			assert.NotContains(t, logs.String(), "retrying")
			assert.NoFileExists(t, out)
		})
	}
}
