package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Defaults for ExtractArgs fields left at zero.
const (
	DefaultSmallWidth  = 640
	DefaultFullQScale  = 2
	DefaultSmallQScale = 4
)

var (
	versionLineRe = regexp.MustCompile(`(?m)^ffmpeg version (\S+)`)
	numericVerRe  = regexp.MustCompile(`^[nv]?(\d+(?:\.\d+)*)`)
)

// Version returns the ffmpeg version string, e.g. "7.1" or "9.0.1" (from `ffmpeg -version`).
// Distribution decorations are stripped ("n7.1" → "7.1", "7.1.1-1+b1" → "7.1.1"); a
// non-numeric build id such as "N-118000-gdeadbeef" is returned unchanged.
func (f *FFmpeg) Version(ctx context.Context) (string, error) {
	out, err := f.run(ctx, versionTimeout, f.ffmpegBin(), "-version")
	if err != nil {
		return "", err
	}
	v, ok := parseVersion(string(out))
	if !ok {
		first, _, _ := strings.Cut(string(out), "\n")
		return "", fmt.Errorf("media: cannot parse ffmpeg version from %q", first)
	}
	return v, nil
}

func parseVersion(out string) (string, bool) {
	m := versionLineRe.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	if n := numericVerRe.FindStringSubmatch(m[1]); n != nil {
		return n[1], true
	}
	return m[1], true
}

// Decode limits (see FFmpeg.MaxPixels and FFmpeg.Threads).
const (
	// maxPixelsCeiling is the largest value ffmpeg accepts for -max_pixels (an int64
	// AVOption whose range is capped at INT_MAX).
	maxPixelsCeiling = math.MaxInt32
	// probePixelFactor: Probe lets frames of up to this multiple of MaxPixels through,
	// so a video that is too large but sane is still measured (and rejected upstream
	// as RESOLUTION_TOO_HIGH) while a decode bomb fails cheaply as CANNOT_DECODE.
	probePixelFactor = 4
)

// stripMetadataArgs are output options that keep nothing but the pixels. No global,
// stream or chapter metadata is copied from the input (phone videos carry GPS
// coordinates, titles and comments, and the preview goes to a third-party model),
// and the muxer and encoder leave out their version strings. Checked on ffmpeg 7.1
// and 9: the decoded pixels are identical with and without the bitexact flags.
var stripMetadataArgs = []string{
	"-map_metadata", "-1", "-map_chapters", "-1",
	"-fflags", "+bitexact", "-flags:v", "+bitexact",
}

// maxPixelsArgs returns the -max_pixels decoder option for px (nil when px <= 0),
// clamped to what ffmpeg accepts. It must come before -i to apply to the decoder.
func maxPixelsArgs(px int64) []string {
	if px <= 0 {
		return nil
	}
	return []string{"-max_pixels", strconv.FormatInt(min(px, maxPixelsCeiling), 10)}
}

// probeMaxPixels is the looser pixel cap Probe uses: probePixelFactor × MaxPixels,
// clamped (0 = no cap).
func (f *FFmpeg) probeMaxPixels() int64 {
	if f.MaxPixels <= 0 {
		return 0
	}
	if f.MaxPixels > maxPixelsCeiling/probePixelFactor {
		return maxPixelsCeiling
	}
	return f.MaxPixels * probePixelFactor
}

// threadArgs returns "<opt> <Threads>" when Threads > 0. With opt "-threads" it sets
// the decoder's thread count when placed before -i and the encoders' thread count
// when placed before an output file; "-filter_threads" and "-filter_complex_threads"
// are global options for simple and complex filter graphs.
func (f *FFmpeg) threadArgs(opt string) []string {
	if f.Threads <= 0 {
		return nil
	}
	return []string{opt, strconv.Itoa(f.Threads)}
}

// inputLimitArgs are the input options (placed right before -i) that bound one
// decode: the decoder thread count and the pixel cap.
func (f *FFmpeg) inputLimitArgs() []string {
	return append(f.threadArgs("-threads"), maxPixelsArgs(f.MaxPixels)...)
}

