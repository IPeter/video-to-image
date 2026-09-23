package httpapi

import (
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

type fileFixture struct {
	e       *testEnv
	jobID   string
	imageID string
	base    string
	full    []byte
	thumb   []byte
	zip     []byte
	name    string
}

func newFileFixture(t *testing.T, mutate ...func(e *testEnv, d *Deps)) *fileFixture {
	t.Helper()
	e := newEnv(t, mutate...)
	f := &fileFixture{e: e, jobID: ids.NewJob(), imageID: ids.NewImage(),
		full: []byte("\xff\xd8full-resolution-jpeg\xff\xd9"), thumb: []byte("\xff\xd8thumb\xff\xd9"), zip: []byte("PK\x03\x04zip")}
	f.name = export.FileName("cars", f.jobID, 1, "front_three_quarter", 6.25)
	f.base = "/v1/domains/cars/jobs/" + f.jobID
	imgKey, thumbKey, zipKey := storage.ImageKey("cars", f.jobID, f.imageID), storage.ThumbKey("cars", f.jobID, f.imageID), storage.ZipKey("cars", f.jobID)
	e.store.addJob(store.Job{ID: f.jobID, DomainKey: "cars", Status: store.StatusCompleted, ZipPath: zipKey})
	e.store.addImages(f.jobID, store.SelectedImage{ID: f.imageID, JobID: f.jobID, Idx: 1, ShotID: "front_three_quarter",
		TimestampSec: 6.25, FileName: f.name, ImagePath: imgKey, ThumbPath: thumbKey})
	e.storage.put(imgKey, f.full)
	e.storage.put(thumbKey, f.thumb)
	e.storage.put(zipKey, f.zip)
	return f
}

func dispositionOf(t *testing.T, v string) (string, string) {
	t.Helper()
	disp, params, err := mime.ParseMediaType(v)
	require.NoError(t, err, v)
	return disp, params["filename"]
}

func TestGetImageStreams(t *testing.T) {
	f := newFileFixture(t)
	rec := f.e.get(f.base + "/images/" + f.imageID)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.full, rec.Body.Bytes())
	h := rec.Header()
	assert.Equal(t, "image/jpeg", h.Get("Content-Type"))
	assert.Equal(t, strconv.Itoa(len(f.full)), h.Get("Content-Length"))
	assert.Equal(t, "private, max-age=86400, immutable", h.Get("Cache-Control"))
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Equal(t, `"`+f.imageID+`"`, h.Get("ETag"))
	disp, name := dispositionOf(t, h.Get("Content-Disposition"))
	assert.Equal(t, "inline", disp)
	assert.Equal(t, f.name, name)
	assert.Equal(t, testOrigin, h.Get("Access-Control-Allow-Origin"))

	// download=1 → attachment with the listing file name.
	rec = f.e.get(f.base + "/images/" + f.imageID + "?download=1")
	require.Equal(t, http.StatusOK, rec.Code)
	disp, name = dispositionOf(t, rec.Header().Get("Content-Disposition"))
	assert.Equal(t, "attachment", disp)
	assert.Equal(t, f.name, name)
	assert.Equal(t, f.full, rec.Body.Bytes())

	// An <img> tag sends no Origin: allowed for GET.
	r := newGet(f.base + "/images/" + f.imageID)
	rec = f.e.do(r)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func newGet(path string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, path, nil)
	return r
}

