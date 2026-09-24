// Package studio gives exterior listing photos a studio background without touching the
// car: a cut-out mask (alpha) of the car comes from the segmentation sidecar
// (services/studio: BiRefNet-lite + SAM 2.1 "rescue union"), and the composite is
//
//	out = alpha·original + (1−alpha)·backdrop·shadow
//
// so every pixel with alpha = 1 is the original pixel (byte-identical before the JPEG
// encode). Gates decide from the review verdict and the mask statistics whether a cut-out
// is trustworthy; a Gemini check (QA) compares the cut-out with the original and
// withholds the studio version when any part of the car is missing. Research, numbers
// and the model choice: docs/research/studio-background/.
package studio

import (
	"context"
	"image"
)

// Box is a normalised rectangle (0 ≤ X0 < X1 ≤ 1, 0 ≤ Y0 < Y1 ≤ 1) in the pixel
// coordinates of the EXPORTED image (after rotation and crop).
type Box struct{ X0, Y0, X1, Y1 float64 }

// Style is a studio backdrop.
type Style string

const (
	StyleGrey  Style = "grey"  // neutral showroom; the default (every car colour keeps its outline)
	StyleLight Style = "light" // white-to-light-grey cyclorama
	StyleDark  Style = "dark"  // charcoal with a top spotlight; never automatic (black cars merge)
)

// EdgeContact is the share (0..1) of each frame edge the mask touches.
type EdgeContact struct {
	Left   float64 `json:"left"`
	Right  float64 `json:"right"`
	Top    float64 `json:"top"`
	Bottom float64 `json:"bottom"`
}

// MaskStats describe a mask for the gates. Fractions are of the image area unless noted.
type MaskStats struct {
	IoU             float64     `json:"iou"`             // BiRefNet vs SAM agreement (0..1)
	RescuedFraction float64     `json:"rescuedFraction"` // area SAM put back into the BiRefNet mask
	Coverage        float64     `json:"coverage"`        // mask area (alpha ≥ 128)
	BoxFill         float64     `json:"boxFill"`         // mask area inside the box / box area
	OutsideBox      float64     `json:"outsideBox"`      // mask area outside the box / mask area
	SecondComponent float64     `json:"secondComponent"` // area of the second-largest component
	SemiTransparent float64     `json:"semiTransparent"` // pixels with 0 < alpha < 250 / mask area
	EdgeContact     EdgeContact `json:"edgeContact"`
}

// MaskResult is one answer of the segmentation sidecar.
type MaskResult struct {
	Alpha     *image.Gray       // same size as the input image; 255 = car
	Stats     MaskStats         // computed by the sidecar on Alpha
	TimingsMS map[string]int64  // e.g. birefnet, sam, total
	Models    map[string]string // model name → "sha256:…" (provenance)
}

// Segmenter cuts the car out of one exported JPEG. The box (from the frame review,
// mapped to the exported image) prompts SAM and crops BiRefNet's input.
//
// HTTP contract of the sidecar (services/studio), reached on localhost in the worker pod:
//
//	POST /v1/mask?box=x0,y0,x1,y1   body: the JPEG (Content-Type: image/jpeg)
//	  200 {"width":W,"height":H,"maskPng":"<base64 8-bit grayscale PNG, W×H>",
//	       "stats":MaskStats,"timingsMs":{"birefnet":…,"sam":…,"total":…},
//	       "models":{"birefnet":"sha256:…","sam":"sha256:…"}}
//	  400 {"error":"…"}  bad image or box
//	  503 {"error":"busy"} + Retry-After  (it runs one request at a time)
//	GET /healthz → 200 {"ok":true}          (process up)
//	GET /readyz  → 200 once the models are loaded and warmed up, else 503
type Segmenter interface {
	Mask(ctx context.Context, jpeg []byte, box Box) (*MaskResult, error)
}
