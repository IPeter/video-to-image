package media

import (
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test fixtures are generated with ffmpeg's lavfi sources into t.TempDir(); nothing
// binary is committed.

const fixtureFPS = 30

func needFFmpeg(t testing.TB) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH: %v", bin, err)
		}
	}
}

// ffmpegGen runs `ffmpeg <args> <tempdir>/name` and returns the output path.
func ffmpegGen(t testing.TB, name string, args ...string) string {
	t.Helper()
	needFFmpeg(t)
	out := filepath.Join(t.TempDir(), name)
	full := append([]string{"-y", "-hide_banner", "-nostdin", "-loglevel", "error"}, args...)
	full = append(full, out)
	if b, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %q: %v\n%s", full, err, b)
	}
	return out
}

// testsrcVideo is a testsrc2 H.264 MP4 (moov at the end, like most encoders write it).
func testsrcVideo(t testing.TB, w, h int, durSec float64) string {
	t.Helper()
	return ffmpegGen(t, fmt.Sprintf("testsrc_%dx%d.mp4", w, h),
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=%d:duration=%s", w, h, fixtureFPS, formatFloat(durSec)),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "30")
}

// gopVideo is a 320x240 30 fps testsrc2 H.264 clip with a keyframe exactly every gop
// frames (no scene-cut keyframes).
func gopVideo(t testing.TB, durSec float64, gop int) string {
	t.Helper()
	return ffmpegGen(t, fmt.Sprintf("gop%d.mp4", gop),
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x240:rate=%d:duration=%s", fixtureFPS, formatFloat(durSec)),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", strconv.Itoa(gop), "-sc_threshold", "0")
}

// barVideo encodes the frame number spatially: on a black w x h canvas, source frame k
// (at k/30 s) shows a white vertical bar covering x ∈ [k·px, (k+1)·px). It is driven
// by the timestamp (not the overlay frame counter) so frame k is exactly t = k/30.
// Keyframes every second, so seeks land between keyframes.
func barVideo(t testing.TB, w, h, px int, durSec float64) string {
	t.Helper()
	return barVideoGOP(t, w, h, px, durSec, fixtureFPS)
}

// barVideoGOP is barVideo with a keyframe every gop frames (x264 without scene-cut
// keyframes, so the keyframes are exactly frames 0, gop, 2·gop, ...).
func barVideoGOP(t testing.TB, w, h, px int, durSec float64, gop int) string {
	t.Helper()
	return ffmpegGen(t, fmt.Sprintf("bar_g%d.mp4", gop), "-f", "lavfi", "-i", barGraph(t, w, h, px, durSec),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(gop), "-sc_threshold", "0")
}

// barVideoKeyframes is barVideo with keyframes exactly at keyTimes (seconds; 0 must be
// among them) and nowhere else.
func barVideoKeyframes(t testing.TB, w, h, px int, durSec float64, keyTimes []float64) string {
	t.Helper()
	times := make([]string, len(keyTimes))
	for i, k := range keyTimes {
		times[i] = formatFloat(k)
	}
	return ffmpegGen(t, "bar_forced.mp4", "-f", "lavfi", "-i", barGraph(t, w, h, px, durSec),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(int(durSec*fixtureFPS)+1), "-sc_threshold", "0",
		"-force_key_frames", strings.Join(times, ","))
}

// barVideoX264 is barVideo encoded with the given -x264-params plus IDR pictures forced
// at idrTimes (seconds), e.g. intra refresh with a periodic IDR.
func barVideoX264(t testing.TB, w, h, px int, durSec float64, params string, idrTimes []float64) string {
	t.Helper()
	times := make([]string, len(idrTimes))
	for i, k := range idrTimes {
		times[i] = formatFloat(k)
	}
	return ffmpegGen(t, "bar_x264.mp4", "-f", "lavfi", "-i", barGraph(t, w, h, px, durSec),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-x264-params", params,
		"-force_key_frames", strings.Join(times, ","), "-forced-idr", "1")
}

// barGraph is barVideo's lavfi source graph.
func barGraph(t testing.TB, w, h, px int, durSec float64) string {
	t.Helper()
	require.LessOrEqual(t, int(durSec*fixtureFPS)*px, w, "bar would leave the frame")
	return fmt.Sprintf(
		"color=c=black:s=%dx%d:r=%d:d=%s[bg];color=c=white:s=%dx%d:r=%d[bar];"+
			"[bg][bar]overlay=x='%d*round(t*%d)':y=0:shortest=1,format=yuv420p",
		w, h, fixtureFPS, formatFloat(durSec), px, h, fixtureFPS, px, fixtureFPS)
}

// x264Video is a 320x240 30 fps testsrc2 H.264 clip encoded with the given
// -x264-params (e.g. "keyint=30:open-gop=1").
func x264Video(t testing.TB, name string, durSec float64, params string) string {
	t.Helper()
	return ffmpegGen(t, name,
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x240:rate=%d:duration=%s", fixtureFPS, formatFloat(durSec)),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-x264-params", params)
}

// barFrame decodes a JPEG written from a barVideo (srcWidth wide, px bar) and returns
// the source frame number it shows.
func barFrame(t testing.TB, path string, srcWidth, px int) int {
	t.Helper()
	img := decodeJPEG(t, path)
	b := img.Bounds()
	y := b.Min.Y + b.Dy()/2
	sum, n := 0.0, 0
	for x := b.Min.X; x < b.Max.X; x++ {
		if luma(img, x, y) > 128 {
			sum += float64(x - b.Min.X)
			n++
		}
	}
	require.NotZero(t, n, "no bar found in %s", path)
	// Centre of the bar in source pixels; the bar for frame k is centred at k·px + px/2.
	cx := (sum/float64(n) + 0.5) * float64(srcWidth) / float64(b.Dx())
	return int(math.Round((cx - float64(px)/2) / float64(px)))
}

func decodeJPEG(t testing.TB, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	img, err := jpeg.Decode(f)
	require.NoError(t, err, path)
	return img
}

func jpegSize(t testing.TB, path string) (w, h int) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	require.NoError(t, err, path)
	return cfg.Width, cfg.Height
}

func luma(img image.Image, x, y int) uint32 {
	r, g, b, _ := img.At(x, y).RGBA()
	return (r + g + b) / 3 >> 8
}

// meanRGB returns the average colour of an image in 0..255.
func meanRGB(t testing.TB, path string) (r, g, b float64) {
	t.Helper()
	img := decodeJPEG(t, path)
	bb := img.Bounds()
	var sr, sg, sb float64
	for y := bb.Min.Y; y < bb.Max.Y; y++ {
		for x := bb.Min.X; x < bb.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			sr += float64(cr >> 8)
			sg += float64(cg >> 8)
			sb += float64(cb >> 8)
		}
	}
	n := float64(bb.Dx() * bb.Dy())
	return sr / n, sg / n, sb / n
}