// ErrPixelLimit marks an ffmpeg/ffprobe failure caused by a frame larger than the
// -max_pixels cap (FFmpeg.MaxPixels). It is a property of the file, so retrying is
// pointless. Probe wraps it together with ErrCannotDecode.
var ErrPixelLimit = errors.New("media: frame exceeds the pixel limit")

// pixelLimitSignature is libavutil's message when a frame exceeds -max_pixels.
const pixelLimitSignature = "exceeds specified max pixel count"

// withPixelLimit wraps err with ErrPixelLimit when it is an ffmpeg/ffprobe exit
// failure whose stderr shows the pixel cap refused a frame.
func withPixelLimit(err error) error {
	var ee *ExecError
	if errors.As(err, &ee) && isExitFailure(err) && strings.Contains(ee.StderrTail, pixelLimitSignature) {
		return fmt.Errorf("%w: %w", ErrPixelLimit, err)
	}
	return err
}

// previewFrameCap is the most frames a preview capped at maxDurationSec may hold at
// fps: ceil(maxDurationSec·fps), at least 1. The fps filter stamps frame k at k/fps,
// so the preview lasts at most ceil(maxDurationSec·fps)/fps < maxDurationSec + 1/fps
// whatever timestamps the source carries.
func previewFrameCap(maxDurationSec float64, fps int) int {
	// The epsilon keeps 180·2 = 360 at 360 when the product is a hair above.
	return max(1, int(math.Ceil(maxDurationSec*float64(fps)-1e-9)))
}

// previewArgs is BuildPreview's ffmpeg argument list (in and tmp are absolute) for
// plan (its fps is the output rate, also for the frame cap).
func (f *FFmpeg) previewArgs(in, tmp string, height int, maxDurationSec float64, plan previewPlan) []string {
	args := []string{"-y", "-hide_banner", "-nostdin", "-loglevel", "error"}
	args = append(args, f.threadArgs("-filter_threads")...)
	args = append(args, f.inputLimitArgs()...)
	if plan.skip != "" {
		// A decoder option, so an input option.
		args = append(args, "-skip_frame:v", plan.skip)
	}
	if maxDurationSec > 0 {
		// An input option: stop reading the source after maxDurationSec, whatever its
		// header claims.
		args = append(args, "-t", formatSeconds(maxDurationSec))
	}
	args = append(args, "-i", in, "-map", "0:V:0")
	args = append(args, stripMetadataArgs...)
	vf := fmt.Sprintf("scale=-2:'min(%d,trunc(ih/2)*2)',", height)
	fps := fmt.Sprintf("fps=%d", plan.fps)
	if plan.keyframesOnly() {
		// The decoder may skip some of the keyframes (see decodedGapShortfall): report
		// the frames it produced on stdout.
		vf += decodedFramesFilter + ","
		// The last keyframe's own tick lies at or past the end the fps filter derives
		// from it (its pts plus one source frame), so "round" would drop it and end the
		// preview a keyframe interval early; "pass" keeps it, like a full decode does.
		fps += ":eof_action=pass"
	}
	args = append(args,
		"-vf", vf+fps,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "28", "-pix_fmt", "yuv420p")
	args = append(args, f.threadArgs("-threads")...)
	if maxDurationSec > 0 {
		// A hard bound on what the model sees, independent of timestamps.
		args = append(args, "-frames:v", strconv.Itoa(previewFrameCap(maxDurationSec, plan.fps)))
	}
	return append(args,
		"-an", "-sn", "-dn",
		"-movflags", "+faststart",
		"-f", "mp4", tmp)
}

