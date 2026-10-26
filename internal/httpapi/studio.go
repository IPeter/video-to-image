package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/store"
	"video-to-image/internal/studio"
)

// Studio backgrounds on the wire (internal/studio makes them, a separate River job after
// the pipeline). The job payload lists exactly what the studio file routes serve: both
// go through imageStudioState, so a variant is downloadable if and only if it is listed.

// Wire values of ImageStudio.Status and JobStudio.Status.
const (
	studioPending  = "pending"
	studioReady    = "ready"
	studioWithheld = "withheld"
	studioFailed   = "failed"
	studioSkipped  = "skipped" // store only: not eligible, never on the wire
)

const (
	// studioFailedReason replaces a failed image's stored reason, which is internal.
	studioFailedReason = "Something went wrong while making the studio background."
	// studioWithheldReason stands in for a withheld image that carries no reason.
	studioWithheldReason = "The cut-out could not be checked well enough, so only the original is offered."
	// maxStudioReasonRunes caps a withheld reason (it may quote the QA model).
	maxStudioReasonRunes = 300
)

// studioStyles are the domain's styles in display order with the default first, and
// the default. Unknown and repeated names are dropped; no styles → studio.DefaultStyles;
// a default that is not rendered → the first style.
func studioStyles(dc *domain.Config) ([]studio.Style, studio.Style) {
	rendered := dc.Studio.Styles
	if len(rendered) == 0 {
		rendered = domain.StudioStyles
	}
	var styles []studio.Style
	for _, s := range dc.Studio.EffectiveStyles() { // the default first
		st := studio.Style(s)
		drawn := slices.Contains(rendered, s)
		generative := st == studio.StyleStudio && dc.Studio.Generative.Enabled
		if validStudioStyle(st) && (drawn || generative) && !slices.Contains(styles, st) {
			styles = append(styles, st)
		}
	}
	if len(styles) == 0 { // nothing usable configured: the package defaults, the default first
		styles = slices.Clone(studio.DefaultStyles)
		if def := studio.Style(dc.Studio.DefaultStyle); def.Drawn() {
			if i := slices.Index(styles, def); i > 0 {
				styles = append(append([]studio.Style{def}, styles[:i]...), styles[i+1:]...)
			}
		}
	}
	return styles, styles[0]
}

func validStudioStyle(s studio.Style) bool { return s.Valid() }

// studioEligible: the domain makes studio versions of the image's view and the review
// gave the image a subject box (the segmentation prompt).
func studioEligible(dc *domain.Config, img *store.SelectedImage) bool {
	if dc.Studio.IsInteriorView(img.ShotID) {
		return true // the generative studio through the glass needs no box
	}
	return dc.Studio.Enabled && img.SubjectBox != nil && slices.Contains(dc.Studio.Views, img.ShotID)
}

// studioVariant is a stored rendering of one style.
type studioVariant struct {
	style studio.Style
	store.StudioVariant
}

// imageStudioOutcome is what the API tells about one image's studio version.
type imageStudioOutcome struct {
	status   string // studioPending | studioReady | studioWithheld | studioFailed
	reason   string // for people; withheld and failed only
	variants []studioVariant
}

// imageStudioState decides an image's studio outcome; ok is false when the image has
// none to tell (not eligible, skipped by the studio step, nothing coming any more).
//   - Not processed yet: pending while the job's studio run is pending or running,
//     failed when the run gave up, nothing otherwise (not requested, or skipped).
//   - ready lists the stored variants of the domain's styles, in its order; a ready
//     image without any of them is treated as not eligible.
//   - A failed image's stored reason is internal, so people get a generic one.
func imageStudioState(dc *domain.Config, job *store.Job, img *store.SelectedImage) (imageStudioOutcome, bool) {
	if !studioEligible(dc, img) {
		return imageStudioOutcome{}, false
	}
	st := img.Studio
	if st == nil || st.Status == studioPending {
		switch job.StudioStatus {
		case store.StudioPending, store.StudioRunning:
			return imageStudioOutcome{status: studioPending}, true
		case store.StudioFailed:
			return imageStudioOutcome{status: studioFailed, reason: studioFailedReason}, true
		}
		return imageStudioOutcome{}, false
	}
	switch st.Status {
	case studioSkipped:
		return imageStudioOutcome{}, false
	case studioReady:
		styles, _ := studioStyles(dc)
		var vs []studioVariant
		for _, s := range styles {
			if v, ok := st.Styles[store.Style(s)]; ok && v.Key != "" {
				vs = append(vs, studioVariant{style: s, StudioVariant: v})
			}
		}
		if len(vs) == 0 {
			return imageStudioOutcome{}, false
		}
		return imageStudioOutcome{status: studioReady, variants: vs}, true
	case studioWithheld:
		return imageStudioOutcome{status: studioWithheld, reason: withheldReason(st.Reason)}, true
	}
	// "failed", or a status this build does not know.
	return imageStudioOutcome{status: studioFailed, reason: studioFailedReason}, true
}

