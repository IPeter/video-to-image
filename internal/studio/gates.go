package studio

import (
	"fmt"
	"math"
	"strings"

	"video-to-image/internal/domain"
)

// Gates are the effective mask checks: domain.StudioGates with zero values replaced by
// the defaults measured in the research spike.
type Gates struct {
	MinIoU             float64 // below → withhold (0.80)
	HardIoU            float64 // below → hard case, QA mandatory (0.95)
	HardRescued        float64 // RescuedFraction above → hard case (0.01)
	BoxMargin          float64 // box closer than this to the left/right edge → car cut off (0.005)
	BoxBottomMargin    float64 // box bottom closer than this to the bottom edge → car cut off (0.01)
	MaxEdgeContact     float64 // mask share of the left, right or bottom edge (0.02)
	MaxTopEdgeContact  float64 // mask share of the top edge (0.10; antennas and roofs reach it)
	MinBoxFill         float64 // mask area inside the box / box area (0.45 ..
	MaxBoxFill         float64 // .. 0.90)
	MaxOutsideBox      float64 // mask area outside the box / mask area (0.03)
	MaxSecondComponent float64 // second-largest mask component / image area (0.002)
	MaxSemiTransparent float64 // semi-transparent pixels / mask area (0.05)
	MinCoverage        float64 // compositor coverage bounds (0.03 .. 0.97)
	MaxCoverage        float64
}

// DefaultGates are the thresholds of the research spike (spike-result.json synth.gates
// G3-G10). On its fixtures every fully visible car measured IoU 0.989-0.992, box fill
// 0.59-0.78, outside the box ≤ 0.7 %, no second component, 1.1-1.5 % semi-transparent
// and no edge contact; the failures measured IoU 0.84-0.86 and edge contact 0.45-0.72.
var DefaultGates = Gates{
	MinIoU:             0.80,
	HardIoU:            0.95,
	HardRescued:        0.01,
	BoxMargin:          0.005,
	BoxBottomMargin:    0.01,
	MaxEdgeContact:     0.02,
	MaxTopEdgeContact:  0.10,
	MinBoxFill:         0.45,
	MaxBoxFill:         0.90,
	MaxOutsideBox:      0.03,
	MaxSecondComponent: 0.002,
	MaxSemiTransparent: 0.05,
	MinCoverage:        0.03,
	MaxCoverage:        0.97,
}

// GatesFor applies the defaults to a domain's overrides.
func GatesFor(g domain.StudioGates) Gates {
	out := DefaultGates
	set := func(dst *float64, v float64) {
		if v != 0 {
			*dst = v
		}
	}
	set(&out.MinIoU, g.MinIoU)
	set(&out.MaxEdgeContact, g.MaxEdgeContact)
	set(&out.MaxTopEdgeContact, g.MaxTopEdgeContact)
	set(&out.MinBoxFill, g.MinBoxFill)
	set(&out.MaxBoxFill, g.MaxBoxFill)
	set(&out.MaxOutsideBox, g.MaxOutsideBox)
	set(&out.MaxSecondComponent, g.MaxSecondComponent)
	set(&out.MaxSemiTransparent, g.MaxSemiTransparent)
	return out
}

// Gate result codes (GateResult.Code), for logs and metrics.
const (
	CodeOK              = "ok"
	CodeHard            = "hard_case"        // passed; QA mandatory
	CodeBoxInvalid      = "box_invalid"      // no usable review box
	CodeCutOff          = "cut_off"          // the review box reaches the left/right/bottom edge
	CodeMaskInvalid     = "mask_invalid"     // statistics out of range
	CodeCoverage        = "coverage"         // mask area outside the compositor bounds
	CodeEdgeContact     = "edge_contact"     // the mask touches a frame edge
	CodeModelsDisagree  = "models_disagree"  // BiRefNet/SAM IoU below MinIoU
	CodeBoxFill         = "box_fill"         // mask area inside the box out of bounds
	CodeOutsideBox      = "outside_box"      // mask leaks out of the box
	CodeSecondObject    = "second_object"    // a second large component
	CodeSemiTransparent = "semi_transparent" // too many see-through pixels
)

// GateResult is the outcome of a check. Reason is for people ("The car touches the
// right edge of the photo, so part of it may be missing."); Code is for logs/metrics.
type GateResult struct {
	OK     bool
	Code   string
	Reason string
	Hard   bool               // passed, but QA is mandatory and the case is logged
	Values map[string]float64 // the measured values that were checked
}

// Reasons shown to people when a gate withholds the studio version.
const (
	reasonBoxInvalid = "The car's position in the photo is not known, so no studio version was made."
	reasonCoverageLo = "The cut-out found too little of the car, so parts of it may be missing."
	reasonCoverageHi = "The cut-out covered almost the whole photo, so it was not reliable."
	reasonDisagree   = "The cut-out did not look reliable enough (the two models disagreed)."
	reasonFillLo     = "The cut-out covers too little of the car's area, so parts of the car may be missing."
	reasonFillHi     = "The cut-out fills the car's whole area, so it probably includes background."
	reasonOutside    = "The cut-out reaches beyond the car, probably into a neighbouring car or object."
	reasonSecond     = "The cut-out contains a second object besides the car (such as another car or a person)."
	reasonSemi       = "Parts of the cut-out were see-through, so the car's outline was not reliable."
	reasonMaskBad    = "The cut-out could not be checked, so no studio version was made."
	reasonHard       = "The two cut-out models disagreed in places, so the AI check is required."
)

