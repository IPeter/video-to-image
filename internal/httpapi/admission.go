package httpapi

import (
	"context"
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

// slotKind tells the two holders of an upload slot apart.
type slotKind int

const (
	slotRequest slotKind = iota // one POST /jobs, while its body is read and validated
	slotSession                 // one chunked upload, from create to complete/abort/expiry/eviction
)

// busyReason is why an upload was not admitted.
type busyReason int

const (
	notBusy    busyReason = iota
	busyClient            // the client's own cap (429)
	busyGlobal            // every global slot is taken (503)
	busyDisk              // the scratch disk cannot hold it (503)
)

// uploadSlots admits uploads before their bytes arrive: at most global at once
// (single-request uploads and open chunked sessions together), at most perClient
// single-request uploads and perClientSessions sessions per client, and never more
// than the scratch disk can hold.
type uploadSlots struct {
	global            chan struct{}
	perClient         int
	perClientSessions int

	mu       sync.Mutex
	clients  map[string]int // rateKey → single-request uploads in flight
	sessions map[string]int // session client key → open chunked uploads
	held     [2]int         // slots held, per slotKind
	reserved int64          // scratch bytes reserved by the uploads admitted
}

func newUploadSlots(global, perClient, perClientSessions int) *uploadSlots {
	return &uploadSlots{
		global: make(chan struct{}, global), perClient: perClient, perClientSessions: perClientSessions,
		clients: map[string]int{}, sessions: map[string]int{},
	}
}

// take reserves one slot of kind and need scratch bytes for client ("" = no
// per-client cap). capClient false skips the per-client cap but still counts the slot
// (sessions restored after a restart). others is what the uploads admitted before
// reserve. release is idempotent.
func (u *uploadSlots) take(kind slotKind, client string, need int64, capClient bool) (others int64, release func(), reason busyReason) {
	counts, limit := u.clients, u.perClient
	if kind == slotSession {
		counts, limit = u.sessions, u.perClientSessions
	}
	u.mu.Lock()
	if capClient && client != "" && counts[client] >= limit {
		u.mu.Unlock()
		return 0, nil, busyClient
	}
	select {
	case u.global <- struct{}{}:
	default:
		u.mu.Unlock()
		return 0, nil, busyGlobal
	}
	others = u.reserved
	u.reserved += need
	u.held[kind]++
	if client != "" {
		counts[client]++
	}
	u.mu.Unlock()

	release = sync.OnceFunc(func() {
		u.mu.Lock()
		u.reserved -= need
		u.held[kind]--
		if client != "" {
			if counts[client]--; counts[client] <= 0 {
				delete(counts, client)
			}
		}
		u.mu.Unlock()
		<-u.global
	})
	return others, release, notBusy
}

// requestsHeld returns the slots single-request uploads hold.
func (u *uploadSlots) requestsHeld() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.held[slotRequest]
}

func errServerBusy() *APIError {
	return NewAPIError(http.StatusServiceUnavailable, CodeServerBusy, "the server is busy, please retry in a moment")
}

