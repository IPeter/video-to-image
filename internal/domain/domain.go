// Package domain holds the per-vertical configuration (cars, homes, ...) loaded from
// YAML files in configs/domains. A domain decides behaviour: prompts, shot list,
// limits and selection tuning. See plan/04-domain-config.md.
package domain

import (
	"net/url"
	"strings"
)

// OtherShotID is the shot ID the model uses for a relevant moment that fits no shot.
const OtherShotID = "other"

// Config is one domain file. Field tags mirror plan/04-domain-config.md.
type Config struct {
	Key            string   `yaml:"key"            validate:"required,alphanum,lowercase"`
	Name           string   `yaml:"name"           validate:"required"`
	AllowedOrigins []string `yaml:"allowedOrigins" validate:"required,min=1,dive,url"`

	Limits    Limits    `yaml:"limits"`
	Model     Model     `yaml:"model"`
	Gate      Gate      `yaml:"gate"`
	Shots     []Shot    `yaml:"shots" validate:"required,min=1,dive"` // ordered by priority
	Selection Selection `yaml:"selection"`
	Review    Review    `yaml:"review"`
	Crop      Crop      `yaml:"crop"`

	RetentionHours int `yaml:"retentionHours" validate:"required,min=1"`

	Copy Copy `yaml:"copy"`
}

type Limits struct {
	MaxUploadBytes     int64 `yaml:"maxUploadBytes"     validate:"required,min=1"`
	MaxDurationSec     int   `yaml:"maxDurationSec"     validate:"required,min=1"`
	MinDurationSec     int   `yaml:"minDurationSec"     validate:"gte=0"`
	MinShortSidePx     int   `yaml:"minShortSidePx"     validate:"gte=0"`
	MaxCandidateFrames int   `yaml:"maxCandidateFrames" validate:"required,min=1"`
	RatePerIPPerHour   int   `yaml:"ratePerIpPerHour"   validate:"required,min=1"`
	// MaxPixels caps width×height of accepted videos (and of every ffmpeg decode) so a
	// tiny upload cannot expand into gigabytes of frames. 0 → DefaultMaxPixels.
	MaxPixels int64 `yaml:"maxPixels" validate:"gte=0"`
	// MaxFPS caps the source frame rate. 0 → DefaultMaxFPS.
	MaxFPS float64 `yaml:"maxFps" validate:"gte=0"`
}

// Defaults for optional limits.
const (
	DefaultMaxPixels       int64   = 4096 * 2176 // DCI 4K; covers 3840x2160 phone video in either orientation
	DefaultMaxFPS          float64 = 120
	DefaultMaxOutputTokens int32   = 4096
)

// EffectiveMaxPixels returns MaxPixels or its default.
func (l Limits) EffectiveMaxPixels() int64 {
	if l.MaxPixels > 0 {
		return l.MaxPixels
	}
	return DefaultMaxPixels
}

// EffectiveMaxFPS returns MaxFPS or its default.
func (l Limits) EffectiveMaxFPS() float64 {
	if l.MaxFPS > 0 {
		return l.MaxFPS
	}
	return DefaultMaxFPS
}

type Model struct {
	Name        string  `yaml:"name"           validate:"required"`
	Temperature float32 `yaml:"temperature"    validate:"gte=0,lte=2"`
	// Reasoning control; set at most one, the models differ (checked 2026-09-23):
	// gemini-3.5-flash-lite rejects thinkingBudget 0 and needs thinkingLevel
	// "minimal"/"low" for no thinking; gemini-3.8-flash rejects "minimal" and takes
	// thinkingBudget 0. Neither set → the model's default.
	ThinkingBudget *int32 `yaml:"thinkingBudget"`
	ThinkingLevel  string `yaml:"thinkingLevel" validate:"omitempty,oneof=minimal low medium high"`
	PreviewHeight  int    `yaml:"previewHeight"  validate:"required,min=64"`
	PreviewFPS     int    `yaml:"previewFps"     validate:"required,min=1"`
	// MaxOutputTokens bounds the model's answer (a runaway or prompt-injected response
	// is otherwise billed in full). 0 → DefaultMaxOutputTokens.
	MaxOutputTokens int32 `yaml:"maxOutputTokens" validate:"gte=0"`
}

// EffectiveMaxOutputTokens returns MaxOutputTokens or its default.
func (m Model) EffectiveMaxOutputTokens() int32 {
	if m.MaxOutputTokens > 0 {
		return m.MaxOutputTokens
	}
	return DefaultMaxOutputTokens
}

type Gate struct {
	Subject       string  `yaml:"subject"       validate:"required"`
	Instructions  string  `yaml:"instructions"  validate:"required"`
	MinConfidence float64 `yaml:"minConfidence" validate:"gte=0,lte=1"`
}

type Shot struct {
	ID          string `yaml:"id"          validate:"required"`
	Label       string `yaml:"label"       validate:"required"`
	Description string `yaml:"description" validate:"required"`
	Required    bool   `yaml:"required"`
}

