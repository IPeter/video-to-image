package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
	"video-to-image/internal/studio"
)

// withStudio turns studio backgrounds on for the cars test domain (exterior views).
func withStudio(styles []string, def string) func(e *testEnv, d *Deps) {
	return func(e *testEnv, _ *Deps) {
		e.cars.Studio = domain.Studio{
			Enabled:      true,
			Views:        []string{"front_three_quarter", "side_profile", "rear_three_quarter"},
			Styles:       styles,
			DefaultStyle: def,
		}
	}
}

// studioKeys are the storage keys of one image's variant (the agreed layout).
func studioKeys(domainKey, jobID, imageID string, style studio.Style) (string, string) {
	dir := storage.JobPrefix(domainKey, jobID) + "studio/" + string(style) + "/"
	return dir + imageID + ".jpg", dir + "thumbs/" + imageID + ".jpg"
}

func studioZipKey(domainKey, jobID string) string {
	return storage.JobPrefix(domainKey, jobID) + "studio.zip"
}

// readyStudio is a ready ImageStudio with the given styles stored.
func readyStudio(jobID, imageID string, styles ...studio.Style) *store.ImageStudio {
	st := &store.ImageStudio{Status: "ready", Styles: map[store.Style]store.StudioVariant{},
		Diagnostics: []byte(`{"iou":0.99,"secret":"diagnostics"}`)}
	for _, s := range styles {
		k, tk := studioKeys("cars", jobID, imageID, s)
		st.Styles[store.Style(s)] = store.StudioVariant{Key: k, ThumbKey: tk, Width: 1440, Height: 1080, Bytes: 123}
	}
	return st
}

var testBox = &store.NRect{X0: 0.1, Y0: 0.2, X1: 0.9, Y1: 0.95}

func exterior(jobID string, idx int, shot string, st *store.ImageStudio) store.SelectedImage {
	id := ids.NewImage()
	return store.SelectedImage{ID: id, JobID: jobID, Idx: idx, ShotID: shot, TimestampSec: float64(idx),
		Width: 1440, Height: 1080, FileName: export.FileName("cars", jobID, idx, shot, float64(idx)),
		ImagePath: storage.ImageKey("cars", jobID, id), ThumbPath: storage.ThumbKey("cars", jobID, id),
		SubjectBox: testBox, Studio: st}
}

func TestStudioStyles(t *testing.T) {
	G, L, D := studio.StyleGrey, studio.StyleLight, studio.StyleDark
	cases := []struct {
		name      string
		styles    []string
		def       string
		want      []studio.Style
		wantStyle studio.Style
	}{
		{"defaults", nil, "", []studio.Style{G, L, D}, G},
		{"default moves first", []string{"light", "dark", "grey"}, "", []studio.Style{G, L, D}, G},
		{"explicit default", []string{"grey", "light", "dark"}, "dark", []studio.Style{D, G, L}, D},
		{"default not rendered → first style", []string{"light", "dark"}, "", []studio.Style{L, D}, L},
		{"unknown and repeated names dropped", []string{"neon", "light", "light", "grey"}, "grey", []studio.Style{G, L}, G},
		{"nothing usable → defaults", []string{"neon"}, "light", []studio.Style{L, G, D}, L},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dc := testDomain("cars")
			dc.Studio = domain.Studio{Enabled: true, Styles: tc.styles, DefaultStyle: tc.def}
			got, def := studioStyles(dc)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantStyle, def)
		})
	}
	// The package default order is not modified by the reordering.
	assert.Equal(t, []studio.Style{G, L, D}, studio.DefaultStyles)
}