func TestGetThumbnailStreams(t *testing.T) {
	f := newFileFixture(t)
	rec := f.e.get(f.base + "/images/" + f.imageID + "/thumb?download=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.thumb, rec.Body.Bytes())
	assert.Equal(t, "image/jpeg", rec.Header().Get("Content-Type"))
	assert.Equal(t, `"`+f.imageID+`-thumb"`, rec.Header().Get("ETag"))
	disp, name := dispositionOf(t, rec.Header().Get("Content-Disposition"))
	assert.Equal(t, "attachment", disp)
	assert.Equal(t, "thumb_"+f.name, name)
}

func TestConditionalRequests(t *testing.T) {
	f := newFileFixture(t)
	etag := `"` + f.imageID + `"`
	for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		rec := f.e.get(f.base+"/images/"+f.imageID, "If-None-Match", inm)
		assert.Equal(t, http.StatusNotModified, rec.Code, inm)
		assert.Empty(t, rec.Body.Bytes(), inm)
		assert.Equal(t, etag, rec.Header().Get("ETag"), inm)
	}
	// The thumbnail's ETag differs from the full image's.
	rec := f.e.get(f.base+"/images/"+f.imageID+"/thumb", "If-None-Match", etag)
	assert.Equal(t, http.StatusOK, rec.Code)
	rec = f.e.get(f.base+"/images/"+f.imageID+"/thumb", "If-None-Match", `"`+f.imageID+`-thumb"`)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	// Not matching → full response.
	rec = f.e.get(f.base+"/images/"+f.imageID, "If-None-Match", `"stale"`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFilesPresignRedirect(t *testing.T) {
	f := newFileFixture(t, func(_ *testEnv, d *Deps) { d.PresignDownloads = true })
	f.e.storage.presign = func(key string, ttl time.Duration, name string) (string, error) {
		return "https://s3.example/" + key + "?sig=1&dl=" + name, nil
	}

	rec := f.e.get(f.base + "/images/" + f.imageID)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	imgKey := storage.ImageKey("cars", f.jobID, f.imageID)
	assert.Equal(t, "https://s3.example/"+imgKey+"?sig=1&dl=", rec.Header().Get("Location"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	rec = f.e.get(f.base + "/images/" + f.imageID + "?download=1")
	require.Equal(t, http.StatusFound, rec.Code)
	rec = f.e.get(f.base + "/download.zip")
	require.Equal(t, http.StatusFound, rec.Code)

	calls := f.e.storage.presignCalls
	require.Len(t, calls, 3)
	assert.Equal(t, presignCall{key: imgKey, ttl: 15 * time.Minute, downloadName: ""}, calls[0])
	assert.Equal(t, f.name, calls[1].downloadName, "download=1 asks storage for an attachment")
	assert.Equal(t, export.ZipName("cars", f.jobID), calls[2].downloadName)
	assert.Equal(t, storage.ZipKey("cars", f.jobID), calls[2].key)
}

func TestFilesPresignFallsBackToStreaming(t *testing.T) {
	for name, presign := range map[string]func(string, time.Duration, string) (string, error){
		"unsupported": nil,
		"error":       func(string, time.Duration, string) (string, error) { return "", errors.New("signer broke") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFileFixture(t, func(_ *testEnv, d *Deps) { d.PresignDownloads = true })
			f.e.storage.presign = presign
			rec := f.e.get(f.base + "/images/" + f.imageID)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, f.full, rec.Body.Bytes())
			assert.Len(t, f.e.storage.presignCalls, 1)
		})
	}
	// Presigning disabled: storage is never asked.
	f := newFileFixture(t)
	f.e.storage.presign = func(string, time.Duration, string) (string, error) { return "https://x", nil }
	require.Equal(t, http.StatusOK, f.e.get(f.base+"/images/"+f.imageID).Code)
	assert.Empty(t, f.e.storage.presignCalls)
}

func TestImageErrors(t *testing.T) {
	f := newFileFixture(t)
	e := f.e
	img := "/images/" + f.imageID

	requireError(t, e.get(f.base+"/images/"+ids.NewImage()), http.StatusNotFound, CodeImageNotFound)
	requireError(t, e.get(f.base+"/images/nope"), http.StatusNotFound, CodeImageNotFound)
	requireError(t, e.get(f.base+"/images/"+ids.NewJob()), http.StatusNotFound, CodeImageNotFound)
	requireError(t, e.get("/v1/domains/cars/jobs/"+ids.NewJob()+img), http.StatusNotFound, CodeJobNotFound)
	requireError(t, e.get("/v1/domains/cars/jobs/bad"+img), http.StatusNotFound, CodeJobNotFound)
	requireError(t, e.get("/v1/domains/homes/jobs/"+f.jobID+img, "Origin", "https://homes.example.com"),
		http.StatusNotFound, CodeJobNotFound)

	// The object vanished from storage (cleanup raced the request) → 410.
	require.NoError(t, e.storage.Delete(t.Context(), storage.ThumbKey("cars", f.jobID, f.imageID)))
	requireError(t, e.get(f.base+img+"/thumb"), http.StatusGone, CodeExpired)

	// Paths cleared on the row → 410.
	other := ids.NewImage()
	e.store.addImages(f.jobID, store.SelectedImage{ID: other, JobID: f.jobID, Idx: 2, FileName: "x.jpg"})
	requireError(t, e.get(f.base+"/images/"+other), http.StatusGone, CodeExpired)
	requireError(t, e.get(f.base+"/images/"+other+"/thumb"), http.StatusGone, CodeExpired)

	// Expired job → 410 for images and the ZIP.
	del := newGet(f.base)
	del.Method = http.MethodDelete
	del.Header.Set("Origin", testOrigin)
	require.Equal(t, http.StatusNoContent, e.do(del).Code)
	requireError(t, e.get(f.base+img), http.StatusGone, CodeExpired)
	requireError(t, e.get(f.base+"/download.zip"), http.StatusGone, CodeExpired)
}

func TestImageErrorDropsEntityHeaders(t *testing.T) {
	f := newFileFixture(t)
	require.NoError(t, f.e.storage.Delete(t.Context(), storage.ImageKey("cars", f.jobID, f.imageID)))
	rec := f.e.get(f.base + "/images/" + f.imageID)
	requireError(t, rec, http.StatusGone, CodeExpired)
	assert.Empty(t, rec.Header().Get("Content-Disposition"))
	assert.Empty(t, rec.Header().Get("ETag"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestDownloadZip(t *testing.T) {
	f := newFileFixture(t)
	rec := f.e.get(f.base + "/download.zip")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.zip, rec.Body.Bytes())
	h := rec.Header()
	assert.Equal(t, "application/zip", h.Get("Content-Type"))
	assert.Equal(t, strconv.Itoa(len(f.zip)), h.Get("Content-Length"))
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Equal(t, `"`+f.jobID+`-zip"`, h.Get("ETag"))
	disp, name := dispositionOf(t, h.Get("Content-Disposition"))
	assert.Equal(t, "attachment", disp)
	assert.Equal(t, "cars_"+ids.Short(f.jobID)+"_images.zip", name)

	rec = f.e.get(f.base+"/download.zip", "If-None-Match", `"`+f.jobID+`-zip"`)
	assert.Equal(t, http.StatusNotModified, rec.Code)
}

func TestDownloadZipStates(t *testing.T) {
	e := newEnv(t)
	for _, st := range []store.JobStatus{store.StatusQueued, store.StatusProcessing, store.StatusRejected, store.StatusFailed} {
		j := e.store.addJob(store.Job{DomainKey: "cars", Status: st})
		requireError(t, e.get("/v1/domains/cars/jobs/"+j.ID+"/download.zip"), http.StatusConflict, CodeNotReady)
	}
	noImages := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted})
	d := requireError(t, e.get("/v1/domains/cars/jobs/"+noImages.ID+"/download.zip"), http.StatusNotFound, CodeNotFound)
	assert.Equal(t, "no images to download", d.Message)

	missing := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted, ZipPath: storage.ZipKey("cars", "gone")})
	requireError(t, e.get("/v1/domains/cars/jobs/"+missing.ID+"/download.zip"), http.StatusGone, CodeExpired)
}

