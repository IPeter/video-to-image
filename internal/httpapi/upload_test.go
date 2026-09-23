package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

func TestCreateJobHappyPath(t *testing.T) {
	video := realMP4(t)
	e := newEnv(t)
	e.prober.info = media.VideoInfo{DurationSec: 42.5, Width: 1080, Height: 1920, FPS: 30, Codec: "h264", Rotation: -90}
	before := counterValue(t, "v2i_jobs_created_total", map[string]string{"domain": "cars"})

	r := uploadRequest(t, "cars", video, `C:\Users\me\Walk around\my car.MP4`)
	rec := e.do(r)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	resp := decodeJSON[CreateJobResponse](t, rec)
	assert.True(t, ids.Valid(ids.PrefixJob, resp.JobID), resp.JobID)
	assert.Equal(t, JobQueued, resp.Status)
	assert.Equal(t, "/v1/domains/cars/jobs/"+resp.JobID, rec.Header().Get("Location"))
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, testOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, before+1, counterValue(t, "v2i_jobs_created_total", map[string]string{"domain": "cars"}))

	// The prober saw the complete upload at ScratchDir/uploads/<jobId>.mp4 ...
	require.Equal(t, 1, e.prober.calls())
	assert.Equal(t, filepath.Join(e.scratch, "uploads", resp.JobID+".mp4"), e.prober.paths[0])
	assert.Equal(t, video, e.prober.seen[0])
	// ... and the temp file is gone afterwards.
	assert.Empty(t, e.uploadsDirEntries())

	// The original is stored under the documented key with identical bytes.
	key := storage.OriginalKey("cars", resp.JobID, "mp4")
	assert.Equal(t, []string{key}, e.storage.keys())
	obj, ok := e.storage.object(key)
	require.True(t, ok)
	assert.Equal(t, video, obj.data)
	assert.Equal(t, "video/mp4", obj.contentType)

	// One job row, enqueued inside the transaction the store handed out.
	created := e.store.createdJobs()
	require.Len(t, created, 1)
	nj := created[0]
	sum := sha256.Sum256(video)
	assert.Equal(t, resp.JobID, nj.ID)
	assert.Equal(t, "cars", nj.DomainKey)
	assert.Equal(t, key, nj.VideoPath)
	assert.Equal(t, "secret-model-cars", nj.ModelName)
	assert.Equal(t, testNow.Add(24*time.Hour), nj.ExpiresAt)
	assert.Equal(t, testIPHash("192.0.2.10"), nj.ClientIPHash)
	assert.Equal(t, media.VideoInfo{
		DurationSec: 42.5, Width: 1080, Height: 1920, FPS: 30, Codec: "h264", Rotation: -90,
		SizeBytes: int64(len(video)), MIME: "video/mp4", SHA256: hex.EncodeToString(sum[:]),
		OriginalName: "my car.MP4",
	}, nj.VideoInfo)

	require.Len(t, e.enq.calls, 1)
	assert.Equal(t, resp.JobID, e.enq.calls[0].jobID)
	assert.Same(t, e.store.enqueueTx, e.enq.calls[0].tx, "EnqueuePipelineTx must get the job-creation transaction")

	// The job is immediately pollable.
	rec = e.get("/v1/domains/cars/jobs/" + resp.JobID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, JobQueued, decodeJSON[Job](t, rec).Status)

	// The request log names the domain and the new job.
	var logged bool
	for _, l := range e.logs.lines(t) {
		if l["msg"] == "http request" && l["route"] == "/v1/domains/{domainKey}/jobs" {
			logged = true
			assert.Equal(t, "cars", l["domain"])
			assert.Equal(t, resp.JobID, l["job_id"])
			assert.EqualValues(t, 202, l["status"])
		}
	}
	assert.True(t, logged, e.logs.String())
}

func TestCreateJobStreamsOverRealHTTP(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.srv.Handler())
	t.Cleanup(srv.Close)

	video := fakeMP4(900 << 10)
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("video", "walkaround.mp4")
		if err == nil {
			// Written in small chunks with no Content-Length: Transfer-Encoding: chunked.
			for off := 0; off < len(video) && err == nil; off += 7000 {
				_, err = part.Write(video[off:min(off+7000, len(video))])
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/domains/cars/jobs", pr)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", testOrigin)

	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	var body CreateJobResponse
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	obj, ok := e.storage.object(storage.OriginalKey("cars", body.JobID, "mp4"))
	require.True(t, ok)
	assert.Equal(t, video, obj.data)
	assert.Equal(t, "walkaround.mp4", e.store.createdJobs()[0].VideoInfo.OriginalName)
	assert.Equal(t, testIPHash("127.0.0.1"), e.store.createdJobs()[0].ClientIPHash)
	assert.Empty(t, e.uploadsDirEntries())
}

