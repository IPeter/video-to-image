package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// BuildPreview's decode modes (the decoder's -skip_frame value). Decoding dominates
// the preview's cost on 4K HEVC phone video (59 s iPhone clip, 2 threads: 173 s
// decoding everything, 90 s with noref, 4.5 s with nokey), and the preview only needs
// a few frames per second.
const (
	skipNonKey = "nokey" // decode keyframes only
	skipNonRef = "noref" // skip frames no other frame references
)

// Decode plan tunables (see planPreview and previewShortfall).
const (
	// keyframeRateSlack absorbs timestamp rounding when the largest keyframe gap is
	// turned into a frame rate (keyframes every 0.5005 s at 29.97 fps are still 2 per
	// second).
	keyframeRateSlack = 0.05
	// keyframeGapToleranceSec is how much longer than one preview frame interval the
	// largest keyframe gap may be for the keyframes-only path. It only matters at 1 fps
	// (a faster rate is derived from the gap itself): gaps of up to 1.25 s, i.e. at
	// least 0.8 keyframes per second everywhere, repeat at most one preview frame.
	keyframeGapToleranceSec = 0.25
	// minPreviewFrameShare is the share of the expected frame count
	// (⌊min(source, cap)·fps⌋) a skip-mode preview must have to be accepted.
	minPreviewFrameShare = 0.8
	// previewCoverageSlackSec: an uncapped skip-mode preview may end up to one frame
	// interval plus this much before the source does.
	previewCoverageSlackSec = 0.25
	// decodedGapSlackSec absorbs timestamp differences between the keyframe packets the
	// probe measured and the frames the decoder stamps (a best-effort timestamp taken
	// from the dts is a few source frames off).
	decodedGapSlackSec = 0.1
)

// decodedFramesKey tags the frames that reach the fps filter of a keyframes-only
// preview so that decodedFramesFilter can print their timestamps.
const decodedFramesKey = "v2i.decoded"

// decodedFramesFilter prints the source timestamp of every frame passing it to ffmpeg's
// stdout ("frame:N pts:P pts_time:T" plus a "key=value" line, ≈55 bytes per frame: a
// keyframes-only plan needs a probe whose ≈105 bytes per packet fit maxStdoutBytes, so
// this output does too). It costs nothing measurable, and the key stays in the filter
// graph: the encoded preview is byte-identical without it.
const decodedFramesFilter = "metadata=mode=add:key=" + decodedFramesKey + ":value=1," +
	"metadata=mode=print:key=" + decodedFramesKey + ":file=-"

// keyframeStats is what probeKeyframes measured over the whole video stream.
type keyframeStats struct {
	Packets   int     // timed video packets
	Keyframes int     // packets flagged as keyframes
	StartSec  float64 // earliest packet time
	EndSec    float64 // latest packet end (its time plus its duration)
	// MaxGapSec is the longest stretch without a keyframe: between two consecutive
	// keyframes, from the stream start to the first one, or from the last one to the
	// stream end (the whole stream when there is no keyframe). A keyframes-only decode
	// shows the same picture for that long, and loses the tail after the last keyframe.
	MaxGapSec float64
}

// DurationSec is the video stream's length as its packets span it.
func (s keyframeStats) DurationSec() float64 { return s.EndSec - s.StartSec }

// previewPlan is how BuildPreview decodes and samples the source.
type previewPlan struct {
	fps  int    // output frame rate
	skip string // decoder -skip_frame value: skipNonKey, skipNonRef, or "" (decode everything)
}

func (p previewPlan) keyframesOnly() bool { return p.skip == skipNonKey }

func (p previewPlan) info() PreviewInfo {
	return PreviewInfo{FPS: float64(p.fps), KeyframesOnly: p.keyframesOnly()}
}

// planPreview picks the decode mode for a preview requested at fps. When no stretch of
// the video lacks a keyframe for longer than one output frame interval (plus
// keyframeGapToleranceSec), only keyframes are decoded and the preview runs at
// min(fps, ⌊1/MaxGapSec⌋), at least 1: sampling faster than the keyframes would only
// repeat frames. Otherwise non-reference frames are skipped and the requested fps is
// kept: the fps filter then picks among the reference frames only, i.e. within a few
// source frames (tens of milliseconds) of what a full decode would show.
//
// The largest gap, not the average rate, decides: a fast-cut intro followed by one
// long take has plenty of keyframes on average, but keyframes-only would freeze the
// take on its first frame.
func planPreview(st keyframeStats, fps int) previewPlan {
	noref := previewPlan{fps: fps, skip: skipNonRef}
	if st.Keyframes == 0 || !(st.MaxGapSec > 0) {
		return noref
	}
	out := fps
	if rate := 1/st.MaxGapSec + keyframeRateSlack; rate < float64(fps) {
		out = max(1, int(rate))
	}
	if st.MaxGapSec > 1/float64(out)+keyframeGapToleranceSec {
		return noref
	}
	return previewPlan{fps: out, skip: skipNonKey}
}

