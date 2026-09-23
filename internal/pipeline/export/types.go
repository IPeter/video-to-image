// Package export is pipeline step 6: store the selected full-resolution frames as-is
// (already -qscale:v 2 from step 3; re-encoding would only lose quality) or, when the
// review asked for it, rotated upright and cropped (re-encoded once), the 640 px
// thumbnails, manifest.json, and a streamed ZIP of all images + manifest.
package export

import "time"

// Scores are copied into manifest.json.
type Scores struct {
	Relevance       float64 `json:"relevance"`
	Sharpness       float64 `json:"sharpness"`
	SharpnessZ      float64 `json:"sharpnessZ"`
	ExposurePenalty float64 `json:"exposurePenalty"`
	Score           float64 `json:"score"`
}

// Image is one selected frame to export.
type Image struct {
	Index        int    // 1-based display order
	ImageID      string // img_ + ULID
	CandidateID  string
	ShotID       string
	ShotLabel    string
	TimestampSec float64
	FullPath     string // local source-resolution JPG
	SmallPath    string // local 640 px JPG
	Reason       string
	Scores       Scores
	// Edit, when non-nil and not a no-op, is applied to the full-resolution frame
	// (rotate, then crop) and the thumbnail is re-made from the result. nil → the frame
	// is copied byte for byte, as before.
	Edit *Edit
}

// Edit reframes one image. Rotation is applied first; Crop is in the UPRIGHT image.
type Edit struct {
	RotateCW int    // 0, 90, 180 or 270 degrees clockwise
	Crop     *NRect // normalised crop of the upright image; nil = no crop
}

// NRect is a rectangle in normalised [0,1] coordinates (X0 < X1, Y0 < Y1).
type NRect struct {
	X0 float64 `json:"x0"`
	Y0 float64 `json:"y0"`
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
}

type MissingShot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Input struct {
	DomainKey    string
	JobID        string
	CreatedAt    time.Time // job creation, written to the manifest
	Summary      string
	Explanation  string
	Images       []Image
	MissingShots []MissingShot
}

// Exported is an Image after upload.
type Exported struct {
	Image
	FileName string // <domainKey>_<jobShortId>_<index>_<shotId>_<mm-ss>.jpg
	ImageKey string // storage key of the full image
	ThumbKey string // storage key of the thumbnail
	Width    int    // of the exported (edited) image
	Height   int
	Bytes    int64 // size of the full image
	Rotation int   // applied rotation (0 when none)
	Cropped  bool  // a crop was applied
}

type Output struct {
	Images      []Exported
	ManifestKey string
	ZipKey      string
	ZipName     string // <domainKey>_<jobShortId>_images.zip, for Content-Disposition
}

// Manifest is manifest.json (also inside the ZIP).
type Manifest struct {
	JobID        string          `json:"jobId"`
	DomainKey    string          `json:"domainKey"`
	CreatedAt    time.Time       `json:"createdAt"`
	Summary      string          `json:"summary"`
	Explanation  string          `json:"explanation"`
	Images       []ManifestImage `json:"images"`
	MissingShots []MissingShot   `json:"missingShots"`
}

type ManifestImage struct {
	Index          int     `json:"index"`
	File           string  `json:"file"`
	ShotID         string  `json:"shotId"`
	ShotLabel      string  `json:"shotLabel"`
	TimestampSec   float64 `json:"timestampSec"`
	TimestampLabel string  `json:"timestampLabel"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	Reason         string  `json:"reason"`
	Scores         Scores  `json:"scores"`
	Rotation       int     `json:"rotation"`       // degrees clockwise applied to the source frame
	Crop           *NRect  `json:"crop,omitempty"` // normalised crop of the upright frame
}
