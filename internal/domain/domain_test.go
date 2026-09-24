package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every file shipped in configs/domains must load; this is the Phase 0 guard.
func TestShippedDomainsLoad(t *testing.T) {
	reg, err := LoadDir(filepath.Join("..", "..", "configs", "domains"))
	require.NoError(t, err)
	require.NotEmpty(t, reg.Keys())

	cars, ok := reg.Get("cars")
	require.True(t, ok)
	assert.Equal(t, 8, cars.Selection.ImageCount)
	assert.Equal(t, "gemini-3.5-flash-lite", cars.Model.Name)
	// gemini-3.5-flash-lite rejects thinkingBudget 0 (HTTP 400); it takes a level.
	assert.Nil(t, cars.Model.ThinkingBudget)
	assert.Equal(t, "minimal", cars.Model.ThinkingLevel)
	assert.Equal(t, "front_three_quarter", cars.Shots[0].ID)
	assert.True(t, cars.OriginAllowed("http://localhost:3000"))
	assert.True(t, cars.OriginAllowed("HTTP://LOCALHOST:3000/"))
	assert.False(t, cars.OriginAllowed("http://localhost:3001"))
	assert.False(t, cars.OriginAllowed(""))
	assert.False(t, cars.OriginAllowed("null"))
}

func TestPublicHidesPrompts(t *testing.T) {
	reg, err := LoadDir(filepath.Join("..", "..", "configs", "domains"))
	require.NoError(t, err)
	cars, _ := reg.Get("cars")
	p := cars.Public()
	assert.Equal(t, "cars", p.Key)
	assert.Len(t, p.Shots, len(cars.Shots))
	assert.Equal(t, int64(524288000), p.Limits.MaxUploadBytes)
	assert.Equal(t, 24, p.RetentionHours)
}

func loadCarsRaw(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "domains", "cars.yaml"))
	require.NoError(t, err)
	return string(raw)
}

func TestValidationFailures(t *testing.T) {
	base := loadCarsRaw(t)
	cases := map[string]func(string) string{
		"weights do not sum to 1": func(s string) string {
			return strings.Replace(s, "exposure: 0.1", "exposure: 0.3", 1)
		},
		"unknown field": func(s string) string {
			return strings.Replace(s, "retentionHours: 24", "retentionHours: 24\nretentionHourz: 1", 1)
		},
		"duplicate shot id": func(s string) string {
			return strings.Replace(s, "id: side_profile", "id: front_three_quarter", 1)
		},
		"no required shot": func(s string) string {
			return strings.ReplaceAll(s, "required: true", "required: false")
		},
		"missing model name": func(s string) string {
			return strings.Replace(s, "name: gemini-3.5-flash-lite", "name: \"\"", 1)
		},
		"bad origin": func(s string) string {
			return strings.Replace(s, "- http://localhost:3000", "- not-an-origin", 1)
		},
		"imageCount below required shots": func(s string) string {
			return strings.Replace(s, "imageCount: 8", "imageCount: 3", 1)
		},
		"both thinking settings": func(s string) string {
			return strings.Replace(s, "thinkingLevel: minimal", "thinkingLevel: minimal\n  thinkingBudget: 0", 1)
		},
		"unknown thinking level": func(s string) string {
			return strings.Replace(s, "thinkingLevel: minimal", "thinkingLevel: none", 1)
		},
		"reserved shot id": func(s string) string {
			return strings.Replace(s, "id: detail", "id: other", 1)
		},
		"imageCount unreachable under maxPerShot": func(s string) string {
			// 13 shots + "other", one image each.
			return strings.Replace(s, "imageCount: 8", "imageCount: 15", 1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(mutate(base)))
			assert.Error(t, err)
		})
	}
}

func TestLoadDirKeyMustMatchFileName(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "autos.yaml"), []byte(loadCarsRaw(t)), 0o644))
	_, err := LoadDir(dir)
	assert.ErrorContains(t, err, "must equal the file name")
}

func TestLoadDirSkipsNonYAMLAndHidden(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cars.yaml"), []byte(loadCarsRaw(t)), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# nope"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "..data"), 0o755))
	reg, err := LoadDir(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"cars"}, reg.Keys())
}

func TestLoadDirEmpty(t *testing.T) {
	_, err := LoadDir(t.TempDir())
	assert.Error(t, err)
}