// BuildPreview writes a small analysis copy (the file the model sees) and reports its
// real frame rate:
//
//	ffmpeg [-filter_threads n] [-threads n] [-max_pixels N] -skip_frame:v nokey|noref [-t max] \
//	  -i in -map 0:V:0 -map_metadata -1 -map_chapters -1 -fflags +bitexact -flags:v +bitexact \
//	  -vf "scale=-2:'min(<height>,trunc(ih/2)*2)',[<decodedFramesFilter>,]fps=<fps>[:eof_action=pass]" \
//	  -c:v libx264 -preset veryfast -crf 28 -pix_fmt yuv420p [-threads n] [-frames:v cap] \
//	  -an -sn -dn -movflags +faststart out.mp4
//
// Rotation is applied (auto-rotate), so the preview is display-oriented.
//
// Decoding the source dominates the cost, so not every frame is decoded. A cheap
// ffprobe pass first reads the keyframe layout of the whole video stream from the
// packet flags (demux only, see probeKeyframes). When no stretch of the video goes
// longer than one output frame interval without a keyframe (phone video: a keyframe
// about every second; see planPreview), only keyframes are decoded
// (-skip_frame:v nokey) and the preview runs at min(fps, ⌊1/largest gap⌋), at least 1.
// Otherwise non-reference frames are skipped (-skip_frame:v noref) at the requested
// fps; the same happens when the probe fails.
//
// A skip mode can silently produce a broken preview: the H.264 decoder decodes only IDR
// pictures under nokey, so an open-GOP or intra-refresh source (whose recovery points
// the container flags as keyframes) yields an empty file or a single frame, and ffmpeg
// still exits 0; when the source also has periodic IDR pictures, the decoder shows one
// picture from IDR to IDR instead. So a skip-mode preview is read back (see
// probePreviewOutput and previewShortfall) and must have a video stream with at least
// 80% of the expected frames and reach the source's end, and a keyframes-only one must
// not have skipped keyframes (the frames the decoder produced are printed during the
// encode, see decodedGapShortfall); if it does not, or ffmpeg fails, the preview is
// rebuilt with noref at the requested fps and then from a full decode (which is
// accepted as long as it has a frame). PreviewInfo describes the preview that was kept,
// and the model must be told its FPS, not the requested fps. On a 59 s 4K HEVC 60 fps
// iPhone clip with a keyframe every 0.93 s (2 threads) the preview took 173 s decoding
// every frame, 90 s with noref and 4.5 s keyframes-only (1 fps, 59 frames).
//
// The source is never upscaled: the output height is min(height, source height),
// rounded down to even, and the width keeps the aspect ratio (also even). Output is
// H.264 yuv420p in MP4 with +faststart, video only (first real video stream). The file
// is written next to out under a temporary name and renamed on success, so out is
// either complete or absent.
//
// Limits:
//   - maxDurationSec > 0 caps the length independently of the container header: the
//     source is read for at most maxDurationSec (-t as an input option) and at most
//     ceil(maxDurationSec·fps) frames are written (fps being the preview's real rate),
//     so the preview lasts less than maxDurationSec + 1/fps even when the header
//     claims a shorter duration than the stream really has. 0 means no cap; a
//     negative or non-finite value is an error.
//   - FFmpeg.MaxPixels makes the decoder refuse larger frames (also after a mid-stream
//     resolution change); a file that trips it fails with an error wrapping
//     ErrPixelLimit.
//   - FFmpeg.Threads bounds the decoder, filter and encoder thread counts.
//   - No metadata of the source is copied (see stripMetadataArgs): no GPS location,
//     title, comment or chapters reach the model.
func (f *FFmpeg) BuildPreview(ctx context.Context, in, out string, height, fps int, maxDurationSec float64) (PreviewInfo, error) {
	if height <= 0 || fps <= 0 {
		return PreviewInfo{}, fmt.Errorf("media: preview: height (%d) and fps (%d) must be positive", height, fps)
	}
	if math.IsNaN(maxDurationSec) || math.IsInf(maxDurationSec, 0) || maxDurationSec < 0 {
		return PreviewInfo{}, fmt.Errorf("media: preview: invalid max duration %v (0 = no cap)", maxDurationSec)
	}
	inAbs, err := absPath(in)
	if err != nil {
		return PreviewInfo{}, fmt.Errorf("media: preview input: %w", err)
	}
	outAbs, err := absPath(out)
	if err != nil {
		return PreviewInfo{}, fmt.Errorf("media: preview output: %w", err)
	}
	if inAbs == outAbs {
		return PreviewInfo{}, fmt.Errorf("media: preview: output %s would overwrite the input", out)
	}
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return PreviewInfo{}, fmt.Errorf("media: preview: %w", err)
	}
	tmp := filepath.Join(filepath.Dir(outAbs), "."+filepath.Base(outAbs)+".partial")
	defer os.Remove(tmp) // no-op after a successful rename

	plan, sourceSec := previewPlan{fps: fps, skip: skipNonRef}, 0.0
	if st, err := f.probeKeyframes(ctx, inAbs); err != nil {
		f.logger().Debug("media: preview keyframe probe failed; skipping non-reference frames only", "err", err)
	} else {
		plan, sourceSec = planPreview(st, fps), st.DurationSec()
		f.logger().Debug("media: preview decode plan", "keyframes", st.Keyframes, "maxKeyframeGapSec", st.MaxGapSec,
			"durationSec", sourceSec, "skipFrame", plan.skip, "fps", plan.fps, "requestedFps", fps)
	}
	return f.encodePreview(ctx, inAbs, tmp, outAbs, height, maxDurationSec, sourceSec, previewAttempts(plan, fps))
}

