package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// probeEntries is the -show_entries selection Probe asks ffprobe for.
const probeEntries = "format=duration,size" +
	":stream=width,height,r_frame_rate,avg_frame_rate,codec_name,duration" +
	":stream_side_data=rotation" +
	":stream_tags=rotate"

// Probe runs ffprobe on path and returns display-oriented VideoInfo. SizeBytes is the
// file size; MIME and SHA256 are left empty (the caller knows them from the upload).
// A file without a decodable video stream, or with zero duration/dimensions, returns
// an error wrapping ErrCannotDecode.
//
// Only the first real video stream is considered ("V:0": attached pictures and cover
// art are skipped). DurationSec comes from the container (format.duration), falling
// back to the stream duration. FPS is avg_frame_rate, falling back to r_frame_rate,
// and is 0 when neither is usable. Rotation comes from the display-matrix side data,
// falling back to the legacy "rotate" tag (converted to the display-matrix sign
// convention), normalised to (-180, 180]; for ±90° the coded width/height are swapped
// so Width/Height are the displayed dimensions.
//
// Errors that say nothing about the file — a missing ffprobe binary, a signal, a
// context deadline or cancellation, a missing path — are returned as-is (usually an
// *ExecError) and do NOT wrap ErrCannotDecode.
//
// Limits: when FFmpeg.MaxPixels > 0, ffprobe runs with -max_pixels at
// probePixelFactor (4) × MaxPixels. ffprobe decodes a few frames to fill in the
// stream parameters, so without a cap a small crafted file (e.g. an 8192x8192 flat
// colour clip of 200 KB) costs hundreds of MB of RAM here. With the looser cap a video
// that is too large but sane still probes, and the caller rejects it on Width×Height
// (RESOLUTION_TOO_HIGH), while a decode bomb beyond 4× fails cheaply with an error
// wrapping both ErrCannotDecode and ErrPixelLimit. ffprobe gets no -threads:
// libavformat already decodes with a single thread while probing.
func (f *FFmpeg) Probe(ctx context.Context, path string) (VideoInfo, error) {
	abs, err := absPath(path)
	if err != nil {
		return VideoInfo{}, fmt.Errorf("media: probe: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return VideoInfo{}, fmt.Errorf("media: probe: %w", err)
	}
	if !st.Mode().IsRegular() {
		return VideoInfo{}, fmt.Errorf("media: probe %s: not a regular file", path)
	}

	out, err := f.run(ctx, probeTimeout, f.ffprobeBin(), f.probeArgs(abs)...)
	if err != nil {
		if isExitFailure(err) {
			return VideoInfo{}, fmt.Errorf("%w: %w", ErrCannotDecode, withPixelLimit(err))
		}
		return VideoInfo{}, err
	}
	info, err := parseProbe(out)
	if err != nil {
		return VideoInfo{}, err
	}
	info.SizeBytes = st.Size()
	return info, nil
}

// probeArgs is Probe's ffprobe argument list (abs is absolute).
func (f *FFmpeg) probeArgs(abs string) []string {
	args := []string{"-v", "error"}
	args = append(args, maxPixelsArgs(f.probeMaxPixels())...)
	return append(args,
		"-select_streams", "V:0",
		"-show_entries", probeEntries,
		"-of", "json",
		abs)
}

type probeJSON struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
		Size     string `json:"size"`
	} `json:"format"`
}

type probeStream struct {
	CodecName    string                       `json:"codec_name"`
	Width        int                          `json:"width"`
	Height       int                          `json:"height"`
	RFrameRate   string                       `json:"r_frame_rate"`
	AvgFrameRate string                       `json:"avg_frame_rate"`
	Duration     string                       `json:"duration"`
	Tags         map[string]string            `json:"tags"`
	SideDataList []map[string]json.RawMessage `json:"side_data_list"`
}

// parseProbe turns ffprobe's JSON into VideoInfo (without SizeBytes).
func parseProbe(out []byte) (VideoInfo, error) {
	var p probeJSON
	if err := json.Unmarshal(out, &p); err != nil {
		return VideoInfo{}, fmt.Errorf("media: parse ffprobe output: %w", err)
	}
	if len(p.Streams) == 0 {
		return VideoInfo{}, fmt.Errorf("%w: no video stream", ErrCannotDecode)
	}
	s := p.Streams[0]
	if s.Width <= 0 || s.Height <= 0 {
		return VideoInfo{}, fmt.Errorf("%w: video stream has no dimensions (%dx%d)", ErrCannotDecode, s.Width, s.Height)
	}
	dur, ok := parsePositive(p.Format.Duration)
	if !ok {
		dur, ok = parsePositive(s.Duration)
	}
	if !ok {
		return VideoInfo{}, fmt.Errorf("%w: no usable duration (format %q, stream %q)", ErrCannotDecode, p.Format.Duration, s.Duration)
	}

	fps, ok := parseRate(s.AvgFrameRate)
	if !ok {
		fps, _ = parseRate(s.RFrameRate)
	}

	rot := streamRotation(s)
	w, h := s.Width, s.Height
	if abs(rot)%180 == 90 {
		w, h = h, w
	}
	return VideoInfo{
		DurationSec: dur,
		Width:       w,
		Height:      h,
		FPS:         math.Round(fps*1000) / 1000,
		Codec:       s.CodecName,
		Rotation:    rot,
	}, nil
}

// streamRotation returns the display-matrix rotation in degrees, normalised to
// (-180, 180]. The legacy "rotate" tag is clockwise while the display matrix (and
// ffprobe's side data) is counter-clockwise, so the tag is negated.
func streamRotation(s probeStream) int {
	for _, sd := range s.SideDataList {
		raw, ok := sd["rotation"]
		if !ok {
			continue
		}
		if v, ok := parseJSONNumber(raw); ok {
			return normalizeRotation(int(math.Round(v)))
		}
	}
	if tag, ok := s.Tags["rotate"]; ok {
		if v, err := strconv.ParseFloat(strings.TrimSpace(tag), 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return normalizeRotation(-int(math.Round(v)))
		}
	}
	return 0
}

// normalizeRotation maps any angle to (-180, 180].
func normalizeRotation(deg int) int {
	deg %= 360
	if deg <= -180 {
		deg += 360
	} else if deg > 180 {
		deg -= 360
	}
	return deg
}

// parseJSONNumber accepts a JSON number or a numeric string.
func parseJSONNumber(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, false
		}
		raw = []byte(strings.TrimSpace(s))
	}
	v, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// parsePositive parses a finite, strictly positive float ("N/A" and "" fail).
func parsePositive(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, false
	}
	return v, true
}

// parseRate parses ffprobe rationals such as "30000/1001" or "25/1" (also a bare
// number). "0/0" and other degenerate values fail.
func parseRate(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	num, den, found := strings.Cut(s, "/")
	if !found {
		return parsePositive(s)
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0, false
	}
	v := n / d
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, false
	}
	return v, true
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
