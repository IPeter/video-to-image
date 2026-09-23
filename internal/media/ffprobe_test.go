package media

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProbe(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    VideoInfo
		wantErr bool // wraps ErrCannotDecode
	}{
		{
			name: "landscape h264",
			json: `{"streams":[{"codec_name":"h264","width":1920,"height":1080,"r_frame_rate":"30/1","avg_frame_rate":"30000/1001","duration":"12.012000"}],"format":{"duration":"12.050000","size":"1234"}}`,
			want: VideoInfo{DurationSec: 12.05, Width: 1920, Height: 1080, FPS: 29.97, Codec: "h264"},
		},
		{
			name: "iphone portrait display matrix -90 swaps to display dims",
			json: `{"streams":[{"codec_name":"hevc","width":1920,"height":1080,"r_frame_rate":"30/1","avg_frame_rate":"30/1","duration":"8.0","tags":{},"side_data_list":[{},{"rotation":-90}]}],"format":{"duration":"8.0"}}`,
			want: VideoInfo{DurationSec: 8, Width: 1080, Height: 1920, FPS: 30, Codec: "hevc", Rotation: -90},
		},
		{
			name: "rotation 180 keeps dims",
			json: `{"streams":[{"codec_name":"h264","width":1280,"height":720,"avg_frame_rate":"25/1","side_data_list":[{"rotation":180}]}],"format":{"duration":"3"}}`,
			want: VideoInfo{DurationSec: 3, Width: 1280, Height: 720, FPS: 25, Codec: "h264", Rotation: 180},
		},
		{
			name: "rotation as string and 270 normalised",
			json: `{"streams":[{"codec_name":"h264","width":1280,"height":720,"avg_frame_rate":"25/1","side_data_list":[{"rotation":"270"}]}],"format":{"duration":"3"}}`,
			want: VideoInfo{DurationSec: 3, Width: 720, Height: 1280, FPS: 25, Codec: "h264", Rotation: -90},
		},
		{
			name: "legacy rotate tag (clockwise) converts to display-matrix sign",
			json: `{"streams":[{"codec_name":"h264","width":1920,"height":1080,"avg_frame_rate":"30/1","tags":{"rotate":"90"}}],"format":{"duration":"5.5"}}`,
			want: VideoInfo{DurationSec: 5.5, Width: 1080, Height: 1920, FPS: 30, Codec: "h264", Rotation: -90},
		},
		{
			name: "side data wins over tag",
			json: `{"streams":[{"codec_name":"h264","width":1920,"height":1080,"avg_frame_rate":"30/1","tags":{"rotate":"180"},"side_data_list":[{"rotation":90}]}],"format":{"duration":"5"}}`,
			want: VideoInfo{DurationSec: 5, Width: 1080, Height: 1920, FPS: 30, Codec: "h264", Rotation: 90},
		},
		{
			name: "webm: no stream duration, format duration used",
			json: `{"streams":[{"codec_name":"vp9","width":640,"height":360,"r_frame_rate":"24/1","avg_frame_rate":"24/1","tags":{}}],"format":{"duration":"2.000000","size":"50726"}}`,
			want: VideoInfo{DurationSec: 2, Width: 640, Height: 360, FPS: 24, Codec: "vp9"},
		},
		{
			name: "format duration N/A falls back to stream duration",
			json: `{"streams":[{"codec_name":"h264","width":640,"height":480,"avg_frame_rate":"30/1","duration":"6.000000"}],"format":{"duration":"N/A"}}`,
			want: VideoInfo{DurationSec: 6, Width: 640, Height: 480, FPS: 30, Codec: "h264"},
		},
		{
			name: "avg_frame_rate 0/0 falls back to r_frame_rate",
			json: `{"streams":[{"codec_name":"h264","width":640,"height":480,"r_frame_rate":"24000/1001","avg_frame_rate":"0/0"}],"format":{"duration":"1"}}`,
			want: VideoInfo{DurationSec: 1, Width: 640, Height: 480, FPS: 23.976, Codec: "h264"},
		},
		{
			name: "no usable frame rate is not fatal",
			json: `{"streams":[{"codec_name":"h264","width":640,"height":480,"r_frame_rate":"0/0","avg_frame_rate":"0/0"}],"format":{"duration":"1"}}`,
			want: VideoInfo{DurationSec: 1, Width: 640, Height: 480, FPS: 0, Codec: "h264"},
		},
		{name: "no video stream (audio only)", json: `{"streams":[],"format":{"duration":"2.000000"}}`, wantErr: true},
		{name: "zero dimensions", json: `{"streams":[{"codec_name":"h264","width":0,"height":0}],"format":{"duration":"2"}}`, wantErr: true},
		{name: "still image: no duration", json: `{"streams":[{"codec_name":"png","width":64,"height":64,"avg_frame_rate":"25/1"}],"format":{"size":"2282"}}`, wantErr: true},
		{name: "zero duration", json: `{"streams":[{"codec_name":"h264","width":64,"height":64,"duration":"0.000000"}],"format":{"duration":"0.000000"}}`, wantErr: true},
		{name: "NaN duration", json: `{"streams":[{"codec_name":"h264","width":64,"height":64}],"format":{"duration":"nan"}}`, wantErr: true},
		{name: "inf duration", json: `{"streams":[{"codec_name":"h264","width":64,"height":64}],"format":{"duration":"inf"}}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProbe([]byte(tt.json))
			if tt.wantErr {
				require.ErrorIs(t, err, ErrCannotDecode)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	_, err := parseProbe([]byte("not json"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrCannotDecode, "garbage from ffprobe itself is an internal error")
}

func TestNormalizeRotation(t *testing.T) {
	for in, want := range map[int]int{0: 0, 90: 90, -90: -90, 180: 180, -180: 180, 270: -90, -270: 90, 360: 0, 450: 90, -450: -90, 45: 45} {
		assert.Equal(t, want, normalizeRotation(in), "in=%d", in)
	}
}

func TestParseRate(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"30000/1001", 30000.0 / 1001, true},
		{"25/1", 25, true},
		{"2", 2, true},
		{"0/0", 0, false},
		{"1/0", 0, false},
		{"-30/1", 0, false},
		{"", 0, false},
		{"N/A", 0, false},
		{"abc/1", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseRate(tt.in)
		assert.Equal(t, tt.ok, ok, tt.in)
		assert.InDelta(t, tt.want, got, 1e-9, tt.in)
	}
}

func TestProbeLandscape(t *testing.T) {
	src := testsrcVideo(t, 640, 480, 6)
	f := New("", "", nil)
	info, err := f.Probe(context.Background(), src)
	require.NoError(t, err)
	st, err := os.Stat(src)
	require.NoError(t, err)

	assert.InDelta(t, 6.0, info.DurationSec, 0.05)
	assert.Equal(t, 640, info.Width)
	assert.Equal(t, 480, info.Height)
	assert.InDelta(t, 30.0, info.FPS, 0.001)
	assert.Equal(t, "h264", info.Codec)
	assert.Equal(t, 0, info.Rotation)
	assert.Equal(t, st.Size(), info.SizeBytes)
	assert.Empty(t, info.MIME)
	assert.Empty(t, info.SHA256)
	assert.Equal(t, 480, info.ShortSide())
}

func TestProbePortraitRotation(t *testing.T) {
	portrait := portraitVideo(t)
	f := New("", "", nil)
	info, err := f.Probe(context.Background(), portrait)
	require.NoError(t, err)
	assert.Equal(t, 360, info.Width, "display width")
	assert.Equal(t, 640, info.Height, "display height")
	assert.Equal(t, -90, info.Rotation)
	assert.InDelta(t, 3.0, info.DurationSec, 0.05)
}

func TestProbeCannotDecode(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()

	text := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(text, []byte("this is not a video\n"), 0o644))

	empty := filepath.Join(dir, "empty.mp4")
	require.NoError(t, os.WriteFile(empty, nil, 0o644))

	// moov atom at the end (no faststart): cutting the file in half loses the index.
	whole, err := os.ReadFile(testsrcVideo(t, 320, 240, 2))
	require.NoError(t, err)
	truncated := filepath.Join(dir, "truncated.mp4")
	require.NoError(t, os.WriteFile(truncated, whole[:len(whole)/2], 0o644))

	audioOnly := ffmpegGen(t, "audio.mp4", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:a", "aac")
	png := ffmpegGen(t, "still.png", "-f", "lavfi", "-i", "testsrc2=size=64x64", "-frames:v", "1")

	f := New("", "", nil)
	for name, path := range map[string]string{
		"text": text, "empty": empty, "truncated mp4": truncated, "audio only": audioOnly, "png": png,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.Probe(context.Background(), path)
			require.ErrorIs(t, err, ErrCannotDecode)
		})
	}

	t.Run("ffprobe failure keeps the ExecError", func(t *testing.T) {
		_, err := f.Probe(context.Background(), text)
		var ee *ExecError
		require.ErrorAs(t, err, &ee)
		assert.Contains(t, ee.StderrTail, "Invalid data")
	})
}

func TestProbeInfrastructureErrorsAreNotCannotDecode(t *testing.T) {
	needFFmpeg(t)
	src := testsrcVideo(t, 320, 240, 1)

	t.Run("missing ffprobe binary", func(t *testing.T) {
		f := New("", "v2i-no-such-ffprobe", nil)
		_, err := f.Probe(context.Background(), src)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrCannotDecode)
		assert.ErrorIs(t, err, exec.ErrNotFound)

		f = New("", filepath.Join(t.TempDir(), "no-such-ffprobe"), nil)
		_, err = f.Probe(context.Background(), src)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrCannotDecode)
		var ee *ExecError
		assert.ErrorAs(t, err, &ee)
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := New("", "", nil).Probe(context.Background(), filepath.Join(t.TempDir(), "gone.mp4"))
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrCannotDecode)
		assert.ErrorIs(t, err, fs.ErrNotExist)
	})
	t.Run("directory", func(t *testing.T) {
		_, err := New("", "", nil).Probe(context.Background(), t.TempDir())
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrCannotDecode)
	})
	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := New("", "", nil).Probe(ctx, src)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrCannotDecode)
		assert.ErrorIs(t, err, context.Canceled)
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		_, err := New("", "", nil).Probe(ctx, src)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrCannotDecode)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
	t.Run("empty path", func(t *testing.T) {
		_, err := New("", "", nil).Probe(context.Background(), "")
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrCannotDecode)
	})
}

// portraitVideo is a phone-style clip: 640x360 coded frames with a -90° display matrix
// (what an iPhone writes for portrait), so it displays as 360x640.
func portraitVideo(t testing.TB) string {
	t.Helper()
	coded := testsrcVideo(t, 640, 360, 3)
	return ffmpegGen(t, "portrait.mp4", "-display_rotation", "-90", "-i", coded, "-c", "copy")
}
