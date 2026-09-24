package studio

import (
	"image"
	"image/color"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
)

func TestStylesFor(t *testing.T) {
	tests := []struct {
		name  string
		in    domain.Studio
		want  []Style
		deflt Style
	}{
		{"defaults", domain.Studio{}, []Style{StyleGrey, StyleLight, StyleDark}, StyleGrey},
		{"default first", domain.Studio{DefaultStyle: "light"}, []Style{StyleLight, StyleGrey, StyleDark}, StyleLight},
		{"listed order", domain.Studio{Styles: []string{"dark", "grey"}}, []Style{StyleGrey, StyleDark}, StyleGrey},
		{"listed with default", domain.Studio{Styles: []string{"light", "dark"}, DefaultStyle: "dark"}, []Style{StyleDark, StyleLight}, StyleDark},
		{"default not listed", domain.Studio{Styles: []string{"light"}, DefaultStyle: "dark"}, []Style{StyleDark, StyleLight}, StyleDark},
		{"repeats and unknown dropped", domain.Studio{Styles: []string{"light", "light", "gray"}, DefaultStyle: "neon"}, []Style{StyleGrey, StyleLight}, StyleGrey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, def := StylesFor(tt.in)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.deflt, def)
		})
	}
	assert.Equal(t, []Style{StyleGrey, StyleLight, StyleDark}, DefaultStyles, "StylesFor must not modify DefaultStyles")
}

func TestParseStyle(t *testing.T) {
	for _, s := range []string{"grey", "light", "dark"} {
		st, err := ParseStyle(s)
		require.NoError(t, err)
		assert.Equal(t, Style(s), st)
		assert.True(t, st.Valid())
		_, ok := looks[st]
		assert.True(t, ok, "no look for %s", s)
	}
	for _, s := range []string{"", "Grey", "gray", "white"} {
		_, err := ParseStyle(s)
		assert.Error(t, err, s)
		assert.False(t, Style(s).Valid())
	}
}

// TestLooksAreTheSpikeColours pins the tuned colours (spike synth.styles).
func TestLooksAreTheSpikeColours(t *testing.T) {
	assert.Equal(t, rgb{0x9f, 0xa1, 0xa4}, looks[StyleGrey].wallTop)
	assert.Equal(t, rgb{0x55, 0x57, 0x5b}, looks[StyleGrey].floorFront)
	assert.Equal(t, 0.16, looks[StyleGrey].vignette)
	assert.Equal(t, rgb{0xf6, 0xf6, 0xf7}, looks[StyleLight].wallTop)
	assert.Equal(t, rgb{0x1e, 0x1f, 0x21}, looks[StyleDark].wallTop)
	assert.Equal(t, 0.70, looks[StyleDark].shadowCore)
	assert.Panics(t, func() { hexRGB("9fa1a4") })
	assert.Panics(t, func() { hexRGB("#9fa1zz") })
}

// TestNoBanding: the dither keeps the smooth backdrop from forming flat bands; two
// neighbouring backdrop pixels are rarely all equal across a wide stretch.
func TestNoBanding(t *testing.T) {
	const w, h = 400, 300
	img, mask := wheelScene(w, h, true)
	p := mustPrepare(t, img, mask, defaultOptions)
	o := mustRender(t, p, StyleGrey)
	opt := defaultOptions
	opt.dither = false
	flat := mustRender(t, mustPrepare(t, img, mask, opt), StyleGrey)
	distinct := func(m *image.RGBA) int {
		seen := map[color.RGBA]bool{}
		for x := 0; x < w; x++ {
			seen[m.RGBAAt(x, 5)] = true
		}
		return len(seen)
	}
	assert.Greater(t, distinct(o), distinct(flat), "dither adds no variation")
	for x := range w {
		a, b := o.RGBAAt(x, 5), flat.RGBAAt(x, 5)
		d := int(a.R) - int(b.R)
		assert.LessOrEqual(t, d*d, 1, "dither is at most 1 LSB")
	}
}