func TestGetJobStudioImages(t *testing.T) {
	e := newEnv(t, withStudio([]string{"light", "grey", "dark"}, ""))
	id := ids.NewJob()
	e.store.addJob(store.Job{ID: id, DomainKey: "cars", Status: store.StatusCompleted, ZipPath: storage.ZipKey("cars", id),
		StudioStatus: store.StudioRunning})

	ready := exterior(id, 1, "front_three_quarter", nil)
	ready.Studio = readyStudio(id, ready.ID, studio.StyleDark, studio.StyleGrey, studio.StyleLight)
	pending := exterior(id, 2, "side_profile", nil)
	withheld := exterior(id, 3, "rear_three_quarter", &store.ImageStudio{Status: "withheld",
		Reason: "  The car touches the right edge of the photo,\n so part of it may be cut off. "})
	failed := exterior(id, 4, "front_three_quarter", &store.ImageStudio{Status: "failed", Reason: "sidecar: dial tcp 127.0.0.1:4100: refused"})
	skipped := exterior(id, 5, "side_profile", &store.ImageStudio{Status: "skipped", Reason: "not fully visible"})
	noBox := exterior(id, 6, "side_profile", nil)
	noBox.SubjectBox = nil
	interior := exterior(id, 7, "interior", nil)
	e.store.addImages(id, ready, pending, withheld, failed, skipped, noBox, interior)

	rec := e.get("/v1/domains/cars/jobs/" + id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "127.0.0.1", "a failed image's internal reason stays private")
	assert.NotContains(t, rec.Body.String(), "diagnostics")
	got := decodeJSON[Job](t, rec)
	assert.Equal(t, JobStudio{Status: "running", Styles: []string{"grey", "light", "dark"}, DefaultStyle: "grey"}, got.Studio)
	imgs := got.Result.Images
	require.Len(t, imgs, 7)

	base := "/v1/domains/cars/jobs/" + id + "/images/" + ready.ID + "/studio/"
	name := strings.TrimSuffix(ready.FileName, ".jpg")
	require.NotNil(t, imgs[0].Studio)
	assert.Equal(t, ImageStudio{Status: "ready", Variants: []StudioVariant{
		{Style: "grey", URL: base + "grey", DownloadURL: base + "grey?download=1", ThumbnailURL: base + "grey/thumb",
			FileName: name + "_studio-grey.jpg", Width: 1440, Height: 1080},
		{Style: "light", URL: base + "light", DownloadURL: base + "light?download=1", ThumbnailURL: base + "light/thumb",
			FileName: name + "_studio-light.jpg", Width: 1440, Height: 1080},
		{Style: "dark", URL: base + "dark", DownloadURL: base + "dark?download=1", ThumbnailURL: base + "dark/thumb",
			FileName: name + "_studio-dark.jpg", Width: 1440, Height: 1080},
	}}, *imgs[0].Studio, "default style first, then the domain's order")

	assert.Equal(t, &ImageStudio{Status: "pending", Variants: []StudioVariant{}}, imgs[1].Studio)
	assert.Equal(t, &ImageStudio{Status: "withheld", Variants: []StudioVariant{},
		Reason: "The car touches the right edge of the photo, so part of it may be cut off."}, imgs[2].Studio)
	assert.Equal(t, &ImageStudio{Status: "failed", Reason: studioFailedReason, Variants: []StudioVariant{}}, imgs[3].Studio)
	assert.Nil(t, imgs[4].Studio, "skipped by the studio step")
	assert.Nil(t, imgs[5].Studio, "no review box")
	assert.Nil(t, imgs[6].Studio, "view not in studio.views")

	raw := rawJob(t, e, id)
	images := raw["result"].(map[string]any)["images"].([]any)
	assert.Contains(t, images[6].(map[string]any), "studio", "null, not omitted")
	assert.Nil(t, images[6].(map[string]any)["studio"])
	assert.Equal(t, []any{}, images[1].(map[string]any)["studio"].(map[string]any)["variants"], "empty list, not null")
	studioRaw := raw["studio"].(map[string]any)
	assert.Contains(t, studioRaw, "zipUrl")
	assert.Nil(t, studioRaw["zipUrl"], "no ZIP while running")
}

