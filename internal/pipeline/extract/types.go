// Package extract is pipeline step 3: dense frame sampling from the ORIGINAL video at
// source resolution around each accepted scene. One ffmpeg invocation per range writes
// the full-resolution candidate and a 640 px scoring copy, so Go never decodes a 4K JPEG.
package extract

// Tunables from plan/03-pipeline.md step 3.
const (
	PaddingSec    = 0.5 // added on both sides to compensate for imprecise model timestamps
	ShortRangeSec = 1.5 // ranges shorter than this (before padding) sample at ShortRangeFPS
	ShortRangeFPS = 8
	SmallWidth    = 640
	FullQScale    = 2
	SmallQScale   = 4
	// DefaultConcurrency is how many ranges Run extracts at once when
	// Input.Concurrency is unset (the worker sets it from V2I_EXTRACT_CONCURRENCY or its
	// CPU budget, see pipeline.ExtractConcurrency). One 4K HEVC 60 fps range holds about
	// 450 MiB of memory at FFMPEG_THREADS=2 (about 700 MiB at 4), so memory scales with
	// the concurrency. Parallel ranges only pay off with idle cores: six ~4 s ranges took
	// 69 s one at a time and 28 s three at a time on an unconstrained 8-core machine, but
	// under a 4 CPU / 4Gi limit three ranges took 34 s at once against 39 s in turn, for
	// three times the memory (1.3 GiB).
	DefaultConcurrency = 3
)

// Range is one accepted scene (from analyze.Scene).
type Range struct {
	SceneIdx  int
	ShotID    string
	StartSec  float64
	EndSec    float64
	Relevance float64
	Reason    string // the model's one-sentence reason, carried to the selected image
}

type Input struct {
	SourcePath    string  // original video (local path)
	WorkDir       string  // per-job scratch dir; Run writes WorkDir/cand/ and WorkDir/small/
	DurationSec   float64 // of the original
	Ranges        []Range
	SampleFPS     int // domain selection.sampleFps (default 4)
	MaxCandidates int // domain limits.maxCandidateFrames (300)
	// Concurrency bounds the ffmpeg processes Run starts at once (one per range);
	// ≤ 0 means DefaultConcurrency. Each process is still bounded by FFmpeg.Threads,
	// so CPU and memory scale with Concurrency × FFmpeg.Threads.
	Concurrency int
}

// RangePlan is the sampling decision for one Range (pure, see Plan).
type RangePlan struct {
	Range
	PadStartSec float64 // max(0, StartSec - PaddingSec)
	PadEndSec   float64 // min(DurationSec, EndSec + PaddingSec)
	FPS         float64 // SampleFPS, ShortRangeFPS for short ranges, scaled down to respect the cap
	Expected    int     // expected frame count for this range
}

// Candidate is one extracted frame.
type Candidate struct {
	ID           string // cf_ + ULID
	SceneIdx     int
	ShotID       string
	Relevance    float64 // scene relevance
	SceneReason  string
	TimestampSec float64 // PadStartSec + frameIndex / FPS (frameIndex 0-based)
	FullPath     string  // source-resolution JPG (-qscale:v 2)
	SmallPath    string  // 640 px wide JPG (-qscale:v 4)
}

type Output struct {
	Candidates []Candidate // ordered by SceneIdx then TimestampSec
	Plans      []RangePlan // for job_steps.meta
}