func TestCreateJobAcceptsOtherContainersAndExtraFields(t *testing.T) {
	e := newEnv(t)
	webm := append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81, 0x01, 0x42, 0xF7, 0x81, 0x01, 0x42, 0xF2, 0x81, 0x04, 0x42, 0xF3, 0x81, 0x08, 0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'}, bytes.Repeat([]byte{0}, 100)...)
	format, ok := media.Sniff(webm)
	require.True(t, ok, "fixture must sniff")
	require.Equal(t, "webm", format.Ext)

	body, ct := multipartBody(t,
		formPart{name: "note", data: []byte("ignored")},
		formPart{name: "video", fileName: "clip.bin", contentType: "application/octet-stream", data: webm},
		formPart{name: "video", fileName: "second.mp4", data: fakeMP4(100)}, // only the first counts
	)
	r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Origin", testOrigin)
	rec := e.do(r)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	id := decodeJSON[CreateJobResponse](t, rec).JobID
	obj, ok := e.storage.object(storage.OriginalKey("cars", id, "webm"))
	require.True(t, ok, "stored with the sniffed extension, not the declared one")
	assert.Equal(t, "video/webm", obj.contentType)
	assert.Equal(t, webm, obj.data)
	assert.Equal(t, "video/webm", e.store.createdJobs()[0].VideoInfo.MIME)
	assert.Equal(t, "clip.bin", e.store.createdJobs()[0].VideoInfo.OriginalName)
}

