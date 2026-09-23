package media

import (
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTailBuffer(t *testing.T) {
	tests := []struct {
		name   string
		max    int
		writes []string
		want   string
	}{
		{"empty", 8, nil, ""},
		{"fits", 8, []string{"abc", "de"}, "abcde"},
		{"exactly full", 5, []string{"abc", "de"}, "abcde"},
		{"rolls over", 5, []string{"abc", "defg"}, "…cdefg"},
		{"single huge write", 4, []string{"0123456789"}, "…6789"},
		{"many small writes", 3, []string{"a", "b", "c", "d", "e"}, "…cde"},
		{"trims whitespace", 16, []string{"  err\n"}, "err"},
		{"drops split rune", 3, []string{"x", "é", "ab"}, "…ab"}, // last 3 bytes start mid-"é"
		{"keeps whole rune", 4, []string{"x", "é", "ab"}, "…éab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := &tailBuffer{max: tt.max}
			for _, w := range tt.writes {
				n, err := tb.Write([]byte(w))
				require.NoError(t, err)
				require.Equal(t, len(w), n)
			}
			assert.Equal(t, tt.want, tb.String())
			assert.LessOrEqual(t, len(tb.buf), tt.max)
		})
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 5}
	n, err := b.Write([]byte("abc"))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.False(t, b.overflow)
	n, err = b.Write([]byte("defgh"))
	require.NoError(t, err)
	assert.Equal(t, 5, n, "reports the full write so the child never sees EPIPE")
	assert.True(t, b.overflow)
	assert.Equal(t, "abcde", string(b.Bytes()))
}

func TestCommandLine(t *testing.T) {
	got := commandLine("ffmpeg", []string{"-i", "/tmp/a b.mp4", "-vf", "scale=-2:'min(480,ih)'", ""})
	assert.Equal(t, `ffmpeg -i "/tmp/a b.mp4" -vf "scale=-2:'min(480,ih)'" ""`, got)
}

func TestRunMissingBinary(t *testing.T) {
	t.Run("absolute path", func(t *testing.T) {
		f := New("/nonexistent/ffmpeg", "/nonexistent/ffprobe", nil)
		_, err := f.run(context.Background(), time.Second, f.ffmpegBin(), "-version")
		var ee *ExecError
		require.ErrorAs(t, err, &ee)
		assert.ErrorIs(t, err, fs.ErrNotExist)
		assert.False(t, isExitFailure(err))
		assert.Contains(t, ee.Cmd, "/nonexistent/ffmpeg -version")
	})
	t.Run("not on PATH", func(t *testing.T) {
		f := New("v2i-no-such-ffmpeg", "", nil)
		_, err := f.run(context.Background(), time.Second, f.ffmpegBin(), "-version")
		var ee *ExecError
		require.ErrorAs(t, err, &ee)
		assert.ErrorIs(t, err, exec.ErrNotFound)
		assert.False(t, isExitFailure(err))
	})
}

func TestRunExitFailureKeepsStderr(t *testing.T) {
	sh := lookPathOrSkip(t, "sh")
	f := New("", "", nil)
	_, err := f.run(context.Background(), 10*time.Second, sh, "-c", "echo first >&2; echo 'boom: bad input' >&2; exit 3")
	var ee *ExecError
	require.ErrorAs(t, err, &ee)
	assert.True(t, isExitFailure(err))
	assert.Equal(t, "first\nboom: bad input", ee.StderrTail)
	assert.Contains(t, ee.Error(), "exit status 3")
}

func TestRunStderrTailIsBounded(t *testing.T) {
	sh := lookPathOrSkip(t, "sh")
	f := New("", "", nil)
	// ~20 KB of stderr, then the line that matters.
	_, err := f.run(context.Background(), 10*time.Second, sh, "-c",
		"i=0; while [ $i -lt 2000 ]; do echo 'noise noise' >&2; i=$((i+1)); done; echo 'the real error' >&2; exit 1")
	var ee *ExecError
	require.ErrorAs(t, err, &ee)
	assert.LessOrEqual(t, len(ee.StderrTail), stderrTailBytes+len("…"))
	assert.True(t, strings.HasSuffix(ee.StderrTail, "the real error"))
	assert.True(t, strings.HasPrefix(ee.StderrTail, "…"))
}

func TestRunCapturesStdout(t *testing.T) {
	sh := lookPathOrSkip(t, "sh")
	f := New("", "", nil)
	out, err := f.run(context.Background(), 10*time.Second, sh, "-c", "printf '{\"ok\":true}'; echo warn >&2")
	require.NoError(t, err)
	assert.Equal(t, `{"ok":true}`, string(out))
}

func TestRunAppliesCeilingWithoutDeadline(t *testing.T) {
	sleep := lookPathOrSkip(t, "sleep")
	f := New("", "", nil)
	start := time.Now()
	_, err := f.run(context.Background(), 200*time.Millisecond, sleep, "10")
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, isExitFailure(err), "a timeout must not look like a bad input")
	var ee *ExecError
	require.ErrorAs(t, err, &ee)
}

func TestRunCallerDeadlineWins(t *testing.T) {
	sleep := lookPathOrSkip(t, "sleep")
	f := New("", "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The 50 ms ceiling is ignored because the caller set a deadline.
	_, err := f.run(ctx, 50*time.Millisecond, sleep, "0.3")
	require.NoError(t, err)
}

func TestRunCancelled(t *testing.T) {
	sleep := lookPathOrSkip(t, "sleep")
	f := New("", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := f.run(ctx, time.Minute, sleep, "10")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, isExitFailure(err))

	// Already cancelled before start.
	_, err = f.run(ctx, time.Minute, sleep, "10")
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, errors.Is(err, context.DeadlineExceeded))
}

func TestZeroValueFFmpegUsesDefaults(t *testing.T) {
	var f FFmpeg
	assert.Equal(t, "ffmpeg", f.ffmpegBin())
	assert.Equal(t, "ffprobe", f.ffprobeBin())
	assert.NotNil(t, f.logger())
}

func lookPathOrSkip(t *testing.T, bin string) string {
	t.Helper()
	p, err := exec.LookPath(bin)
	if err != nil {
		t.Skipf("%s not on PATH", bin)
	}
	return p
}