// previewAttempts is the order BuildPreview tries decode modes in, starting with plan:
// keyframes only, then non-reference frames skipped at the requested fps, then a full
// decode at the requested fps.
func previewAttempts(plan previewPlan, requested int) []previewPlan {
	full := previewPlan{fps: requested}
	switch plan.skip {
	case "":
		return []previewPlan{full}
	case skipNonRef:
		return []previewPlan{{fps: requested, skip: skipNonRef}, full}
	default:
		return []previewPlan{plan, {fps: requested, skip: skipNonRef}, full}
	}
}

// encodePreview runs the preview encode for each attempt in turn into tmp until one is
// usable (see tryPreview), and renames it to out. sourceSec is the source stream's
// length (0 = unknown).
func (f *FFmpeg) encodePreview(ctx context.Context, in, tmp, out string, height int, maxDurationSec, sourceSec float64, attempts []previewPlan) (PreviewInfo, error) {
	err := errors.New("media: preview: no decode mode to try")
	var prev previewPlan
	for i, p := range attempts {
		if i > 0 {
			f.logger().Warn("media: preview unusable; retrying with another decode mode",
				"skipFrame", prev.skip, "fps", prev.fps, "retrySkipFrame", p.skip, "retryFps", p.fps, "err", err)
		}
		var retry bool
		retry, err = f.tryPreview(ctx, in, tmp, height, maxDurationSec, sourceSec, p, i == len(attempts)-1)
		if err == nil {
			if err := os.Rename(tmp, out); err != nil {
				return PreviewInfo{}, fmt.Errorf("media: preview: %w", err)
			}
			return p.info(), nil
		}
		if !retry || ctx.Err() != nil {
			break
		}
		prev = p
	}
	return PreviewInfo{}, err
}

// tryPreview encodes one attempt into tmp and reads the result back. The attempt is
// unusable when ffmpeg exits with a failure or its output has no video frame (ffmpeg
// exits 0 and writes an empty MP4 when the decoder produced nothing); a skip-mode
// attempt that is not the last one must also pass previewShortfall and, keyframes
// only, decodedGapShortfall. retry reports whether another decode mode may do better:
// not after a pixel-limit failure (every mode hits it) or when ffmpeg or ffprobe did
// not run to completion (a missing binary, a signal, a timeout).
func (f *FFmpeg) tryPreview(ctx context.Context, in, tmp string, height int, maxDurationSec, sourceSec float64, p previewPlan, last bool) (retry bool, err error) {
	stdout, err := f.run(ctx, previewTimeout, f.ffmpegBin(), f.previewArgs(in, tmp, height, maxDurationSec, p)...)
	if err != nil {
		err = withPixelLimit(err)
		return isExitFailure(err) && !errors.Is(err, ErrPixelLimit), err
	}
	got, err := f.probePreviewOutput(ctx, tmp, p.fps)
	if err != nil {
		var ee *ExecError
		return isExitFailure(err) || !errors.As(err, &ee),
			fmt.Errorf("media: preview check (-skip_frame %q, %d fps): %w", p.skip, p.fps, err)
	}
	if last || p.skip == "" {
		return false, nil
	}
	why := previewShortfall(got, p.fps, sourceSec, maxDurationSec)
	if why == "" && p.keyframesOnly() {
		why = decodedGapShortfall(parseDecodedFrames(stdout), p.fps)
	}
	if why != "" {
		return true, fmt.Errorf("media: preview with -skip_frame %s at %d fps is incomplete: %s", p.skip, p.fps, why)
	}
	return false, nil
}