func TestGetJobStudioOnlyListsTheDomainsStyles(t *testing.T) {
	// A style the domain no longer renders is neither listed nor served; an image left
	// with none of the domain's styles has no studio version.
	e := newEnv(t, withStudio([]string{"light"}, "light"))
	id := ids.NewJob()
	e.store.addJob(store.Job{ID: id, DomainKey: "cars", Status: store.StatusCompleted, StudioStatus: store.StudioDone})
	both := exterior(id, 1, "front_three_quarter", nil)
	both.Studio = readyStudio(id, both.ID, studio.StyleGrey, studio.StyleLight)
	greyOnly := exterior(id, 2, "side_profile", nil)
	greyOnly.Studio = readyStudio(id, greyOnly.ID, studio.StyleGrey)
	noKey := exterior(id, 3, "rear_three_quarter", &store.ImageStudio{Status: "ready",
		Styles: map[store.Style]store.StudioVariant{"light": {ThumbKey: "t"}}})
	e.store.addImages(id, both, greyOnly, noKey)

	got := decodeJSON[Job](t, e.get("/v1/domains/cars/jobs/"+id))
	assert.Equal(t, []string{"light"}, got.Studio.Styles)
	assert.Equal(t, "light", got.Studio.DefaultStyle)
	require.NotNil(t, got.Result.Images[0].Studio)
	require.Len(t, got.Result.Images[0].Studio.Variants, 1)
	assert.Equal(t, "light", got.Result.Images[0].Studio.Variants[0].Style)
	assert.Nil(t, got.Result.Images[1].Studio)
	assert.Nil(t, got.Result.Images[2].Studio, "a variant without a stored file is not listed")

	requireError(t, e.get("/v1/domains/cars/jobs/"+id+"/images/"+both.ID+"/studio/grey"), http.StatusNotFound, CodeImageNotFound)
}

func TestGetJobStudioUnprocessedImages(t *testing.T) {
	cases := []struct {
		status store.StudioStatus
		want   *ImageStudio
	}{
		{store.StudioPending, &ImageStudio{Status: "pending", Variants: []StudioVariant{}}},
		{store.StudioRunning, &ImageStudio{Status: "pending", Variants: []StudioVariant{}}},
		{store.StudioFailed, &ImageStudio{Status: "failed", Reason: studioFailedReason, Variants: []StudioVariant{}}},
		{store.StudioDone, nil},
		{store.StudioSkipped, nil},
		{store.StudioNone, nil},
		{"", nil},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			e := newEnv(t, withStudio(nil, ""))
			id := ids.NewJob()
			e.store.addJob(store.Job{ID: id, DomainKey: "cars", Status: store.StatusCompleted, StudioStatus: tc.status})
			e.store.addImages(id, exterior(id, 1, "front_three_quarter", nil))
			got := decodeJSON[Job](t, e.get("/v1/domains/cars/jobs/"+id))
			assert.Equal(t, tc.want, got.Result.Images[0].Studio)
		})
	}
}

