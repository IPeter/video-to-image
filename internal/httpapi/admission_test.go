package httpapi

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heldUpload is an upload whose body is stuck half-way, so it keeps its slot.
type heldUpload struct {
	pw   *io.PipeWriter
	rest []byte
	done chan *httptest.ResponseRecorder
}

// holdUpload starts an upload from remote and returns once the handler has admitted
// it and is reading the body.
func holdUpload(t *testing.T, e *testEnv, remote string, hdr ...string) *heldUpload {
	t.Helper()
	body, ct := multipartBody(t, formPart{name: "video", fileName: "a.mp4", contentType: "video/mp4", data: fakeMP4(200 << 10)})
	pr, pw := io.Pipe()
	r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", pr)
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Origin", testOrigin)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r.RemoteAddr = remote
	const head = 64 << 10
	h := &heldUpload{pw: pw, rest: body[head:], done: make(chan *httptest.ResponseRecorder, 1)}
	go func() {
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, r)
		_ = pr.Close() // unblock the writer if the handler gave up early
		h.done <- rec
	}()
	// io.Pipe writes return once the reader consumed them: the body is being read, so
	// the upload holds its slot.
	_, err := pw.Write(body[:head])
	require.NoError(t, err, "the upload was not admitted")
	return h
}

func (h *heldUpload) finish(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	_, err := h.pw.Write(h.rest)
	require.NoError(t, err)
	require.NoError(t, h.pw.Close())
	select {
	case rec := <-h.done:
		return rec
	case <-time.After(10 * time.Second):
		t.Fatal("held upload did not finish")
		return nil
	}
}

// unreadUpload is an upload whose body must never be read (refused before).
func unreadUpload(t *testing.T, remote string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", io.NopCloser(bodyTrap{t}))
	r.ContentLength = 5000
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	r.Header.Set("Origin", testOrigin)
	r.RemoteAddr = remote
	return r
}