// ExpectedFrames is the number of frames ExtractFrames writes for a window of
// durationSec sampled at fps, provided the window lies inside the video:
// max(1, floor(durationSec·fps)). A window running past the last decoded frame
// yields fewer. Returns 0 for non-positive or non-finite inputs.
func ExpectedFrames(durationSec, fps float64) int {
	if !finitePositive(durationSec) || !finitePositive(fps) {
		return 0
	}
	// The epsilon keeps 0.6·5 = 2.9999999999999996 at 3.
	return max(1, int(math.Floor(durationSec*fps+1e-9)))
}

// ExtractFrames runs one dense-sampling invocation (see ExtractArgs) and returns the
// written files sorted by frame number.
//
// Sampling contract (verified against a frame-numbered fixture in the tests): frame i
// (0-based) is sampled at StartSec + i/FPS for i < ExpectedFrames(DurationSec, FPS).
// Precisely, frame i is the last source frame at or before that instant, and frame 0
// is the first source frame at or after StartSec, so the error is under one source
// frame period (33 ms at 30 fps). This needs `fps=<FPS>:round=up:start_time=0`:
// ffmpeg's default fps rounding ("near", start at the first frame) picks the last
// source frame before StartSec + (i+0.5)/FPS, a systematic lag of up to half a
// sampling interval (+125 ms at 4 fps). A `trim=end_frame=N` after it makes the frame
// count exact, and `format=yuvj420p` lets ffmpeg exit cleanly when the window has no
// frames at all (e.g. it starts after the last frame): that returns an empty result,
// not an error.
//
// Output directories are created. Files that already match either pattern (a
// previous attempt) are removed first, so the result lists only this run's frames.
// On failure the partial frames are removed. The small copy is always SmallWidth wide
// (height keeps the display aspect ratio, rounded to even); rotation metadata is
// applied to both copies.
//
// Limits: FFmpeg.MaxPixels makes the decoder refuse larger frames (a file that trips
// it fails with an error wrapping ErrPixelLimit; frames after a mid-stream resolution
// change beyond the cap are dropped by the decoder); FFmpeg.Threads bounds the
// decoder, filter graph and both JPEG encoders. No source metadata is copied and the
// JPEGs carry no encoder comment (see stripMetadataArgs).
func (f *FFmpeg) ExtractFrames(ctx context.Context, a ExtractArgs) (*ExtractResult, error) {
	a = a.withDefaults()
	if err := a.validate(); err != nil {
		return nil, err
	}
	input, err := absPath(a.Input)
	if err != nil {
		return nil, fmt.Errorf("media: extract input: %w", err)
	}
	full, err := parseFramePattern(a.FullPattern)
	if err != nil {
		return nil, fmt.Errorf("media: extract full pattern: %w", err)
	}
	small, err := parseFramePattern(a.SmallPattern)
	if err != nil {
		return nil, fmt.Errorf("media: extract small pattern: %w", err)
	}
	if full.dir == small.dir && full.prefix == small.prefix && full.suffix == small.suffix {
		return nil, errors.New("media: extract: full and small patterns must differ")
	}
	for _, p := range []framePattern{full, small} {
		if err := os.MkdirAll(p.dir, 0o755); err != nil {
			return nil, fmt.Errorf("media: extract: %w", err)
		}
		if err := p.removeAll(); err != nil {
			return nil, fmt.Errorf("media: extract: remove stale frames: %w", err)
		}
	}

	_, runErr := f.run(ctx, extractTimeout, f.ffmpegBin(), f.extractArgs(a, input, full.path, small.path)...)
	runErr = withPixelLimit(runErr)

	fullFiles, err1 := full.list()
	smallFiles, err2 := small.list()
	if runErr != nil || err1 != nil || err2 != nil {
		_ = full.removeAll()
		_ = small.removeAll()
		switch {
		case runErr != nil && len(fullFiles) == 0 && len(smallFiles) == 0 && isNoFramesFailure(runErr):
			// Older ffmpeg builds fail instead of exiting 0 when no frame reached the
			// encoders; that is an empty window, not a broken file.
			f.logger().Debug("media: extract window has no frames", "start", a.StartSec, "duration", a.DurationSec)
			return &ExtractResult{}, nil
		case runErr != nil:
			return nil, runErr
		default:
			return nil, fmt.Errorf("media: extract: list frames: %w", errors.Join(err1, err2))
		}
	}

	res := &ExtractResult{}
	for num := 1; ; num++ { // image2 numbers from 1
		fp, okF := fullFiles[num]
		sp, okS := smallFiles[num]
		if !okF || !okS {
			break
		}
		res.Full = append(res.Full, fp)
		res.Small = append(res.Small, sp)
		delete(fullFiles, num)
		delete(smallFiles, num)
	}
	if len(fullFiles) > 0 || len(smallFiles) > 0 {
		f.logger().Warn("media: extract produced unpaired frames; truncating",
			"kept", len(res.Full), "extraFull", len(fullFiles), "extraSmall", len(smallFiles), "pattern", full.path)
		for _, m := range []map[int]string{fullFiles, smallFiles} {
			for _, p := range m {
				_ = os.Remove(p)
			}
		}
	}
	return res, nil
}

