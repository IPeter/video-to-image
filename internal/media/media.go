// Package media wraps ffprobe and ffmpeg via os/exec with context timeouts. Plain
// arguments, no wrapper library. Every command's stderr tail is captured in ExecError
// for jobs.internal_log (never shown to users).
package media

import (
	"errors"
	"fmt"
	"log/slog"
)

// VideoInfo is what Step 0 records on the job (jobs.video_info). Width and Height are
// the DISPLAY dimensions, i.e. with rotation metadata applied (a portrait phone video
// coded as 1920x1080 with rotation -90 reports Width=1080, Height=1920).
type VideoInfo struct {
	DurationSec float64 `json:"durationSec"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	FPS         float64 `json:"fps"`
	Codec       string  `json:"codec"`
	Rotation    int     `json:"rotation"` // degrees as reported by the display matrix, e.g. -90, 90, 180, 0
	SizeBytes   int64   `json:"sizeBytes"`
	MIME        string  `json:"mime"`
	SHA256      string  `json:"sha256,omitempty"` // hex digest of the original upload (recording/replay key)
	// OriginalName is the sanitised client file name (base name, ≤ 200 runes), for display.
	OriginalName string `json:"originalName,omitempty"`
}

// ShortSide returns min(Width, Height).
func (v VideoInfo) ShortSide() int { return min(v.Width, v.Height) }

// ErrCannotDecode means ffprobe could not read a video stream from the file, or the
// stream has no usable duration/dimensions. Maps to the CANNOT_DECODE error code.
var ErrCannotDecode = errors.New("media: cannot decode video")

// ExecError is returned when an ffmpeg/ffprobe process fails.
type ExecError struct {
	Cmd        string // binary + args, for logs
	Err        error  // underlying exec error (exit status, context deadline, ...)
	StderrTail string // last ~4 KB of stderr
}

func (e *ExecError) Error() string { return fmt.Sprintf("media: %s: %v", e.Cmd, e.Err) }
func (e *ExecError) Unwrap() error { return e.Err }

// FFmpeg runs the ffmpeg and ffprobe binaries.
type FFmpeg struct {
	FFmpegPath  string // default "ffmpeg"
	FFprobePath string // default "ffprobe"
	Logger      *slog.Logger
	// MaxPixels, when > 0, is passed as -max_pixels (width×height) to the decoder of
	// every ffmpeg run (BuildPreview, ExtractFrames) so a crafted file cannot allocate
	// huge frames, even after a mid-stream resolution change. Probe uses 4×MaxPixels
	// so an oversized-but-sane video still probes (and is rejected as
	// RESOLUTION_TOO_HIGH) while a decode bomb fails cheaply. Values above ffmpeg's
	// ceiling (INT_MAX) are clamped. Set it to the largest limits.maxPixels of any
	// domain.
	MaxPixels int64
	// Threads, when > 0, bounds the decoder (-threads before -i), filter graph
	// (-filter_threads / -filter_complex_threads) and encoder (-threads per output)
	// thread counts of every ffmpeg run. Memory and CPU scale with the thread count,
	// which ffmpeg otherwise takes from the node's CPU count, not the cgroup quota.
	// ffprobe is unaffected (it already decodes single-threaded while probing).
	Threads int
}

// New returns an FFmpeg using the given binary paths ("" → look up on PATH).
func New(ffmpegPath, ffprobePath string, logger *slog.Logger) *FFmpeg {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FFmpeg{FFmpegPath: ffmpegPath, FFprobePath: ffprobePath, Logger: logger}
}

// Format is a sniffed container format.
type Format struct {
	MIME string // "video/mp4", "video/quicktime", "video/webm", "video/x-matroska"
	Ext  string // "mp4", "mov", "webm", "mkv" (no dot)
}

// ExtractArgs describes one dense-sampling run over a time range (plan/03-pipeline.md
// step 3). One ffmpeg invocation writes both the full-resolution candidates and the
// small scoring copies via a split filter graph.
type ExtractArgs struct {
	Input        string  // original video path
	StartSec     float64 // seek position (already padded and clamped by the caller)
	DurationSec  float64 // length of the range to sample
	FPS          float64 // sampling rate; may be fractional after the candidate cap
	FullPattern  string  // e.g. /scratch/job/cand/0003_%04d.jpg  (source resolution, -qscale:v 2)
	SmallPattern string  // e.g. /scratch/job/small/0003_%04d.jpg (SmallWidth wide, -qscale:v 4)
	SmallWidth   int     // default 640
	FullQScale   int     // default 2
	SmallQScale  int     // default 4
}

// ExtractResult lists the files written, sorted by frame number. Full[i] and Small[i]
// are the same frame; frame i was sampled at StartSec + i/FPS.
type ExtractResult struct {
	Full  []string
	Small []string
}