func TestUploadGlobalConcurrencyCap(t *testing.T) {
	e := newEnv(t, func(_ *testEnv, d *Deps) { d.MaxConcurrentUploads, d.MaxUploadsPerClient = 1, 5 })
	before := rejected(t, "cars", CodeServerBusy)
	held := holdUpload(t, e, "192.0.2.10:1")

	rec := e.do(unreadUpload(t, "198.51.100.1:1"))
	requireError(t, rec, http.StatusServiceUnavailable, CodeServerBusy)
	assert.Equal(t, "10", rec.Header().Get("Retry-After"))
	assert.Equal(t, testOrigin, rec.Header().Get("Access-Control-Allow-Origin"), "the browser can read Retry-After")
	assert.Equal(t, before+1, rejected(t, "cars", CodeServerBusy))
	// The internal token does not bypass the global cap (it protects the disk).
	r := unreadUpload(t, "198.51.100.2:1")
	r.Header.Set(headerInternalToken, testToken)
	requireError(t, e.do(r), http.StatusServiceUnavailable, CodeServerBusy)

	require.Equal(t, http.StatusAccepted, held.finish(t).Code)
	// The slot is free again.
	assert.Equal(t, http.StatusAccepted, e.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")).Code)
	assert.Empty(t, e.srv.uploads.clients, "per-client counters are released")
	assert.Zero(t, e.srv.uploads.reserved)
}

func TestUploadPerClientConcurrencyCap(t *testing.T) {
	e := newEnv(t, func(_ *testEnv, d *Deps) { d.MaxConcurrentUploads, d.MaxUploadsPerClient = 10, 1 })
	before := rejected(t, "cars", CodeRateLimited)
	held := holdUpload(t, e, "[2001:db8:1:2::1]:1")

	for _, remote := range []string{"[2001:db8:1:2::1]:2", "[2001:db8:1:2::77]:2"} { // same /64
		rec := e.do(unreadUpload(t, remote))
		requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
		assert.Equal(t, "10", rec.Header().Get("Retry-After"))
	}
	assert.Equal(t, before+2, rejected(t, "cars", CodeRateLimited))

	// Other clients, and internal-token callers from the same one, still get in.
	other := uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")
	other.RemoteAddr = "198.51.100.1:1"
	assert.Equal(t, http.StatusAccepted, e.do(other).Code)
	internal := uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")
	internal.RemoteAddr = "[2001:db8:1:2::1]:3"
	internal.Header.Set(headerInternalToken, testToken)
	assert.Equal(t, http.StatusAccepted, e.do(internal).Code)

	require.Equal(t, http.StatusAccepted, held.finish(t).Code)
	again := uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")
	again.RemoteAddr = "[2001:db8:1:2::1]:4"
	assert.Equal(t, http.StatusAccepted, e.do(again).Code)
}

func TestUploadRefusedWhenScratchDiskLow(t *testing.T) {
	e := newEnv(t, func(_ *testEnv, d *Deps) { d.MaxConcurrentUploads = 4 })
	perUpload := uint64(2*testMaxUpload + minScratchFree)
	var free atomic.Uint64
	var mu sync.Mutex
	var dirs []string
	e.srv.freeSpace = func(dir string) (uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		dirs = append(dirs, dir)
		return free.Load(), nil
	}
	before := rejected(t, "cars", CodeServerBusy)

	free.Store(perUpload - 1)
	rec := e.do(unreadUpload(t, testRemoteAddr))
	requireError(t, rec, http.StatusServiceUnavailable, CodeServerBusy)
	assert.Equal(t, "10", rec.Header().Get("Retry-After"))
	assert.Equal(t, before+1, rejected(t, "cars", CodeServerBusy))
	assert.Contains(t, e.logs.String(), "scratch disk low")
	mu.Lock()
	assert.Equal(t, []string{filepath.Join(e.scratch, "uploads")}, dirs, "the probe looks at the upload volume")
	mu.Unlock()

	free.Store(perUpload)
	assert.Equal(t, http.StatusAccepted, e.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")).Code, "exactly enough")

	// An upload in flight reserves its share: the same free space fits one, not two.
	held := holdUpload(t, e, "198.51.100.1:1")
	requireError(t, e.do(unreadUpload(t, "198.51.100.2:1")), http.StatusServiceUnavailable, CodeServerBusy)
	free.Store(2 * perUpload)
	require.Equal(t, http.StatusAccepted, e.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")).Code)
	require.Equal(t, http.StatusAccepted, held.finish(t).Code)
	assert.Zero(t, e.srv.uploads.reserved)
	assert.Empty(t, e.srv.uploads.global, "every slot was released")
}

func TestUploadFreeSpaceProbeFailuresFailOpen(t *testing.T) {
	for _, tc := range []struct {
		err      error
		wantWarn bool
	}{
		{errDiskFreeUnsupported, false},
		{errors.New("statfs: input/output error"), true},
	} {
		e := newEnv(t)
		e.srv.freeSpace = func(string) (uint64, error) { return 0, tc.err }
		assert.Equal(t, http.StatusAccepted, e.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")).Code, tc.err)
		assert.Equal(t, tc.wantWarn, strings.Contains(e.logs.String(), "free-space check failed"), tc.err)
	}
}

func TestDiskFreeBytes(t *testing.T) {
	n, err := diskFreeBytes(t.TempDir())
	if errors.Is(err, errDiskFreeUnsupported) {
		t.Skip("no statfs on this platform")
	}
	require.NoError(t, err)
	assert.Positive(t, n)
	_, err = diskFreeBytes(filepath.Join(t.TempDir(), "missing"))
	assert.Error(t, err)
}

// realServer serves e over a real TCP listener (read deadlines need a connection).
func realServer(t *testing.T, e *testEnv) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(e.srv.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestUploadTrickledBodyIsCutOff(t *testing.T) {
	// The client stalls: before the multipart headers, inside them, or mid-file.
	for _, sent := range []int{0, 60, 32 << 10} {
		t.Run(fmt.Sprint("sent=", sent), func(t *testing.T) {
			e := newEnv(t, func(_ *testEnv, d *Deps) { d.MaxConcurrentUploads = 1 })
			e.srv.uploadReadWindow = 300 * time.Millisecond
			srv := realServer(t, e)
			before := rejected(t, "cars", CodeInvalidRequest)

			body, ct := multipartBody(t, formPart{name: "video", fileName: "a.mp4", contentType: "video/mp4", data: fakeMP4(500 << 10)})
			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			start := time.Now()
			_, err = fmt.Fprintf(conn, "POST /v1/domains/cars/jobs HTTP/1.1\r\nHost: x\r\nOrigin: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
				testOrigin, ct, len(body))
			require.NoError(t, err)
			_, err = conn.Write(body[:sent]) // then nothing more
			require.NoError(t, err)

			require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
			res, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err, "the server answers instead of waiting for the rest")
			defer func() { _ = res.Body.Close() }()
			elapsed := time.Since(start)
			require.Equal(t, http.StatusBadRequest, res.StatusCode)
			raw, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			assert.Contains(t, string(raw), `"INVALID_REQUEST"`)
			assert.Contains(t, string(raw), "too slow")
			assert.GreaterOrEqual(t, elapsed, 300*time.Millisecond)
			assert.Less(t, elapsed, 5*time.Second)

			assert.Equal(t, before+1, rejected(t, "cars", CodeInvalidRequest))
			assert.Empty(t, e.uploadsDirEntries(), "a partial scratch file is removed")
			assert.Empty(t, e.store.createdJobs())
			// Its slot was released: the only one there is.
			assert.Equal(t, http.StatusAccepted, postOver(t, srv, fakeMP4(5000), 0, 0))
		})
	}
}

func TestUploadRejectedEarlyWithTrickledTail(t *testing.T) {
	// An upload rejected before its body was read (here: not a video) leaves a tail
	// that net/http drains before answering; a client that stops sending must not
	// keep that connection (and goroutine) forever.
	e := newEnv(t)
	e.srv.uploadReadWindow = 300 * time.Millisecond
	srv := realServer(t, e)

	body, ct := multipartBody(t, formPart{name: "video", fileName: "a.mp4", data: bytes.Repeat([]byte("not a video "), 10_000)})
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = fmt.Fprintf(conn, "POST /v1/domains/cars/jobs HTTP/1.1\r\nHost: x\r\nOrigin: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
		testOrigin, ct, len(body))
	require.NoError(t, err)
	_, err = conn.Write(body[:16<<10])
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "answered within the drain window, not left hanging")
	defer func() { _ = res.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, res.StatusCode)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.True(t, res.Close, "the half-sent connection is not reused")
}

func TestUploadPacingMovesTheDeadlineForward(t *testing.T) {
	// 16 KiB must arrive per 400 ms; the client sends 8 KiB every 40 ms, so the whole
	// body takes ~1.2 s (far past one window) and is still accepted.
	e := newEnv(t)
	e.srv.uploadReadWindow = 400 * time.Millisecond
	e.srv.uploadReadChunk = 16 << 10
	srv := realServer(t, e)

	start := time.Now()
	assert.Equal(t, http.StatusAccepted, postOver(t, srv, fakeMP4(240<<10), 8<<10, 40*time.Millisecond))
	assert.Greater(t, time.Since(start), e.srv.uploadReadWindow, "the upload outlived the first deadline")
}

func TestUploadDrainAfterSlowProbeKeepsConnection(t *testing.T) {
	// ffprobe may take longer than the read window, and afterwards net/http drains the
	// rest of the body (here a trailing form field): the expired pacing deadline must
	// not fail that drain and drop the keep-alive connection.
	e := newEnv(t)
	e.srv.uploadReadWindow = 200 * time.Millisecond
	e.prober.delay = 500 * time.Millisecond
	srv := realServer(t, e)

	body, ct := multipartBody(t,
		formPart{name: "video", fileName: "a.mp4", contentType: "video/mp4", data: fakeMP4(5000)},
		formPart{name: "note", data: bytes.Repeat([]byte("n"), 64<<10)},
	)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/domains/cars/jobs", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Origin", testOrigin)
	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	assert.Equal(t, http.StatusAccepted, res.StatusCode)
	assert.False(t, res.Close, "the connection stays reusable")
	assert.Len(t, e.store.createdJobs(), 1)
}

// postOver uploads video to the real server, optionally in pieces of chunk bytes
// with a pause between them (chunked transfer encoding), and returns the status.
func postOver(t *testing.T, srv *httptest.Server, video []byte, chunk int, pause time.Duration) int {
	t.Helper()
	var body io.Reader
	var ct string
	if chunk <= 0 {
		b, c := multipartBody(t, formPart{name: "video", fileName: "a.mp4", contentType: "video/mp4", data: video})
		body, ct = bytes.NewReader(b), c
	} else {
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		ct = mw.FormDataContentType()
		go func() {
			part, err := mw.CreateFormFile("video", "a.mp4")
			for off := 0; err == nil && off < len(video); off += chunk {
				if off > 0 {
					time.Sleep(pause)
				}
				_, err = part.Write(video[off:min(off+chunk, len(video))])
			}
			if err == nil {
				err = mw.Close()
			}
			pw.CloseWithError(err)
		}()
		body = pr
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/domains/cars/jobs", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Origin", testOrigin)
	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}