// extractArgs is ExtractFrames' ffmpeg argument list (a has its defaults applied and
// is valid; input and the patterns are absolute).
func (f *FFmpeg) extractArgs(a ExtractArgs, input, fullPattern, smallPattern string) []string {
	n := strconv.Itoa(ExpectedFrames(a.DurationSec, a.FPS))
	graph := fmt.Sprintf(
		"[0:V:0]fps=%s:round=up:start_time=0,trim=end_frame=%s,format=yuvj420p,split=2[full][small];"+
			"[small]scale=%d:-2[thumb]",
		formatFloat(a.FPS), n, a.SmallWidth)

	args := []string{"-y", "-hide_banner", "-nostdin", "-loglevel", "error"}
	args = append(args, f.threadArgs("-filter_complex_threads")...)
	args = append(args, f.inputLimitArgs()...)
	args = append(args,
		"-ss", formatSeconds(a.StartSec), "-t", formatSeconds(a.DurationSec),
		"-i", input,
		"-filter_complex", graph)
	// Output options apply to the next output file only, so each JPEG sequence gets
	// its own frame cap, encoder thread cap and metadata stripping. -frames:v repeats
	// the trim bound because a mid-stream resolution change rebuilds the filter graph,
	// which restarts trim's frame count; it also bounds the scratch disk used.
	for _, o := range []struct {
		label, pattern string
		q              int
	}{
		{"[full]", fullPattern, a.FullQScale},
		{"[thumb]", smallPattern, a.SmallQScale},
	} {
		args = append(args, "-map", o.label, "-q:v", strconv.Itoa(o.q), "-frames:v", n)
		args = append(args, f.threadArgs("-threads")...)
		args = append(args, stripMetadataArgs...)
		args = append(args, o.pattern)
	}
	return args
}

func (a ExtractArgs) withDefaults() ExtractArgs {
	if a.SmallWidth <= 0 {
		a.SmallWidth = DefaultSmallWidth
	}
	if a.FullQScale <= 0 {
		a.FullQScale = DefaultFullQScale
	}
	if a.SmallQScale <= 0 {
		a.SmallQScale = DefaultSmallQScale
	}
	return a
}

