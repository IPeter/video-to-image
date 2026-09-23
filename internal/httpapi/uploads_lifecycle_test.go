package httpapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/storage"
)

// Session lifetime and eviction, superseded chunk PUTs, refused chunk bodies and
// chunk bodies without Content-Length.

// postUploadFrom creates a session on cars from the client at remote.
func (e *testEnv) postUploadFrom(remote string, size int64, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	body, err := json.Marshal(CreateUploadRequest{FileName: "a.mp4", SizeBytes: size})
	require.NoError(e.t, err)
	r := httptest.NewRequest(http.MethodPost, uploadsPath("cars"), bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = remote
	return e.do(browser(r, hdr...))
}

func (e *testEnv) newUploadFrom(remote string, size int64) UploadSession {
	e.t.Helper()
	rec := e.postUploadFrom(remote, size)
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	return decodeJSON[UploadSession](e.t, rec)
}

func (e *testEnv) putChunkFrom(remote, id string, index int, data []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	r := browser(chunkRequest("cars", id, fmt.Sprint(index), data))
	r.RemoteAddr = remote
	return e.do(r)
}

func (e *testEnv) session(id string) *uploadSession {
	e.t.Helper()
	sess, apiErr := e.srv.lookupSession(e.cars, id)
	require.Nil(e.t, apiErr, "session %s is open", id)
	return sess
}

// evictions returns the upload IDs the eviction log lines name, with their reasons.
func (e *testEnv) evictions() map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, l := range e.logs.lines(e.t) {
		if l["msg"] == "httpapi: upload session evicted" {
			out[l["upload_id"].(string)] = l["reason"].(string)
		}
	}
	return out
}

// ---- D1: lifetime and eviction ----

func TestClientAtItsCapEvictsItsOwnIdleSession(t *testing.T) {
	e := newEnv(t, withChunk(testChunk), func(_ *testEnv, d *Deps) { d.MaxConcurrentUploads = 10 })
	chunk := make([]byte, testChunk)
	a := e.newUpload(2*testChunk, "a.mp4")
	e.clock.Advance(10 * time.Second)
	b := e.newUpload(2*testChunk, "b.mp4")
	// Another client's session, idle all along, is never evicted for this client's cap.
	const other = "198.51.100.7:1"
	o := e.newUploadFrom(other, 2*testChunk)
	require.Equal(t, http.StatusNoContent, e.putChunkFrom(other, o.UploadID, 0, chunk).Code)

	e.clock.Advance(20 * time.Second) // a idle 30 s, b 20 s
	rec := e.postUpload("cars", testChunk, "c.mp4")
	requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	assert.Equal(t, "30", rec.Header().Get("Retry-After"), "a can be evicted after a minute idle")

	e.clock.Advance(10 * time.Second)
	require.Equal(t, http.StatusNoContent, e.putChunk("cars", a.UploadID, 0, chunk).Code, "a is active again")
	e.clock.Advance(20 * time.Second) // a idle 20 s, b 50 s
	rec = e.postUpload("cars", testChunk, "c.mp4")
	requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	assert.Equal(t, "10", rec.Header().Get("Retry-After"), "now b is the first one")

	e.clock.Advance(10 * time.Second) // b idle 60 s
	c := e.newUpload(testChunk, "c.mp4")
	assert.NoDirExists(t, e.sessionDir(b.UploadID), "b was evicted")
	assert.DirExists(t, e.sessionDir(a.UploadID))
	assert.DirExists(t, e.sessionDir(o.UploadID))
	requireError(t, e.putChunk("cars", b.UploadID, 0, chunk), http.StatusNotFound, CodeUploadNotFound)
	requireError(t, e.complete("cars", b.UploadID), http.StatusNotFound, CodeUploadNotFound)
	assert.Equal(t, map[string]string{b.UploadID: "client_session_cap"}, e.evictions())

	// A session with a request in flight is never evicted, however long it is idle.
	sa := e.session(a.UploadID)
	gen, apiErr, _ := sa.beginChunk(1, e.clock.Now(), nil)
	require.Nil(t, apiErr)
	e.clock.Advance(2 * time.Minute)
	d := e.newUpload(testChunk, "d.mp4")
	assert.NoDirExists(t, e.sessionDir(c.UploadID), "c was evicted, not the busy a")
	assert.DirExists(t, e.sessionDir(a.UploadID))
	rec = e.postUpload("cars", testChunk, "e.mp4")
	requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	assert.Equal(t, "60", rec.Header().Get("Retry-After"), "a is busy, d is fresh")
	_ = sa.endChunk(1, gen, false, e.clock.Now())

	require.Equal(t, http.StatusNoContent, e.abort("cars", a.UploadID).Code)
	require.Equal(t, http.StatusNoContent, e.abort("cars", d.UploadID).Code)
	r := browser(httptest.NewRequest(http.MethodDelete, sessionPath("cars", o.UploadID), nil))
	r.RemoteAddr = other
	require.Equal(t, http.StatusNoContent, e.do(r).Code)
	e.requireSlotsFree()
}

