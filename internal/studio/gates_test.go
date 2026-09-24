package studio

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
)

func TestDefaultGatesAreTheSpikeValues(t *testing.T) {
	assert.Equal(t, Gates{
		MinIoU: 0.80, HardIoU: 0.95, HardRescued: 0.01,
		BoxMargin: 0.005, BoxBottomMargin: 0.01,
		MaxEdgeContact: 0.02, MaxTopEdgeContact: 0.10,
		MinBoxFill: 0.45, MaxBoxFill: 0.90, MaxOutsideBox: 0.03,
		MaxSecondComponent: 0.002, MaxSemiTransparent: 0.05,
		MinCoverage: 0.03, MaxCoverage: 0.97,
	}, DefaultGates)
	assert.Equal(t, DefaultGates, GatesFor(domain.StudioGates{}), "zero overrides → defaults")
}

func TestGatesFor(t *testing.T) {
	g := GatesFor(domain.StudioGates{
		MinIoU: 0.9, MaxEdgeContact: 0.05, MaxTopEdgeContact: 0.2, MinBoxFill: 0.4, MaxBoxFill: 0.95,
		MaxOutsideBox: 0.05, MaxSecondComponent: 0.01, MaxSemiTransparent: 0.08,
	})
	want := DefaultGates
	want.MinIoU, want.MaxEdgeContact, want.MaxTopEdgeContact = 0.9, 0.05, 0.2
	want.MinBoxFill, want.MaxBoxFill, want.MaxOutsideBox = 0.4, 0.95, 0.05
	want.MaxSecondComponent, want.MaxSemiTransparent = 0.01, 0.08
	assert.Equal(t, want, g)

	// One override leaves the rest alone.
	g = GatesFor(domain.StudioGates{MaxOutsideBox: 0.01})
	assert.Equal(t, 0.01, g.MaxOutsideBox)
	assert.Equal(t, DefaultGates.MinIoU, g.MinIoU)
	assert.Equal(t, 0.80, DefaultGates.MinIoU, "GatesFor must not modify DefaultGates")
}

func TestCheckBox(t *testing.T) {
	g := DefaultGates
	tests := []struct {
		name   string
		box    Box
		ok     bool
		code   string
		reason string
	}{
		// The spike's review boxes (fixtures manifest).
		{"pub01 car nearly fills the frame", Box{0.02, 0.02, 0.985, 0.94}, true, CodeOK, ""},
		{"pub03 antenna reaches the top", Box{0.045, 0.005, 0.955, 0.925}, true, CodeOK, ""},
		{"trunk view, tailgate at the top edge", Box{0.13, 0, 0.865, 0.945}, true, CodeOK, ""},
		{"rear 3/4 cut off on the right", Box{0.29, 0, 1, 0.93}, false, CodeCutOff,
			"The car touches the right edge of the photo, so part of it may be missing."},
		{"left", Box{0.004, 0.2, 0.8, 0.9}, false, CodeCutOff,
			"The car touches the left edge of the photo, so part of it may be missing."},
		{"bottom", Box{0.1, 0.2, 0.9, 0.995}, false, CodeCutOff,
			"The car touches the bottom edge of the photo, so part of it may be missing."},
		{"left and right", Box{0, 0.2, 1, 0.9}, false, CodeCutOff,
			"The car touches the left and right edges of the photo, so part of it may be missing."},
		{"three edges", Box{0, 0.2, 1, 1}, false, CodeCutOff,
			"The car touches the left, right and bottom edges of the photo, so part of it may be missing."},
		{"exactly at the margins", Box{0.005, 0, 0.995, 0.99}, true, CodeOK, ""},
		{"empty", Box{0.5, 0.2, 0.5, 0.9}, false, CodeBoxInvalid, reasonBoxInvalid},
		{"inverted", Box{0.8, 0.2, 0.2, 0.9}, false, CodeBoxInvalid, reasonBoxInvalid},
		{"outside [0,1]", Box{-0.1, 0.2, 0.8, 0.9}, false, CodeBoxInvalid, reasonBoxInvalid},
		{"NaN", Box{math.NaN(), 0.2, 0.8, 0.9}, false, CodeBoxInvalid, reasonBoxInvalid},
		{"zero box", Box{}, false, CodeBoxInvalid, reasonBoxInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := CheckBox(tt.box, g)
			assert.Equal(t, tt.ok, r.OK)
			assert.Equal(t, tt.code, r.Code)
			assert.Equal(t, tt.reason, r.Reason)
			assert.False(t, r.Hard)
			if !math.IsNaN(tt.box.X0) {
				assert.Equal(t, tt.box.X1, r.Values["x1"])
			}
		})
	}
	// A wider margin from the gates is honoured.
	g.BoxMargin = 0.05
	assert.False(t, CheckBox(Box{0.03, 0.2, 0.9, 0.9}, g).OK)
}

