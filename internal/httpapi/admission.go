package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	// uploadBusyRetryAfter is the Retry-After (seconds) of a refused upload slot.
	uploadBusyRetryAfter = 10
	// minScratchFree is the free space an upload must leave on the scratch volume.
	minScratchFree = 256 << 20
	// defaultUploadReadWindow and defaultUploadReadChunk set the minimum upload
	// throughput: the body read deadline starts at window and moves window ahead after
	// every chunk received, so a client must deliver 1 MiB per minute (~17 KB/s) or
	// the upload is cut off. That is far below any real upstream, and it stops a
	// trickled body from holding an upload slot, a scratch file and a goroutine for
	// as long as the ingress allows.
	defaultUploadReadWindow = time.Minute
	defaultUploadReadChunk  = 1 << 20
)

// errDiskFreeUnsupported: this platform has no free-space probe (the check is skipped).
var errDiskFreeUnsupported = errors.New("httpapi: free disk space not available on this platform")

// errUploadTooSlow marks a body read that hit the pacing deadline.
var errUploadTooSlow = errors.New("httpapi: upload too slow")

// uploadSlots admits uploads before their body is read: at most global at once, at
// most perClient per client, and never more than the scratch disk can hold.
type uploadSlots struct {
	global    chan struct{}
	perClient int

	mu       sync.Mutex
	clients  map[string]int // rateKey → uploads in flight
	reserved int64          // scratch bytes reserved by the uploads in flight
}

func newUploadSlots(global, perClient int) *uploadSlots {
	return &uploadSlots{global: make(chan struct{}, global), perClient: perClient, clients: map[string]int{}}
}

// admitUpload takes a slot for one upload of at most maxBytes, or returns the
// envelope to answer with (Retry-After already set). Internal-token callers skip the
// per-client cap but not the global one or the disk check. The returned release must
// be called once the upload's scratch file is gone.
func (s *Server) admitUpload(w http.ResponseWriter, r *http.Request, maxBytes int64) (release func(), apiErr *APIError) {
	u := s.uploads
	client := ""
	if !internalFrom(r.Context()) {
		client = s.rateKey(r)
	}
	busy := func(status int, code ErrorCode, msg string) *APIError {
		w.Header().Set("Retry-After", strconv.Itoa(uploadBusyRetryAfter))
		return NewAPIError(status, code, msg)
	}

	// Each upload may need its spool file plus a local-storage copy on the same disk.
	need := 2 * maxBytes
	u.mu.Lock()
	if client != "" && u.clients[client] >= u.perClient {
		u.mu.Unlock()
		return nil, busy(http.StatusTooManyRequests, CodeRateLimited,
			"another upload from you is still in progress, please retry when it has finished")
	}
	select {
	case u.global <- struct{}{}:
	default:
		u.mu.Unlock()
		return nil, busy(http.StatusServiceUnavailable, CodeServerBusy, "the server is busy, please retry in a moment")
	}
	others := u.reserved
	u.reserved += need
	if client != "" {
		u.clients[client]++
	}
	u.mu.Unlock()

	release = func() {
		u.mu.Lock()
		u.reserved -= need
		if client != "" {
			if u.clients[client]--; u.clients[client] <= 0 {
				delete(u.clients, client)
			}
		}
		u.mu.Unlock()
		<-u.global
	}

	// Conservative: what the other uploads already wrote is counted again in their
	// reservation, so a full scratch disk is refused early rather than hit mid-upload.
	required := others + need + minScratchFree
	free, err := s.scratchFree()
	switch {
	case errors.Is(err, errDiskFreeUnsupported):
	case err != nil:
		// Fail open: a broken statfs must not stop uploads; spooling reports real I/O errors.
		s.d.Logger.WarnContext(r.Context(), "httpapi: scratch free-space check failed", "err", err)
	case free < uint64(required):
		release()
		s.d.Logger.WarnContext(r.Context(), "httpapi: upload refused: scratch disk low",
			"free_bytes", free, "need_bytes", required, "dir", s.uploadDir())
		return nil, busy(http.StatusServiceUnavailable, CodeServerBusy, "the server is busy, please retry in a moment")
	}
	return release, nil
}

// scratchFree returns the free bytes of the upload scratch directory (created first,
// so the probe sees the right volume).
func (s *Server) scratchFree() (uint64, error) {
	dir := s.uploadDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("create upload dir: %w", err)
	}
	return s.freeSpace(dir)
}

// pacedBody enforces the minimum upload throughput (see defaultUploadReadWindow) with
// http.ResponseController read deadlines. A read that hits the deadline fails with an
// error wrapping errUploadTooSlow; net/http also cancels the request context then.
type pacedBody struct {
	io.ReadCloser
	rc     *http.ResponseController
	window time.Duration
	chunk  int64
	since  int64 // bytes read since the deadline was last moved
	slow   bool  // a read hit the deadline
}

// paceBody installs the first read deadline and wraps r.Body. The returned func must
// run when the handler is done (defer it). net/http then drains what the handler left
// unread (up to 256 KiB, else it closes the connection) before it sends the response,
// so the func gives that drain one more window: a well-behaved client keeps its
// keep-alive connection, a trickling one cannot hold the connection open beyond it,
// and after a pacing failure the expired deadline is kept so the connection is dropped
// at once. The next request on the connection gets net/http's own deadlines. (A
// deadline passing during ffprobe or the storage upload cancels nothing: nothing
// reads then, and once the body hit EOF net/http's background read clears it.) Where
// deadlines are not supported (e.g. httptest.ResponseRecorder) the body is left as it is.
func (s *Server) paceBody(w http.ResponseWriter, r *http.Request) (done func()) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(s.uploadReadWindow)); err != nil {
		return func() {}
	}
	b := &pacedBody{ReadCloser: r.Body, rc: rc, window: s.uploadReadWindow, chunk: s.uploadReadChunk}
	r.Body = b
	return func() {
		if !b.slow {
			_ = rc.SetReadDeadline(time.Now().Add(b.window))
		}
	}
}

func (b *pacedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.since += int64(n); b.since >= b.chunk {
		b.since = 0
		_ = b.rc.SetReadDeadline(time.Now().Add(b.window))
	}
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		b.slow = true
		err = fmt.Errorf("%w: %w", errUploadTooSlow, err)
	}
	return n, err
}