func TestFullServerEvictsTheLongestIdleSession(t *testing.T) {
	e := newEnv(t, withChunk(testChunk), func(_ *testEnv, d *Deps) { d.MaxConcurrentUploads = 2 })
	chunk := make([]byte, testChunk)
	// Two clients' uploads, each with a chunk (so the session TTL applies, not 5 min).
	s1 := e.newUploadFrom("198.51.100.1:1", 2*testChunk)
	require.Equal(t, http.StatusNoContent, e.putChunkFrom("198.51.100.1:1", s1.UploadID, 0, chunk).Code)
	e.clock.Advance(time.Minute)
	s2 := e.newUploadFrom("198.51.100.2:1", 2*testChunk)
	require.Equal(t, http.StatusNoContent, e.putChunkFrom("198.51.100.2:1", s2.UploadID, 0, chunk).Code)

	e.clock.Advance(time.Minute) // s1 idle 2 min, s2 1 min
	rec := e.do(unreadUpload(t, "198.51.100.3:1"))
	requireError(t, rec, http.StatusServiceUnavailable, CodeServerBusy)
	assert.Equal(t, "180", rec.Header().Get("Retry-After"), "s1 can be evicted after 5 minutes idle")
	rec = e.postUploadFrom("198.51.100.3:1", testChunk, headerInternalToken, testToken)
	requireError(t, rec, http.StatusServiceUnavailable, CodeServerBusy)
	assert.Equal(t, "180", rec.Header().Get("Retry-After"))

	// Five minutes idle: a single-request upload gets s1's slot.
	e.clock.Advance(3 * time.Minute)
	single := uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")
	single.RemoteAddr = "198.51.100.3:1"
	require.Equal(t, http.StatusAccepted, e.do(single).Code)
	assert.Equal(t, map[string]string{s1.UploadID: "upload_slots_full"}, e.evictions())
	requireError(t, e.putChunkFrom("198.51.100.1:1", s1.UploadID, 1, chunk), http.StatusNotFound, CodeUploadNotFound)
	assert.DirExists(t, e.sessionDir(s2.UploadID))

	// Its slot is free again once it is done: a new session takes it.
	s3 := e.newUploadFrom("198.51.100.4:1", 2*testChunk)
	require.Equal(t, http.StatusNoContent, e.putChunkFrom("198.51.100.4:1", s3.UploadID, 0, chunk).Code)

	// A session with a complete in flight is never evicted.
	sess2 := e.session(s2.UploadID)
	require.True(t, sess2.hold())
	e.clock.Advance(2 * time.Minute) // s2 idle 6 min but busy, s3 idle 2 min
	rec = e.postUploadFrom("198.51.100.5:1", testChunk)
	requireError(t, rec, http.StatusServiceUnavailable, CodeServerBusy)
	assert.Equal(t, "180", rec.Header().Get("Retry-After"), "s3 is the first that can go")
	sess2.unhold(e.clock.Now()) // the complete counts as activity

	e.clock.Advance(3 * time.Minute) // s3 idle 5 min, s2 3 min
	s4 := e.newUploadFrom("198.51.100.5:1", testChunk)
	assert.Equal(t, "upload_slots_full", e.evictions()[s3.UploadID])
	assert.DirExists(t, e.sessionDir(s2.UploadID))

	for _, id := range []string{s2.UploadID, s4.UploadID} {
		r := httptest.NewRequest(http.MethodDelete, sessionPath("cars", id), nil)
		r.Header.Set(headerInternalToken, testToken)
		require.Equal(t, http.StatusNoContent, e.do(r).Code)
	}
	e.requireSlotsFree()
}