func TestGetJobStudioJobLevel(t *testing.T) {
	zip := func(id string) *string { z := "/v1/domains/cars/jobs/" + id + "/studio.zip"; return &z }
	cases := []struct {
		name     string
		mutate   []func(e *testEnv, d *Deps)
		job      store.Job
		images   func(id string) []store.SelectedImage
		want     func(id string) JobStudio
		wantNull bool // every image's studio is null
	}{
		{
			name:   "done with a ready image offers the ZIP",
			mutate: []func(e *testEnv, d *Deps){withStudio(nil, "")},
			job:    store.Job{Status: store.StatusCompleted, StudioStatus: store.StudioDone},
			images: func(id string) []store.SelectedImage {
				img := exterior(id, 1, "front_three_quarter", nil)
				img.Studio = readyStudio(id, img.ID, studio.StyleGrey)
				return []store.SelectedImage{img}
			},
			want: func(id string) JobStudio {
				return JobStudio{Status: "done", Styles: []string{"grey", "light", "dark"}, DefaultStyle: "grey", ZipURL: zip(id)}
			},
		},
		{
			name:   "done but every image withheld: no ZIP",
			mutate: []func(e *testEnv, d *Deps){withStudio(nil, "")},
			job:    store.Job{Status: store.StatusCompleted, StudioStatus: store.StudioDone},
			images: func(id string) []store.SelectedImage {
				return []store.SelectedImage{exterior(id, 1, "front_three_quarter", &store.ImageStudio{Status: "withheld", Reason: "r"})}
			},
			want: func(string) JobStudio {
				return JobStudio{Status: "done", Styles: []string{"grey", "light", "dark"}, DefaultStyle: "grey"}
			},
		},
		{
			name:   "feature off for the domain hides finished studio versions",
			mutate: nil,
			job:    store.Job{Status: store.StatusCompleted, StudioStatus: store.StudioDone},
			images: func(id string) []store.SelectedImage {
				img := exterior(id, 1, "front_three_quarter", nil)
				img.Studio = readyStudio(id, img.ID, studio.StyleGrey)
				return []store.SelectedImage{img}
			},
			want:     func(string) JobStudio { return JobStudio{Status: "none", Styles: []string{}} },
			wantNull: true,
		},
		{
			name:   "a job still processing has none",
			mutate: []func(e *testEnv, d *Deps){withStudio(nil, "dark")},
			job:    store.Job{Status: store.StatusProcessing},
			want:   func(string) JobStudio { return JobStudio{Status: "none", Styles: []string{}} },
		},
		{
			name:   "an unknown stored status reads none",
			mutate: []func(e *testEnv, d *Deps){withStudio(nil, "")},
			job:    store.Job{Status: store.StatusCompleted, StudioStatus: "exploded"},
			want:   func(string) JobStudio { return JobStudio{Status: "none", Styles: []string{}} },
		},
		{
			name:   "skipped (cost ceiling)",
			mutate: []func(e *testEnv, d *Deps){withStudio([]string{"dark", "grey"}, "dark")},
			job:    store.Job{Status: store.StatusCompleted, StudioStatus: store.StudioSkipped, StudioZipPath: "left/over.zip"},
			images: func(id string) []store.SelectedImage {
				return []store.SelectedImage{exterior(id, 1, "front_three_quarter", nil)}
			},
			want: func(string) JobStudio {
				return JobStudio{Status: "skipped", Styles: []string{"dark", "grey"}, DefaultStyle: "dark"}
			},
			wantNull: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.mutate...)
			j := tc.job
			j.ID, j.DomainKey = ids.NewJob(), "cars"
			if j.StudioStatus == store.StudioDone && j.StudioZipPath == "" {
				j.StudioZipPath = studioZipKey("cars", j.ID)
			}
			e.store.addJob(j)
			if tc.images != nil {
				e.store.addImages(j.ID, tc.images(j.ID)...)
			}
			got := decodeJSON[Job](t, e.get("/v1/domains/cars/jobs/"+j.ID))
			assert.Equal(t, tc.want(j.ID), got.Studio)
			if tc.wantNull && got.Result != nil {
				for _, img := range got.Result.Images {
					assert.Nil(t, img.Studio)
				}
			}
			raw := rawJob(t, e, j.ID)["studio"].(map[string]any)
			_, isList := raw["styles"].([]any)
			assert.True(t, isList, "styles is a list, never null")
			assert.Contains(t, raw, "zipUrl")
			assert.Contains(t, raw, "defaultStyle")
		})
	}
}

func TestWithheldReason(t *testing.T) {
	assert.Equal(t, studioWithheldReason, withheldReason(" \n "))
	assert.Equal(t, "Part of the roof bar is missing.", withheldReason("Part of the roof bar\tis missing."))
	long := withheldReason(strings.Repeat("é", 400))
	assert.Equal(t, maxStudioReasonRunes, len([]rune(long)))
	assert.True(t, strings.HasSuffix(long, "…"))
}

func TestStudioFileNames(t *testing.T) {
	img := &store.SelectedImage{ID: "img_1", FileName: "cars_7K2M9QXA_1_front_three_quarter_00-06.jpg"}
	assert.Equal(t, "cars_7K2M9QXA_1_front_three_quarter_00-06_studio-grey.jpg", studioFileName(img, studio.StyleGrey))
	assert.Equal(t, "img_1_studio-dark.jpg", studioFileName(&store.SelectedImage{ID: "img_1"}, studio.StyleDark))
	id := ids.NewJob()
	assert.Equal(t, "cars_"+ids.Short(id)+"_studio.zip", studioZipName("cars", id))
}

