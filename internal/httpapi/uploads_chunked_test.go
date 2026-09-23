package httpapi

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/storage"
)

const testChunk = 16 << 10

// withChunk sets a small chunk size for the test server.
func withChunk(size int64) func(*testEnv, *Deps) {
	return func(_ *testEnv, d *Deps) { d.ChunkSizeBytes = size }
}

func uploadsPath(domainKey string) string { return "/v1/domains/" + domainKey + "/uploads" }

func sessionPath(domainKey, id string) string { return uploadsPath(domainKey) + "/" + id }

// browser sets the headers of a browser request from origin (default testOrigin) and
// the optional extra header pairs.
func browser(r *http.Request, hdr ...string) *http.Request {
	r.Header.Set("Origin", testOrigin)
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			r.Header.Del(hdr[i])
			continue
		}
		r.Header.Set(hdr[i], hdr[i+1])
	}
	return r
}

func (e *testEnv) postUpload(domainKey string, size int64, fileName string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	body, err := json.Marshal(CreateUploadRequest{FileName: fileName, SizeBytes: size, ContentType: "video/mp4"})
	require.NoError(e.t, err)
	r := httptest.NewRequest(http.MethodPost, uploadsPath(domainKey), bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return e.do(browser(r, hdr...))
}

// newUpload creates a session on cars and returns it.
func (e *testEnv) newUpload(size int64, fileName string, hdr ...string) UploadSession {
	e.t.Helper()
	rec := e.postUpload("cars", size, fileName, hdr...)
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	return decodeJSON[UploadSession](e.t, rec)
}

func chunkRequest(domainKey, id string, index string, data []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPut, sessionPath(domainKey, id)+"/chunks/"+index, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/octet-stream")
	return r
}

func (e *testEnv) putChunk(domainKey, id string, index int, data []byte, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(browser(chunkRequest(domainKey, id, strconv.Itoa(index), data), hdr...))
}

func (e *testEnv) complete(domainKey, id string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(browser(httptest.NewRequest(http.MethodPost, sessionPath(domainKey, id)+"/complete", nil), hdr...))
}

func (e *testEnv) abort(domainKey, id string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(browser(httptest.NewRequest(http.MethodDelete, sessionPath(domainKey, id), nil), hdr...))
}

// sendAll PUTs every chunk of data to the session and requires 204s.
func (e *testEnv) sendAll(sess UploadSession, data []byte) {
	e.t.Helper()
	for i, c := range split(data, sess.ChunkSizeBytes) {
		rec := e.putChunk("cars", sess.UploadID, i, c)
		require.Equal(e.t, http.StatusNoContent, rec.Code, "chunk %d: %s", i, rec.Body.String())
	}
}

func split(data []byte, size int64) [][]byte {
	var out [][]byte
	for off := int64(0); off < int64(len(data)); off += size {
		out = append(out, data[off:min(off+size, int64(len(data)))])
	}
	return out
}

func (e *testEnv) sessionDir(id string) string { return filepath.Join(e.scratch, "uploads", id) }

// requireSlotsFree asserts that no upload holds a slot or a reservation.
func (e *testEnv) requireSlotsFree() {
	e.t.Helper()
	u := e.srv.uploads
	u.mu.Lock()
	defer u.mu.Unlock()
	assert.Zero(e.t, u.reserved, "reservations released")
	assert.Empty(e.t, u.global, "global slots released")
	assert.Empty(e.t, u.sessions, "per-client session counters released")
	assert.Empty(e.t, u.clients)
}

// ---- happy path ----

func TestChunkedUploadHappyPath(t *testing.T) {
	video := realMP4(t)
	chunk := int64(4001)
	if int64(len(video))%chunk == 0 {
		chunk = 4003
	}
	e := newEnv(t, withChunk(chunk))
	e.prober.info = media.VideoInfo{DurationSec: 42.5, Width: 1080, Height: 1920, FPS: 30, Codec: "h264", Rotation: -90}
	before := counterValue(t, "v2i_jobs_created_total", map[string]string{"domain": "cars"})

	rec := e.postUpload("cars", int64(len(video)), `C:\Users\me\Walk around\my car.MP4`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, testOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
	sess := decodeJSON[UploadSession](t, rec)
	assert.True(t, ids.Valid(ids.PrefixUpload, sess.UploadID), sess.UploadID)
	assert.Equal(t, chunk, sess.ChunkSizeBytes)
	wantChunks := (len(video) + int(chunk) - 1) / int(chunk)
	require.Equal(t, wantChunks, sess.ChunkCount)
	require.GreaterOrEqual(t, sess.ChunkCount, 3, "the clip must span several chunks")
	assert.Equal(t, testNow.Add(firstChunkIdle), sess.ExpiresAt, "5 minutes until the first chunk")

	// The file is preallocated next to its record, which holds no client address.
	st, err := os.Stat(filepath.Join(e.sessionDir(sess.UploadID), "data"))
	require.NoError(t, err)
	assert.Equal(t, int64(len(video)), st.Size())
	record, err := os.ReadFile(filepath.Join(e.sessionDir(sess.UploadID), "session.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(record), "192.0.2.10")
	assert.Contains(t, string(record), `"fileName":"my car.MP4"`)

	// Out of order, the last (shorter) chunk first, and chunk 1 twice.
	chunks := split(video, chunk)
	assert.Less(t, len(chunks[len(chunks)-1]), int(chunk), "the last chunk is shorter")
	order := []int{len(chunks) - 1, 1}
	for i := len(chunks) - 2; i >= 0; i-- {
		order = append(order, i)
	}
	for _, i := range order {
		rec := e.putChunk("cars", sess.UploadID, i, chunks[i])
		require.Equal(t, http.StatusNoContent, rec.Code, "chunk %d: %s", i, rec.Body.String())
		assert.Equal(t, testOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
	}

	rec = e.complete("cars", sess.UploadID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	resp := decodeJSON[CreateJobResponse](t, rec)
	assert.True(t, ids.Valid(ids.PrefixJob, resp.JobID), resp.JobID)
	assert.Equal(t, JobQueued, resp.Status)
	assert.Equal(t, "/v1/domains/cars/jobs/"+resp.JobID, rec.Header().Get("Location"))
	assert.Equal(t, before+1, counterValue(t, "v2i_jobs_created_total", map[string]string{"domain": "cars"}))

	// ffprobe saw the assembled file; the stored original is byte-identical.
	require.Equal(t, 1, e.prober.calls())
	assert.Equal(t, filepath.Join(e.sessionDir(sess.UploadID), "data"), e.prober.paths[0])
	assert.Equal(t, video, e.prober.seen[0])
	key := storage.OriginalKey("cars", resp.JobID, "mp4")
	assert.Equal(t, []string{key}, e.storage.keys())
	obj, ok := e.storage.object(key)
	require.True(t, ok)
	assert.Equal(t, video, obj.data, "stored original is byte-identical")
	assert.Equal(t, "video/mp4", obj.contentType)

	created := e.store.createdJobs()
	require.Len(t, created, 1)
	sum := sha256.Sum256(video)
	assert.Equal(t, media.VideoInfo{
		DurationSec: 42.5, Width: 1080, Height: 1920, FPS: 30, Codec: "h264", Rotation: -90,
		SizeBytes: int64(len(video)), MIME: "video/mp4", SHA256: hex.EncodeToString(sum[:]),
		OriginalName: "my car.MP4",
	}, created[0].VideoInfo)
	assert.Equal(t, testIPHash("192.0.2.10"), created[0].ClientIPHash)
	assert.Equal(t, testNow.Add(24*time.Hour), created[0].ExpiresAt)
	require.Len(t, e.enq.calls, 1)
	assert.Same(t, e.store.enqueueTx, e.enq.calls[0].tx)

	// The session is gone, with its files and its slot.
	assert.Empty(t, e.uploadsDirEntries())
	assert.Zero(t, e.srv.sessions.count())
	e.requireSlotsFree()

	// A retried complete gets the same job; the session cannot take chunks again.
	rec = e.complete("cars", sess.UploadID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, resp.JobID, decodeJSON[CreateJobResponse](t, rec).JobID)
	assert.Len(t, e.store.createdJobs(), 1, "no second job")
	requireError(t, e.putChunk("cars", sess.UploadID, 0, chunks[0]), http.StatusNotFound, CodeUploadNotFound)
	requireError(t, e.abort("cars", sess.UploadID), http.StatusNotFound, CodeUploadNotFound)

	var logged bool
	for _, l := range e.logs.lines(t) {
		if l["msg"] == "httpapi: job created" {
			logged = true
			assert.Equal(t, "chunked", l["upload"])
		}
		if l["msg"] == "http request" && l["route"] == "/v1/domains/{domainKey}/uploads/{uploadId}/complete" && l["status"] == float64(202) {
			assert.Equal(t, resp.JobID, l["job_id"])
		}
	}
	assert.True(t, logged)
}

func TestChunkedUploadSingleChunk(t *testing.T) {
	e := newEnv(t)
	video := fakeMP4(5000)
	sess := e.newUpload(int64(len(video)), "a.mp4")
	assert.Equal(t, int64(defaultChunkSizeBytes), sess.ChunkSizeBytes)
	assert.Equal(t, 1, sess.ChunkCount)
	e.sendAll(sess, video)
	require.Equal(t, http.StatusAccepted, e.complete("cars", sess.UploadID).Code)
}

func TestChunkedUploadInternalTokenWithoutOrigin(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	video := fakeMP4(40_000)
	tok := []string{"Origin", "", headerInternalToken, testToken}
	sess := e.newUpload(int64(len(video)), "a.mp4", tok...)
	for i, c := range split(video, sess.ChunkSizeBytes) {
		require.Equal(t, http.StatusNoContent, e.putChunk("cars", sess.UploadID, i, c, tok...).Code)
	}
	rec := e.complete("cars", sess.UploadID, tok...)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

// ---- chunk validation ----

func TestChunkLengthMustMatch(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	size := int64(3*testChunk + 100)
	sess := e.newUpload(size, "a.mp4")
	require.Equal(t, 4, sess.ChunkCount)

	detailsOf := func(rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		d := requireError(t, rec, http.StatusBadRequest, CodeInvalidRequest)
		m, ok := d.Details.(map[string]any)
		require.True(t, ok, rec.Body.String())
		return m
	}

	m := detailsOf(e.putChunk("cars", sess.UploadID, 0, make([]byte, testChunk-1)))
	assert.Equal(t, map[string]any{"index": float64(0), "expectedBytes": float64(testChunk), "contentLength": float64(testChunk - 1)}, m)
	m = detailsOf(e.putChunk("cars", sess.UploadID, 1, make([]byte, testChunk+1)))
	assert.Equal(t, float64(testChunk+1), m["contentLength"])
	m = detailsOf(e.putChunk("cars", sess.UploadID, 3, make([]byte, testChunk)))
	assert.Equal(t, float64(100), m["expectedBytes"], "the last chunk is the remainder")

	// Content-Length right, body short or long (a client lying about it).
	r := browser(chunkRequest("cars", sess.UploadID, "3", make([]byte, 50)))
	r.ContentLength = 100
	d := requireError(t, e.do(r), http.StatusBadRequest, CodeInvalidRequest)
	assert.Contains(t, d.Message, "ended after 50 of 100 bytes")
	r = browser(chunkRequest("cars", sess.UploadID, "3", make([]byte, 150)))
	r.ContentLength = 100
	d = requireError(t, e.do(r), http.StatusBadRequest, CodeInvalidRequest)
	assert.Contains(t, d.Message, "longer than 100 bytes")
	// No Content-Length (chunked transfer): the body itself must be short or long.
	r = browser(chunkRequest("cars", sess.UploadID, "3", make([]byte, 99)))
	r.ContentLength = -1
	d = requireError(t, e.do(r), http.StatusBadRequest, CodeInvalidRequest)
	assert.Contains(t, d.Message, "ended after 99 of 100 bytes")
	assert.Equal(t, map[string]any{"index": float64(3), "expectedBytes": float64(100), "contentLength": float64(-1)}, d.Details)
	r = browser(chunkRequest("cars", sess.UploadID, "3", make([]byte, 101)))
	r.ContentLength = -1
	d = requireError(t, e.do(r), http.StatusBadRequest, CodeInvalidRequest)
	assert.Contains(t, d.Message, "longer than 100 bytes")

	// None of them counts as received.
	rec := e.complete("cars", sess.UploadID)
	d = requireError(t, rec, http.StatusConflict, CodeUploadIncomplete)
	assert.Equal(t, map[string]any{"missing": []any{float64(0), float64(1), float64(2), float64(3)}, "received": float64(0), "chunkCount": float64(4)}, d.Details)

	require.Equal(t, http.StatusNoContent, e.putChunk("cars", sess.UploadID, 3, make([]byte, 100)).Code)
}

func TestChunkIndexValidation(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	sess := e.newUpload(2*testChunk, "a.mp4")
	for _, idx := range []string{"2", "-1", "+1", "1e3", "x", "0x1", "9999999999", " 1"} {
		r := browser(chunkRequest("cars", sess.UploadID, escapeSegment(idx), make([]byte, testChunk)))
		rec := e.do(r)
		d := requireError(t, rec, http.StatusBadRequest, CodeInvalidRequest)
		assert.Equal(t, float64(2), d.Details.(map[string]any)["chunkCount"], idx)
	}
	// Leading zeros are still the number.
	require.Equal(t, http.StatusNoContent, e.do(browser(chunkRequest("cars", sess.UploadID, "01", make([]byte, testChunk)))).Code)
}

// escapeSegment escapes a path segment for httptest.NewRequest.
func escapeSegment(s string) string { return strings.ReplaceAll(s, " ", "%20") }

func TestUnknownAndForeignSessionsAreNotFound(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	sess := e.newUpload(testChunk, "a.mp4")
	homes := []string{"Origin", "https://homes.example.com"}
	data := make([]byte, testChunk)

	for _, id := range []string{ids.NewUpload(), "upl_x", ids.NewJob(), strings.ToLower(sess.UploadID)} {
		requireError(t, e.putChunk("cars", id, 0, data), http.StatusNotFound, CodeUploadNotFound)
		requireError(t, e.complete("cars", id), http.StatusNotFound, CodeUploadNotFound)
		requireError(t, e.abort("cars", id), http.StatusNotFound, CodeUploadNotFound)
	}
	// The session belongs to cars: the homes frontend cannot see or touch it.
	requireError(t, e.putChunk("homes", sess.UploadID, 0, data, homes...), http.StatusNotFound, CodeUploadNotFound)
	requireError(t, e.complete("homes", sess.UploadID, homes...), http.StatusNotFound, CodeUploadNotFound)
	requireError(t, e.abort("homes", sess.UploadID, homes...), http.StatusNotFound, CodeUploadNotFound)
	requireError(t, e.putChunk("boats", sess.UploadID, 0, data), http.StatusNotFound, CodeDomainNotFound)

	require.Equal(t, http.StatusNoContent, e.putChunk("cars", sess.UploadID, 0, data).Code, "still intact")
}

func TestCompleteWithMissingChunks(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	sess := e.newUpload(25*testChunk, "a.mp4")
	require.Equal(t, 25, sess.ChunkCount)
	require.Equal(t, http.StatusNoContent, e.putChunk("cars", sess.UploadID, 3, fakeMP4(testChunk)).Code)

	d := requireError(t, e.complete("cars", sess.UploadID), http.StatusConflict, CodeUploadIncomplete)
	details := d.Details.(map[string]any)
	var missing []int
	for _, v := range details["missing"].([]any) {
		missing = append(missing, int(v.(float64)))
	}
	assert.Equal(t, []int{0, 1, 2, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, missing, "the first 20")
	assert.Equal(t, float64(1), details["received"])
	assert.Equal(t, float64(25), details["chunkCount"])
	assert.Contains(t, d.Message, "24 of 25")
	assert.Zero(t, e.prober.calls())
	assert.Equal(t, 1, e.srv.sessions.count(), "the session stays open")
}

// ---- create: size, admission ----

func TestCreateUploadSizeLimits(t *testing.T) {
	e := newEnv(t)
	before := rejected(t, "cars", CodeUploadTooLarge)
	d := requireError(t, e.postUpload("cars", testMaxUpload+1, "big.mp4"), http.StatusRequestEntityTooLarge, CodeUploadTooLarge)
	assert.Equal(t, map[string]any{"maxUploadBytes": float64(testMaxUpload), "sizeBytes": float64(testMaxUpload + 1)}, d.Details)
	assert.Contains(t, d.Message, "1 MB")
	assert.Equal(t, before+1, rejected(t, "cars", CodeUploadTooLarge))
	assert.Empty(t, e.uploadsDirEntries())

	for _, body := range []string{`{"fileName":"a.mp4","sizeBytes":0}`, `{"fileName":"a.mp4"}`, `{"sizeBytes":5}`, `{"fileName":"a.mp4","sizeBytes":5,"x":1}`, `nope`} {
		r := browser(httptest.NewRequest(http.MethodPost, uploadsPath("cars"), strings.NewReader(body)))
		r.Header.Set("Content-Type", "application/json")
		rec := e.do(r)
		assert.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, rec.Code, body)
		requireError(t, rec, rec.Code, CodeInvalidRequest)
	}
	assert.Empty(t, e.uploadsDirEntries())

	// Exactly the limit is fine.
	e.newUpload(testMaxUpload, "a.mp4")
}

func TestMaxRequestBytesSendsLargeVideosThroughChunks(t *testing.T) {
	const maxRequest = 1<<20 + 300<<10 // chunks: 300 KiB
	e := newEnv(t, func(e *testEnv, d *Deps) {
		e.cars.Limits.MaxUploadBytes = 4 << 20
		d.MaxRequestBytes = maxRequest
	})
	video := fakeMP4(2 << 20)

	// One request: refused before the body is read, whether it declares its length...
	before := rejected(t, "cars", CodeUploadTooLarge)
	r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", io.NopCloser(bodyTrap{t}))
	r.ContentLength = int64(len(video)) + 300
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	d := requireError(t, e.do(browser(r)), http.StatusRequestEntityTooLarge, CodeUploadTooLarge)
	assert.Equal(t, map[string]any{"maxUploadBytes": float64(4 << 20), "maxRequestBytes": float64(maxRequest)}, d.Details)
	assert.Contains(t, d.Message, "chunked upload")
	assert.Equal(t, before+1, rejected(t, "cars", CodeUploadTooLarge))
	// ... or not.
	r = uploadRequest(t, "cars", video, "a.mp4")
	r.ContentLength = -1
	d = requireError(t, e.do(r), http.StatusRequestEntityTooLarge, CodeUploadTooLarge)
	assert.Equal(t, float64(maxRequest), d.Details.(map[string]any)["maxRequestBytes"])
	// Beyond the domain limit it is the plain limit.
	r = httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", io.NopCloser(bodyTrap{t}))
	r.ContentLength = 4<<20 + multipartSlack + 1
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	d = requireError(t, e.do(browser(r)), http.StatusRequestEntityTooLarge, CodeUploadTooLarge)
	assert.Equal(t, map[string]any{"maxUploadBytes": float64(4 << 20)}, d.Details)
	assert.Empty(t, e.uploadsDirEntries())
	e.requireSlotsFree()

	// A small video still takes one request.
	require.Equal(t, http.StatusAccepted, e.do(uploadRequest(t, "cars", fakeMP4(5000), "small.mp4")).Code)

	// The same large video in chunks is accepted.
	sess := e.newUpload(int64(len(video)), "a.mp4")
	assert.Equal(t, int64(300<<10), sess.ChunkSizeBytes, "chunks stay 1 MiB below the request cap")
	assert.Equal(t, 7, sess.ChunkCount)
	e.sendAll(sess, video)
	rec := e.complete("cars", sess.UploadID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	obj, ok := e.storage.object(storage.OriginalKey("cars", decodeJSON[CreateJobResponse](t, rec).JobID, "mp4"))
	require.True(t, ok)
	assert.Equal(t, video, obj.data)
}

func TestChunkSizeDefaultsAndClamp(t *testing.T) {
	for _, tc := range []struct {
		chunk, maxRequest, want int64
	}{
		{0, 0, 16 << 20},
		{0, 100 << 20, 16 << 20},
		{0, 95 << 20, 16 << 20},
		{0, 10 << 20, 9 << 20},
		{64 << 20, 99614720, 64 << 20},
		{128 << 20, 99614720, 99614720 - 1<<20},
		{1 << 20, 3 << 20, 1 << 20},
		{0, 1 << 20, 512 << 10}, // not a real deployment: still below the cap
		{0, 1, 1},
	} {
		assert.Equal(t, tc.want, effectiveChunkSize(tc.chunk, tc.maxRequest), "%+v", tc)
	}

	e := newEnv(t, func(e *testEnv, d *Deps) {
		e.cars.Limits.MaxUploadBytes = 500 << 20
		d.MaxRequestBytes = 3 << 20
	})
	sess := e.newUpload(500<<20, "big.mp4")
	assert.Equal(t, int64(2<<20), sess.ChunkSizeBytes)
	assert.Equal(t, 250, sess.ChunkCount)

	// A tiny chunk size is raised so that no session has more than maxChunkCount chunks.
	e = newEnv(t, withChunk(1), func(e *testEnv, _ *Deps) { e.cars.Limits.MaxUploadBytes = 500 << 20 })
	sess = e.newUpload(500<<20, "big.mp4")
	assert.LessOrEqual(t, sess.ChunkCount, maxChunkCount)
	assert.Equal(t, int64(52429), sess.ChunkSizeBytes)
}

func TestUploadSessionsPerClientCap(t *testing.T) {
	e := newEnv(t, withChunk(testChunk), func(_ *testEnv, d *Deps) { d.MaxConcurrentUploads = 10 })
	before := rejected(t, "cars", CodeRateLimited)
	a := e.newUpload(testChunk, "a.mp4")
	e.newUpload(testChunk, "b.mp4")

	rec := e.postUpload("cars", testChunk, "c.mp4")
	d := requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	assert.Equal(t, "60", rec.Header().Get("Retry-After"), "both are fresh: one can be evicted after a minute idle")
	assert.Equal(t, testOrigin, rec.Header().Get("Access-Control-Allow-Origin"), "the browser can read Retry-After")
	assert.Contains(t, d.Message, "2 uploads in progress")
	assert.Equal(t, before+1, rejected(t, "cars", CodeRateLimited))
	assert.Len(t, e.uploadsDirEntries(), 2)

	// Other clients, and internal-token callers, still get in.
	r := func() *http.Request {
		body, _ := json.Marshal(CreateUploadRequest{FileName: "c.mp4", SizeBytes: testChunk})
		r := httptest.NewRequest(http.MethodPost, uploadsPath("cars"), bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "198.51.100.7:1"
		return browser(r)
	}()
	assert.Equal(t, http.StatusCreated, e.do(r).Code)
	assert.Equal(t, http.StatusCreated, e.postUpload("cars", testChunk, "d.mp4", headerInternalToken, testToken).Code)

	// A single-request upload is a separate allowance.
	assert.Equal(t, http.StatusAccepted, e.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")).Code)

	// Finishing one frees its place.
	require.Equal(t, http.StatusNoContent, e.abort("cars", a.UploadID).Code)
	assert.Equal(t, http.StatusCreated, e.postUpload("cars", testChunk, "e.mp4").Code)
}

func TestUploadSessionsShareTheGlobalCap(t *testing.T) {
	e := newEnv(t, withChunk(testChunk), func(_ *testEnv, d *Deps) { d.MaxConcurrentUploads = 2 })
	remote := func(addr string) func(r *http.Request) { return func(r *http.Request) { r.RemoteAddr = addr } }
	create := func(mod func(*http.Request)) *httptest.ResponseRecorder {
		body, _ := json.Marshal(CreateUploadRequest{FileName: "a.mp4", SizeBytes: testChunk})
		r := httptest.NewRequest(http.MethodPost, uploadsPath("cars"), bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		mod(r)
		return e.do(browser(r))
	}
	before := rejected(t, "cars", CodeServerBusy)
	first := decodeJSON[UploadSession](t, create(remote("198.51.100.1:1")))
	require.Equal(t, http.StatusCreated, create(remote("198.51.100.2:1")).Code)

	rec := create(remote("198.51.100.3:1"))
	requireError(t, rec, http.StatusServiceUnavailable, CodeServerBusy)
	assert.Equal(t, "300", rec.Header().Get("Retry-After"), "the first can be evicted after 5 minutes idle")
	assert.Equal(t, before+1, rejected(t, "cars", CodeServerBusy))
	// Single-request uploads wait for the same slots, internal token or not.
	requireError(t, e.do(unreadUpload(t, "198.51.100.4:1")), http.StatusServiceUnavailable, CodeServerBusy)
	requireError(t, create(func(r *http.Request) { r.Header.Set(headerInternalToken, testToken) }), http.StatusServiceUnavailable, CodeServerBusy)

	require.Equal(t, http.StatusNoContent, e.abort("cars", first.UploadID).Code)
	assert.Equal(t, http.StatusCreated, create(remote("198.51.100.3:1")).Code)
}

func TestCreateUploadNeedsScratchSpace(t *testing.T) {
	e := newEnv(t)
	var free uint64
	var mu sync.Mutex
	e.srv.freeSpace = func(string) (uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		return free, nil
	}
	size := int64(300 << 10)
	mu.Lock()
	free = uint64(2*size+minScratchFree) - 1
	mu.Unlock()
	rec := e.postUpload("cars", size, "a.mp4")
	requireError(t, rec, http.StatusServiceUnavailable, CodeServerBusy)
	assert.Equal(t, "10", rec.Header().Get("Retry-After"))
	assert.Contains(t, e.logs.String(), "scratch disk low")
	assert.Empty(t, e.uploadsDirEntries())
	e.requireSlotsFree()

	mu.Lock()
	free++
	mu.Unlock()
	e.newUpload(size, "a.mp4")
	// The open session keeps its reservation: the same free space does not fit a second.
	requireError(t, e.postUpload("cars", size, "b.mp4"), http.StatusServiceUnavailable, CodeServerBusy)
}

func TestUploadRateLimitIsChargedPerUploadNotPerChunk(t *testing.T) {
	e := newEnv(t, withChunk(testChunk), func(e *testEnv, _ *Deps) { e.cars.Limits.RatePerIPPerHour = 1 })
	video := fakeMP4(10 * testChunk)
	sess := e.newUpload(int64(len(video)), "a.mp4")

	rec := e.postUpload("cars", testChunk, "b.mp4")
	requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	requireError(t, e.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")), http.StatusTooManyRequests, CodeRateLimited)

	e.sendAll(sess, video) // ten chunks, no limit hit
	require.Equal(t, http.StatusAccepted, e.complete("cars", sess.UploadID).Code)
}

func TestChunkRouteRateLimit(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	sess := e.newUpload(testChunk, "a.mp4")
	data := make([]byte, testChunk)
	for i := range chunkRatePerMinute {
		require.Equal(t, http.StatusNoContent, e.putChunk("cars", sess.UploadID, 0, data).Code, i)
	}
	rec := e.putChunk("cars", sess.UploadID, 0, data)
	requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	e.clock.Advance(time.Minute)
	require.Equal(t, http.StatusNoContent, e.putChunk("cars", sess.UploadID, 0, data).Code)
}

// ---- complete: validation and failures ----

func TestCompleteRejectionsDeleteTheSession(t *testing.T) {
	cases := []struct {
		name     string
		data     []byte
		info     media.VideoInfo
		probeErr error
		code     ErrorCode
	}{
		{name: "not a video", data: bytes.Repeat([]byte("hello, not a video "), 3000), code: CodeUnsupportedFormat},
		{name: "tiny", data: []byte("x"), code: CodeUnsupportedFormat},
		{name: "cannot decode", data: fakeMP4(50_000), probeErr: fmt.Errorf("media: probe: %w", media.ErrCannotDecode), code: CodeCannotDecode},
		{name: "too long", data: fakeMP4(50_000), info: media.VideoInfo{DurationSec: 181, Width: 1920, Height: 1080}, code: CodeTooLong},
		{name: "resolution too low", data: fakeMP4(50_000), info: media.VideoInfo{DurationSec: 30, Width: 640, Height: 360}, code: CodeResolutionTooLow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withChunk(testChunk))
			if tc.info.DurationSec > 0 {
				e.prober.info = tc.info
			}
			e.prober.err = tc.probeErr
			before := rejected(t, "cars", tc.code)
			sess := e.newUpload(int64(len(tc.data)), "a.mp4")
			e.sendAll(sess, tc.data)

			d := requireError(t, e.complete("cars", sess.UploadID), http.StatusBadRequest, tc.code)
			assert.Equal(t, before+1, rejected(t, "cars", tc.code))
			assert.Empty(t, e.uploadsDirEntries(), "files deleted")
			assert.Empty(t, e.storage.keys())
			assert.Empty(t, e.store.createdJobs())
			e.requireSlotsFree()

			// Asking again gives the same answer, not a second attempt.
			d2 := requireError(t, e.complete("cars", sess.UploadID), http.StatusBadRequest, tc.code)
			assert.Equal(t, d, d2)
			assert.Equal(t, before+1, rejected(t, "cars", tc.code), "a replay is not counted again")
			requireError(t, e.putChunk("cars", sess.UploadID, 0, tc.data[:min(len(tc.data), testChunk)]), http.StatusNotFound, CodeUploadNotFound)
		})
	}
}

func TestCompleteServerFailuresKeepTheSession(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(e *testEnv)
		fix     func(e *testEnv)
	}{
		{"storage down", func(e *testEnv) { e.storage.putErr = errors.New("s3 down") }, func(e *testEnv) { e.storage.putErr = nil }},
		{"database down", func(e *testEnv) { e.store.createErr = errors.New("db down") }, func(e *testEnv) { e.store.createErr = nil }},
		{"ffprobe crashed", func(e *testEnv) { e.prober.err = errors.New("ffprobe crashed") }, func(e *testEnv) { e.prober.err = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withChunk(testChunk))
			video := fakeMP4(50_000)
			sess := e.newUpload(int64(len(video)), "a.mp4")
			e.sendAll(sess, video)

			tc.breakIt(e)
			d := requireError(t, e.complete("cars", sess.UploadID), http.StatusInternalServerError, CodeInternal)
			assert.NotContains(t, d.Message, "down")
			assert.Empty(t, e.storage.keys(), "a stored original is deleted again")
			assert.Len(t, e.uploadsDirEntries(), 1, "the chunks are kept for a retry")
			assert.Contains(t, e.logs.String(), "httpapi: complete upload: ")

			tc.fix(e)
			rec := e.complete("cars", sess.UploadID)
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			obj, ok := e.storage.object(storage.OriginalKey("cars", decodeJSON[CreateJobResponse](t, rec).JobID, "mp4"))
			require.True(t, ok)
			assert.Equal(t, video, obj.data)
			assert.Empty(t, e.uploadsDirEntries())
		})
	}
}

func TestConcurrentCompletesCreateOneJob(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	e.prober.delay = 150 * time.Millisecond
	video := fakeMP4(50_000)
	sess := e.newUpload(int64(len(video)), "a.mp4")
	e.sendAll(sess, video)

	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 4)
	for i := range recs {
		wg.Go(func() { recs[i] = e.complete("cars", sess.UploadID) })
	}
	wg.Wait()
	var jobID string
	for _, rec := range recs {
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		id := decodeJSON[CreateJobResponse](t, rec).JobID
		if jobID == "" {
			jobID = id
		}
		assert.Equal(t, jobID, id)
	}
	assert.Len(t, e.store.createdJobs(), 1)
	assert.Equal(t, 1, e.prober.calls())
}

func TestChunksAndAbortWaitForARunningComplete(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	e.prober.delay = 300 * time.Millisecond
	video := fakeMP4(3 * testChunk)
	sess := e.newUpload(int64(len(video)), "a.mp4")
	e.sendAll(sess, video)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.complete("cars", sess.UploadID) }()
	require.Eventually(t, func() bool {
		s, _ := e.srv.lookupSession(e.cars, sess.UploadID)
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.completing
	}, 5*time.Second, 5*time.Millisecond)

	d := requireError(t, e.putChunk("cars", sess.UploadID, 0, video[:testChunk]), http.StatusBadRequest, CodeInvalidRequest)
	assert.Contains(t, d.Message, "being completed")
	// Abort waits for the complete and then finds the upload gone.
	requireError(t, e.abort("cars", sess.UploadID), http.StatusNotFound, CodeUploadNotFound)
	rec := <-done
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Len(t, e.store.createdJobs(), 1)
}

// ---- abort and expiry ----

func TestAbortUploadDeletesFiles(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	sess := e.newUpload(3*testChunk, "a.mp4")
	require.Equal(t, http.StatusNoContent, e.putChunk("cars", sess.UploadID, 1, make([]byte, testChunk)).Code)
	require.DirExists(t, e.sessionDir(sess.UploadID))

	rec := e.abort("cars", sess.UploadID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Body.String())
	assert.NoDirExists(t, e.sessionDir(sess.UploadID))
	assert.Empty(t, e.uploadsDirEntries())
	e.requireSlotsFree()
	requireError(t, e.putChunk("cars", sess.UploadID, 0, make([]byte, testChunk)), http.StatusNotFound, CodeUploadNotFound)
	requireError(t, e.complete("cars", sess.UploadID), http.StatusNotFound, CodeUploadNotFound)
	requireError(t, e.abort("cars", sess.UploadID), http.StatusNotFound, CodeUploadNotFound)
	assert.Contains(t, e.logs.String(), "upload session aborted")
}

func TestIdleSessionsExpire(t *testing.T) {
	e := newEnv(t, withChunk(testChunk), func(_ *testEnv, d *Deps) { d.UploadSessionTTL = time.Hour })
	a := e.newUpload(2*testChunk, "a.mp4") // never gets a chunk
	b := e.newUpload(2*testChunk, "b.mp4")
	require.Equal(t, http.StatusNoContent, e.putChunk("cars", b.UploadID, 0, make([]byte, testChunk)).Code)

	e.clock.Advance(firstChunkIdle - time.Second)
	e.srv.sweepUploadSessions()
	assert.DirExists(t, e.sessionDir(a.UploadID), "kept for 5 minutes")
	e.clock.Advance(time.Second)
	e.srv.sweepUploadSessions()
	assert.NoDirExists(t, e.sessionDir(a.UploadID), "no chunk after 5 minutes idle")
	assert.DirExists(t, e.sessionDir(b.UploadID), "b has a chunk: the TTL applies")
	requireError(t, e.putChunk("cars", a.UploadID, 0, make([]byte, testChunk)), http.StatusNotFound, CodeUploadNotFound)
	requireError(t, e.complete("cars", a.UploadID), http.StatusNotFound, CodeUploadNotFound)
	assert.Contains(t, e.logs.String(), "expired upload sessions removed")

	e.clock.Advance(30 * time.Minute)
	require.Equal(t, http.StatusConflict, e.complete("cars", b.UploadID).Code, "a complete is activity too")
	e.clock.Advance(time.Hour - time.Second)
	e.srv.sweepUploadSessions()
	assert.DirExists(t, e.sessionDir(b.UploadID))
	e.clock.Advance(time.Second)
	e.srv.sweepUploadSessions()
	assert.Empty(t, e.uploadsDirEntries(), "idle for the TTL")
	e.requireSlotsFree()
}

func TestSweepSkipsBusySessions(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	info := e.newUpload(2*testChunk, "a.mp4")
	sess, apiErr := e.srv.lookupSession(e.cars, info.UploadID)
	require.Nil(t, apiErr)

	gen, apiErr, _ := sess.beginChunk(0, e.clock.Now(), nil)
	require.Nil(t, apiErr)
	e.clock.Advance(2 * time.Hour) // a very slow chunk
	e.srv.sweepUploadSessions()
	assert.Equal(t, 1, e.srv.sessions.count(), "a chunk in flight keeps the session")
	assert.False(t, sess.endChunk(0, gen, true, e.clock.Now()).closed)

	e.clock.Advance(defaultUploadSessionTTL - time.Minute)
	e.srv.sweepUploadSessions()
	assert.Equal(t, 1, e.srv.sessions.count(), "the chunk's end counts as activity")

	// A complete in flight (here: waiting for another one) keeps it too.
	require.True(t, sess.hold())
	e.clock.Advance(time.Hour)
	e.srv.sweepUploadSessions()
	assert.Equal(t, 1, e.srv.sessions.count(), "a complete in flight keeps the session")
	sess.unhold(e.clock.Now())
	e.clock.Advance(defaultUploadSessionTTL)
	e.srv.sweepUploadSessions()
	assert.Zero(t, e.srv.sessions.count())
}

func TestExpiredSessionsFreeSlotsForNewUploads(t *testing.T) {
	e := newEnv(t, withChunk(testChunk), func(_ *testEnv, d *Deps) { d.MaxConcurrentUploads = 1 })
	e.newUpload(testChunk, "a.mp4")
	rec := e.postUpload("cars", testChunk, "b.mp4", headerInternalToken, testToken)
	requireError(t, rec, http.StatusServiceUnavailable, CodeServerBusy)
	assert.Equal(t, "300", rec.Header().Get("Retry-After"))
	e.clock.Advance(time.Hour + time.Minute)
	assert.Equal(t, http.StatusCreated, e.postUpload("cars", testChunk, "b.mp4").Code, "the idle one was swept first")
	assert.Len(t, e.uploadsDirEntries(), 1)
}

// ---- restart ----

func TestSessionsSurviveARestart(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	video := fakeMP4(3*testChunk + 500)
	sess := e.newUpload(int64(len(video)), "walk.mp4")
	chunks := split(video, testChunk)
	for _, i := range []int{0, 3} {
		require.Equal(t, http.StatusNoContent, e.putChunk("cars", sess.UploadID, i, chunks[i]).Code)
	}
	second := e.newUpload(testChunk, "other.mp4")

	// Leftovers that are not sessions, or broken ones.
	uploads := filepath.Join(e.scratch, "uploads")
	require.NoError(t, os.WriteFile(filepath.Join(uploads, ids.NewJob()+".mp4"), []byte("spool"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(uploads, "not-a-session"), 0o700))
	noRecord := ids.NewUpload()
	require.NoError(t, os.Mkdir(filepath.Join(uploads, noRecord), 0o700))
	writeRecord := func(mut func(rec *sessionRecord), dataSize int64) string {
		id := ids.NewUpload()
		dir := filepath.Join(uploads, id)
		require.NoError(t, os.Mkdir(dir, 0o700))
		rec := sessionRecord{Version: 1, UploadID: id, DomainKey: "cars", SizeBytes: 100, ChunkSizeBytes: testChunk, ChunkCount: 1,
			Received: []int{}, CreatedAt: testNow, LastActivity: testNow}
		mut(&rec)
		b, err := json.Marshal(rec)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "session.json"), b, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "data"), make([]byte, dataSize), 0o600))
		return id
	}
	expired := writeRecord(func(r *sessionRecord) { r.LastActivity = testNow.Add(-2 * time.Hour) }, 100)
	unknownDomain := writeRecord(func(r *sessionRecord) { r.DomainKey = "boats" }, 100)
	wrongSize := writeRecord(func(*sessionRecord) {}, 99)
	badIndex := writeRecord(func(r *sessionRecord) { r.Received = []int{1} }, 100)
	badCount := writeRecord(func(r *sessionRecord) { r.ChunkCount = 2 }, 100)
	otherID := writeRecord(func(r *sessionRecord) { r.UploadID = ids.NewUpload() }, 100)

	// The old process dies (its memory is gone); a new one starts on the same scratch.
	e.srv.Close()
	e.clock.Advance(4 * time.Minute) // within 5 minutes: second (no chunk yet) is kept too
	e.srv = New(e.deps)
	t.Cleanup(e.srv.Close)

	assert.Equal(t, 2, e.srv.sessions.count())
	for _, id := range []string{noRecord, expired, unknownDomain, wrongSize, badIndex, badCount, otherID} {
		assert.NoDirExists(t, filepath.Join(uploads, id))
	}
	assert.DirExists(t, filepath.Join(uploads, "not-a-session"), "not ours")
	assert.Len(t, e.uploadsDirEntries(), 4, "two sessions, the spool file and the foreign dir")
	assert.Contains(t, e.logs.String(), "upload sessions restored")

	// The restored session remembers its chunks and its slot.
	d := requireError(t, e.complete("cars", sess.UploadID), http.StatusConflict, CodeUploadIncomplete)
	assert.Equal(t, []any{float64(1), float64(2)}, d.Details.(map[string]any)["missing"])
	e.srv.uploads.mu.Lock()
	assert.Equal(t, []int{2}, slices.Collect(maps.Values(e.srv.uploads.sessions)), "both count toward the client's cap")
	assert.Len(t, e.srv.uploads.global, 2)
	e.srv.uploads.mu.Unlock()
	for _, i := range []int{1, 2} {
		require.Equal(t, http.StatusNoContent, e.putChunk("cars", sess.UploadID, i, chunks[i]).Code)
	}
	rec := e.complete("cars", sess.UploadID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	job := e.store.createdJobs()[0]
	assert.Equal(t, "walk.mp4", job.VideoInfo.OriginalName)
	assert.Equal(t, testIPHash("192.0.2.10"), job.ClientIPHash)
	obj, ok := e.storage.object(job.VideoPath)
	require.True(t, ok)
	assert.Equal(t, video, obj.data)

	require.Equal(t, http.StatusNoContent, e.abort("cars", second.UploadID).Code)
	e.requireSlotsFree()
}

func TestRestoreKeepsTheNewestSessionsThatFit(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	older := e.newUpload(testChunk, "a.mp4")
	e.clock.Advance(time.Minute)
	newer := e.newUpload(testChunk, "b.mp4")
	e.srv.Close()

	d := e.deps
	d.MaxConcurrentUploads = 1
	e.srv = New(d)
	t.Cleanup(e.srv.Close)
	assert.Equal(t, 1, e.srv.sessions.count())
	assert.NoDirExists(t, e.sessionDir(older.UploadID))
	assert.DirExists(t, e.sessionDir(newer.UploadID))
	assert.Contains(t, e.logs.String(), "no free upload slot")
}

func TestNoRestoreWithoutAScratchDir(t *testing.T) {
	// Servers without an explicit scratch dir (tools, tests) never touch the default
	// one, which a running API may be using.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	leftover := filepath.Join(tmp, "v2i", "uploads", ids.NewUpload())
	require.NoError(t, os.MkdirAll(leftover, 0o700))
	s := New(Deps{Logger: slog.New(slog.DiscardHandler), IPHashSalt: testSalt})
	t.Cleanup(s.Close)
	assert.Zero(t, s.sessions.count())
	assert.DirExists(t, leftover)
}

// ---- guard ----

func TestChunkRoutesThroughTheDomainGuard(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	sess := e.newUpload(testChunk, "a.mp4")
	path := sessionPath("cars", sess.UploadID) + "/chunks/0"

	// Preflight for the PUT.
	r := httptest.NewRequest(http.MethodOptions, path, nil)
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Access-Control-Request-Method", "PUT")
	r.Header.Set("Access-Control-Request-Headers", "content-type")
	rec := e.do(r)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, testOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "PUT")
	assert.Equal(t, "Content-Type", rec.Header().Get("Access-Control-Allow-Headers"))
	r.Header.Set("Origin", "https://evil.example")
	requireError(t, e.do(r), http.StatusForbidden, CodeOriginNotAllowed)

	// No Origin, no token: refused like any unsafe method. The chunk's body is read
	// first (so an HTTP/1.1 client reads the 403 rather than a reset), unless it is
	// declared larger than any chunk of the domain.
	cr := &countingReader{r: bytes.NewReader(make([]byte, testChunk))}
	r = httptest.NewRequest(http.MethodPut, path, cr)
	r.ContentLength = testChunk
	requireError(t, e.do(r), http.StatusForbidden, CodeOriginNotAllowed)
	assert.Equal(t, testChunk, cr.n, "the refused body was drained")
	r = httptest.NewRequest(http.MethodPut, path, io.NopCloser(bodyTrap{t}))
	r.ContentLength = e.srv.maxChunkSize(e.cars) + chunkRequestHeadroom + 1
	requireError(t, e.do(r), http.StatusForbidden, CodeOriginNotAllowed)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, uploadsPath("cars"), strings.NewReader(`{"fileName":"a","sizeBytes":1}`)),
		httptest.NewRequest(http.MethodPost, sessionPath("cars", sess.UploadID)+"/complete", nil),
		httptest.NewRequest(http.MethodDelete, sessionPath("cars", sess.UploadID), nil),
	} {
		requireError(t, e.do(req), http.StatusForbidden, CodeOriginNotAllowed)
	}
	// A foreign Origin.
	r = browser(chunkRequest("cars", sess.UploadID, "0", make([]byte, testChunk)), "Origin", "https://evil.example")
	requireError(t, e.do(r), http.StatusForbidden, CodeOriginNotAllowed)
	// The token without Origin reaches the handler.
	r = chunkRequest("cars", sess.UploadID, "0", make([]byte, testChunk))
	r.Header.Set(headerInternalToken, testToken)
	assert.Equal(t, http.StatusNoContent, e.do(r).Code)

	// Unknown methods on the route answer 405 with the Allow list.
	r = httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set(headerInternalToken, testToken)
	rec = e.do(r)
	requireError(t, rec, http.StatusMethodNotAllowed, CodeInvalidRequest)
	assert.Equal(t, "PUT", rec.Header().Get("Allow"))
}

// ---- concurrency ----

func TestConcurrentChunkPUTs(t *testing.T) {
	e := newEnv(t, withChunk(4096))
	video := fakeMP4(16*4096 + 123)
	sess := e.newUpload(int64(len(video)), "a.mp4")
	chunks := split(video, sess.ChunkSizeBytes)
	require.Len(t, chunks, 17)

	var wg sync.WaitGroup
	codes := make(chan int, 2*len(chunks))
	for i := range chunks {
		for range 2 { // every chunk twice, at the same time
			wg.Go(func() {
				rec := httptest.NewRecorder()
				r := browser(chunkRequest("cars", sess.UploadID, strconv.Itoa(i), chunks[i]))
				r.RemoteAddr = testRemoteAddr
				e.srv.Handler().ServeHTTP(rec, r)
				codes <- rec.Code
			})
		}
	}
	wg.Go(func() { e.srv.sweepUploadSessions() })
	wg.Wait()
	close(codes)
	for c := range codes {
		assert.Equal(t, http.StatusNoContent, c)
	}

	rec := e.complete("cars", sess.UploadID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	obj, ok := e.storage.object(storage.OriginalKey("cars", decodeJSON[CreateJobResponse](t, rec).JobID, "mp4"))
	require.True(t, ok)
	assert.Equal(t, video, obj.data)
}

func TestOnlyTheLatestPUTOfAChunkDecides(t *testing.T) {
	// Generations: an older PUT of a chunk that ends after a newer one does not decide.
	e := newEnv(t, withChunk(testChunk))
	info := e.newUpload(testChunk, "a.mp4")
	sess, _ := e.srv.lookupSession(e.cars, info.UploadID)
	g1, _, _ := sess.beginChunk(0, e.clock.Now(), nil)
	g2, _, _ := sess.beginChunk(0, e.clock.Now(), nil)
	_ = sess.endChunk(0, g2, false, e.clock.Now()) // the newer one failed
	_ = sess.endChunk(0, g1, true, e.clock.Now())  // the older one's bytes may be overwritten
	closed, apiErr := sess.startCompleting()
	assert.False(t, closed)
	require.NotNil(t, apiErr)
	assert.Equal(t, CodeUploadIncomplete, apiErr.Err.Code)
}

// ---- over real HTTP ----

func TestChunkedUploadOverRealHTTP(t *testing.T) {
	e := newEnv(t, withChunk(64<<10))
	srv := realServer(t, e)
	client := srv.Client()
	video := fakeMP4(300 << 10)
	type reply struct {
		status int
		close  bool
		header http.Header
		body   []byte
	}
	do := func(method, path string, body io.Reader, ct string) reply {
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, body)
		if err != nil {
			return reply{}
		}
		req.Header.Set("Origin", testOrigin)
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		res, err := client.Do(req)
		if err != nil {
			return reply{}
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return reply{status: res.StatusCode, close: res.Close, header: res.Header, body: b}
	}
	body, _ := json.Marshal(CreateUploadRequest{FileName: "walk.mov", SizeBytes: int64(len(video))})
	res := do(http.MethodPost, uploadsPath("cars"), bytes.NewReader(body), "application/json")
	require.Equal(t, http.StatusCreated, res.status, string(res.body))
	var sess UploadSession
	require.NoError(t, json.Unmarshal(res.body, &sess))

	var wg sync.WaitGroup
	for i, c := range split(video, sess.ChunkSizeBytes) {
		wg.Go(func() {
			res := do(http.MethodPut, fmt.Sprintf("%s/chunks/%d", sessionPath("cars", sess.UploadID), i), bytes.NewReader(c), "application/octet-stream")
			assert.Equal(t, http.StatusNoContent, res.status, string(res.body))
			assert.False(t, res.close, "keep-alive")
		})
	}
	wg.Wait()
	res = do(http.MethodPost, sessionPath("cars", sess.UploadID)+"/complete", nil, "")
	require.Equal(t, http.StatusAccepted, res.status, string(res.body))
	var job CreateJobResponse
	require.NoError(t, json.Unmarshal(res.body, &job))
	assert.Equal(t, "/v1/domains/cars/jobs/"+job.JobID, res.header.Get("Location"))
	obj, ok := e.storage.object(storage.OriginalKey("cars", job.JobID, "mp4"))
	require.True(t, ok)
	assert.Equal(t, video, obj.data)
}

// rawChunk sends a chunk PUT with the given Content-Length but only body, then waits
// (stall) or half-closes the connection, and returns the response status and body.
func rawChunk(t *testing.T, srv *httptest.Server, path string, contentLength int, body []byte, halfClose bool) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: x\r\nOrigin: %s\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n",
		path, testOrigin, contentLength)
	require.NoError(t, err)
	_, err = conn.Write(body)
	require.NoError(t, err)
	if halfClose {
		require.NoError(t, conn.(*net.TCPConn).CloseWrite())
	}
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, string(raw)
}

func TestChunkBodyCutShortOrTrickled(t *testing.T) {
	e := newEnv(t, withChunk(64<<10))
	e.srv.uploadReadWindow = 300 * time.Millisecond
	srv := realServer(t, e)
	sess := e.newUpload(128<<10, "a.mp4")
	path := sessionPath("cars", sess.UploadID) + "/chunks/0"

	// The client gives up half-way.
	status, raw := rawChunk(t, srv, path, 64<<10, make([]byte, 20<<10), true)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, raw, "interrupted")

	// The client stalls: cut off after the read window.
	start := time.Now()
	status, raw = rawChunk(t, srv, path, 64<<10, make([]byte, 20<<10), false)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, raw, "too slow")
	assert.Less(t, time.Since(start), 5*time.Second)

	d := requireError(t, e.complete("cars", sess.UploadID), http.StatusConflict, CodeUploadIncomplete)
	assert.Equal(t, []any{float64(0), float64(1)}, d.Details.(map[string]any)["missing"])
	s, _ := e.srv.lookupSession(e.cars, sess.UploadID)
	s.mu.Lock()
	assert.Zero(t, s.inflight)
	s.mu.Unlock()
}
