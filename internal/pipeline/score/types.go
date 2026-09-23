// Package score is pipeline step 4: sharpness (variance of the Laplacian), exposure
// penalty and dHash near-duplicate detection, computed on the 640 px scoring copies.
package score

import "video-to-image/internal/pipeline/extract"

// Rejection reasons.
const (
	RejectBlurry    = "blurry"    // below the job's BlurPercentile of sharpness (never a scene's best frame)
	RejectExposure  = "exposure"  // ExposurePenalty above ExposureRejectShare
	RejectDuplicate = "duplicate" // within DedupeMaxHamming of a sharper kept frame
)

type Params struct {
	DedupeMaxHamming    int     // domain selection.dedupeMaxHamming (6)
	BlurPercentile      float64 // 0.15: drop frames below this percentile of job sharpness
	ExposureRejectShare float64 // 0.40: hard reject when this share of pixels is crushed/blown
	// ShotRank is the dedupe priority of each shot ID, lower first (see ShotRanks): of a
	// near-duplicate pair from two different shots, the frame of the higher-priority shot
	// is kept. IDs missing from the map (domain.OtherShotID, shots outside the domain)
	// rank after every listed shot. nil → all shots rank the same, so the sharper frame
	// of every pair is kept.
	ShotRank map[string]int
	Workers  int // bounded pool size; 0 → runtime.NumCPU()
}

// Scored is a candidate with its quality measurements.
type Scored struct {
	extract.Candidate
	Sharpness       float64 // variance of the 3x3 Laplacian over the grayscale image
	SharpnessZ      float64 // z-score of Sharpness within the candidate's scene (0 when n<2 or σ=0)
	ExposurePenalty float64 // share of pixels in the darkest or brightest 5% of the luma range
	DHash           uint64  // 64-bit difference hash
	DuplicateOf     string  // ID of the kept frame this one duplicates (sharper, or of a higher-priority shot), "" if none
	Rejected        string  // "" or one of the Reject* reasons
}
