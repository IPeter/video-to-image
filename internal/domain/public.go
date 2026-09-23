package domain

// PublicDomain is the subset of a domain config the frontend may see. Prompts, model
// names and weights never leave the server.
type PublicDomain struct {
	Key            string       `json:"key"`
	Name           string       `json:"name"`
	Limits         PublicLimits `json:"limits"`
	Shots          []PublicShot `json:"shots"`
	ImageCount     int          `json:"imageCount"`
	RetentionHours int          `json:"retentionHours"`
	Copy           Copy         `json:"copy"`
}

type PublicLimits struct {
	MaxUploadBytes int64 `json:"maxUploadBytes"`
	MaxDurationSec int   `json:"maxDurationSec"`
	MinDurationSec int   `json:"minDurationSec"`
	MinShortSidePx int   `json:"minShortSidePx"`
	MaxPixels      int64 `json:"maxPixels" doc:"Maximum width × height of an accepted video."`
}

type PublicShot struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
}

// Public returns the frontend-safe view of the config.
func (c *Config) Public() PublicDomain {
	shots := make([]PublicShot, len(c.Shots))
	for i, s := range c.Shots {
		shots[i] = PublicShot{ID: s.ID, Label: s.Label, Required: s.Required}
	}
	return PublicDomain{
		Key:  c.Key,
		Name: c.Name,
		Limits: PublicLimits{
			MaxUploadBytes: c.Limits.MaxUploadBytes,
			MaxDurationSec: c.Limits.MaxDurationSec,
			MinDurationSec: c.Limits.MinDurationSec,
			MinShortSidePx: c.Limits.MinShortSidePx,
			MaxPixels:      c.Limits.EffectiveMaxPixels(),
		},
		Shots:          shots,
		ImageCount:     c.Selection.ImageCount,
		RetentionHours: c.RetentionHours,
		Copy:           c.Copy,
	}
}