func TestCreateJobInternalTokenWithoutOrigin(t *testing.T) {
	e := newEnv(t)
	r := uploadRequest(t, "cars", fakeMP4(4096), "a.mp4")
	r.Header.Del("Origin")
	r.Header.Set(headerInternalToken, testToken)
	rec := e.do(r)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCreateJobRejections(t *testing.T) {
	cases := []struct {
		name       string
		req        func(t *testing.T) *http.Request
		info       media.VideoInfo
		probeErr   error
		wantStatus int
		wantCode   ErrorCode
		wantProbed bool
	}{
		{
			name: "text file",
			req: func(t *testing.T) *http.Request {
				return uploadRequest(t, "cars", []byte("hello, I am not a video\n"), "a.mp4")
			},
			wantStatus: 400, wantCode: CodeUnsupportedFormat,
		},
		{
			name: "jpeg declared as video",
			req: func(t *testing.T) *http.Request {
				return uploadRequest(t, "cars", append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, make([]byte, 5000)...), "a.mp4")
			},
			wantStatus: 400, wantCode: CodeUnsupportedFormat,
		},
		{
			name:       "empty file",
			req:        func(t *testing.T) *http.Request { return uploadRequest(t, "cars", nil, "a.mp4") },
			wantStatus: 400, wantCode: CodeUnsupportedFormat,
		},
		{
			name:     "cannot decode",
			req:      func(t *testing.T) *http.Request { return uploadRequest(t, "cars", fakeMP4(8192), "a.mp4") },
			probeErr: fmt.Errorf("media: probe x: %w", media.ErrCannotDecode),
			// ffprobe ran on the spooled file
			wantStatus: 400, wantCode: CodeCannotDecode, wantProbed: true,
		},
		{
			name:       "too long",
			req:        func(t *testing.T) *http.Request { return uploadRequest(t, "cars", fakeMP4(8192), "a.mp4") },
			info:       media.VideoInfo{DurationSec: 180.01, Width: 1920, Height: 1080},
			wantStatus: 400, wantCode: CodeTooLong, wantProbed: true,
		},
		{
			name:       "too short",
			req:        func(t *testing.T) *http.Request { return uploadRequest(t, "cars", fakeMP4(8192), "a.mp4") },
			info:       media.VideoInfo{DurationSec: 4.99, Width: 1920, Height: 1080},
			wantStatus: 400, wantCode: CodeTooShort, wantProbed: true,
		},
		{
			name:       "resolution too low",
			req:        func(t *testing.T) *http.Request { return uploadRequest(t, "cars", fakeMP4(8192), "a.mp4") },
			info:       media.VideoInfo{DurationSec: 30, Width: 1280, Height: 479},
			wantStatus: 400, wantCode: CodeResolutionTooLow, wantProbed: true,
		},
		{
			name:       "portrait resolution too low",
			req:        func(t *testing.T) *http.Request { return uploadRequest(t, "cars", fakeMP4(8192), "a.mp4") },
			info:       media.VideoInfo{DurationSec: 30, Width: 360, Height: 640},
			wantStatus: 400, wantCode: CodeResolutionTooLow, wantProbed: true,
		},
		{
			name:       "too many pixels",
			req:        func(t *testing.T) *http.Request { return uploadRequest(t, "cars", fakeMP4(8192), "a.mp4") },
			info:       media.VideoInfo{DurationSec: 30, Width: 16384, Height: 12288, FPS: 30},
			wantStatus: 400, wantCode: CodeResolutionTooHigh, wantProbed: true,
		},
		{
			name:       "one pixel row over the default cap",
			req:        func(t *testing.T) *http.Request { return uploadRequest(t, "cars", fakeMP4(8192), "a.mp4") },
			info:       media.VideoInfo{DurationSec: 30, Width: 4096, Height: 2177, FPS: 30},
			wantStatus: 400, wantCode: CodeResolutionTooHigh, wantProbed: true,
		},
		{
			name:       "frame rate too high",
			req:        func(t *testing.T) *http.Request { return uploadRequest(t, "cars", fakeMP4(8192), "a.mp4") },
			info:       media.VideoInfo{DurationSec: 30, Width: 1920, Height: 1080, FPS: 1000},
			wantStatus: 400, wantCode: CodeResolutionTooHigh, wantProbed: true,
		},
		{
			name: "missing video field",
			req: func(t *testing.T) *http.Request {
				body, ct := multipartBody(t, formPart{name: "file", fileName: "a.mp4", data: fakeMP4(4096)})
				r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", bytes.NewReader(body))
				r.Header.Set("Content-Type", ct)
				r.Header.Set("Origin", testOrigin)
				return r
			},
			wantStatus: 400, wantCode: CodeInvalidRequest,
		},
		{
			name: "not multipart",
			req: func(t *testing.T) *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", bytes.NewReader(fakeMP4(4096)))
				r.Header.Set("Content-Type", "video/mp4")
				r.Header.Set("Origin", testOrigin)
				return r
			},
			wantStatus: 400, wantCode: CodeInvalidRequest,
		},
		{
			name: "no content type",
			req: func(t *testing.T) *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", strings.NewReader("x"))
				r.Header.Set("Origin", testOrigin)
				return r
			},
			wantStatus: 400, wantCode: CodeInvalidRequest,
		},
		{
			name: "truncated multipart body",
			req: func(t *testing.T) *http.Request {
				body, ct := multipartBody(t, formPart{name: "video", fileName: "a.mp4", data: fakeMP4(20000)})
				r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", bytes.NewReader(body[:10000]))
				r.Header.Set("Content-Type", ct)
				r.Header.Set("Origin", testOrigin)
				return r
			},
			wantStatus: 400, wantCode: CodeInvalidRequest,
		},
		{
			name: "chunked body larger than the limit",
			req: func(t *testing.T) *http.Request {
				r := uploadRequest(t, "cars", fakeMP4(testMaxUpload+1), "big.mp4")
				r.ContentLength = -1 // unknown length, as with Transfer-Encoding: chunked
				return r
			},
			wantStatus: 413, wantCode: CodeUploadTooLarge,
		},
		{
			name: "oversized junk field before the video",
			req: func(t *testing.T) *http.Request {
				body, ct := multipartBody(t,
					formPart{name: "junk", data: bytes.Repeat([]byte("x"), testMaxUpload+multipartSlack)},
					formPart{name: "video", fileName: "a.mp4", data: fakeMP4(4096)},
				)
				r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", bytes.NewReader(body))
				r.ContentLength = -1
				r.Header.Set("Content-Type", ct)
				r.Header.Set("Origin", testOrigin)
				return r
			},
			wantStatus: 413, wantCode: CodeUploadTooLarge,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.prober.info, e.prober.err = tc.info, tc.probeErr
			before := rejected(t, "cars", tc.wantCode)

			rec := e.do(tc.req(t))

			requireError(t, rec, tc.wantStatus, tc.wantCode)
			assert.Equal(t, before+1, rejected(t, "cars", tc.wantCode), "rejection counted")
			assert.Equal(t, tc.wantProbed, e.prober.calls() == 1, "probed")
			assert.Empty(t, e.storage.keys(), "nothing stored")
			assert.Empty(t, e.store.createdJobs(), "no job")
			assert.Empty(t, e.enq.calls, "nothing enqueued")
			assert.Empty(t, e.uploadsDirEntries(), "temp file removed")
			assert.Equal(t, testOrigin, rec.Header().Get("Access-Control-Allow-Origin"), "the browser can read the error")

			var logged bool
			for _, l := range e.logs.lines(t) {
				if l["msg"] == "httpapi: upload rejected" {
					logged = true
					assert.Equal(t, string(tc.wantCode), l["code"])
				}
			}
			assert.True(t, logged, "rejection logged with its code")
		})
	}
}