func TestExpiredSessionsAreEvictableBeforeTheSweep(t *testing.T) {
	// A TTL shorter than the idle rules: once past its expiry a session goes, even if
	// the sweeper has not run since.
	e := newEnv(t, withChunk(testChunk), func(_ *testEnv, d *Deps) {
		d.MaxConcurrentUploads = 1
		d.UploadSessionTTL = 30 * time.Second
	})
	a := e.newUploadFrom("198.51.100.1:1", testChunk)
	e.srv.sweepUploadSessions() // the next sweep is a minute away
	e.clock.Advance(10 * time.Second)
	rec := e.postUploadFrom("198.51.100.2:1", testChunk)
	requireError(t, rec, http.StatusServiceUnavailable, CodeServerBusy)
	assert.Equal(t, "20", rec.Header().Get("Retry-After"), "a expires 30 s after its creation")
	e.clock.Advance(20 * time.Second)
	e.newUploadFrom("198.51.100.2:1", testChunk)
	assert.Equal(t, "upload_slots_full", e.evictions()[a.UploadID])
}

func TestScratchDiskLowEvictsIdleSessions(t *testing.T) {
	e := newEnv(t)
	size := int64(300 << 10)
	e.srv.freeSpace = func(string) (uint64, error) { return uint64(2*size + minScratchFree), nil } // room for one
	a := e.newUpload(size, "a.mp4")
	require.Equal(t, http.StatusNoContent, e.putChunk("cars", a.UploadID, 0, make([]byte, size)).Code)

	rec := e.postUploadFrom("198.51.100.1:1", size)
	requireError(t, rec, http.StatusServiceUnavailable, CodeServerBusy)
	assert.Equal(t, "300", rec.Header().Get("Retry-After"))

	e.clock.Advance(globalEvictIdle)
	e.newUploadFrom("198.51.100.1:1", size)
	assert.Equal(t, map[string]string{a.UploadID: "scratch_disk_low"}, e.evictions())
	assert.Len(t, e.uploadsDirEntries(), 1)
}

func TestRestoreAppliesTheFirstChunkRule(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	idle := e.newUpload(testChunk, "idle.mp4") // never got a chunk
	started := e.newUploadFrom("198.51.100.1:1", 2*testChunk)
	require.Equal(t, http.StatusNoContent, e.putChunkFrom("198.51.100.1:1", started.UploadID, 0, make([]byte, testChunk)).Code)
	e.srv.Close()

	e.clock.Advance(firstChunkIdle + time.Second)
	e.srv = New(e.deps)
	t.Cleanup(e.srv.Close)
	assert.NoDirExists(t, e.sessionDir(idle.UploadID), "no chunk and idle past 5 minutes")
	assert.DirExists(t, e.sessionDir(started.UploadID), "within the TTL")
	assert.Equal(t, 1, e.srv.sessions.count())
	assert.Contains(t, e.logs.String(), `"reason":"expired"`)
}

// ---- D3: superseded chunk PUTs ----

