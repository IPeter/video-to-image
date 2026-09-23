package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the decode limits (MaxPixels, Threads), the preview duration
// cap and metadata stripping.

// argIndex returns the index of the first "opt value" pair in args at or after from,
// or -1.
func argIndex(args []string, from int, opt, value string) int {
	for i := from; i+1 < len(args); i++ {
		if args[i] == opt && args[i+1] == value {
			return i
		}
	}
	return -1
}

func countPairs(args []string, opt, value string) int {
	n := 0
	for i := 0; i+1 < len(args); i++ {
		if args[i] == opt && args[i+1] == value {
			n++
		}
	}
	return n
}

func TestMaxPixelsArgs(t *testing.T) {
	assert.Nil(t, maxPixelsArgs(0))
	assert.Nil(t, maxPixelsArgs(-5))
	assert.Equal(t, []string{"-max_pixels", "8912896"}, maxPixelsArgs(8912896))
	assert.Equal(t, []string{"-max_pixels", "2147483647"}, maxPixelsArgs(1<<40), "clamped to ffmpeg's INT_MAX range")

	for _, tt := range []struct{ max, want int64 }{
		{0, 0},
		{-1, 0},
		{8912896, 4 * 8912896},
		{math.MaxInt32 / 4, math.MaxInt32 / 4 * 4},
		{math.MaxInt32/4 + 1, math.MaxInt32},
		{math.MaxInt64, math.MaxInt32}, // no overflow
	} {
		assert.Equal(t, tt.want, (&FFmpeg{MaxPixels: tt.max}).probeMaxPixels(), "MaxPixels=%d", tt.max)
	}
}

func TestPreviewFrameCap(t *testing.T) {
	for _, tt := range []struct {
		dur  float64
		fps  int
		want int
	}{
		{180, 2, 360},
		{5, 2, 10},
		{4.9, 2, 10},
		{5.25, 2, 11},
		{0.3, 2, 1},
		{1e-9, 2, 1},
		{0.7, 10, 7}, // 7.000000000000001 without the epsilon → 8
	} {
		assert.Equal(t, tt.want, previewFrameCap(tt.dur, tt.fps), "dur=%v fps=%d", tt.dur, tt.fps)
	}
}

func TestPreviewArgsLimits(t *testing.T) {
	f := &FFmpeg{MaxPixels: 8912896, Threads: 2}
	for _, tt := range []struct {
		plan      previewPlan
		frameCap  string
		fpsFilter string
	}{
		{previewPlan{fps: 2, skip: skipNonRef}, "360", "fps=2"},
		// The frame cap follows the preview's real (keyframe-limited) rate, and the
		// frames the decoder produced are printed (see decodedGapShortfall).
		{previewPlan{fps: 1, skip: skipNonKey}, "180", decodedFramesFilter + ",fps=1:eof_action=pass"},
	} {
		args := f.previewArgs("/in.mp4", "/out.mp4", 480, 180, tt.plan)
		in := argIndex(args, 0, "-i", "/in.mp4")
		require.Positive(t, in, "%q", args)

		for _, pair := range [][2]string{
			{"-max_pixels", "8912896"}, {"-threads", "2"}, {"-t", "180"}, {"-filter_threads", "2"},
			{"-skip_frame:v", tt.plan.skip},
		} {
			i := argIndex(args, 0, pair[0], pair[1])
			assert.True(t, i >= 0 && i < in, "%s %s must be an input option (before -i): %q", pair[0], pair[1], args)
		}
		for _, pair := range [][2]string{
			{"-threads", "2"}, // encoder
			{"-frames:v", tt.frameCap},
			{"-map_metadata", "-1"}, {"-map_chapters", "-1"},
			{"-fflags", "+bitexact"}, {"-flags:v", "+bitexact"},
			{"-vf", "scale=-2:'min(480,trunc(ih/2)*2)'," + tt.fpsFilter},
		} {
			assert.Positive(t, argIndex(args, in, pair[0], pair[1]), "%s %s must be an output option (after -i): %q", pair[0], pair[1], args)
		}
		assert.Equal(t, 1, countPairs(args, "-skip_frame:v", tt.plan.skip))
		assert.Equal(t, "/out.mp4", args[len(args)-1])
	}

	// No limits configured and a full decode: none of the optional options appear.
	args := (&FFmpeg{}).previewArgs("/in.mp4", "/out.mp4", 480, 0, previewPlan{fps: 2})
	for _, opt := range []string{"-max_pixels", "-threads", "-filter_threads", "-t", "-frames:v", "-skip_frame:v"} {
		assert.NotContains(t, args, opt)
	}
	assert.Positive(t, argIndex(args, 0, "-map_metadata", "-1"), "metadata is always stripped")
	assert.Positive(t, argIndex(args, 0, "-vf", "scale=-2:'min(480,trunc(ih/2)*2)',fps=2"))
}

