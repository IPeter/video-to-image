// Package preview is pipeline step 1: build the low-res analysis copy. The original is
// never sent to the model.
package preview

import (
	"context"
	"fmt"
	"os"
	"time"

	"video-to-image/internal/media"
)

type Input struct {
	SourcePath string // original video
	OutPath    string // where to write preview.mp4
	Height     int    // domain model.previewHeight (480)
	FPS        int    // domain model.previewFps (2)
	// MaxDurationSec caps the encoded length (-t) so a container header that lies about
	// its duration cannot make the model see (and bill) a much longer video. 0 = no cap.
	MaxDurationSec float64
}

type Output struct {
	Path          string
	SizeBytes     int64
	Took          time.Duration
	FPS           float64 // the preview's real frame rate (see media.PreviewInfo)
	KeyframesOnly bool
}

// Build encodes the preview (480p, 2 fps, no audio for cars).
func Build(ctx context.Context, ff *media.FFmpeg, in Input) (*Output, error) {
	start := time.Now()
	info, err := ff.BuildPreview(ctx, in.SourcePath, in.OutPath, in.Height, in.FPS, in.MaxDurationSec)
	if err != nil {
		return nil, fmt.Errorf("preview: %w", err)
	}
	st, err := os.Stat(in.OutPath)
	if err != nil {
		return nil, fmt.Errorf("preview: stat output: %w", err)
	}
	return &Output{Path: in.OutPath, SizeBytes: st.Size(), Took: time.Since(start), FPS: info.FPS, KeyframesOnly: info.KeyframesOnly}, nil
}