// ---- files ----

type studioFixture struct {
	e       *testEnv
	jobID   string
	imageID string
	base    string // …/images/{imageId}/studio
	img     store.SelectedImage
	full    []byte
	thumb   []byte
	zip     []byte
}

func newStudioFixture(t *testing.T, mutate ...func(e *testEnv, d *Deps)) *studioFixture {
	t.Helper()
	e := newEnv(t, append([]func(e *testEnv, d *Deps){withStudio(nil, "")}, mutate...)...)
	f := &studioFixture{e: e, jobID: ids.NewJob(),
		full: []byte("\xff\xd8studio-grey\xff\xd9"), thumb: []byte("\xff\xd8studio-thumb\xff\xd9"), zip: []byte("PK\x03\x04studio")}
	e.store.addJob(store.Job{ID: f.jobID, DomainKey: "cars", Status: store.StatusCompleted, ZipPath: storage.ZipKey("cars", f.jobID),
		StudioStatus: store.StudioDone, StudioZipPath: studioZipKey("cars", f.jobID)})
	f.img = exterior(f.jobID, 1, "front_three_quarter", nil)
	f.imageID = f.img.ID
	f.img.Studio = readyStudio(f.jobID, f.imageID, studio.StyleGrey, studio.StyleLight)
	e.store.addImages(f.jobID, f.img)
	for _, s := range []studio.Style{studio.StyleGrey, studio.StyleLight} {
		k, tk := studioKeys("cars", f.jobID, f.imageID, s)
		e.storage.put(k, append([]byte(nil), f.full...))
		e.storage.put(tk, f.thumb)
	}
	e.storage.put(studioZipKey("cars", f.jobID), f.zip)
	f.base = "/v1/domains/cars/jobs/" + f.jobID + "/images/" + f.imageID + "/studio"
	return f
}

func TestGetStudioImageStreams(t *testing.T) {
	f := newStudioFixture(t)
	name := strings.TrimSuffix(f.img.FileName, ".jpg") + "_studio-grey.jpg"

	rec := f.e.get(f.base + "/grey")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.full, rec.Body.Bytes())
	h := rec.Header()
	assert.Equal(t, "image/jpeg", h.Get("Content-Type"))
	assert.Equal(t, strconv.Itoa(len(f.full)), h.Get("Content-Length"))
	assert.Equal(t, fileCacheControl, h.Get("Cache-Control"))
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Equal(t, `"`+f.imageID+`-studio-grey"`, h.Get("ETag"))
	disp, got := dispositionOf(t, h.Get("Content-Disposition"))
	assert.Equal(t, "inline", disp)
	assert.Equal(t, name, got)
	assert.Equal(t, testOrigin, h.Get("Access-Control-Allow-Origin"))

	rec = f.e.get(f.base + "/grey?download=1")
	require.Equal(t, http.StatusOK, rec.Code)
	disp, got = dispositionOf(t, rec.Header().Get("Content-Disposition"))
	assert.Equal(t, "attachment", disp)
	assert.Equal(t, name, got)

	rec = f.e.get(f.base + "/light/thumb?download=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.thumb, rec.Body.Bytes())
	assert.Equal(t, `"`+f.imageID+`-studio-light-thumb"`, rec.Header().Get("ETag"))
	disp, got = dispositionOf(t, rec.Header().Get("Content-Disposition"))
	assert.Equal(t, "attachment", disp)
	assert.Equal(t, "thumb_"+strings.TrimSuffix(f.img.FileName, ".jpg")+"_studio-light.jpg", got)

	// Conditional requests per style.
	rec = f.e.get(f.base+"/grey", "If-None-Match", `"`+f.imageID+`-studio-grey"`)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	rec = f.e.get(f.base+"/light", "If-None-Match", `"`+f.imageID+`-studio-grey"`)
	assert.Equal(t, http.StatusOK, rec.Code, "each style has its own ETag")

	// An <img> tag sends no Origin: allowed for GET.
	rec = f.e.do(newGet(f.base + "/grey/thumb"))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestStudioFilesPresignRedirect(t *testing.T) {
	f := newStudioFixture(t, func(_ *testEnv, d *Deps) { d.PresignDownloads = true })
	f.e.storage.presign = func(key string, _ time.Duration, name string) (string, error) {
		return "https://s3.example/" + key + "?dl=" + name, nil
	}
	rec := f.e.get(f.base + "/grey?download=1")
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	rec = f.e.get("/v1/domains/cars/jobs/" + f.jobID + "/studio.zip")
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())

	calls := f.e.storage.presignCalls
	require.Len(t, calls, 2)
	k, _ := studioKeys("cars", f.jobID, f.imageID, studio.StyleGrey)
	assert.Equal(t, presignCall{key: k, ttl: presignTTL, downloadName: studioFileName(&f.img, studio.StyleGrey)}, calls[0])
	assert.Equal(t, presignCall{key: studioZipKey("cars", f.jobID), ttl: presignTTL, downloadName: studioZipName("cars", f.jobID)}, calls[1])
}