func (a ExtractArgs) validate() error {
	switch {
	case a.Input == "":
		return errors.New("media: extract: empty input path")
	case math.IsNaN(a.StartSec) || math.IsInf(a.StartSec, 0) || a.StartSec < 0:
		return fmt.Errorf("media: extract: invalid start %v", a.StartSec)
	case !finitePositive(a.DurationSec):
		return fmt.Errorf("media: extract: invalid duration %v", a.DurationSec)
	case !finitePositive(a.FPS):
		return fmt.Errorf("media: extract: invalid fps %v", a.FPS)
	}
	return nil
}

func finitePositive(v float64) bool { return v > 0 && !math.IsInf(v, 1) }

// isNoFramesFailure recognises ffmpeg's complaints when a filter graph produced no
// frame at all (older versions exit non-zero in that case).
func isNoFramesFailure(err error) bool {
	var ee *ExecError
	if !errors.As(err, &ee) || !isExitFailure(err) {
		return false
	}
	for _, sig := range []string{
		"Could not open encoder before EOF",
		"received no packets",
		"Output file is empty",
	} {
		if strings.Contains(ee.StderrTail, sig) {
			return true
		}
	}
	return false
}

// formatFloat renders v without exponent notation ("0.0000001", "3.3333333333333335").
func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// formatSeconds renders a time in seconds rounded to microseconds (ffmpeg's time
// resolution), without exponent notation.
func formatSeconds(v float64) string { return formatFloat(math.Round(v*1e6) / 1e6) }

// framePattern is a parsed image2 output pattern such as /dir/0003_%04d.jpg.
type framePattern struct {
	path   string // absolute pattern, as passed to ffmpeg
	dir    string
	prefix string // literal text before the number (%% unescaped)
	suffix string // literal text after the number
}

// parseFramePattern accepts exactly one %d / %0Nd directive in the file name part
// ("%%" is a literal percent sign).
func parseFramePattern(p string) (framePattern, error) {
	absP, err := absPath(p)
	if err != nil {
		return framePattern{}, err
	}
	dir, base := filepath.Split(absP)
	if strings.Contains(dir, "%") {
		return framePattern{}, fmt.Errorf("%q: the frame number directive must be in the file name", p)
	}
	var pre, suf strings.Builder
	cur, found := &pre, false
	for i := 0; i < len(base); i++ {
		c := base[i]
		if c != '%' {
			cur.WriteByte(c)
			continue
		}
		if i+1 < len(base) && base[i+1] == '%' {
			cur.WriteByte('%')
			i++
			continue
		}
		j := i + 1
		for j < len(base) && base[j] >= '0' && base[j] <= '9' {
			j++
		}
		if j >= len(base) || base[j] != 'd' {
			return framePattern{}, fmt.Errorf("%q: unsupported directive (want %%d or %%0Nd)", p)
		}
		if found {
			return framePattern{}, fmt.Errorf("%q: more than one frame number directive", p)
		}
		found, cur, i = true, &suf, j
	}
	if !found {
		return framePattern{}, fmt.Errorf("%q: missing frame number directive (%%d or %%0Nd)", p)
	}
	return framePattern{path: absP, dir: filepath.Clean(dir), prefix: pre.String(), suffix: suf.String()}, nil
}

// list returns the existing files matching the pattern, keyed by frame number.
func (p framePattern) list() (map[int]string, error) {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[int]string{}, nil
		}
		return nil, err
	}
	files := make(map[int]string)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if len(name) <= len(p.prefix)+len(p.suffix) || !strings.HasPrefix(name, p.prefix) || !strings.HasSuffix(name, p.suffix) {
			continue
		}
		digits := name[len(p.prefix) : len(name)-len(p.suffix)]
		if strings.Trim(digits, "0123456789") != "" {
			continue
		}
		num, err := strconv.Atoi(digits)
		if err != nil {
			continue
		}
		files[num] = filepath.Join(p.dir, name)
	}
	return files, nil
}

// removeAll deletes every file matching the pattern.
func (p framePattern) removeAll() error {
	files, err := p.list()
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range files {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
