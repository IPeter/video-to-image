// Package crop plans how a selected frame is reframed: rotate it upright, then crop a
// fixed-aspect rectangle around the reviewed subject box, never cutting into the
// subject and never producing an image smaller than a minimum short side. Pure
// geometry, no image I/O (export applies the result).
package crop

// Params come from domain.Crop (zero values already replaced by defaults).
//
// MarginPct is best effort: when the full margin would push the Aspect rectangle out
// of the frame, the largest margin that fits is used (down to none).
//
// MinShortSidePx applies as is when the upright frame already has the target's
// orientation (or the target is square). A crop that changes the orientation — a
// landscape target cut from an upright portrait frame, or a portrait target from a
// landscape one — can never have a short side above the frame's short side / aspect
// (1080x810 for 4:3 from a 1080x1920 frame). For such a crop the floor is lowered to
// that largest achievable short side (on even pixels) when MinShortSidePx exceeds it,
// as long as it stays at or above ReorientedMinShortSidePx; otherwise MinShortSidePx
// stays and the frame is exported uncropped.
type Params struct {
	Aspect         float64 // target width / height, e.g. 4.0/3
	MarginPct      float64 // space around the subject, % of the box's larger side
	MinShortSidePx int     // smallest acceptable short side of the result, in pixels
	// FallbackAspect is tried, with the same margin and floor, when the subject does
	// not fit Aspect without being cut; 0 → none (the frame stays uncropped).
	FallbackAspect float64
	// MinMarginPct is the smallest margin (same unit as MarginPct) a crop may end up
	// with when the full margin does not fit: below it the frame is left uncropped.
	// The review's box is measured on a 512 px copy and runs tight by a few percent;
	// a crop with no room around it put the car on the photo edge, and the studio
	// step then withheld the studio version for that. 0 → DefaultMinMarginPct.
	MinMarginPct float64
}

// Frame describes a stored source frame and the rotation that makes it upright.
type Frame struct {
	Width, Height int        // pixels of the full-resolution frame AS STORED (before rotation)
	RotateCW      int        // 0, 90, 180, 270
	Box           [4]float64 // subject box X0, Y0, X1, Y1 normalised in the frame AS STORED
}

// Result is the plan. Crop is normalised in the UPRIGHT frame; nil means "do not crop"
// (the subject cannot fit the aspect without being cut even with no margin, the result
// would be too small, or the crop would change almost nothing). Reason explains the
// decision for logs.
type Result struct {
	RotateCW int
	Crop     *[4]float64 // X0, Y0, X1, Y1 normalised in the upright frame
	Width    int         // resulting pixel size (upright, after crop)
	Height   int
	Reason   string
}