func TestExtractArgsLimits(t *testing.T) {
	f := &FFmpeg{MaxPixels: 8912896, Threads: 3}
	a := ExtractArgs{Input: "/in.mp4", StartSec: 1.5, DurationSec: 2, FPS: 4}.withDefaults()
	args := f.extractArgs(a, "/in.mp4", "/c/0001_%04d.jpg", "/s/0001_%04d.jpg")
	in := argIndex(args, 0, "-i", "/in.mp4")
	require.Positive(t, in, "%q", args)

	for _, pair := range [][2]string{{"-max_pixels", "8912896"}, {"-threads", "3"}, {"-filter_complex_threads", "3"}, {"-ss", "1.5"}, {"-t", "2"}} {
		i := argIndex(args, 0, pair[0], pair[1])
		assert.True(t, i >= 0 && i < in, "%s %s must come before -i: %q", pair[0], pair[1], args)
	}
	// Output options apply per output file: each JPEG sequence needs its own.
	fullAt := slices.Index(args, "/c/0001_%04d.jpg")
	smallAt := slices.Index(args, "/s/0001_%04d.jpg")
	require.True(t, in < fullAt && fullAt < smallAt, "%q", args)
	for _, pair := range [][2]string{
		{"-threads", "3"}, {"-frames:v", "8"},
		{"-map_metadata", "-1"}, {"-map_chapters", "-1"}, {"-fflags", "+bitexact"}, {"-flags:v", "+bitexact"},
	} {
		i := argIndex(args, in, pair[0], pair[1])
		assert.True(t, i > in && i < fullAt, "%s %s before the full-size output: %q", pair[0], pair[1], args)
		j := argIndex(args, fullAt, pair[0], pair[1])
		assert.True(t, j > fullAt && j < smallAt, "%s %s before the small output: %q", pair[0], pair[1], args)
	}

	args = (&FFmpeg{}).extractArgs(a, "/in.mp4", "/c/0001_%04d.jpg", "/s/0001_%04d.jpg")
	for _, opt := range []string{"-max_pixels", "-threads", "-filter_complex_threads"} {
		assert.NotContains(t, args, opt)
	}
	assert.Equal(t, 2, countPairs(args, "-frames:v", "8"))
}

func TestProbeArgsLimits(t *testing.T) {
	args := (&FFmpeg{MaxPixels: 8912896, Threads: 2}).probeArgs("/v.mp4")
	i := argIndex(args, 0, "-max_pixels", strconv.Itoa(4*8912896))
	assert.True(t, i >= 0 && i < slices.Index(args, "/v.mp4"), "%q", args)
	assert.NotContains(t, args, "-threads", "ffprobe already probes single-threaded")
	assert.NotContains(t, (&FFmpeg{}).probeArgs("/v.mp4"), "-max_pixels")
}

