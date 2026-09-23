package media

import (
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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

// barVideo encodes the frame number spatially: on a black w x h canvas, source frame k
// (at k/30 s) shows a white vertical bar covering x ∈ [k·px, (k+1)·px). It is driven
// by the timestamp (not the overlay frame counter) so frame k is exactly t = k/30.
// Keyframes every second, so seeks land between keyframes.
func barVideo(t testing.TB, w, h, px int, durSec float64) string {
	t.Helper()
	require.LessOrEqual(t, int(durSec*fixtureFPS)*px, w, "bar would leave the frame")
	graph := fmt.Sprintf(
		"color=c=black:s=%dx%d:r=%d:d=%s[bg];color=c=white:s=%dx%d:r=%d[bar];"+
			"[bg][bar]overlay=x='%d*round(t*%d)':y=0:shortest=1,format=yuv420p",
		w, h, fixtureFPS, formatFloat(durSec), px, h, fixtureFPS, px, fixtureFPS)
	return ffmpegGen(t, "bar.mp4", "-f", "lavfi", "-i", graph,
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "30")
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