func TestStudioImageErrors(t *testing.T) {
	f := newStudioFixture(t)
	e := f.e
	jobBase := "/v1/domains/cars/jobs/" + f.jobID

	requireError(t, e.get(f.base+"/neon"), http.StatusNotFound, CodeImageNotFound)
	requireError(t, e.get(f.base+"/GREY"), http.StatusNotFound, CodeImageNotFound)
	requireError(t, e.get(f.base+"/dark"), http.StatusNotFound, CodeImageNotFound) // not rendered
	requireError(t, e.get(f.base+"/dark/thumb"), http.StatusNotFound, CodeImageNotFound)
	requireError(t, e.get(jobBase+"/images/"+ids.NewImage()+"/studio/grey"), http.StatusNotFound, CodeImageNotFound)
	requireError(t, e.get(jobBase+"/images/nope/studio/grey"), http.StatusNotFound, CodeImageNotFound)
	requireError(t, e.get("/v1/domains/cars/jobs/"+ids.NewJob()+"/images/"+f.imageID+"/studio/grey"), http.StatusNotFound, CodeJobNotFound)
	requireError(t, e.get("/v1/domains/homes/jobs/"+f.jobID+"/images/"+f.imageID+"/studio/grey", "Origin", "https://homes.example.com"),
		http.StatusNotFound, CodeJobNotFound)

	add := func(img store.SelectedImage) string {
		e.store.addImages(f.jobID, img)
		return jobBase + "/images/" + img.ID + "/studio/grey"
	}
	requireError(t, e.get(add(exterior(f.jobID, 2, "side_profile", &store.ImageStudio{Status: "withheld", Reason: "r"}))),
		http.StatusNotFound, CodeImageNotFound)
	requireError(t, e.get(add(exterior(f.jobID, 3, "side_profile", &store.ImageStudio{Status: "failed"}))),
		http.StatusNotFound, CodeImageNotFound)
	requireError(t, e.get(add(exterior(f.jobID, 4, "side_profile", &store.ImageStudio{Status: "skipped"}))),
		http.StatusNotFound, CodeImageNotFound)
	interior := exterior(f.jobID, 5, "interior", nil)
	interior.Studio = readyStudio(f.jobID, interior.ID, studio.StyleGrey)
	requireError(t, e.get(add(interior)), http.StatusNotFound, CodeImageNotFound)

	// Cleared thumbnail key, and an object that vanished from storage → 410.
	noThumb := exterior(f.jobID, 6, "side_profile", readyStudio(f.jobID, "x", studio.StyleGrey))
	v := noThumb.Studio.Styles["grey"]
	v.ThumbKey = ""
	noThumb.Studio.Styles["grey"] = v
	requireError(t, e.get(add(noThumb)+"/thumb"), http.StatusGone, CodeExpired)
	k, _ := studioKeys("cars", f.jobID, f.imageID, studio.StyleLight)
	require.NoError(t, e.storage.Delete(t.Context(), k))
	requireError(t, e.get(f.base+"/light"), http.StatusGone, CodeExpired)

	// Still being made → 409.
	running := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted, StudioStatus: store.StudioRunning})
	img := exterior(running.ID, 1, "front_three_quarter", nil)
	e.store.addImages(running.ID, img)
	d := requireError(t, e.get("/v1/domains/cars/jobs/"+running.ID+"/images/"+img.ID+"/studio/grey"), http.StatusConflict, CodeNotReady)
	assert.Equal(t, "the studio version is not ready yet", d.Message)

	// Expired job → 410.
	del := newGet(jobBase)
	del.Method = http.MethodDelete
	del.Header.Set("Origin", testOrigin)
	require.Equal(t, http.StatusNoContent, e.do(del).Code)
	requireError(t, e.get(f.base+"/grey"), http.StatusGone, CodeExpired)
	requireError(t, e.get(jobBase+"/studio.zip"), http.StatusGone, CodeExpired)
}