// colourVideo is a flat-colour H.264 clip (cheap to generate at any size).
func colourVideo(t testing.TB, name, colour string, w, h int, durSec float64) string {
	t.Helper()
	return ffmpegGen(t, name, "-f", "lavfi",
		"-i", "color=c="+colour+":s="+strconv.Itoa(w)+"x"+strconv.Itoa(h)+":r=30:d="+formatFloat(durSec),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p")
}

// resolutionSwitchVideo is 1 s of red at 320x240 followed by 1 s of blue at 1280x720
// in ONE H.264 stream (a new SPS mid-stream), in MP4. The container and the first SPS
// say 320x240, so only the decoder can notice the larger frames.
func resolutionSwitchVideo(t testing.TB) string {
	t.Helper()
	a := ffmpegGen(t, "a.h264", "-f", "lavfi", "-i", "color=c=red:s=320x240:r=30:d=1",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-f", "h264")
	b := ffmpegGen(t, "b.h264", "-f", "lavfi", "-i", "color=c=blue:s=1280x720:r=30:d=1",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-f", "h264")
	ab := filepath.Join(t.TempDir(), "ab.h264")
	var raw []byte
	for _, p := range []string{a, b} {
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		raw = append(raw, data...)
	}
	require.NoError(t, os.WriteFile(ab, raw, 0o644))
	return ffmpegGen(t, "switch.mp4", "-f", "h264", "-framerate", "30", "-i", ab, "-c", "copy")
}

type probedFile struct {
	Streams []struct {
		NbFrames string            `json:"nb_frames"`
		Tags     map[string]string `json:"tags"`
	} `json:"streams"`
	Chapters []json.RawMessage `json:"chapters"`
	Format   struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
}

func probeFile(t testing.TB, path string) probedFile {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration:format_tags:stream=nb_frames:stream_tags",
		"-show_chapters", "-of", "json", path).Output()
	require.NoError(t, err)
	var p probedFile
	require.NoError(t, json.Unmarshal(out, &p))
	return p
}

func (p probedFile) duration(t testing.TB) float64 {
	t.Helper()
	d, err := strconv.ParseFloat(p.Format.Duration, 64)
	require.NoError(t, err, "duration %q", p.Format.Duration)
	return d
}

// TestProbeMaxPixels: Probe caps decoding at 4×MaxPixels. A video between MaxPixels
// and 4×MaxPixels still probes (so the API can say RESOLUTION_TOO_HIGH); beyond that
// the probe fails cheaply as CANNOT_DECODE.
func TestProbeMaxPixels(t *testing.T) {
	needFFmpeg(t)
	const maxPixels = 320 * 240                               // probe cap 307200
	sane := colourVideo(t, "sane.mp4", "gray", 640, 360, 1)   // 230400: over MaxPixels, under 4×
	bomb := colourVideo(t, "bomb.mp4", "gray", 800, 600, 1)   // 480000: over 4×
	exact := colourVideo(t, "exact.mp4", "gray", 640, 480, 1) // 307200: exactly 4× passes

	f := New("", "", nil)
	f.MaxPixels = maxPixels
	for _, tt := range []struct {
		path string
		w, h int
	}{{sane, 640, 360}, {exact, 640, 480}} {
		info, err := f.Probe(context.Background(), tt.path)
		require.NoError(t, err, tt.path)
		assert.Equal(t, [2]int{tt.w, tt.h}, [2]int{info.Width, info.Height})
	}

	_, err := f.Probe(context.Background(), bomb)
	require.ErrorIs(t, err, ErrCannotDecode)
	require.ErrorIs(t, err, ErrPixelLimit)
	var ee *ExecError
	require.ErrorAs(t, err, &ee)
	assert.Contains(t, ee.Cmd, "-max_pixels 307200")

	// Without a cap the same file probes fine (the cap is what rejects it).
	info, err := New("", "", nil).Probe(context.Background(), bomb)
	require.NoError(t, err)
	assert.Equal(t, 800, info.Width)
}

// TestBuildPreviewMaxPixels: the preview decoder refuses frames above MaxPixels, both
// when the file is oversized from the start and after a mid-stream resolution change.
func TestBuildPreviewMaxPixels(t *testing.T) {
	needFFmpeg(t)
	src := colourVideo(t, "src.mp4", "gray", 640, 360, 1)

	t.Run("at the cap", func(t *testing.T) {
		f := New("", "", nil)
		f.MaxPixels, f.Threads = 640*360, 2
		out := filepath.Join(t.TempDir(), "preview.mp4")
		_, err := f.BuildPreview(context.Background(), src, out, 480, 2, 0)
		require.NoError(t, err)
		assert.FileExists(t, out)
	})
	t.Run("above the cap", func(t *testing.T) {
		f := New("", "", nil)
		f.MaxPixels = 640*360 - 1
		out := filepath.Join(t.TempDir(), "preview.mp4")
		_, err := f.BuildPreview(context.Background(), src, out, 480, 2, 0)
		require.ErrorIs(t, err, ErrPixelLimit)
		var ee *ExecError
		require.ErrorAs(t, err, &ee)
		assert.NoFileExists(t, out)
		entries, err := os.ReadDir(filepath.Dir(out))
		require.NoError(t, err)
		assert.Empty(t, entries, "partial output removed")
	})
	t.Run("mid-stream resolution change", func(t *testing.T) {
		sw := resolutionSwitchVideo(t)
		f := New("", "", nil)
		f.MaxPixels = 100_000 // 320x240 passes, 1280x720 does not
		out := filepath.Join(t.TempDir(), "preview.mp4")
		_, err := f.BuildPreview(context.Background(), sw, out, 480, 2, 0)
		require.NoError(t, err)
		// Only the red 320x240 second was decoded (uncapped, the preview lasts 2 s).
		assert.InDelta(t, 1.0, probeFile(t, out).duration(t), 0.05)

		uncapped := filepath.Join(t.TempDir(), "uncapped.mp4")
		_, err = New("", "", nil).BuildPreview(context.Background(), sw, uncapped, 480, 2, 0)
		require.NoError(t, err)
		assert.InDelta(t, 2.0, probeFile(t, uncapped).duration(t), 0.1, "fixture sanity: the stream really switches")
	})
}

// TestExtractFramesMaxPixels: ExtractFrames refuses oversized sources and never
// decodes frames above the cap after a mid-stream resolution change.
func TestExtractFramesMaxPixels(t *testing.T) {
	needFFmpeg(t)

	t.Run("above the cap", func(t *testing.T) {
		src := colourVideo(t, "src.mp4", "gray", 640, 360, 1)
		dir := t.TempDir()
		f := New("", "", nil)
		f.MaxPixels, f.Threads = 320*240, 2
		_, err := f.ExtractFrames(context.Background(), ExtractArgs{
			Input: src, StartSec: 0, DurationSec: 1, FPS: 4,
			FullPattern:  filepath.Join(dir, "cand", "000_%04d.jpg"),
			SmallPattern: filepath.Join(dir, "small", "000_%04d.jpg"),
		})
		require.ErrorIs(t, err, ErrPixelLimit)
		entries, err := os.ReadDir(filepath.Join(dir, "cand"))
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("mid-stream resolution change", func(t *testing.T) {
		sw := resolutionSwitchVideo(t)
		extract := func(f *FFmpeg) *ExtractResult {
			dir := t.TempDir()
			res, err := f.ExtractFrames(context.Background(), ExtractArgs{
				Input: sw, StartSec: 0, DurationSec: 1.9, FPS: 4,
				FullPattern:  filepath.Join(dir, "cand", "000_%04d.jpg"),
				SmallPattern: filepath.Join(dir, "small", "000_%04d.jpg"),
				SmallWidth:   160,
			})
			require.NoError(t, err)
			require.Len(t, res.Small, len(res.Full))
			return res
		}
		blue := func(path string) bool { r, _, b := meanRGB(t, path); return b > 180 && r < 80 }

		f := New("", "", nil)
		f.MaxPixels, f.Threads = 100_000, 2
		capped := extract(f)
		require.NotEmpty(t, capped.Full)
		assert.LessOrEqual(t, len(capped.Full), 4, "only the first (red, 320x240) second is decoded")
		for _, p := range capped.Full {
			assert.False(t, blue(p), "%s comes from the 1280x720 part", p)
		}

		// Uncapped the blue part is decoded; the frame count still honours
		// ExpectedFrames although the resolution change rebuilds the filter graph.
		uncapped := extract(New("", "", nil))
		assert.LessOrEqual(t, len(uncapped.Full), ExpectedFrames(1.9, 4))
		assert.True(t, slices.ContainsFunc(uncapped.Full, blue), "fixture sanity: the stream really switches")
	})
}

// TestBuildPreviewMaxDuration: the preview never runs longer than maxDurationSec,
// including when the container header understates the real stream length.
func TestBuildPreviewMaxDuration(t *testing.T) {
	needFFmpeg(t)
	f := New("", "", nil)

	t.Run("longer source", func(t *testing.T) {
		// Both decode paths: a keyframe every 0.5 s (keyframes only, 2 fps) and a
		// single keyframe (non-reference frames skipped, 2 fps).
		for name, src := range map[string]string{
			"keyframes only": gopVideo(t, 12, 15),
			"noref":          gopVideo(t, 12, 1000),
		} {
			t.Run(name, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "preview.mp4")
				info, err := f.BuildPreview(context.Background(), src, out, 480, 2, 5)
				require.NoError(t, err)
				require.Equal(t, name == "keyframes only", info.KeyframesOnly, "fixture: decode path")
				require.Equal(t, 2.0, info.FPS)
				p := probeFile(t, out)
				assert.LessOrEqual(t, p.duration(t), 5.1)
				assert.GreaterOrEqual(t, p.duration(t), 4.9)
				require.Len(t, p.Streams, 1)
				assert.Equal(t, "10", p.Streams[0].NbFrames)

				// A cap longer than the video changes nothing.
				out = filepath.Join(t.TempDir(), "preview.mp4")
				_, err = f.BuildPreview(context.Background(), src, out, 480, 2, 180)
				require.NoError(t, err)
				assert.InDelta(t, 12.0, probeFile(t, out).duration(t), 0.05)
			})
		}

		// The frame cap uses the preview's real rate: 1 keyframe per second → 1 fps.
		src := testsrcVideo(t, 320, 240, 12)
		out := filepath.Join(t.TempDir(), "preview.mp4")
		info, err := f.BuildPreview(context.Background(), src, out, 480, 2, 5)
		require.NoError(t, err)
		require.Equal(t, PreviewInfo{FPS: 1, KeyframesOnly: true}, info)
		p := probeFile(t, out)
		assert.InDelta(t, 5.0, p.duration(t), 0.1)
		assert.Equal(t, "5", p.Streams[0].NbFrames)
	})

	t.Run("mkv header understates the duration", func(t *testing.T) {
		lie := lyingMKV(t, 12, 3)
		info, err := f.Probe(context.Background(), lie)
		require.NoError(t, err)
		require.InDelta(t, 3.0, info.DurationSec, 0.01, "fixture: the header says 3 s")

		uncapped := filepath.Join(t.TempDir(), "uncapped.mp4")
		_, err = f.BuildPreview(context.Background(), lie, uncapped, 480, 2, 0)
		require.NoError(t, err)
		require.InDelta(t, 12.0, probeFile(t, uncapped).duration(t), 0.05, "fixture: the stream really is 12 s")

		out := filepath.Join(t.TempDir(), "preview.mp4")
		_, err = f.BuildPreview(context.Background(), lie, out, 480, 2, info.DurationSec)
		require.NoError(t, err)
		assert.LessOrEqual(t, probeFile(t, out).duration(t), 3.1)
	})

	t.Run("invalid cap", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "in.mp4")
		out := filepath.Join(t.TempDir(), "out.mp4")
		bad := New("/nonexistent/ffmpeg", "", nil) // validation happens before any exec
		for _, v := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
			_, err := bad.BuildPreview(context.Background(), src, out, 480, 2, v)
			require.Error(t, err, "%v", v)
			var ee *ExecError
			assert.False(t, errors.As(err, &ee), "must fail validation, not exec: %v", err)
		}
	})
}