type Selection struct {
	ImageCount       int     `yaml:"imageCount"       validate:"required,min=1"`
	Instructions     string  `yaml:"instructions"     validate:"required"`
	MaxScenes        int     `yaml:"maxScenes"        validate:"required,min=1"`
	SampleFPS        int     `yaml:"sampleFps"        validate:"required,min=1"`
	Weights          Weights `yaml:"weights"`
	DedupeMaxHamming int     `yaml:"dedupeMaxHamming" validate:"gte=0,lte=64"`
	// MaxPerShot caps images per shot (and per "other"), so the fill pass cannot add a
	// second, near-identical view of a covered shot. 0 → no cap: the fill pass adds
	// any distinct frame by score (plan/03 step 5).
	MaxPerShot int `yaml:"maxPerShot" validate:"gte=0"`
}

// EffectiveMaxPerShot returns the per-shot cap selection applies; 0 means no cap.
func (s Selection) EffectiveMaxPerShot() int {
	return max(s.MaxPerShot, 0)
}

// Review configures the model's check of the shortlisted still frames (it replaces
// the plan's optional re-rank): for each frame the model reports the rotation that
// makes it upright, the view it really shows, whether the subject is fully in frame,
// a 1-5 quality score and the subject's bounding box. Selection then only uses frames
// that pass, and export rotates and crops them.
type Review struct {
	Enabled bool `yaml:"enabled"`
	// CandidatesPerShot is how many top-scored frames per shot are sent. 0 → 3.
	CandidatesPerShot int `yaml:"candidatesPerShot" validate:"gte=0,lte=10"`
	// MinQuality is the lowest acceptable 1-5 quality. 0 → 3.
	MinQuality int `yaml:"minQuality" validate:"gte=0,lte=5"`
	// MaxImageSide is the long side (px) of the frames sent to the model. 0 → 512.
	MaxImageSide int `yaml:"maxImageSide" validate:"gte=0"`
	// Instructions describe what a good frame is for this domain.
	Instructions string `yaml:"instructions" validate:"required_if=Enabled true"`
}

// Crop configures reframing of the selected images around the reviewed subject box.
// It never cuts into the subject and never produces an image with a short side below
// the floor (MinShortSidePx, see there): the frame is kept uncropped instead. Needs
// review.enabled. crop.Params has the exact rules.
type Crop struct {
	Enabled bool `yaml:"enabled"`
	// Aspect is the target width:height, e.g. "4:3" (a listing photo).
	Aspect string `yaml:"aspect" validate:"required_if=Enabled true"`
	// MarginPct is the space kept around the subject box, in percent of the box's
	// larger side. Best effort: where the full margin would not fit the aspect in the
	// frame, the largest margin that does is used (down to none). 0 → 6.
	MarginPct float64 `yaml:"marginPct" validate:"gte=0,lte=50"`
	// MinShortSidePx is the smallest acceptable short side of a cropped image. 0 → 1080.
	// A crop that changes the orientation (a landscape aspect from an upright portrait
	// frame, or the reverse) may go lower, to the largest short side the frame allows,
	// but never below 480 px (crop.ReorientedMinShortSidePx).
	MinShortSidePx int `yaml:"minShortSidePx" validate:"gte=0"`
}

type Weights struct {
	Relevance float64 `yaml:"relevance" validate:"gte=0,lte=1"`
	Sharpness float64 `yaml:"sharpness" validate:"gte=0,lte=1"`
	Exposure  float64 `yaml:"exposure"  validate:"gte=0,lte=1"`
}

type Copy struct {
	UploadTitle   string `yaml:"uploadTitle"   json:"uploadTitle"   validate:"required"`
	UploadHint    string `yaml:"uploadHint"    json:"uploadHint"    validate:"required"`
	RejectedTitle string `yaml:"rejectedTitle" json:"rejectedTitle" validate:"required"`
}

// ShotByID returns the shot with the given ID.
func (c *Config) ShotByID(id string) (Shot, bool) {
	for _, s := range c.Shots {
		if s.ID == id {
			return s, true
		}
	}
	return Shot{}, false
}

// ShotLabel returns the human label for a shot ID, "Other" for OtherShotID and the ID
// itself for anything unknown.
func (c *Config) ShotLabel(id string) string {
	if s, ok := c.ShotByID(id); ok {
		return s.Label
	}
	if id == OtherShotID {
		return "Other"
	}
	return id
}

// ShotIDs returns the configured shot IDs in priority order.
func (c *Config) ShotIDs() []string {
	ids := make([]string, len(c.Shots))
	for i, s := range c.Shots {
		ids[i] = s.ID
	}
	return ids
}

// OriginAllowed reports whether a browser Origin header value is in the allowlist.
// Comparison is on normalised scheme://host[:port], so a trailing slash or letter case
// in the config does not matter.
func (c *Config) OriginAllowed(origin string) bool {
	o := NormalizeOrigin(origin)
	if o == "" {
		return false
	}
	for _, a := range c.AllowedOrigins {
		if NormalizeOrigin(a) == o {
			return true
		}
	}
	return false
}

// NormalizeOrigin returns lower-case scheme://host[:port] or "" if s is not an origin.
func NormalizeOrigin(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}