func TestShotLabel(t *testing.T) {
	reg, err := LoadDir(filepath.Join("..", "..", "configs", "domains"))
	require.NoError(t, err)
	cars, _ := reg.Get("cars")
	assert.Equal(t, "Front 3/4", cars.ShotLabel("front_three_quarter"))
	assert.Equal(t, "Other", cars.ShotLabel(OtherShotID))
	assert.Equal(t, "mystery", cars.ShotLabel("mystery"))
}

func TestMaxPerShot(t *testing.T) {
	base := loadCarsRaw(t)
	require.Contains(t, base, "maxPerShot: 1 ")
	parse := func(t *testing.T, from, to string) (*Config, error) {
		t.Helper()
		mutated := strings.Replace(base, from, to, 1)
		require.NotEqual(t, base, mutated, "%q not found", from)
		return Parse([]byte(mutated))
	}

	t.Run("unset means no cap", func(t *testing.T) {
		c, err := parse(t, "maxPerShot: 1 ", "# maxPerShot unset ")
		require.NoError(t, err)
		assert.Equal(t, 0, c.Selection.MaxPerShot)
		assert.Equal(t, 0, c.Selection.EffectiveMaxPerShot())
	})
	t.Run("explicit cap", func(t *testing.T) {
		c, err := parse(t, "maxPerShot: 1 ", "maxPerShot: 2 ")
		require.NoError(t, err)
		assert.Equal(t, 2, c.Selection.EffectiveMaxPerShot())
	})
	t.Run("negative is invalid", func(t *testing.T) {
		_, err := parse(t, "maxPerShot: 1 ", "maxPerShot: -1 ")
		assert.Error(t, err)
	})

	// cars: 13 shots + "other" under maxPerShot 1 → at most 14 images.
	t.Run("imageCount at the limit", func(t *testing.T) {
		_, err := parse(t, "imageCount: 8", "imageCount: 14")
		assert.NoError(t, err)
	})
	t.Run("imageCount over the limit", func(t *testing.T) {
		_, err := parse(t, "imageCount: 8", "imageCount: 15")
		assert.ErrorContains(t, err, "selection.imageCount (15) is unreachable with maxPerShot 1: at most 14 images (13 shots + other)")
	})
	t.Run("a higher cap lifts the limit", func(t *testing.T) {
		c, err := parse(t, "maxPerShot: 1 ", "maxPerShot: 2 ")
		require.NoError(t, err)
		c.Selection.ImageCount = 28
		require.NoError(t, Validate(c))
		c.Selection.ImageCount = 29
		assert.ErrorContains(t, Validate(c), "at most 28 images")
	})
	t.Run("a huge cap does not overflow into a limit", func(t *testing.T) {
		// maxPerShot × 14 wraps around to a negative "at most" with naive arithmetic.
		for _, m := range []string{"4611686018427387904", "9223372036854775807"} {
			c, err := parse(t, "maxPerShot: 1 ", "maxPerShot: "+m+" ")
			require.NoError(t, err, "maxPerShot %s", m)
			assert.Equal(t, 8, c.Selection.ImageCount)
		}
	})
	t.Run("no cap, no limit", func(t *testing.T) {
		c, err := parse(t, "maxPerShot: 1 ", "# maxPerShot unset ")
		require.NoError(t, err)
		c.Selection.ImageCount = 100
		assert.NoError(t, Validate(c))
	})
}

func TestStudioShippedConfig(t *testing.T) {
	reg, err := LoadDir(filepath.Join("..", "..", "configs", "domains"))
	require.NoError(t, err)
	cars, _ := reg.Get("cars")
	st := cars.Studio
	assert.True(t, st.Enabled)
	assert.True(t, st.QA)
	assert.Equal(t, []string{"front_three_quarter", "side_profile", "rear_three_quarter", "front", "rear"}, st.Views)
	assert.Equal(t, []string{"grey", "light", "dark"}, st.EffectiveStyles())
	assert.Equal(t, "grey", st.EffectiveDefaultStyle())
	assert.Equal(t, "gemini-3.8-flash", st.EffectiveQAModel())
	assert.True(t, st.HasView("side_profile"))
	for _, v := range []string{"trunk", "interior_dashboard", "wheel", OtherShotID, ""} {
		assert.False(t, st.HasView(v), v)
	}
}

