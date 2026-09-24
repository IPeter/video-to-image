package studiojob

import (
	"strings"

	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

// Storage layout of the studio variants. Everything is under the job prefix
// (storage.JobPrefix), so retention and user deletion remove it with the job:
//
//	<domainKey>/jobs/<jobId>/studio/<style>/<imageId>.jpg
//	<domainKey>/jobs/<jobId>/studio/<style>/thumbs/<imageId>.jpg
//	<domainKey>/jobs/<jobId>/studio.zip

// Prefix is the key prefix of every studio variant of the job, ending in "/".
func Prefix(domainKey, jobID string) string { return storage.JobPrefix(domainKey, jobID) + "studio/" }

// ImageKey is the full-size variant of an image in one style.
func ImageKey(domainKey, jobID, style, imageID string) string {
	return Prefix(domainKey, jobID) + style + "/" + imageID + ".jpg"
}

// ThumbKey is the 640 px thumbnail of an image's variant in one style.
func ThumbKey(domainKey, jobID, style, imageID string) string {
	return Prefix(domainKey, jobID) + style + "/thumbs/" + imageID + ".jpg"
}

// ZipKey is studio.zip: every ready variant, a folder per style, plus manifest.json.
func ZipKey(domainKey, jobID string) string {
	return storage.JobPrefix(domainKey, jobID) + "studio.zip"
}

// ZipName is the download name of studio.zip: <domainKey>_<jobShortId>_studio.zip.
func ZipName(domainKey, jobID string) string {
	return strings.TrimSuffix(export.ZipName(domainKey, jobID), "_images.zip") + "_studio.zip"
}

// FileName is the download name of a variant: the original photo's file name with
// "_studio-<style>" before the extension, e.g.
// cars_7K2M9QXA_1_front_three_quarter_00-06_studio-grey.jpg.
func FileName(original, style string) string {
	return strings.TrimSuffix(original, ".jpg") + "_studio-" + style + ".jpg"
}

// Reasons stored with an outcome (store.ImageStudio.Reason). They are shown to people,
// so they name the consequence, never internals.
const (
	ReasonNotExterior = "Studio backgrounds are made for exterior views of the whole car only."
	ReasonNoBox       = "The frame review did not locate the car in this photo."
	ReasonFailed      = "The studio background could not be made for this photo."
	ReasonQAFailed    = "The studio version could not be checked, so only the original is offered."
	ReasonCeiling     = "The daily budget for automatic checks was reached, so no studio version was made."
	ReasonCoverage    = "The car fills too little or too much of the photo for a studio background."
)

// Eligible reports whether an exported image gets a studio version: its view is one of
// the domain's studio views (studio enabled) and the frame review gave it a subject box
// (store.SelectedImage.SubjectBox). reason says why not.
func Eligible(dom *domain.Config, im store.SelectedImage) (ok bool, reason string) {
	switch {
	case dom == nil || !dom.Studio.HasView(im.ShotID):
		return false, ReasonNotExterior
	case im.SubjectBox == nil:
		return false, ReasonNoBox
	}
	return true, ""
}