func TestCompleteWaitsForASupersededPUT(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	video := fakeMP4(2 * testChunk)
	info := e.newUpload(int64(len(video)), "a.mp4")
	sess := e.session(info.UploadID)

	// The client gave up on a PUT of chunk 0, but the request is still open here.
	var cuts atomic.Int32
	stale, apiErr, _ := sess.beginChunk(0, e.clock.Now(), func() { cuts.Add(1) })
	require.Nil(t, apiErr)
	e.sendAll(info, video) // the re-send of chunk 0 is stored, and chunk 1

	const drainAfter = 150 * time.Millisecond
	go func() {
		time.Sleep(drainAfter)
		_ = sess.endChunk(0, stale, false, e.clock.Now()) // cut off: it fails
	}()
	start := time.Now()
	rec := e.complete("cars", info.UploadID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.GreaterOrEqual(t, time.Since(start), drainAfter, "complete waited for it")
	assert.Equal(t, int32(1), cuts.Load(), "the superseded PUT was cut off")
	obj, ok := e.storage.object(storage.OriginalKey("cars", decodeJSON[CreateJobResponse](t, rec).JobID, "mp4"))
	require.True(t, ok)
	assert.Equal(t, video, obj.data)
}

func TestCompleteWaitsOnlyBoundedForSupersededPUTs(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	video := fakeMP4(2 * testChunk)
	info := e.newUpload(int64(len(video)), "a.mp4")
	sess := e.session(info.UploadID)

	// A chunk sent for the first time is not waited for: it is simply missing.
	first, _, _ := sess.beginChunk(1, e.clock.Now(), nil)
	start := time.Now()
	d := requireError(t, e.complete("cars", info.UploadID), http.StatusConflict, CodeUploadIncomplete)
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, []any{float64(0), float64(1)}, d.Details.(map[string]any)["missing"])
	_ = sess.endChunk(1, first, false, e.clock.Now())

	// A superseded PUT that does not end (it cannot be cut off) is waited for at most
	// staleWriterWait; then its chunk counts as missing.
	e.srv.staleWriterWait = 200 * time.Millisecond
	stale, _, _ := sess.beginChunk(0, e.clock.Now(), nil)
	e.sendAll(info, video)
	start = time.Now()
	d = requireError(t, e.complete("cars", info.UploadID), http.StatusConflict, CodeUploadIncomplete)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond)
	assert.Less(t, elapsed, 2*time.Second)
	assert.Equal(t, []any{float64(0)}, d.Details.(map[string]any)["missing"])

	// Once it ended, the newer PUT's chunk is there: complete succeeds without a re-send.
	_ = sess.endChunk(0, stale, false, e.clock.Now())
	require.Equal(t, http.StatusAccepted, e.complete("cars", info.UploadID).Code)
}

func TestCompleteCutsOffASupersededPUTOverRealHTTP(t *testing.T) {
	const chunk = 64 << 10
	e := newEnv(t, withChunk(chunk))
	srv := realServer(t, e)
	video := fakeMP4(2 * chunk)
	info := e.newUpload(int64(len(video)), "a.mp4")
	chunks := split(video, chunk)
	sess := e.session(info.UploadID)

	// The first PUT of chunk 0 stalls half-way; it stays open at the origin.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = fmt.Fprintf(conn, "PUT %s/chunks/0 HTTP/1.1\r\nHost: x\r\nOrigin: %s\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n",
		sessionPath("cars", info.UploadID), testOrigin, chunk)
	require.NoError(t, err)
	_, err = conn.Write(chunks[0][:chunk/2])
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.inflight == 1
	}, 5*time.Second, 5*time.Millisecond)

	// The browser re-sent it (and sent chunk 1); complete does not wait 5 s for the old one.
	e.sendAll(info, video)
	start := time.Now()
	rec := e.complete("cars", info.UploadID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Less(t, time.Since(start), 2*time.Second)
	obj, ok := e.storage.object(storage.OriginalKey("cars", decodeJSON[CreateJobResponse](t, rec).JobID, "mp4"))
	require.True(t, ok)
	assert.Equal(t, video, obj.data)

	// The old request is answered: its chunk is stored (by the newer PUT).
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	_ = res.Body.Close()
	assert.Equal(t, http.StatusNoContent, res.StatusCode)
	assert.Contains(t, e.logs.String(), "superseded chunk upload cut off")
}

// ---- D6: refused chunk bodies are read first ----

// rawConn is one HTTP/1.1 connection to srv for hand-written requests.
type rawConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func dialRaw(t *testing.T, srv *httptest.Server) *rawConn {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &rawConn{t: t, conn: conn, br: bufio.NewReader(conn)}
}

