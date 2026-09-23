package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		out  string
		want string
		ok   bool
	}{
		{"ffmpeg version 9.0.1 Copyright (c) 2000-2026 the FFmpeg developers\nbuilt with clang", "9.0.1", true},
		{"ffmpeg version n7.1 Copyright (c) 2000-2024 the FFmpeg developers", "7.1", true},
		{"ffmpeg version 7.1.1-1+b1 Copyright (c) 2000-2025 the FFmpeg developers", "7.1.1", true},
		{"ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023 the FFmpeg developers", "6.1.1", true},
		{"ffmpeg version 4.4.2-0ubuntu0.22.04.1 Copyright (c) 2000-2021", "4.4.2", true},
		{"ffmpeg version 7.0 Copyright", "7.0", true},
		{"ffmpeg version N-118000-gdeadbeef-static https://johnvansickle.com/ffmpeg/", "N-118000-gdeadbeef-static", true},
		{"some warning\nffmpeg version 5.1.6-0+deb12u1 Copyright", "5.1.6", true},
		{"", "", false},
		{"ffprobe version 9.0.1", "", false},
	}
	for _, tt := range tests {
		got, ok := parseVersion(tt.out)
		assert.Equal(t, tt.ok, ok, tt.out)
		assert.Equal(t, tt.want, got, tt.out)
	}
}

