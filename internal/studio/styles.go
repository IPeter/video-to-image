package studio

import (
	"fmt"
	"slices"
	"strconv"

	"video-to-image/internal/domain"
)

// DefaultStyles is the render order when the domain lists none (grey is the default).
var DefaultStyles = []Style{StyleGrey, StyleLight, StyleDark}

// DefaultStyle is shown first when the domain names none.
const DefaultStyle = StyleGrey

// Valid reports whether s is one of the known styles (the drawn ones and the
// generative StyleStudio).
func (s Style) Valid() bool { return s.Drawn() || s == StyleStudio }

// Drawn reports whether s is rendered by Render (grey, light, dark).
func (s Style) Drawn() bool { return s == StyleGrey || s == StyleLight || s == StyleDark }

// ParseStyle returns the style named s (grey, light, dark, studio).
func ParseStyle(s string) (Style, error) {
	if st := Style(s); st.Valid() {
		return st, nil
	}
	return "", fmt.Errorf("studio: unknown style %q (grey, light, dark, studio)", s)
}

// StylesFor resolves a domain's studio block into the styles to render, in display
// order with the default first, and that default: Styles empty → DefaultStyles,
// DefaultStyle "" → grey. A default that the list does not contain is put in front of
// it; unknown names and repeats are dropped (the domain validation rejects them).
func StylesFor(d domain.Studio) ([]Style, Style) {
	def := DefaultStyle
	if st, err := ParseStyle(d.DefaultStyle); err == nil {
		def = st
	}
	list := DefaultStyles
	if len(d.Styles) > 0 {
		list = nil
		for _, s := range d.Styles {
			if st, err := ParseStyle(s); err == nil {
				list = append(list, st)
			}
		}
	}
	out := []Style{def}
	for _, st := range list {
		if !slices.Contains(out, st) {
			out = append(out, st)
		}
	}
	return out, def
}

// look is a style's backdrop and contact shadow, as tuned in the research spike
// (docs/research/studio-background). Geometry is relative to the car as the mask shows
// it: carH is the height from the top of the car to its lowest tyre contact, carW its
// width.
type look struct {
	// Cyclorama colours: the wall runs wallTop (top edge) → wallBottom (at the cove), the
	// floor floorNear (at the cove) → floorFront (bottom edge).
	wallTop, wallBottom, floorNear, floorFront rgb

	// horizonAbove places the middle of the soft wall→floor cove this far (× carH) above
	// the highest tyre contact, so the floor runs on behind the far wheels. coveWidth is
	// the half height (× carH) of the smooth blend between wall and floor.
	horizonAbove, coveWidth float64

	// vignette darkens towards the corners (0 = none, 0.2 = corners about 20 % darker).
	vignette float64
	// spot adds a soft spotlight on the wall (0..255 added brightness), centred above the
	// car at spotY (× image height, 0 = top edge), Gaussian with sigmas spotWidth (× image
	// width) and spotHeight (× image height).
	spot, spotY, spotWidth, spotHeight float64
	// floorPool adds a soft pool of light on the floor around the car (0..255).
	floorPool float64

	// Contact shadow, multiplied into the backdrop only (never over car pixels).
	// shadowCore is the darkness (0..1) of the tight core right under the tyres,
	// shadowUnder the darkness of the visible floor under the body between the tyres
	// (relative to the core), shadowLine the core darkness along the ground line under the
	// sills and bumpers, shadowAmbient the darkness of the wide soft shadow and
	// shadowSpread its horizontal blur (sigma × carW; vertical is shadowSquash × that).
	shadowCore, shadowUnder, shadowLine, shadowAmbient, shadowSpread, shadowSquash float64
}

// rgb is an sRGB colour, 0..255 per channel.
type rgb [3]float64

// hexRGB parses "#rrggbb"; it panics on anything else (constants only).
func hexRGB(s string) rgb {
	if len(s) != 7 || s[0] != '#' {
		panic("studio: bad colour " + s)
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		panic("studio: bad colour " + s)
	}
	return rgb{float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)}
}

var looks = map[Style]look{
	// Grey showroom: a neutral mid-grey wall with a darker floor, faint spotlight and
	// floor pool. Every car colour (silver, white, red, green, black) keeps a readable
	// outline, and a dirty or rusty car looks honest on it; the default.
	StyleGrey: {
		wallTop:       hexRGB("#9fa1a4"),
		wallBottom:    hexRGB("#8a8c90"),
		floorNear:     hexRGB("#76787c"),
		floorFront:    hexRGB("#55575b"),
		horizonAbove:  0.20,
		coveWidth:     0.20,
		vignette:      0.16,
		spot:          14,
		spotY:         0.1,
		spotWidth:     0.35,
		spotHeight:    0.45,
		floorPool:     6,
		shadowCore:    0.62,
		shadowUnder:   0.80,
		shadowLine:    0.55,
		shadowAmbient: 0.34,
		shadowSpread:  0.045,
		shadowSquash:  0.35,
	},
	// Light: a seamless white-to-light-grey cyclorama with a slightly darker floor (the
	// classic marketplace look; white and silver cars lose some outline contrast).
	StyleLight: {
		wallTop:       hexRGB("#f6f6f7"),
		wallBottom:    hexRGB("#e9eaec"),
		floorNear:     hexRGB("#e2e3e5"),
		floorFront:    hexRGB("#cbccce"),
		horizonAbove:  0.20,
		coveWidth:     0.22,
		vignette:      0.10,
		shadowCore:    0.62,
		shadowUnder:   0.80,
		shadowLine:    0.55,
		shadowAmbient: 0.30,
		shadowSpread:  0.045,
		shadowSquash:  0.35,
	},
	// Dark: charcoal with a soft top-centre spotlight. Flattering for light cars; black
	// cars merge with it, so it is never chosen automatically.
	StyleDark: {
		wallTop:       hexRGB("#1e1f21"),
		wallBottom:    hexRGB("#18191b"),
		floorNear:     hexRGB("#1c1d1f"),
		floorFront:    hexRGB("#0e0f10"),
		horizonAbove:  0.20,
		coveWidth:     0.20,
		vignette:      0.25,
		spot:          46,
		spotY:         0.0,
		spotWidth:     0.30,
		spotHeight:    0.55,
		floorPool:     16,
		shadowCore:    0.70,
		shadowUnder:   0.80,
		shadowLine:    0.55,
		shadowAmbient: 0.40,
		shadowSpread:  0.045,
		shadowSquash:  0.35,
	},
}