// put sends a PUT with header lines hdr and body, which must be written completely
// (the server reads it), and returns the response status and body.
func (c *rawConn) put(path string, hdr []string, body []byte) (int, string) {
	c.t.Helper()
	require.NoError(c.t, c.conn.SetDeadline(time.Now().Add(20*time.Second)))
	head := "PUT " + path + " HTTP/1.1\r\nHost: x\r\nContent-Type: application/octet-stream\r\n" + strings.Join(hdr, "\r\n") + "\r\n\r\n"
	_, err := io.WriteString(c.conn, head)
	require.NoError(c.t, err)
	_, err = c.conn.Write(body)
	require.NoError(c.t, err, "the server read the whole body")
	res, err := http.ReadResponse(c.br, nil)
	require.NoError(c.t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(c.t, err)
	require.False(c.t, res.Close, "the connection is kept: %s", raw)
	return res.StatusCode, string(raw)
}

// chunkedBody encodes data with Transfer-Encoding: chunked framing.
func chunkedBody(data []byte, piece int) []byte {
	var b bytes.Buffer
	for _, p := range split(data, int64(piece)) {
		fmt.Fprintf(&b, "%x\r\n", len(p))
		b.Write(p)
		b.WriteString("\r\n")
	}
	b.WriteString("0\r\n\r\n")
	return b.Bytes()
}

func TestRefusedChunksAreReadBeforeTheAnswer(t *testing.T) {
	// Far above what net/http drains by itself (256 KiB) and what socket buffers hold:
	// without the drain the writes below fail with a broken pipe or a reset.
	const chunk = 4 << 20
	e := newEnv(t, withChunk(chunk), func(e *testEnv, _ *Deps) { e.cars.Limits.MaxUploadBytes = 3 * chunk })
	srv := realServer(t, e)
	info := e.newUpload(2*chunk, "a.mp4")
	data := make([]byte, chunk)
	path := func(id string, index int) string { return fmt.Sprintf("%s/chunks/%d", sessionPath("cars", id), index) }
	origin := "Origin: " + testOrigin
	length := func(n int) string { return fmt.Sprint("Content-Length: ", n) }

	c := dialRaw(t, srv) // one connection for all of them: it survives every refusal
	for _, tc := range []struct {
		name   string
		path   string
		hdr    []string
		body   []byte
		status int
		code   ErrorCode
	}{
		{"unknown upload", path(ids.NewUpload(), 0), []string{origin, length(chunk)}, data, http.StatusNotFound, CodeUploadNotFound},
		{"bad index", path(info.UploadID, 9), []string{origin, length(chunk)}, data, http.StatusBadRequest, CodeInvalidRequest},
		{"wrong length", path(info.UploadID, 0), []string{origin, length(chunk - 1)}, data[:chunk-1], http.StatusBadRequest, CodeInvalidRequest},
		{"one byte long", path(info.UploadID, 0), []string{origin, length(chunk + 1)}, append(data, 0), http.StatusBadRequest, CodeInvalidRequest},
		{"foreign origin", path(info.UploadID, 0), []string{"Origin: https://evil.example", length(chunk)}, data, http.StatusForbidden, CodeOriginNotAllowed},
		{"no origin", path(info.UploadID, 0), []string{length(chunk)}, data, http.StatusForbidden, CodeOriginNotAllowed},
		{"unknown upload, chunked", path(ids.NewUpload(), 0), []string{origin, "Transfer-Encoding: chunked"}, chunkedBody(data, 256<<10), http.StatusNotFound, CodeUploadNotFound},
	} {
		status, raw := c.put(tc.path, tc.hdr, tc.body)
		assert.Equal(t, tc.status, status, "%s: %s", tc.name, raw)
		assert.Contains(t, raw, `"code":"`+string(tc.code)+`"`, tc.name)
	}
	status, raw := c.put(path(info.UploadID, 0), []string{origin, length(chunk)}, data)
	assert.Equal(t, http.StatusNoContent, status, raw)
}

func TestRefusedChunkBodiesAreReadOnlyWithinBounds(t *testing.T) {
	e := newEnv(t, withChunk(testChunk))
	info := e.newUpload(2*testChunk, "a.mp4")
	bound := int64(testChunk + chunkRequestHeadroom)

	// Declared larger than a chunk (plus headroom): not ours, not read.
	r := browser(httptest.NewRequest(http.MethodPut, sessionPath("cars", info.UploadID)+"/chunks/0", io.NopCloser(bodyTrap{t})))
	r.ContentLength = bound + 1
	requireError(t, e.do(r), http.StatusBadRequest, CodeInvalidRequest)
	unknownBound := e.srv.maxChunkSize(e.cars) + chunkRequestHeadroom
	r = browser(httptest.NewRequest(http.MethodPut, sessionPath("cars", ids.NewUpload())+"/chunks/0", io.NopCloser(bodyTrap{t})))
	r.ContentLength = unknownBound + 1
	requireError(t, e.do(r), http.StatusNotFound, CodeUploadNotFound)

	// Expect: 100-continue: the client has not sent the body; it gets the answer at once.
	r = browser(httptest.NewRequest(http.MethodPut, sessionPath("cars", ids.NewUpload())+"/chunks/0", io.NopCloser(bodyTrap{t})), "Expect", "100-continue")
	r.ContentLength = testChunk
	requireError(t, e.do(r), http.StatusNotFound, CodeUploadNotFound)

	// Without Content-Length, at most the bound is read.
	cr := &countingReader{r: bytes.NewReader(make([]byte, 3*bound))}
	r = browser(httptest.NewRequest(http.MethodPut, sessionPath("cars", info.UploadID)+"/chunks/7", cr))
	r.ContentLength = -1
	requireError(t, e.do(r), http.StatusBadRequest, CodeInvalidRequest)
	assert.Equal(t, int(bound), cr.n)
}

func TestRefusedChunkDrainIsPaced(t *testing.T) {
	e := newEnv(t, withChunk(64<<10))
	e.srv.uploadReadWindow = 300 * time.Millisecond
	srv := realServer(t, e)
	// Unknown upload, and the client stalls after a few bytes: cut off after the window.
	start := time.Now()
	status, raw := rawChunk(t, srv, sessionPath("cars", ids.NewUpload())+"/chunks/0", 64<<10, make([]byte, 1000), false)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, raw, string(CodeUploadNotFound))
	assert.Less(t, time.Since(start), 5*time.Second)
}