func TestContentDisposition(t *testing.T) {
	cases := []struct {
		disp, name, wantASCII string
	}{
		{"attachment", "cars_7K2M9QXA_1_front_three_quarter_00-06.jpg", "cars_7K2M9QXA_1_front_three_quarter_00-06.jpg"},
		{"inline", "autó ünnep.jpg", "aut_ _nnep.jpg"},
		{"attachment", `evil"; filename="x.exe`, `evil_; filename=_x.exe`},
		{"attachment", "a/b\\c\r\nSet-Cookie: x.jpg", "a_b_c__Set-Cookie: x.jpg"},
		{"attachment", "日本.zip", "__.zip"},
	}
	for _, tc := range cases {
		v := contentDisposition(tc.disp, tc.name)
		assert.NotContains(t, v, "\r", v)
		assert.NotContains(t, v, "\n", v)
		disp, params, err := mime.ParseMediaType(v)
		require.NoError(t, err, v)
		assert.Equal(t, tc.disp, disp)
		// Go (like browsers) prefers filename*; it decodes back to the exact name.
		assert.Equal(t, tc.name, params["filename"], v)

		// Old clients only see the ASCII fallback.
		fallback := contentDispositionASCII(t, v)
		assert.Equal(t, tc.wantASCII, fallback, v)
	}
	assert.Equal(t, "inline", contentDisposition("inline", ""))
}

// contentDispositionASCII parses v without its filename* parameter.
func contentDispositionASCII(t *testing.T, v string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(stripExtended(v))
	require.NoError(t, err, v)
	return params["filename"]
}

func stripExtended(v string) string {
	if i := strings.Index(v, "; filename*="); i >= 0 {
		return v[:i]
	}
	return v
}

func TestETagMatches(t *testing.T) {
	assert.True(t, etagMatches(`"a"`, `"a"`))
	assert.True(t, etagMatches(`W/"a"`, `"a"`))
	assert.True(t, etagMatches(` "b" , "a" `, `"a"`))
	assert.True(t, etagMatches(`*`, `"a"`))
	assert.False(t, etagMatches(``, `"a"`))
	assert.False(t, etagMatches(`"ab"`, `"a"`))
	assert.False(t, etagMatches(`a`, `"a"`))
}