// admitUpload takes a slot for one upload of at most maxBytes, or returns the
// envelope to answer with (Retry-After already set). Internal-token callers skip the
// per-client cap but not the global one or the disk check. The returned release must
// be called once the upload's scratch file is gone.
func (s *Server) admitUpload(w http.ResponseWriter, r *http.Request, maxBytes int64) (release func(), apiErr *APIError) {
	client := ""
	if !internalFrom(r.Context()) {
		client = s.rateKey(r)
	}
	// Each upload may need its spool file plus a local-storage copy on the same disk.
	release, apiErr, retryAfter := s.admitEvicting(r.Context(), slotRequest, client, 2*maxBytes)
	if apiErr != nil {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	return release, apiErr
}

// admitEvicting is admit, making room when idle chunked uploads are in the way:
//
//   - a client at its own session cap (creating a session) gets its longest-idle
//     session evicted when that one has no request in flight and has been idle for
//     clientEvictIdle;
//   - when every global slot is taken, or the scratch disk is short, the longest-idle
//     session of anyone is evicted when it has been idle for globalEvictIdle.
//
// A session past its expiry counts as evictable whatever the idle rule (the sweeper
// may not have run yet). When nothing can be evicted it returns the envelope and the
// Retry-After (seconds) until something can be (see busyRetryAfter).
func (s *Server) admitEvicting(ctx context.Context, kind slotKind, client string, need int64) (release func(), apiErr *APIError, retryAfter int) {
	// Every round evicts one session, so this ends; the bound only guards against
	// concurrent creates taking each freed slot.
	for range s.d.MaxConcurrentUploads + 2 {
		release, reason := s.admit(ctx, kind, client, need)
		if reason == notBusy {
			return release, nil, 0
		}
		var victim *uploadSession
		switch {
		case reason == busyClient && kind == slotSession:
			victim = s.evictSession(ctx, reason, func(u *uploadSession) bool { return u.clientKey == client }, clientEvictIdle)
		case reason == busyGlobal || reason == busyDisk:
			victim = s.evictSession(ctx, reason, nil, globalEvictIdle)
		}
		if victim == nil {
			return nil, s.busyError(kind, reason), s.busyRetryAfter(kind, client, reason)
		}
	}
	return nil, errServerBusy(), uploadBusyRetryAfter
}

// busyError is the envelope of an upload refused for reason.
func (s *Server) busyError(kind slotKind, reason busyReason) *APIError {
	switch {
	case reason != busyClient:
		return errServerBusy()
	case kind == slotSession:
		return NewAPIError(http.StatusTooManyRequests, CodeRateLimited, fmt.Sprintf(
			"you already have %d uploads in progress; finish or cancel one first", s.d.MaxUploadSessionsPerClient))
	}
	return NewAPIError(http.StatusTooManyRequests, CodeRateLimited,
		"another upload from you is still in progress, please retry when it has finished")
}

// busyRetryAfter is the Retry-After (seconds) of an upload refused for reason: when
// the first session in the way can be evicted (or expires), or uploadBusyRetryAfter
// when single-request uploads (which end on their own) hold slots or nothing else is
// known.
func (s *Server) busyRetryAfter(kind slotKind, client string, reason busyReason) int {
	switch {
	case reason == busyClient && kind == slotSession:
		if wait, ok := s.sessionsWait(func(u *uploadSession) bool { return u.clientKey == client }, clientEvictIdle); ok {
			return retryAfterSeconds(wait)
		}
	case reason == busyGlobal || reason == busyDisk:
		wait, ok := s.sessionsWait(nil, globalEvictIdle)
		if !ok || (s.uploads.requestsHeld() > 0 && wait > uploadBusyRetryAfter*time.Second) {
			wait = uploadBusyRetryAfter * time.Second
		}
		return retryAfterSeconds(wait)
	}
	return uploadBusyRetryAfter
}

// admit takes a slot (uploadSlots.take) and checks that the scratch disk can hold
// need more bytes on top of what the admitted uploads reserved.
func (s *Server) admit(ctx context.Context, kind slotKind, client string, need int64) (release func(), reason busyReason) {
	others, release, reason := s.uploads.take(kind, client, need, true)
	if reason != notBusy {
		return nil, reason
	}
	// Conservative: what the other uploads already wrote is counted again in their
	// reservation, so a full scratch disk is refused early rather than hit mid-upload.
	required := others + need + minScratchFree
	free, err := s.scratchFree()
	switch {
	case errors.Is(err, errDiskFreeUnsupported):
	case err != nil:
		// Fail open: a broken statfs must not stop uploads; spooling reports real I/O errors.
		s.d.Logger.WarnContext(ctx, "httpapi: scratch free-space check failed", "err", err)
	case free < uint64(required):
		release()
		s.d.Logger.WarnContext(ctx, "httpapi: upload refused: scratch disk low",
			"free_bytes", free, "need_bytes", required, "dir", s.uploadDir())
		return nil, busyDisk
	}
	return release, notBusy
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

	mu       sync.Mutex // serialises deadline changes with cutOff, which runs on another goroutine
	cut      bool       // cutOff was called: the deadline stays in the past
	finished bool       // the handler is done: the connection may serve the next request
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
		b.mu.Lock()
		defer b.mu.Unlock()
		b.finished = true
		if !b.slow && !b.cut {
			_ = rc.SetReadDeadline(time.Now().Add(b.window))
		}
	}
}

func (b *pacedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.since += int64(n); b.since >= b.chunk {
		b.since = 0
		b.extend()
	}
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		b.slow = true
		err = fmt.Errorf("%w: %w", errUploadTooSlow, err)
	}
	return n, err
}

// extend moves the read deadline one window ahead, unless the body was cut off.
func (b *pacedBody) extend() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.cut {
		_ = b.rc.SetReadDeadline(time.Now().Add(b.window))
	}
}

// cutOff ends the body now, from any goroutine: a blocked Read returns at once with
// a deadline error, and the connection is not reused. Once the handler is done it
// does nothing (the connection may be serving another request by then).
func (b *pacedBody) cutOff() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return
	}
	b.cut = true
	_ = b.rc.SetReadDeadline(time.Now())
}

func (b *pacedBody) wasCut() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cut
}

// cutBody cuts a paced body off (pacedBody.cutOff); other bodies are left alone.
func cutBody(body io.Reader) {
	if b, ok := body.(*pacedBody); ok {
		b.cutOff()
	}
}

// bodyWasCut reports whether cutBody cut body off.
func bodyWasCut(body io.Reader) bool {
	b, ok := body.(*pacedBody)
	return ok && b.wasCut()
}