// ---- chunk bodies without Content-Length ----

func TestChunksWithoutContentLengthOverRealHTTP(t *testing.T) {
	// An HTTP/2 upload re-framed by a proxy reaches the origin as
	// Transfer-Encoding: chunked, without Content-Length.
	const chunk = 64 << 10
	e := newEnv(t, withChunk(chunk))
	srv := realServer(t, e)
	video := fakeMP4(2*chunk + 777)
	info := e.newUpload(int64(len(video)), "a.mp4")
	put := func(index int, body []byte) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
			fmt.Sprintf("%s%s/chunks/%d", srv.URL, sessionPath("cars", info.UploadID), index), io.MultiReader(bytes.NewReader(body)))
		require.NoError(t, err)
		req.TransferEncoding = []string{"chunked"}
		req.Header.Set("Origin", testOrigin)
		res, err := srv.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(raw)
	}
	chunks := split(video, chunk)

	// The body must still be exactly the chunk.
	status, raw := put(0, chunks[0][:chunk-1])
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, raw, "ended after 65535 of 65536 bytes")
	assert.Contains(t, raw, `"contentLength":-1`)
	status, raw = put(2, append(bytes.Clone(chunks[2]), 'x'))
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, raw, "is longer than 777 bytes")

	var wg sync.WaitGroup
	for i, c := range chunks {
		wg.Go(func() {
			status, raw := put(i, c)
			assert.Equal(t, http.StatusNoContent, status, raw)
		})
	}
	wg.Wait()
	rec := e.complete("cars", info.UploadID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	obj, ok := e.storage.object(storage.OriginalKey("cars", decodeJSON[CreateJobResponse](t, rec).JobID, "mp4"))
	require.True(t, ok)
	assert.Equal(t, video, obj.data)
	_, err := os.Stat(filepath.Join(e.scratch, "uploads", info.UploadID))
	assert.True(t, os.IsNotExist(err))
}

// deadlineWriter records the read deadlines a handler sets through
// http.ResponseController.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (w *deadlineWriter) SetReadDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadlines = append(w.deadlines, t)
	return nil
}

func (w *deadlineWriter) last() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deadlines[len(w.deadlines)-1]
}

func TestCutBodyOnlyWhileTheHandlerRuns(t *testing.T) {
	e := newEnv(t)
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPut, "/", strings.NewReader("x"))
	done := e.srv.paceBody(w, r)
	body := r.Body

	// Cut while the handler runs: the deadline is now, and extending it is refused.
	cutBody(body)
	assert.True(t, bodyWasCut(body))
	cutAt := w.last()
	assert.WithinDuration(t, time.Now(), cutAt, time.Second)
	done()
	assert.Equal(t, cutAt, w.last(), "a cut body keeps its expired deadline")

	// Once the handler is done, a late cut must not touch the connection, which may
	// be reading the next request.
	w = &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	r = httptest.NewRequest(http.MethodPut, "/", strings.NewReader("x"))
	done = e.srv.paceBody(w, r)
	body = r.Body
	done()
	n := len(w.deadlines)
	cutBody(body)
	assert.False(t, bodyWasCut(body))
	assert.Len(t, w.deadlines, n, "no deadline set after the handler returned")
	cutBody(strings.NewReader("not paced")) // other bodies are left alone
}