// lyingMKV encodes a realSec-long MKV and patches its Segment Info Duration element
// (EBML ID 0x4489, an 8-byte big-endian float in milliseconds) to claimSec.
func lyingMKV(t testing.TB, realSec, claimSec float64) string {
	t.Helper()
	src := ffmpegGen(t, "real.mkv", "-f", "lavfi",
		"-i", "testsrc2=size=320x240:rate=30:duration="+formatFloat(realSec),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "30")
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	i := bytes.Index(data, []byte{0x44, 0x89, 0x88}) // ID + size (8 bytes)
	require.Positive(t, i, "no Duration element")
	val := data[i+3 : i+11]
	require.InDelta(t, realSec*1000, math.Float64frombits(binary.BigEndian.Uint64(val)), 1, "found the wrong element")
	binary.BigEndian.PutUint64(val, math.Float64bits(claimSec*1000))
	out := filepath.Join(t.TempDir(), "lie.mkv")
	require.NoError(t, os.WriteFile(out, data, 0o644))
	return out
}

// TestMetadataIsStripped: a phone-style clip tagged with a GPS location, title,
// comment and a chapter must not pass any of it to the preview (which goes to the
// model provider) or to the extracted JPEGs.
func TestMetadataIsStripped(t *testing.T) {
	needFFmpeg(t)
	const (
		location = "+47.4979+019.0402/"
		title    = "Peter's home"
		comment  = "secret-comment"
	)
	metaFile := filepath.Join(t.TempDir(), "meta.txt")
	require.NoError(t, os.WriteFile(metaFile, []byte(";FFMETADATA1\n"+
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1000\ntitle=secret-chapter\n"), 0o644))
	src := ffmpegGen(t, "tagged.mov",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30:duration=2",
		"-i", metaFile, "-map", "0:v", "-map_chapters", "1",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-metadata", "location="+location, "-metadata", "title="+title, "-metadata", "comment="+comment,
		"-metadata:s:v:0", "title=secret-stream-title", "-metadata:s:v:0", "comment=secret-stream-comment")

	// Fixture sanity: the source carries everything.
	sp := probeFile(t, src)
	require.Equal(t, location, sp.Format.Tags["location"])
	require.Equal(t, title, sp.Format.Tags["title"])
	require.Equal(t, comment, sp.Format.Tags["comment"])
	require.NotEmpty(t, sp.Chapters)

	secrets := []string{"47.4979", title, comment, "secret-"}
	f := New("", "", nil)

	t.Run("preview", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "preview.mp4")
		_, err := f.BuildPreview(context.Background(), src, out, 480, 2, 0)
		require.NoError(t, err)
		p := probeFile(t, out)
		allowed := []string{"major_brand", "minor_version", "compatible_brands", "handler_name", "language", "vendor_id", "encoder"}
		for k, v := range p.Format.Tags {
			assert.Contains(t, allowed, k, "format tag %s=%q", k, v)
		}
		require.Len(t, p.Streams, 1)
		for k, v := range p.Streams[0].Tags {
			assert.Contains(t, allowed, k, "stream tag %s=%q", k, v)
		}
		for _, k := range []string{"location", "location-eng", "title", "comment"} {
			assert.NotContains(t, p.Format.Tags, k)
			assert.NotContains(t, p.Streams[0].Tags, k)
		}
		assert.Empty(t, p.Chapters)
		assert.NotRegexp(t, `Lavc\d`, p.Streams[0].Tags["encoder"], "bitexact: no version string")
		assert.NotContains(t, p.Format.Tags, "encoder", "bitexact: no muxer version")

		data, err := os.ReadFile(out)
		require.NoError(t, err)
		for _, s := range secrets {
			assert.False(t, bytes.Contains(data, []byte(s)), "preview bytes contain %q", s)
		}
	})

	t.Run("extract", func(t *testing.T) {
		dir := t.TempDir()
		res, err := f.ExtractFrames(context.Background(), ExtractArgs{
			Input: src, StartSec: 0, DurationSec: 1, FPS: 2,
			FullPattern:  filepath.Join(dir, "cand", "000_%04d.jpg"),
			SmallPattern: filepath.Join(dir, "small", "000_%04d.jpg"),
		})
		require.NoError(t, err)
		require.Len(t, res.Full, 2)
		for _, p := range append(slices.Clone(res.Full), res.Small...) {
			data, err := os.ReadFile(p)
			require.NoError(t, err)
			for _, s := range append(secrets, "Lavc") {
				assert.False(t, bytes.Contains(data, []byte(s)), "%s contains %q", filepath.Base(p), s)
			}
		}
	})
}

// TestLimitsKeepOutputUnchanged: with generous limits (MaxPixels exactly the source
// size, a thread cap) the outputs match the unlimited run frame for frame.
func TestLimitsKeepOutputUnchanged(t *testing.T) {
	const w, h, px = 640, 360, 3
	src := barVideo(t, w, h, px, 4)
	limited := New("", "", nil)
	limited.MaxPixels, limited.Threads = w*h, 1

	frames := func(ff *FFmpeg) []int {
		dir := t.TempDir()
		res, err := ff.ExtractFrames(context.Background(), ExtractArgs{
			Input: src, StartSec: 1.21, DurationSec: 1.5, FPS: 4,
			FullPattern:  filepath.Join(dir, "cand", "001_%04d.jpg"),
			SmallPattern: filepath.Join(dir, "small", "001_%04d.jpg"),
		})
		require.NoError(t, err)
		require.Len(t, res.Full, ExpectedFrames(1.5, 4))
		var got []int
		for i, p := range res.Full {
			got = append(got, barFrame(t, p, w, px))
			assert.Equal(t, got[i], barFrame(t, res.Small[i], w, px))
		}
		return got
	}
	// Frame i is the first source frame at/after 1.21 s, then the last at/before
	// 1.21 + i/4 s (see TestExtractFramesTiming).
	want := []int{37, 43, 51, 58, 66, 73}
	assert.Equal(t, want, frames(New("", "", nil)))
	assert.Equal(t, want, frames(limited))

	out := filepath.Join(t.TempDir(), "preview.mp4")
	info, err := limited.BuildPreview(context.Background(), src, out, 480, 2, 0)
	require.NoError(t, err)
	streams := probeStreams(t, out)
	require.Len(t, streams, 1)
	assert.Equal(t, [2]int{w, h}, [2]int{streams[0].Width, streams[0].Height})
	assert.True(t, strings.HasPrefix(streams[0].AvgFrameRate, strconv.Itoa(int(info.FPS))+"/"))
	unlimited, err := New("", "", nil).BuildPreview(context.Background(), src, filepath.Join(t.TempDir(), "p.mp4"), 480, 2, 0)
	require.NoError(t, err)
	assert.Equal(t, unlimited, info, "the limits do not change the decode plan")
}
