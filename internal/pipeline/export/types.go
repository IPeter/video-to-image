// Package export is pipeline step 6: store the selected full-resolution frames as-is
// (already -qscale:v 2 from step 3; re-encoding would only lose quality), the 640 px
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
	Width    int    // decoded from the JPEG header
	Height   int
	Bytes    int64 // size of the full image
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
}