func TestVersionHost(t *testing.T) {
	needFFmpeg(t)
	v, err := New("", "", nil).Version(context.Background())
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^\d+(\.\d+)+$|^N-`), v)
	t.Logf("ffmpeg %s", v)
}

func TestVersionMissingBinary(t *testing.T) {
	_, err := New(filepath.Join(t.TempDir(), "nope"), "", nil).Version(context.Background())
	var ee *ExecError
	require.ErrorAs(t, err, &ee)
}

func TestExpectedFrames(t *testing.T) {
	tests := []struct {
		dur, fps float64
		want     int
	}{
		{0.6, 5, 3}, // 2.9999999999999996 without the epsilon
		{1, 4, 4},
		{1.99, 4, 7},
		{0.1, 4, 1}, // never zero for a valid window
		{3, 3.3333333333333335, 10},
		{0, 4, 0},
		{-1, 4, 0},
		{1, 0, 0},
		{math.NaN(), 4, 0},
		{math.Inf(1), 4, 0},
		{1, math.Inf(1), 0},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, ExpectedFrames(tt.dur, tt.fps), "dur=%v fps=%v", tt.dur, tt.fps)
	}
}

func TestFormatNumbers(t *testing.T) {
	assert.Equal(t, "0.0000001", formatFloat(1e-7))
	assert.Equal(t, "3.3333333333333335", formatFloat(10.0/3))
	assert.Equal(t, "4", formatFloat(4))
	assert.Equal(t, "1234567.5", formatFloat(1234567.5))
	assert.Equal(t, "0.3", formatSeconds(0.1+0.2))
	assert.Equal(t, "0.000001", formatSeconds(1.4e-6))
	assert.Equal(t, "0", formatSeconds(1e-9))
	assert.Equal(t, "12.345678", formatSeconds(12.3456781))
}

func TestParseFramePattern(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		pattern      string
		prefix, suf  string
		wantErrMatch string
	}{
		{pattern: filepath.Join(dir, "0003_%04d.jpg"), prefix: "0003_", suf: ".jpg"},
		{pattern: filepath.Join(dir, "%d.jpg"), prefix: "", suf: ".jpg"},
		{pattern: filepath.Join(dir, "a%%b_%3d_x"), prefix: "a%b_", suf: "_x"},
		{pattern: filepath.Join(dir, "frame.jpg"), wantErrMatch: "missing"},
		{pattern: filepath.Join(dir, "%04d_%04d.jpg"), wantErrMatch: "more than one"},
		{pattern: filepath.Join(dir, "%s.jpg"), wantErrMatch: "unsupported"},
		{pattern: filepath.Join(dir, "%04"), wantErrMatch: "unsupported"},
		{pattern: filepath.Join(dir, "%d", "x.jpg"), wantErrMatch: "file name"},
		{pattern: "", wantErrMatch: "empty"},
	}
	for _, tt := range tests {
		p, err := parseFramePattern(tt.pattern)
		if tt.wantErrMatch != "" {
			require.ErrorContains(t, err, tt.wantErrMatch, tt.pattern)
			continue
		}
		require.NoError(t, err, tt.pattern)
		assert.Equal(t, tt.prefix, p.prefix, tt.pattern)
		assert.Equal(t, tt.suf, p.suffix, tt.pattern)
		assert.Equal(t, filepath.Clean(dir), p.dir)
	}

	// Relative patterns become absolute.
	p, err := parseFramePattern("rel_%04d.jpg")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(p.path))
}

func TestFramePatternList(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"001_0001.jpg", "001_0002.jpg", "001_0010.jpg", "001_12345.jpg", "001_x001.jpg", "001_.jpg", "0011_0001.jpg", "002_0001.jpg", "001_0003.png"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o644))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, "001_0004.jpg"), 0o755)) // directories never match
	p, err := parseFramePattern(filepath.Join(dir, "001_%04d.jpg"))
	require.NoError(t, err)
	got, err := p.list()
	require.NoError(t, err)
	assert.Equal(t, map[int]string{
		1:     filepath.Join(dir, "001_0001.jpg"),
		2:     filepath.Join(dir, "001_0002.jpg"),
		10:    filepath.Join(dir, "001_0010.jpg"),
		12345: filepath.Join(dir, "001_12345.jpg"),
	}, got)

	require.NoError(t, p.removeAll())
	got, err = p.list()
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.FileExists(t, filepath.Join(dir, "002_0001.jpg"), "other patterns untouched")
	assert.FileExists(t, filepath.Join(dir, "0011_0001.jpg"))

	missing, err := parseFramePattern(filepath.Join(dir, "nodir", "%d.jpg"))
	require.NoError(t, err)
	got, err = missing.list()
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestExtractFramesTiming proves the ExtractResult contract "frame i was sampled at
// StartSec + i/FPS" on a video whose every frame shows its own number: frame 0 is the
// first source frame at/after StartSec and every later frame is the last source frame
// at/before StartSec + i/FPS, so the error is below one source frame (1/30 s).
func TestExtractFramesTiming(t *testing.T) {
	const (
		w, h, px = 640, 360, 3
		dur      = 6.0
	)
	src := barVideo(t, w, h, px, dur)
	f := New("", "", nil)
	srcPeriod := 1.0 / fixtureFPS

	tests := []struct {
		start, dur, fps float64
	}{
		{1.2, 0.6, 5}, // aligned with source frames
		{1.21, 0.6, 5},
		{2.345, 1.5, 10.0 / 3}, // fractional fps (after the candidate cap)
		{0.95, 1.0, 2.7},
		{0, 1, 8},
		{0.017, 2, 4},
		{4.99, 0.9, 7.5},
		{3.0, 0.1, 4}, // shorter than one sampling interval → exactly one frame
		{5.5, 1.0, 4}, // runs past the end: fewer frames, never an error
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("ss=%v_t=%v_fps=%.4g", tt.start, tt.dur, tt.fps), func(t *testing.T) {
			dir := t.TempDir()
			res, err := f.ExtractFrames(context.Background(), ExtractArgs{
				Input:        src,
				StartSec:     tt.start,
				DurationSec:  tt.dur,
				FPS:          tt.fps,
				FullPattern:  filepath.Join(dir, "cand", "007_%04d.jpg"),
				SmallPattern: filepath.Join(dir, "small", "007_%04d.jpg"),
				SmallWidth:   320,
			})
			require.NoError(t, err)
			require.Len(t, res.Small, len(res.Full))

			want := ExpectedFrames(tt.dur, tt.fps)
			if tt.start+tt.dur <= dur {
				require.Len(t, res.Full, want)
			} else {
				require.NotEmpty(t, res.Full)
				require.LessOrEqual(t, len(res.Full), want)
				// Every frame that exists in the source is delivered.
				last := tt.start + float64(len(res.Full)-1)/tt.fps
				require.Greater(t, last+1/tt.fps, dur-srcPeriod-1e-6)
			}

			for i := range res.Full {
				formula := tt.start + float64(i)/tt.fps
				got := float64(barFrame(t, res.Full[i], w, px)) / fixtureFPS
				diff := got - formula
				assert.Less(t, math.Abs(diff), srcPeriod+1e-3, "frame %d: formula %.4f, got %.4f", i, formula, got)
				if i == 0 {
					assert.GreaterOrEqual(t, diff, -1e-3, "frame 0 must be at/after StartSec")
				} else {
					assert.LessOrEqual(t, diff, 1e-3, "frame %d must be at/before its formula time", i)
				}
				// The small copy is the same frame.
				assert.Equal(t, barFrame(t, res.Full[i], w, px), barFrame(t, res.Small[i], w, px), "frame %d small copy", i)

				fw, fh := jpegSize(t, res.Full[i])
				assert.Equal(t, [2]int{w, h}, [2]int{fw, fh})
				sw, sh := jpegSize(t, res.Small[i])
				assert.Equal(t, [2]int{320, 180}, [2]int{sw, sh})
			}
		})
	}
}

// TestExtractFramesColourSegments: red 0–1 s, green 1–2 s, blue 2–3 s. Sampling
// 1.2 s + 0.6 s at 5 fps must give three green frames whose formula timestamps land
// in the green segment.
func TestExtractFramesColourSegments(t *testing.T) {
	graph := "color=c=0xFF0000:s=320x240:r=30:d=1[r];color=c=0x00FF00:s=320x240:r=30:d=1[g];" +
		"color=c=0x0000FF:s=320x240:r=30:d=1[b];[r][g][b]concat=n=3:v=1:a=0,format=yuv420p"
	src := ffmpegGen(t, "segments.mp4", "-f", "lavfi", "-i", graph, "-c:v", "libx264", "-preset", "veryfast", "-g", "30")

	colourAt := func(t *testing.T, path string) string {
		r, g, b := meanRGB(t, path)
		switch {
		case r > 180 && g < 80 && b < 80:
			return "red"
		case g > 180 && r < 80 && b < 80:
			return "green"
		case b > 180 && r < 80 && g < 80:
			return "blue"
		}
		return fmt.Sprintf("rgb(%.0f,%.0f,%.0f)", r, g, b)
	}
	segmentAt := func(ts float64) string { return []string{"red", "green", "blue"}[int(math.Floor(ts))] }

	f := New("", "", nil)
	tests := []struct {
		start, dur, fps float64
		want            []string
	}{
		{1.2, 0.6, 5, []string{"green", "green", "green"}},
		{0.5, 2.0, 2, []string{"red", "green", "green", "blue"}},
		{2.0, 0.5, 4, []string{"blue", "blue"}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("ss=%v", tt.start), func(t *testing.T) {
			dir := t.TempDir()
			res, err := f.ExtractFrames(context.Background(), ExtractArgs{
				Input: src, StartSec: tt.start, DurationSec: tt.dur, FPS: tt.fps,
				FullPattern:  filepath.Join(dir, "full_%04d.jpg"),
				SmallPattern: filepath.Join(dir, "small_%04d.jpg"),
			})
			require.NoError(t, err)
			require.Len(t, res.Full, len(tt.want))
			for i := range res.Full {
				ts := tt.start + float64(i)/tt.fps
				assert.Equal(t, tt.want[i], colourAt(t, res.Full[i]), "full frame %d", i)
				assert.Equal(t, tt.want[i], colourAt(t, res.Small[i]), "small frame %d", i)
				assert.Equal(t, tt.want[i], segmentAt(ts), "formula timestamp %.3f of frame %d", ts, i)
				sw, _ := jpegSize(t, res.Small[i])
				assert.Equal(t, DefaultSmallWidth, sw, "default small width")
			}
		})
	}
}

func TestExtractFramesPortrait(t *testing.T) {
	src := portraitVideo(t)
	dir := t.TempDir()
	res, err := New("", "", nil).ExtractFrames(context.Background(), ExtractArgs{
		Input: src, StartSec: 0.5, DurationSec: 1, FPS: 4,
		FullPattern:  filepath.Join(dir, "cand", "000_%04d.jpg"),
		SmallPattern: filepath.Join(dir, "small", "000_%04d.jpg"),
	})
	require.NoError(t, err)
	require.Len(t, res.Full, 4)
	for i := range res.Full {
		w, h := jpegSize(t, res.Full[i])
		assert.Equal(t, [2]int{360, 640}, [2]int{w, h}, "full frame is display-oriented")
		w, h = jpegSize(t, res.Small[i])
		assert.Equal(t, 640, w, "small copy is SmallWidth wide")
		assert.Equal(t, 1138, h, "and keeps the portrait aspect (640·640/360, even)")
	}
}

func TestExtractFramesEmptyWindow(t *testing.T) {
	src := testsrcVideo(t, 320, 240, 2)
	dir := t.TempDir()
	// The container says 2.0 s but the last frame starts at 1.967 s.
	res, err := New("", "", nil).ExtractFrames(context.Background(), ExtractArgs{
		Input: src, StartSec: 1.99, DurationSec: 1, FPS: 4,
		FullPattern:  filepath.Join(dir, "cand", "a_%04d.jpg"),
		SmallPattern: filepath.Join(dir, "small", "a_%04d.jpg"),
	})
	require.NoError(t, err)
	assert.Empty(t, res.Full)
	assert.Empty(t, res.Small)

	// Past the end entirely.
	res, err = New("", "", nil).ExtractFrames(context.Background(), ExtractArgs{
		Input: src, StartSec: 30, DurationSec: 1, FPS: 4,
		FullPattern:  filepath.Join(dir, "cand", "b_%04d.jpg"),
		SmallPattern: filepath.Join(dir, "small", "b_%04d.jpg"),
	})
	require.NoError(t, err)
	assert.Empty(t, res.Full)
}

func TestExtractFramesRemovesStaleFrames(t *testing.T) {
	src := testsrcVideo(t, 320, 240, 2)
	dir := t.TempDir()
	cand, small := filepath.Join(dir, "cand"), filepath.Join(dir, "small")
	require.NoError(t, os.MkdirAll(cand, 0o755))
	require.NoError(t, os.MkdirAll(small, 0o755))
	// Leftovers of an earlier, longer attempt plus an unrelated file.
	for _, p := range []string{filepath.Join(cand, "003_0009.jpg"), filepath.Join(small, "003_0009.jpg"), filepath.Join(cand, "004_0001.jpg")} {
		require.NoError(t, os.WriteFile(p, []byte("stale"), 0o644))
	}
	res, err := New("", "", nil).ExtractFrames(context.Background(), ExtractArgs{
		Input: src, StartSec: 0, DurationSec: 1, FPS: 2,
		FullPattern:  filepath.Join(cand, "003_%04d.jpg"),
		SmallPattern: filepath.Join(small, "003_%04d.jpg"),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(cand, "003_0001.jpg"), filepath.Join(cand, "003_0002.jpg")}, res.Full)
	assert.Equal(t, []string{filepath.Join(small, "003_0001.jpg"), filepath.Join(small, "003_0002.jpg")}, res.Small)
	assert.NoFileExists(t, filepath.Join(cand, "003_0009.jpg"))
	assert.NoFileExists(t, filepath.Join(small, "003_0009.jpg"))
	assert.FileExists(t, filepath.Join(cand, "004_0001.jpg"))
}

func TestExtractFramesFailureCleansUp(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.mp4")
	require.NoError(t, os.WriteFile(garbage, bytes.Repeat([]byte("nope"), 1000), 0o644))
	_, err := New("", "", nil).ExtractFrames(context.Background(), ExtractArgs{
		Input: garbage, StartSec: 0, DurationSec: 1, FPS: 4,
		FullPattern:  filepath.Join(dir, "cand", "000_%04d.jpg"),
		SmallPattern: filepath.Join(dir, "small", "000_%04d.jpg"),
	})
	var ee *ExecError
	require.ErrorAs(t, err, &ee)
	assert.NotEmpty(t, ee.StderrTail)
	entries, err := os.ReadDir(filepath.Join(dir, "cand"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestExtractFramesCancelled(t *testing.T) {
	src := testsrcVideo(t, 320, 240, 2)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New("", "", nil).ExtractFrames(ctx, ExtractArgs{
		Input: src, StartSec: 0, DurationSec: 1, FPS: 4,
		FullPattern:  filepath.Join(dir, "f_%04d.jpg"),
		SmallPattern: filepath.Join(dir, "s_%04d.jpg"),
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestExtractFramesValidation(t *testing.T) {
	dir := t.TempDir()
	ok := ExtractArgs{
		Input: filepath.Join(dir, "in.mp4"), StartSec: 0, DurationSec: 1, FPS: 4,
		FullPattern: filepath.Join(dir, "f_%04d.jpg"), SmallPattern: filepath.Join(dir, "s_%04d.jpg"),
	}
	tests := map[string]func(a *ExtractArgs){
		"empty input":       func(a *ExtractArgs) { a.Input = "" },
		"negative start":    func(a *ExtractArgs) { a.StartSec = -1 },
		"NaN start":         func(a *ExtractArgs) { a.StartSec = math.NaN() },
		"zero duration":     func(a *ExtractArgs) { a.DurationSec = 0 },
		"inf duration":      func(a *ExtractArgs) { a.DurationSec = math.Inf(1) },
		"zero fps":          func(a *ExtractArgs) { a.FPS = 0 },
		"NaN fps":           func(a *ExtractArgs) { a.FPS = math.NaN() },
		"no directive":      func(a *ExtractArgs) { a.FullPattern = filepath.Join(dir, "f.jpg") },
		"same patterns":     func(a *ExtractArgs) { a.SmallPattern = a.FullPattern },
		"same after decode": func(a *ExtractArgs) { a.SmallPattern = filepath.Join(dir, "f_%d.jpg") },
	}
	f := New("/nonexistent/ffmpeg", "", nil) // validation happens before any exec
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			a := ok
			mutate(&a)
			_, err := f.ExtractFrames(context.Background(), a)
			require.Error(t, err)
			var ee *ExecError
			assert.False(t, errors.As(err, &ee), "must fail validation, not exec: %v", err)
		})
	}
}

func TestBuildPreview(t *testing.T) {
	needFFmpeg(t)
	withAudio := ffmpegGen(t, "src.mp4",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest")
	small := testsrcVideo(t, 640, 360, 2)
	odd := ffmpegGen(t, "odd.mp4", "-f", "lavfi", "-i", "testsrc2=size=641x361:rate=30:duration=2",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv444p")
	portrait := portraitVideo(t)

	// withAudio and odd have one keyframe (x264's default GOP is longer than the clip),
	// so non-reference frames are skipped at the requested 2 fps; small and portrait
	// have a keyframe every second, so only keyframes are decoded, at 1 fps.
	tests := []struct {
		name  string
		src   string
		wantW int
		wantH int
		want  PreviewInfo
	}{
		{"downscales 720p to 480p", withAudio, 854, 480, PreviewInfo{FPS: 2}},
		{"never upscales", small, 640, 360, PreviewInfo{FPS: 1, KeyframesOnly: true}},
		{"odd source height rounds down to even", odd, 640, 360, PreviewInfo{FPS: 2}},
		{"portrait is display-oriented", portrait, 270, 480, PreviewInfo{FPS: 1, KeyframesOnly: true}},
	}
	f := New("", "", nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "nested", "preview.mp4")
			got, err := f.BuildPreview(context.Background(), tt.src, out, 480, 2, 0)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			streams := probeStreams(t, out)
			require.Len(t, streams, 1, "video only: no audio, subtitle or data streams")
			s := streams[0]
			assert.Equal(t, "video", s.CodecType)
			assert.Equal(t, "h264", s.CodecName)
			assert.Equal(t, "yuv420p", s.PixFmt)
			assert.Equal(t, fmt.Sprintf("%v/1", got.FPS), s.AvgFrameRate, "the reported fps is the real one")
			assert.Equal(t, tt.wantW, s.Width)
			assert.Equal(t, tt.wantH, s.Height)
			assert.LessOrEqual(t, s.Height, 480)
			assert.Zero(t, s.Width%2)

			info, err := f.Probe(context.Background(), out)
			require.NoError(t, err)
			assert.Equal(t, 0, info.Rotation, "rotation is baked in")
			assert.Equal(t, tt.wantW, info.Width)
			assert.Equal(t, tt.wantH, info.Height)

			data, err := os.ReadFile(out)
			require.NoError(t, err)
			assert.Less(t, bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat")), "+faststart puts moov first")

			entries, err := os.ReadDir(filepath.Dir(out))
			require.NoError(t, err)
			require.Len(t, entries, 1, "no temporary file left behind")
		})
	}
}

func TestBuildPreviewFailure(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.mp4")
	require.NoError(t, os.WriteFile(garbage, []byte("definitely not a video"), 0o644))
	out := filepath.Join(dir, "out", "preview.mp4")
	info, err := New("", "", nil).BuildPreview(context.Background(), garbage, out, 480, 2, 0)
	assert.Zero(t, info)
	var ee *ExecError
	require.ErrorAs(t, err, &ee)
	assert.NotEmpty(t, ee.StderrTail)
	assert.Contains(t, ee.Cmd, "ffmpeg", "the keyframe probe failing falls through to the encode")
	assert.NoFileExists(t, out)
	entries, err := os.ReadDir(filepath.Dir(out))
	require.NoError(t, err)
	assert.Empty(t, entries, "partial output removed")

	f := New("", "", nil)
	for _, args := range []struct {
		out           string
		height, fps   int
		wantErrSubstr string
	}{
		{out, 0, 2, "must be positive"},
		{out, 480, 0, "must be positive"},
		{garbage, 480, 2, "overwrite the input"},
	} {
		_, err := f.BuildPreview(context.Background(), garbage, args.out, args.height, args.fps, 0)
		assert.ErrorContains(t, err, args.wantErrSubstr)
	}
}

type streamInfo struct {
	CodecType    string `json:"codec_type"`
	CodecName    string `json:"codec_name"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	PixFmt       string `json:"pix_fmt"`
	AvgFrameRate string `json:"avg_frame_rate"`
}

func probeStreams(t *testing.T, path string) []streamInfo {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "stream=codec_type,codec_name,width,height,pix_fmt,avg_frame_rate",
		"-of", "json", path).Output()
	require.NoError(t, err)
	var v struct {
		Streams []streamInfo `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(out, &v))
	return v.Streams
}