func TestStudioFilesFollowTheDomainSwitch(t *testing.T) {
	f := newStudioFixture(t, func(e *testEnv, _ *Deps) { e.cars.Studio.Enabled = false })
	requireError(t, f.e.get(f.base+"/grey"), http.StatusNotFound, CodeImageNotFound)
	requireError(t, f.e.get("/v1/domains/cars/jobs/"+f.jobID+"/studio.zip"), http.StatusNotFound, CodeNotFound)
}

func TestDownloadStudioZip(t *testing.T) {
	f := newStudioFixture(t)
	rec := f.e.get("/v1/domains/cars/jobs/" + f.jobID + "/studio.zip")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.zip, rec.Body.Bytes())
	h := rec.Header()
	assert.Equal(t, "application/zip", h.Get("Content-Type"))
	assert.Equal(t, `"`+f.jobID+`-studio-zip"`, h.Get("ETag"))
	disp, name := dispositionOf(t, h.Get("Content-Disposition"))
	assert.Equal(t, "attachment", disp)
	assert.Equal(t, "cars_"+ids.Short(f.jobID)+"_studio.zip", name)

	rec = f.e.get("/v1/domains/cars/jobs/"+f.jobID+"/studio.zip", "If-None-Match", `"`+f.jobID+`-studio-zip"`)
	assert.Equal(t, http.StatusNotModified, rec.Code)

	// The job payload points at it.
	got := decodeJSON[Job](t, f.e.get("/v1/domains/cars/jobs/"+f.jobID))
	require.NotNil(t, got.Studio.ZipURL)
	assert.Equal(t, "/v1/domains/cars/jobs/"+f.jobID+"/studio.zip", *got.Studio.ZipURL)
}

func TestDownloadStudioZipStates(t *testing.T) {
	e := newEnv(t, withStudio(nil, ""))
	zipOf := func(j *store.Job) string { return "/v1/domains/cars/jobs/" + j.ID + "/studio.zip" }
	for _, st := range []store.JobStatus{store.StatusQueued, store.StatusProcessing, store.StatusRejected, store.StatusFailed} {
		j := e.store.addJob(store.Job{DomainKey: "cars", Status: st})
		requireError(t, e.get(zipOf(j)), http.StatusConflict, CodeNotReady)
	}
	for _, st := range []store.StudioStatus{store.StudioPending, store.StudioRunning} {
		j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted, StudioStatus: st})
		d := requireError(t, e.get(zipOf(j)), http.StatusConflict, CodeNotReady)
		assert.Equal(t, "the studio photos are not ready yet", d.Message)
	}
	for _, st := range []store.StudioStatus{store.StudioNone, store.StudioFailed, store.StudioSkipped, store.StudioDone} {
		j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted, StudioStatus: st})
		d := requireError(t, e.get(zipOf(j)), http.StatusNotFound, CodeNotFound)
		assert.Equal(t, "no studio photos to download", d.Message, st)
	}
	gone := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted, StudioStatus: store.StudioDone,
		StudioZipPath: studioZipKey("cars", "gone")})
	requireError(t, e.get(zipOf(gone)), http.StatusGone, CodeExpired)
	requireError(t, e.get("/v1/domains/cars/jobs/nope/studio.zip"), http.StatusNotFound, CodeJobNotFound)
}