// spikeStats are the mask statistics the spike's sidecar measured on its 10 fixtures
// (synth/masks/S1_box_bp/*.json): 7 public photos and 3 phone frames (numbers only).
var spikeStats = []struct {
	name  string
	stats MaskStats
	ok    bool
	code  string
}{
	{"pub01 silver front 3/4 (QA withholds: antenna)", MaskStats{IoU: 0.9899, Coverage: 0.607, BoxFill: 0.6828, OutsideBox: 0.0012, SemiTransparent: 0.0112}, true, CodeOK},
	{"pub02 black front 3/4", MaskStats{IoU: 0.989, Coverage: 0.3365, BoxFill: 0.6988, SemiTransparent: 0.0127}, true, CodeOK},
	{"pub03 white rear 3/4", MaskStats{IoU: 0.99, RescuedFraction: 0.0003, Coverage: 0.5539, BoxFill: 0.6614, OutsideBox: 0.0003, SemiTransparent: 0.0121}, true, CodeOK},
	{"pub04 vandalised rear 3/4", MaskStats{IoU: 0.9921, Coverage: 0.423, BoxFill: 0.586, OutsideBox: 0.0006, SemiTransparent: 0.0144}, true, CodeOK},
	{"pub05 rusty side", MaskStats{IoU: 0.9894, Coverage: 0.3331, BoxFill: 0.7148, SemiTransparent: 0.0129}, true, CodeOK},
	{"pub06 rusty front 3/4", MaskStats{IoU: 0.9921, Coverage: 0.436, BoxFill: 0.7137, OutsideBox: 0.0068, SemiTransparent: 0.0136}, true, CodeOK},
	{"pub07 black rear 3/4, busy", MaskStats{IoU: 0.9896, Coverage: 0.3901, BoxFill: 0.7037, OutsideBox: 0.0019, SemiTransparent: 0.0149}, true, CodeOK},
	{"phone front (QA withholds: antenna tip)", MaskStats{IoU: 0.989, Coverage: 0.3637, BoxFill: 0.7798, OutsideBox: 0.0006, SemiTransparent: 0.0136}, true, CodeOK},
	{"phone trunk, tailgate open to the top edge", MaskStats{IoU: 0.8637, RescuedFraction: 0.0616, Coverage: 0.4533, BoxFill: 0.6527, SemiTransparent: 0.0155,
		EdgeContact: EdgeContact{Top: 0.6}}, false, CodeEdgeContact},
	{"phone rear 3/4 cut off right and top", MaskStats{IoU: 0.8381, RescuedFraction: 0.0817, Coverage: 0.5049, BoxFill: 0.7644, SemiTransparent: 0.0142,
		EdgeContact: EdgeContact{Right: 0.717, Top: 0.445}}, false, CodeEdgeContact},
}

// TestCheckMaskSpikeFixtures: the spike's outcome with the proposed gates — every fully
// visible car passes the mask gates (two are then withheld by QA, not by the gates),
// the two cut-off frames fail on edge contact.
func TestCheckMaskSpikeFixtures(t *testing.T) {
	passed := 0
	for _, fx := range spikeStats {
		t.Run(fx.name, func(t *testing.T) {
			r := CheckMask(fx.stats, DefaultGates)
			assert.Equal(t, fx.ok, r.OK, r.Reason)
			assert.Equal(t, fx.code, r.Code)
			assert.False(t, r.Hard, "no normal fixture is a hard case")
			if r.OK {
				passed++
				assert.Empty(t, r.Reason)
			}
			assert.Equal(t, fx.stats.IoU, r.Values["iou"])
			assert.Len(t, r.Values, 11)
		})
	}
	assert.Equal(t, 8, passed)
	r := CheckMask(spikeStats[9].stats, DefaultGates)
	assert.Equal(t, "The car touches the right and top edges of the photo, so part of it may be missing.", r.Reason)
	r = CheckMask(spikeStats[8].stats, DefaultGates)
	assert.Equal(t, "The car touches the top edge of the photo, so part of it may be missing.", r.Reason)
}

