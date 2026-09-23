package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Hard ceilings applied when the caller's context has no deadline, so a wedged
// ffmpeg/ffprobe process can never block a worker forever. A caller-supplied
// deadline always wins (it may be shorter or longer).
const (
	versionTimeout = 30 * time.Second
	probeTimeout   = 60 * time.Second
	previewTimeout = 10 * time.Minute
	extractTimeout = 10 * time.Minute
)

// keyframeProbeTimeout bounds BuildPreview's keyframe probe even when the caller's
// deadline is longer: the probe only demuxes the video packets (≈0.1 s on a 377 MB 4K
// phone clip) and a slow probe just means the noref decode path is used.
const keyframeProbeTimeout = 20 * time.Second

const (
	stderrTailBytes = 4 << 10  // ExecError.StderrTail
	maxStdoutBytes  = 16 << 20 // ffprobe JSON is a few KB; anything bigger is a bug
	// waitDelay bounds how long Wait blocks on the output pipes after the process
	// was killed (e.g. a grandchild that inherited them).
	waitDelay = 5 * time.Second
)

func (f *FFmpeg) ffmpegBin() string {
	if f.FFmpegPath == "" {
		return "ffmpeg"
	}
	return f.FFmpegPath
}

func (f *FFmpeg) ffprobeBin() string {
	if f.FFprobePath == "" {
		return "ffprobe"
	}
	return f.FFprobePath
}

func (f *FFmpeg) logger() *slog.Logger {
	if f.Logger == nil {
		return slog.Default()
	}
	return f.Logger
}

// run executes bin with args and returns its stdout. When ctx has no deadline, ceiling
// is applied. Any failure (missing binary, non-zero exit, context end) is returned as
// *ExecError carrying the last ~4 KB of stderr. When the context ended, ExecError.Err
// wraps ctx.Err() so errors.Is(err, context.DeadlineExceeded) works, and the process
// exit status is only included as text, so exit-status checks (see isExitFailure) do
// not mistake a timeout for a bad input file.
func (f *FFmpeg) run(ctx context.Context, ceiling time.Duration, bin string, args ...string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok && ceiling > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ceiling)
		defer cancel()
	}

	stdout := &limitedBuffer{max: maxStdoutBytes}
	stderr := &tailBuffer{max: stderrTailBytes}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	start := time.Now()
	err := cmd.Run()
	took := time.Since(start)
	if err == nil && stdout.overflow {
		err = fmt.Errorf("stdout exceeded %d bytes", maxStdoutBytes)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(err, ctxErr) {
				err = ctxErr
			} else {
				// %v on purpose: the exit status must not be unwrappable (see isExitFailure).
				err = fmt.Errorf("%w (%v)", ctxErr, err) //nolint:errorlint // intentional, see above

			}
		}
		ee := &ExecError{Cmd: commandLine(bin, args), Err: err, StderrTail: stderr.String()}
		f.logger().Debug("media: command failed", "cmd", ee.Cmd, "took", took, "err", err, "stderr", ee.StderrTail)
		return stdout.Bytes(), ee
	}
	log := f.logger().With("cmd", filepath.Base(bin), "took", took)
	if tail := stderr.String(); tail != "" {
		// Non-fatal decoder complaints (-loglevel error still prints them).
		log.Debug("media: command succeeded with stderr output", "stderr", tail)
	} else {
		log.Debug("media: command succeeded")
	}
	return stdout.Bytes(), nil
}

// isExitFailure reports whether err is a process that ran to completion and exited
// with a non-zero status (as opposed to a missing binary, a signal or a context end).
func isExitFailure(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.Exited()
}

// commandLine renders bin + args for logs, quoting arguments that need it.
func commandLine(bin string, args []string) string {
	var b strings.Builder
	b.WriteString(bin)
	for _, a := range args {
		b.WriteByte(' ')
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\$`;&|<>()[]*?!#") {
			b.WriteString(strconv.Quote(a))
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

// tailBuffer is an io.Writer that keeps only the last max bytes written.
type tailBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if t.max <= 0 {
		t.truncated = t.truncated || n > 0
		return n, nil
	}
	if n >= t.max {
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		t.truncated = true
		return n, nil
	}
	if over := len(t.buf) + n - t.max; over > 0 {
		m := copy(t.buf, t.buf[over:])
		t.buf = t.buf[:m]
		t.truncated = true
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// String returns the retained tail, trimmed, as valid UTF-8. A tail that was cut
// starts with "…" and a possibly split first rune is dropped.
func (t *tailBuffer) String() string {
	b := t.buf
	if t.truncated {
		// Skip continuation bytes of a rune cut in half by the ring.
		for len(b) > 0 && !utf8.RuneStart(b[0]) {
			b = b[1:]
		}
	}
	s := strings.TrimSpace(strings.ToValidUTF8(string(b), "�"))
	if t.truncated && s != "" {
		return "…" + s
	}
	return s
}

// limitedBuffer collects up to max bytes and silently discards the rest (setting
// overflow), so a runaway process can't exhaust memory or block on a full pipe.
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); len(p) > room {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

// absPath makes p absolute so it can never be mistaken for an ffmpeg option ("-x")
// or a protocol URL ("name:with-colon.mp4").
func absPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	return filepath.Abs(p)
}