// cutOffReason names the frame edges the car touches.
func cutOffReason(edges []string) string {
	s := edges[0]
	switch len(edges) {
	case 1:
		return fmt.Sprintf("The car touches the %s edge of the photo, so part of it may be missing.", s)
	case 2:
		s = edges[0] + " and " + edges[1]
	default:
		s = strings.Join(edges[:len(edges)-1], ", ") + " and " + edges[len(edges)-1]
	}
	return fmt.Sprintf("The car touches the %s edges of the photo, so part of it may be missing.", s)
}

func refuse(code, reason string, v map[string]float64) GateResult {
	return GateResult{Code: code, Reason: reason, Values: v}
}

func finite01(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || v < 0 || v > 1 {
			return false
		}
	}
	return true
}

// CheckBox runs the checks that need only the review box (before calling the sidecar):
// a box that is not a valid normalised rectangle, or that comes within BoxMargin of the
// left or right edge or within BoxBottomMargin of the bottom edge, means the car is (or
// may be) cut off by the frame. The top edge is allowed: antennas and roofs reach it.
func CheckBox(box Box, g Gates) GateResult {
	v := map[string]float64{"x0": box.X0, "y0": box.Y0, "x1": box.X1, "y1": box.Y1}
	if !finite01(box.X0, box.Y0, box.X1, box.Y1) || !(box.X1 > box.X0) || !(box.Y1 > box.Y0) {
		return refuse(CodeBoxInvalid, reasonBoxInvalid, v)
	}
	var edges []string
	if box.X0 < g.BoxMargin {
		edges = append(edges, "left")
	}
	if box.X1 > 1-g.BoxMargin {
		edges = append(edges, "right")
	}
	if box.Y1 > 1-g.BoxBottomMargin {
		edges = append(edges, "bottom")
	}
	if len(edges) > 0 {
		return refuse(CodeCutOff, cutOffReason(edges), v)
	}
	return GateResult{OK: true, Code: CodeOK, Values: v}
}

// CheckMask runs the mask checks on the sidecar's statistics (spike gates G4-G10), in
// this order: coverage, edge contact, model agreement, box fill, mask outside the box,
// second component, semi-transparency; the first failure is the result. A mask that
// passes is a hard case (QA mandatory) when the models agreed less than HardIoU or SAM
// had to put back more than HardRescued of the image.
func CheckMask(stats MaskStats, g Gates) GateResult {
	e := stats.EdgeContact
	v := map[string]float64{
		"iou": stats.IoU, "rescuedFraction": stats.RescuedFraction, "coverage": stats.Coverage,
		"boxFill": stats.BoxFill, "outsideBox": stats.OutsideBox, "secondComponent": stats.SecondComponent,
		"semiTransparent": stats.SemiTransparent,
		"edgeLeft":        e.Left, "edgeRight": e.Right, "edgeTop": e.Top, "edgeBottom": e.Bottom,
	}
	if !finite01(stats.IoU, stats.RescuedFraction, stats.Coverage, stats.BoxFill, stats.OutsideBox,
		stats.SecondComponent, stats.SemiTransparent, e.Left, e.Right, e.Top, e.Bottom) {
		return refuse(CodeMaskInvalid, reasonMaskBad, v)
	}
	switch {
	case stats.Coverage < g.MinCoverage:
		return refuse(CodeCoverage, reasonCoverageLo, v)
	case stats.Coverage > g.MaxCoverage:
		return refuse(CodeCoverage, reasonCoverageHi, v)
	}
	var edges []string
	if e.Left > g.MaxEdgeContact {
		edges = append(edges, "left")
	}
	if e.Right > g.MaxEdgeContact {
		edges = append(edges, "right")
	}
	if e.Top > g.MaxTopEdgeContact {
		edges = append(edges, "top")
	}
	if e.Bottom > g.MaxEdgeContact {
		edges = append(edges, "bottom")
	}
	if len(edges) > 0 {
		return refuse(CodeEdgeContact, cutOffReason(edges), v)
	}
	switch {
	case stats.IoU < g.MinIoU:
		return refuse(CodeModelsDisagree, reasonDisagree, v)
	case stats.BoxFill < g.MinBoxFill:
		return refuse(CodeBoxFill, reasonFillLo, v)
	case stats.BoxFill > g.MaxBoxFill:
		return refuse(CodeBoxFill, reasonFillHi, v)
	case stats.OutsideBox > g.MaxOutsideBox:
		return refuse(CodeOutsideBox, reasonOutside, v)
	case stats.SecondComponent > g.MaxSecondComponent:
		return refuse(CodeSecondObject, reasonSecond, v)
	case stats.SemiTransparent > g.MaxSemiTransparent:
		return refuse(CodeSemiTransparent, reasonSemi, v)
	}
	if stats.IoU < g.HardIoU || stats.RescuedFraction > g.HardRescued {
		return GateResult{OK: true, Code: CodeHard, Reason: reasonHard, Hard: true, Values: v}
	}
	return GateResult{OK: true, Code: CodeOK, Values: v}
}