func TestCheckMask(t *testing.T) {
	good := MaskStats{IoU: 0.99, Coverage: 0.4, BoxFill: 0.7, OutsideBox: 0.001, SemiTransparent: 0.012}
	tests := []struct {
		name   string
		edit   func(*MaskStats)
		ok     bool
		hard   bool
		code   string
		reason string
	}{
		{"good", func(*MaskStats) {}, true, false, CodeOK, ""},
		{"models disagree", func(s *MaskStats) { s.IoU = 0.79 }, false, false, CodeModelsDisagree,
			"The cut-out did not look reliable enough (the two models disagreed)."},
		{"hard: IoU below 0.95", func(s *MaskStats) { s.IoU = 0.94 }, true, true, CodeHard, reasonHard},
		{"hard: SAM rescued > 1 %", func(s *MaskStats) { s.RescuedFraction = 0.02 }, true, true, CodeHard, reasonHard},
		{"IoU exactly at the bounds", func(s *MaskStats) { s.IoU = 0.95; s.RescuedFraction = 0.01 }, true, false, CodeOK, ""},
		{"too little coverage", func(s *MaskStats) { s.Coverage = 0.02 }, false, false, CodeCoverage, reasonCoverageLo},
		{"coverage of the whole frame", func(s *MaskStats) { s.Coverage = 0.98 }, false, false, CodeCoverage, reasonCoverageHi},
		{"left edge", func(s *MaskStats) { s.EdgeContact.Left = 0.03 }, false, false, CodeEdgeContact,
			"The car touches the left edge of the photo, so part of it may be missing."},
		{"bottom edge", func(s *MaskStats) { s.EdgeContact.Bottom = 0.021 }, false, false, CodeEdgeContact,
			"The car touches the bottom edge of the photo, so part of it may be missing."},
		{"antenna tip at the top is fine", func(s *MaskStats) { s.EdgeContact.Top = 0.01 }, true, false, CodeOK, ""},
		{"roof cut off at the top", func(s *MaskStats) { s.EdgeContact.Top = 0.11 }, false, false, CodeEdgeContact,
			"The car touches the top edge of the photo, so part of it may be missing."},
		{"box fill low", func(s *MaskStats) { s.BoxFill = 0.44 }, false, false, CodeBoxFill, reasonFillLo},
		{"box fill high", func(s *MaskStats) { s.BoxFill = 0.91 }, false, false, CodeBoxFill, reasonFillHi},
		{"leak outside the box", func(s *MaskStats) { s.OutsideBox = 0.031 }, false, false, CodeOutsideBox, reasonOutside},
		{"second object", func(s *MaskStats) { s.SecondComponent = 0.0021 }, false, false, CodeSecondObject, reasonSecond},
		{"see-through panels", func(s *MaskStats) { s.SemiTransparent = 0.051 }, false, false, CodeSemiTransparent, reasonSemi},
		{"NaN", func(s *MaskStats) { s.BoxFill = math.NaN() }, false, false, CodeMaskInvalid, reasonMaskBad},
		{"negative", func(s *MaskStats) { s.OutsideBox = -0.1 }, false, false, CodeMaskInvalid, reasonMaskBad},
		{"above 1", func(s *MaskStats) { s.IoU = 1.2 }, false, false, CodeMaskInvalid, reasonMaskBad},
		{"all zero (no stats) is withheld", func(s *MaskStats) { *s = MaskStats{} }, false, false, CodeCoverage, reasonCoverageLo},
		// Order: edge contact is reported before the model disagreement it causes.
		{"edge before IoU", func(s *MaskStats) { s.IoU = 0.5; s.EdgeContact.Right = 0.5 }, false, false, CodeEdgeContact,
			"The car touches the right edge of the photo, so part of it may be missing."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := good
			tt.edit(&s)
			r := CheckMask(s, DefaultGates)
			assert.Equal(t, tt.ok, r.OK)
			assert.Equal(t, tt.hard, r.Hard)
			assert.Equal(t, tt.code, r.Code)
			assert.Equal(t, tt.reason, r.Reason)
		})
	}
}

// TestCheckMaskUsesTheGates: overrides move the thresholds.
func TestCheckMaskUsesTheGates(t *testing.T) {
	s := MaskStats{IoU: 0.85, Coverage: 0.4, BoxFill: 0.7}
	require.True(t, CheckMask(s, DefaultGates).OK)
	require.True(t, CheckMask(s, DefaultGates).Hard)
	g := GatesFor(domain.StudioGates{MinIoU: 0.9})
	assert.Equal(t, CodeModelsDisagree, CheckMask(s, g).Code)
	s.EdgeContact.Left = 0.03
	g = GatesFor(domain.StudioGates{MaxEdgeContact: 0.05})
	assert.True(t, CheckMask(s, g).OK)
}
