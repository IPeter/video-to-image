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