// withheldReason trims and caps a stored withheld reason; empty → the generic one.
func withheldReason(r string) string {
	r = strings.Join(strings.Fields(r), " ")
	if r == "" {
		return studioWithheldReason
	}
	if utf8.RuneCountInString(r) > maxStudioReasonRunes {
		r = string([]rune(r)[:maxStudioReasonRunes-1]) + "…"
	}
	return r
}

// buildImageStudio is ResultImage.Studio; imageURL is the image's API path.
func buildImageStudio(dc *domain.Config, job *store.Job, img *store.SelectedImage, imageURL string) *ImageStudio {
	o, ok := imageStudioState(dc, job, img)
	if !ok {
		return nil
	}
	out := &ImageStudio{Status: o.status, Reason: o.reason, Variants: make([]StudioVariant, 0, len(o.variants))}
	for _, v := range o.variants {
		u := imageURL + "/studio/" + string(v.style)
		out.Variants = append(out.Variants, StudioVariant{
			Style:        string(v.style),
			URL:          u,
			DownloadURL:  u + "?download=1",
			ThumbnailURL: u + "/thumb",
			FileName:     studioFileName(img, v.style),
			Width:        v.Width,
			Height:       v.Height,
		})
	}
	return out
}

// jobStudioStatus is the job's studio status on the wire: none when the domain has the
// feature off (a kill switch also hides finished studio versions) or the row has no
// (known) status.
func jobStudioStatus(dc *domain.Config, job *store.Job) store.StudioStatus {
	if !dc.Studio.Enabled {
		return store.StudioNone
	}
	switch s := job.StudioStatus; s {
	case store.StudioPending, store.StudioRunning, store.StudioDone, store.StudioFailed, store.StudioSkipped:
		return s
	}
	return store.StudioNone
}

// buildJobStudio is Job.Studio. res is the job's result (nil unless completed): the
// studio ZIP is offered once the run is done and at least one image is ready.
func buildJobStudio(dc *domain.Config, job *store.Job, res *JobResult) JobStudio {
	status := jobStudioStatus(dc, job)
	out := JobStudio{Status: string(status), Styles: []string{}}
	if status == store.StudioNone {
		return out
	}
	styles, def := studioStyles(dc)
	for _, s := range styles {
		out.Styles = append(out.Styles, string(s))
	}
	out.DefaultStyle = string(def)
	if status == store.StudioDone && job.StudioZipPath != "" && res != nil && slices.ContainsFunc(res.Images, func(img ResultImage) bool {
		return img.Studio != nil && img.Studio.Status == studioReady
	}) {
		z := jobPath(dc.Key, job.ID) + "/studio.zip"
		out.ZipURL = &z
	}
	return out
}

// studioFileName is <image file base>_studio-<style>.jpg (the image ID stands in for a
// missing file name), e.g. cars_7K2M9QXA_1_front_three_quarter_00-06_studio-grey.jpg.
func studioFileName(img *store.SelectedImage, style studio.Style) string {
	base := strings.TrimSuffix(img.FileName, path.Ext(img.FileName))
	if base == "" {
		base = img.ID
	}
	return base + "_studio-" + string(style) + ".jpg"
}

// studioZipName is <domainKey>_<ids.Short(jobID)>_studio.zip, next to export.ZipName.
func studioZipName(domainKey, jobID string) string {
	return strings.TrimSuffix(export.ZipName(domainKey, jobID), "_images.zip") + "_studio.zip"
}

func errNoStudioImage(msg string) *APIError {
	return NewAPIError(http.StatusNotFound, CodeImageNotFound, msg)
}