// bodyTrap fails the test when the handler reads a body it should reject unread.
type bodyTrap struct{ t *testing.T }

func (b bodyTrap) Read([]byte) (int, error) {
	b.t.Error("body was read")
	return 0, io.EOF
}

func TestCreateJobTooLargeByContentLength(t *testing.T) {
	e := newEnv(t)
	before := rejected(t, "cars", CodeUploadTooLarge)
	r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", io.NopCloser(bodyTrap{t}))
	r.ContentLength = testMaxUpload + multipartSlack + 1
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	r.Header.Set("Origin", testOrigin)

	rec := e.do(r)

	detail := requireError(t, rec, http.StatusRequestEntityTooLarge, CodeUploadTooLarge)
	assert.Contains(t, detail.Message, "1 MB")
	assert.Equal(t, before+1, rejected(t, "cars", CodeUploadTooLarge))
	assert.Empty(t, e.uploadsDirEntries())
}

// countingReader counts how much of the body the handler consumed.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestCreateJobEarlyRejectionDoesNotReadTheBody(t *testing.T) {
	e := newEnv(t)
	notVideo := bytes.Repeat([]byte("this is text, not a video. "), 36000) // ~970 KB, under the limit
	body, ct := multipartBody(t, formPart{name: "video", fileName: "a.mp4", data: notVideo})
	cr := &countingReader{r: bytes.NewReader(body)}
	r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", cr)
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Origin", testOrigin)

	requireError(t, e.do(r), http.StatusBadRequest, CodeUnsupportedFormat)
	assert.Less(t, cr.n, 128<<10, "only the sniffing window is read before rejecting, not the %d byte body", len(body))
}

func TestCreateJobSizeBoundary(t *testing.T) {
	// Exactly maxUploadBytes is accepted; one more byte is not (covered above).
	e := newEnv(t)
	rec := e.do(uploadRequest(t, "cars", fakeMP4(testMaxUpload), "a.mp4"))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, int64(testMaxUpload), e.store.createdJobs()[0].VideoInfo.SizeBytes)

	// A Content-Length within the slack is read and judged by the file size.
	rec = e.do(uploadRequest(t, "cars", fakeMP4(testMaxUpload+10), "a.mp4"))
	requireError(t, rec, http.StatusRequestEntityTooLarge, CodeUploadTooLarge)
}

func TestCreateJobDurationBoundariesAccepted(t *testing.T) {
	for _, info := range []media.VideoInfo{
		{DurationSec: 180, Width: 854, Height: 480},
		{DurationSec: 5, Width: 480, Height: 854},
		{DurationSec: 30, Width: 4096, Height: 2176, FPS: 120}, // exactly the default caps
		{DurationSec: 30, Width: 2160, Height: 3840, FPS: 60},  // 4K portrait phone video
	} {
		e := newEnv(t)
		e.prober.info = info
		rec := e.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4"))
		assert.Equal(t, http.StatusAccepted, rec.Code, "%+v: %s", info, rec.Body.String())
	}
}