// probeKeyframes reads the keyframe layout of the first real video stream from the
// container's packet flags, over the whole stream. Nothing is decoded beyond what
// ffprobe's stream analysis needs (capped like Probe): on a 59 s 4K HEVC iPhone clip
// (377 MB, 3,543 packets) it takes 0.05–0.2 s with the file in the page cache, against
// ≈1.1 s for decoding the keyframes with -skip_frame nokey. The JSON stays far below
// maxStdoutBytes (≈105 bytes per packet: 180 s at 240 fps is ≈4.5 MB). It is bounded by
// keyframeProbeTimeout.
func (f *FFmpeg) probeKeyframes(ctx context.Context, in string) (keyframeStats, error) {
	ctx, cancel := context.WithTimeout(ctx, keyframeProbeTimeout)
	defer cancel()
	out, err := f.run(ctx, 0, f.ffprobeBin(), f.keyframeProbeArgs(in)...)
	if err != nil {
		return keyframeStats{}, err
	}
	return parseKeyframePackets(out)
}

// keyframeProbeArgs is probeKeyframes' ffprobe argument list (in is absolute).
func (f *FFmpeg) keyframeProbeArgs(in string) []string {
	args := []string{"-v", "error"}
	args = append(args, maxPixelsArgs(f.probeMaxPixels())...)
	return append(args,
		"-select_streams", "V:0",
		"-show_entries", "packet=pts_time,dts_time,duration_time,flags",
		"-of", "json=compact=1",
		in)
}

type keyframeJSON struct {
	Packets []struct {
		PtsTime      string `json:"pts_time"`
		DtsTime      string `json:"dts_time"`
		DurationTime string `json:"duration_time"`
		Flags        string `json:"flags"` // "K__": keyframe, discard, corrupt
	} `json:"packets"`
}

// parseKeyframePackets turns probeKeyframes' JSON into keyframeStats. Packets marked
// discard ('D', before an edit list's start) are ignored; a packet's time is its pts,
// else its dts. Packets come in decode order, so the keyframe times are sorted before
// the gaps are measured.
func parseKeyframePackets(out []byte) (keyframeStats, error) {
	var p keyframeJSON
	if err := json.Unmarshal(out, &p); err != nil {
		return keyframeStats{}, fmt.Errorf("media: parse keyframe probe: %w", err)
	}
	var st keyframeStats
	var keys []float64
	first, last := math.Inf(1), math.Inf(-1)
	for _, pk := range p.Packets {
		if len(pk.Flags) > 1 && pk.Flags[1] == 'D' {
			continue
		}
		t, ok := parseFinite(pk.PtsTime)
		if !ok {
			t, ok = parseFinite(pk.DtsTime)
		}
		if !ok {
			continue
		}
		st.Packets++
		if strings.HasPrefix(pk.Flags, "K") {
			keys = append(keys, t)
		}
		d, ok := parseFinite(pk.DurationTime)
		if !ok || d < 0 {
			d = 0
		}
		first, last = math.Min(first, t), math.Max(last, t+d)
	}
	if st.Packets == 0 {
		return keyframeStats{}, errors.New("media: keyframe probe: no timed video packets")
	}
	st.Keyframes, st.StartSec, st.EndSec = len(keys), first, last
	slices.Sort(keys)
	prev := first
	for _, k := range keys {
		st.MaxGapSec = math.Max(st.MaxGapSec, k-prev)
		prev = k
	}
	st.MaxGapSec = math.Max(st.MaxGapSec, last-prev)
	return st, nil
}

// previewOutput is what probePreviewOutput read back from an encoded preview.
type previewOutput struct {
	Frames      int     // video packets (one per frame in the MP4 BuildPreview writes)
	DurationSec float64 // container duration, else the stream's, else Frames/fps
}