// getStudioImage serves one studio variant at full resolution.
func (s *Server) getStudioImage(w http.ResponseWriter, r *http.Request) {
	s.serveStudioImage(w, r, false)
}

// getStudioThumbnail serves one studio variant's 640 px JPG.
func (s *Server) getStudioThumbnail(w http.ResponseWriter, r *http.Request) {
	s.serveStudioImage(w, r, true)
}

// serveStudioImage answers like serveImage, plus: an unknown style, an image without a
// studio version (not eligible, withheld, failed) or without this style is 404
// IMAGE_NOT_FOUND; one still being made is 409 NOT_READY.
func (s *Server) serveStudioImage(w http.ResponseWriter, r *http.Request, thumb bool) {
	ctx := r.Context()
	dc, job, err := s.loadJob(ctx, chi.URLParam(r, "domainKey"), chi.URLParam(r, "jobId"))
	if err != nil {
		s.writeErr(w, r, "get studio image", err)
		return
	}
	if job.Status == store.StatusExpired {
		writeAPIError(w, errExpired())
		return
	}
	imageID := chi.URLParam(r, "imageId")
	if !ids.Valid(ids.PrefixImage, imageID) {
		writeAPIError(w, errImageNotFound())
		return
	}
	style := studio.Style(chi.URLParam(r, "style"))
	if !validStudioStyle(style) {
		writeAPIError(w, errNoStudioImage("unknown studio style"))
		return
	}
	img, err := s.d.Store.GetSelectedImage(ctx, job.ID, imageID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, errImageNotFound())
		return
	}
	if err != nil {
		s.writeErr(w, r, "get studio image", fmt.Errorf("get selected image %s: %w", imageID, err))
		return
	}

	o, ok := imageStudioState(dc, job, img)
	switch {
	case !ok || o.status == studioWithheld || o.status == studioFailed:
		writeAPIError(w, errNoStudioImage("this image has no studio version"))
		return
	case o.status == studioPending:
		writeAPIError(w, NewAPIError(http.StatusConflict, CodeNotReady, "the studio version is not ready yet"))
		return
	}
	i := slices.IndexFunc(o.variants, func(v studioVariant) bool { return v.style == style })
	if i < 0 {
		writeAPIError(w, errNoStudioImage("this image has no studio version in this style"))
		return
	}
	v := o.variants[i]
	key, name, etag := v.Key, studioFileName(img, style), imageID+"-studio-"+string(style)
	if thumb {
		key, name, etag = v.ThumbKey, "thumb_"+name, etag+"-thumb"
	}
	if key == "" {
		writeAPIError(w, errExpired())
		return
	}
	s.serveObject(w, r, object{
		key:         key,
		contentType: "image/jpeg",
		etag:        `"` + etag + `"`,
		fileName:    name,
		attachment:  r.URL.Query().Get("download") == "1",
	})
}

// downloadStudioZip serves studio.zip (every studio variant + manifest.json).
func (s *Server) downloadStudioZip(w http.ResponseWriter, r *http.Request) {
	dc, job, err := s.loadJob(r.Context(), chi.URLParam(r, "domainKey"), chi.URLParam(r, "jobId"))
	if err != nil {
		s.writeErr(w, r, "download studio zip", err)
		return
	}
	status := jobStudioStatus(dc, job)
	switch {
	case job.Status == store.StatusExpired:
		writeAPIError(w, errExpired())
		return
	case job.Status != store.StatusCompleted:
		writeAPIError(w, NewAPIError(http.StatusConflict, CodeNotReady, "the job has not completed yet"))
		return
	case status == store.StudioPending || status == store.StudioRunning:
		writeAPIError(w, NewAPIError(http.StatusConflict, CodeNotReady, "the studio photos are not ready yet"))
		return
	case status != store.StudioDone || job.StudioZipPath == "":
		writeAPIError(w, NewAPIError(http.StatusNotFound, CodeNotFound, "no studio photos to download"))
		return
	}
	s.serveObject(w, r, object{
		key:         job.StudioZipPath,
		contentType: "application/zip",
		etag:        `"` + job.ID + `-studio-zip"`,
		fileName:    studioZipName(dc.Key, job.ID),
		attachment:  true,
	})
}