func TestCreateJobResolutionTooHighDetails(t *testing.T) {
	e := newEnv(t, func(e *testEnv, _ *Deps) {
		e.cars.Limits.MaxPixels = 1920 * 1080
		e.cars.Limits.MaxFPS = 60
	})
	for _, info := range []media.VideoInfo{
		{DurationSec: 30, Width: 2560, Height: 1440, FPS: 30},
		{DurationSec: 30, Width: 1920, Height: 1080, FPS: 60.5},
	} {
		e.prober.info = info
		rec := e.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4"))
		detail := requireError(t, rec, http.StatusBadRequest, CodeResolutionTooHigh)
		assert.Equal(t, map[string]any{
			"width": float64(info.Width), "height": float64(info.Height), "fps": info.FPS,
			"maxPixels": float64(1920 * 1080), "maxFps": float64(60),
		}, detail.Details)
		assert.Contains(t, detail.Message, "2073600 pixels")
		assert.Contains(t, detail.Message, "60 fps")
	}
	e.prober.info = media.VideoInfo{DurationSec: 30, Width: 1080, Height: 1920, FPS: 60}
	assert.Equal(t, http.StatusAccepted, e.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")).Code, "the domain's own caps apply")
	assert.Empty(t, e.uploadsDirEntries())
}

func TestCreateJobFailuresCleanUp(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(e *testEnv)
		wantStored  bool // the original was written before the failure
		wantCreated bool
	}{
		{name: "store insert fails", setup: func(e *testEnv) { e.store.createErr = errors.New("db down") }, wantStored: true},
		{name: "enqueue fails", setup: func(e *testEnv) { e.enq.err = errors.New("river down") }, wantStored: true},
		{name: "storage put fails", setup: func(e *testEnv) { e.storage.putErr = errors.New("s3 down") }},
		{name: "probe fails", setup: func(e *testEnv) { e.prober.err = errors.New("ffprobe crashed") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.setup(e)
			rec := e.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4"))

			detail := requireError(t, rec, http.StatusInternalServerError, CodeInternal)
			assert.NotContains(t, detail.Message, "down", "internal errors are not echoed")
			assert.NotContains(t, detail.Message, "crashed")
			assert.Empty(t, e.storage.keys(), "stored original deleted")
			assert.Empty(t, e.store.createdJobs())
			assert.Empty(t, e.uploadsDirEntries(), "temp file removed")
			if tc.wantStored {
				require.Len(t, e.storage.deletedKeys, 1)
				assert.True(t, strings.HasSuffix(e.storage.deletedKeys[0], "/original.mp4"))
			}
		})
	}
}

func TestCreateJobUnknownDomainNotCounted(t *testing.T) {
	e := newEnv(t)
	r := uploadRequest(t, "boats", fakeMP4(5000), "a.mp4")
	requireError(t, e.do(r), http.StatusNotFound, CodeDomainNotFound)
	assert.Zero(t, rejected(t, "boats", CodeDomainNotFound), "no metric series for arbitrary path input")
}

func TestCreateJobUsesStoreStatusQueued(t *testing.T) {
	e := newEnv(t)
	rec := e.do(uploadRequest(t, "homes", fakeMP4(5000), "a.mp4"))
	// homes does not allow the cars origin.
	requireError(t, rec, http.StatusForbidden, CodeOriginNotAllowed)

	r := uploadRequest(t, "homes", fakeMP4(5000), "a.mp4")
	r.Header.Set("Origin", "https://homes.example.com")
	rec = e.do(r)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	id := decodeJSON[CreateJobResponse](t, rec).JobID
	j, err := e.store.GetJob(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "homes", j.DomainKey)
	assert.Equal(t, store.StatusQueued, j.Status)
	assert.Equal(t, "secret-model-homes", j.ModelName)
}

func TestSanitizeFileName(t *testing.T) {
	cases := map[string]string{
		"clip.mp4":                        "clip.mp4",
		"/etc/passwd":                     "passwd",
		`C:\Users\me\clip.mov`:            "clip.mov",
		"../../secret.mp4":                "secret.mp4",
		"bad\x00na\x1bme\n.mp4":           "badname.mp4",
		"  spaced.mp4  ":                  "spaced.mp4",
		"autó ünnep.mp4":                  "autó ünnep.mp4",
		"inval\xffid.mp4":                 "invalid.mp4",
		"..":                              "",
		"":                                "",
		strings.Repeat("é", 300):          strings.Repeat("é", 200),
		"dir/" + strings.Repeat("a", 250): strings.Repeat("a", 200),
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitizeFileName(in), "%q", in)
	}
}

func TestFormatBytes(t *testing.T) {
	assert.Equal(t, "500 MB", formatBytes(524288000))
	assert.Equal(t, "1 MB", formatBytes(1<<20))
	assert.Equal(t, "1.5 MB", formatBytes(3<<19))
	assert.Equal(t, "512 KB", formatBytes(512<<10))
	assert.Equal(t, "900 bytes", formatBytes(900))
}