// probePreviewOutput counts the frames of the first video stream of a preview BuildPreview
// just wrote (demux only: a few hundred small packets). A file without a video stream
// (ffmpeg exits 0 and writes an empty MP4 when the decoder produced nothing) is an
// error, like one ffprobe cannot read.
func (f *FFmpeg) probePreviewOutput(ctx context.Context, path string, fps int) (previewOutput, error) {
	out, err := f.run(ctx, probeTimeout, f.ffprobeBin(), "-v", "error", "-count_packets",
		"-select_streams", "V:0", "-show_entries", "stream=nb_read_packets,duration:format=duration",
		"-of", "json", path)
	if err != nil {
		return previewOutput{}, err
	}
	var p struct {
		Streams []struct {
			Packets  string `json:"nb_read_packets"`
			Duration string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return previewOutput{}, fmt.Errorf("media: parse preview probe: %w", err)
	}
	if len(p.Streams) == 0 {
		return previewOutput{}, errors.New("media: preview has no video stream")
	}
	n, err := strconv.Atoi(strings.TrimSpace(p.Streams[0].Packets))
	if err != nil || n <= 0 {
		return previewOutput{}, fmt.Errorf("media: preview has no video frames (%q packets)", p.Streams[0].Packets)
	}
	po := previewOutput{Frames: n, DurationSec: float64(n) / float64(fps)}
	if d, ok := parsePositive(p.Format.Duration); ok {
		po.DurationSec = d
	} else if d, ok := parsePositive(p.Streams[0].Duration); ok {
		po.DurationSec = d
	}
	return po, nil
}

// previewShortfall says why a skip-mode preview at fps does not cover the source ("" when
// it does). sourceSec is the source stream's length (0 when unknown: only the frame
// count probePreviewOutput already requires is checked) and maxDurationSec the encode's
// cap (0 = none). The preview must have at least minPreviewFrameShare of
// ⌊min(sourceSec, maxDurationSec)·fps⌋ frames and, when the cap did not cut it, reach
// within one frame interval plus previewCoverageSlackSec of the source's end. A
// decoder that honours -skip_frame nokey only for IDR pictures (H.264 open GOP or
// intra refresh, whose recovery points are flagged as keyframes) or a keyframe layout
// the probe misjudged fails this.
func previewShortfall(got previewOutput, fps int, sourceSec, maxDurationSec float64) string {
	if !(sourceSec > 0) {
		return ""
	}
	span, capped := sourceSec, false
	if maxDurationSec > 0 && maxDurationSec < sourceSec {
		span, capped = maxDurationSec, true
	}
	if want := math.Floor(span*float64(fps) + 1e-9); float64(got.Frames) < minPreviewFrameShare*want {
		return fmt.Sprintf("%d frames, expected about %.0f (%.2f s at %d fps)", got.Frames, want, span, fps)
	}
	if minSec := sourceSec - 1/float64(fps) - previewCoverageSlackSec; !capped && got.DurationSec < minSec {
		return fmt.Sprintf("%.2f s long, the source %.2f s (at least %.2f s expected)", got.DurationSec, sourceSec, minSec)
	}
	return ""
}

// parseDecodedFrames returns the timestamps decodedFramesFilter printed, in seconds
// (lines it did not write and frames without a timestamp are skipped).
func parseDecodedFrames(out []byte) []float64 {
	var times []float64
	for line := range strings.Lines(string(out)) {
		if !strings.HasPrefix(line, "frame:") {
			continue
		}
		_, v, ok := strings.Cut(line, "pts_time:")
		if !ok {
			continue
		}
		if t, ok := parseFinite(strings.TrimSpace(v)); ok {
			times = append(times, t)
		}
	}
	return times
}

// decodedGapShortfall says why a keyframes-only preview at fps froze ("" when it did
// not); times are the source timestamps of the frames the decoder produced (see
// decodedFramesFilter). planPreview chose keyframes only because no two keyframe
// packets are further apart than 1/fps + keyframeGapToleranceSec, so a longer stretch
// between two decoded frames means the decoder skipped keyframes. H.264 decodes only
// IDR pictures under -skip_frame nokey: a stream that mixes IDR pictures with recovery
// points flagged as keyframes (intra refresh or open GOP with a periodic IDR) shows the
// same picture from one IDR to the next, and the fps filter fills that stretch with
// copies, so neither the frame count nor the length of the preview gives it away. A
// missing start or end is previewShortfall's to catch.
func decodedGapShortfall(times []float64, fps int) string {
	if len(times) == 0 {
		return "no decoded frame was reported"
	}
	slices.Sort(times)
	limit := 1/float64(fps) + keyframeGapToleranceSec + decodedGapSlackSec
	gap, after := 0.0, 0.0
	for i := 1; i < len(times); i++ {
		if d := times[i] - times[i-1]; d > gap {
			gap, after = d, times[i-1]
		}
	}
	if gap > limit {
		return fmt.Sprintf("the decoder skipped keyframes: nothing decoded for %.2f s after %.2f s (%d frames, at most %.2f s apart expected)",
			gap, after, len(times), limit)
	}
	return ""
}

// parseFinite parses a finite float ("N/A" and "" fail).
func parseFinite(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}