func TestStudioDefaults(t *testing.T) {
	var st Studio
	assert.Equal(t, []string{"grey", "light", "dark"}, st.EffectiveStyles())
	assert.Equal(t, "grey", st.EffectiveDefaultStyle())
	assert.Equal(t, DefaultStudioQAModel, st.EffectiveQAModel())
	assert.False(t, st.HasView("front"), "disabled")

	// The default style is shown first, the rest keep their order.
	st = Studio{Styles: []string{"light", "dark", "grey"}, DefaultStyle: "dark", QAModel: "m"}
	assert.Equal(t, []string{"dark", "light", "grey"}, st.EffectiveStyles())
	st = Studio{DefaultStyle: "light"}
	assert.Equal(t, []string{"light", "grey", "dark"}, st.EffectiveStyles())
	st = Studio{Styles: []string{"light", "grey"}}
	assert.Equal(t, []string{"grey", "light"}, st.EffectiveStyles())
	assert.Equal(t, "m", Studio{QAModel: "m"}.EffectiveQAModel())

	// EffectiveStyles never aliases the configured slice.
	st = Studio{Styles: []string{"light", "grey"}}
	got := st.EffectiveStyles()
	got[0] = "dark"
	assert.Equal(t, []string{"light", "grey"}, st.Styles)
	got = Studio{}.EffectiveStyles()
	got[0] = "dark"
	assert.Equal(t, []string{"grey", "light", "dark"}, StudioStyles)
}

func TestStudioValidation(t *testing.T) {
	base := loadCarsRaw(t)
	parse := func(t *testing.T, from, to string) error {
		t.Helper()
		mutated := strings.Replace(base, from, to, 1)
		require.NotEqual(t, base, mutated, "%q not found", from)
		_, err := Parse([]byte(mutated))
		return err
	}
	const views = "views: [front_three_quarter, side_profile, rear_three_quarter, front, rear]"
	const styles = "styles: [grey, light, dark]"
	cases := map[string]struct {
		from, to string
		want     string // "" = valid
	}{
		"needs the review":        {"review:\n  enabled: true", "review:\n  enabled: false", "studio.enabled needs review.enabled"},
		"view not a shot":         {views, "views: [front, roof]", `studio.views: "roof" is not a shot id`},
		"other is not a shot":     {views, "views: [front, other]", `studio.views: "other" is not a shot id`},
		"duplicate view":          {views, "views: [front, front]", `studio.views: duplicate "front"`},
		"views required":          {views, "views: []", "studio.views must list at least one shot id"},
		"unknown style":           {styles, "styles: [grey, pink]", "Styles"},
		"duplicate style":         {styles, "styles: [grey, light, grey]", `studio.styles: duplicate "grey"`},
		"default not a style":     {styles, "styles: [light, dark]", `studio.defaultStyle "grey" is not one of studio.styles`},
		"unknown default":         {"defaultStyle: grey", "defaultStyle: white", "DefaultStyle"},
		"gate out of range":       {"qa: true ", "qa: true\n  gates:\n    minIoU: 1.5\n ", "MinIoU"},
		"unknown key":             {"qa: true ", "qa: true\n  qaModle: x\n ", "qaModle"},
		"styles may be unset":     {styles, "# styles unset", ""},
		"default may be unset":    {"defaultStyle: grey", "# default unset", ""},
		"disabled needs no views": {"studio:\n  enabled: true\n", "studio:\n  enabled: false\n", ""},
		"gates override":          {"qa: true ", "qa: true\n  qaModel: gemini-3.8-flash\n  gates:\n    minIoU: 0.85\n    maxBoxFill: 0.95\n ", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := parse(t, tc.from, tc.to)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.want)
		})
	}
	t.Run("disabled studio still checks its views", func(t *testing.T) {
		c, err := Parse([]byte(base))
		require.NoError(t, err)
		c.Studio.Enabled = false
		c.Studio.Views = []string{"roof"}
		assert.ErrorContains(t, Validate(c), `"roof" is not a shot id`)
		c.Studio.Views = nil
		c.Review.Enabled = false
		c.Crop.Enabled = false
		assert.NoError(t, Validate(c), "a disabled studio needs neither views nor the review")
	})
}
