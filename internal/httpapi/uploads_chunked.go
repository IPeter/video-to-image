package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/media"
)

// Chunked uploads: for videos larger than one request may carry (Cloudflare refuses
// bodies over 100 MB). POST .../uploads admits the upload like POST /jobs would (rate
// limit, size, upload slot, scratch space) and preallocates the file; the client PUTs
// the chunks (in any order, in parallel, re-sending any), then POST .../complete runs
// the same ingest as POST /jobs. Session bookkeeping is in upload_sessions.go.

const (
	// completeTimeout bounds hashing, probing and storing a completed upload. Complete
	// runs detached from the request: once it started, a client that goes away (or a
	// proxy timeout) does not lose the job; a retried complete gets the same answer.
	completeTimeout = 10 * time.Minute
	// maxChunkIndexDigits bounds the {index} path parameter.
	maxChunkIndexDigits = 9
	// maxContentTypeLen caps the declared content type kept for the rejection log.
	maxContentTypeLen = 100
)

// createUpload is POST /v1/domains/{domainKey}/uploads.
func (s *Server) createUpload(ctx context.Context, in *CreateUploadInput) (*CreateUploadOutput, error) {
	dc, err := s.domainFor(ctx, in.DomainKey)
	if err != nil {
		return nil, s.apiError(ctx, "create upload", err)
	}
	r := requestFrom(ctx)
	if r == nil {
		r = &http.Request{Header: http.Header{}}
	}
	reject := func(e *APIError, attrs ...any) *APIError {
		s.countRejection(ctx, dc, e, attrs...)
		return e
	}
	size := in.Body.SizeBytes
	if size > dc.Limits.MaxUploadBytes {
		return nil, reject(errTooLarge(dc.Limits.MaxUploadBytes).
			WithDetails(map[string]any{"maxUploadBytes": dc.Limits.MaxUploadBytes, "sizeBytes": size}), "size_bytes", size)
	}
	if size < 1 { // Huma validates minimum:1; this guards direct calls
		return nil, NewAPIError(http.StatusBadRequest, CodeInvalidRequest, "sizeBytes must be at least 1")
	}

	s.maybeSweepUploadSessions() // expired sessions give their slots back first
	client := ""
	if !internalFrom(ctx) {
		client = s.sessionClientKey(r)
	}
	// Like a single-request upload: the file plus a local-storage copy on the same disk.
	// Idle sessions in the way (this client's own, or anyone's when the server is full)
	// are evicted first.
	release, busy, retryAfter := s.admitEvicting(ctx, slotSession, client, 2*size)
	if busy != nil {
		return nil, huma.ErrorWithHeaders(reject(busy, "size_bytes", size),
			http.Header{"Retry-After": {strconv.Itoa(retryAfter)}})
	}
	sess, err := s.openUploadSession(newSessionParams{
		dc: dc, clientKey: client, ipHash: s.ipHash(r),
		fileName: sanitizeFileName(in.Body.FileName), contentType: truncate(in.Body.ContentType, maxContentTypeLen),
		size: size, release: release,
	})
	if err != nil {
		release()
		return nil, s.apiError(ctx, "create upload", err)
	}
	sess.mu.Lock()
	expires := sess.expiresAtLocked(s.d.UploadSessionTTL)
	sess.mu.Unlock()
	s.d.Logger.InfoContext(ctx, "httpapi: upload session created", "domain", dc.Key, "upload_id", sess.id,
		"bytes", size, "chunk_bytes", sess.chunkSize, "chunks", sess.chunkCount)
	return &CreateUploadOutput{Body: UploadSession{
		UploadID: sess.id, ChunkSizeBytes: sess.chunkSize, ChunkCount: sess.chunkCount, ExpiresAt: expires,
	}}, nil
}

// putUploadChunk is PUT /v1/domains/{domainKey}/uploads/{uploadId}/chunks/{index}:
// the raw bytes of one chunk, streamed into the session file at index*chunkSize
// through a small buffer, at the same minimum pace as POST /jobs. A declared
// Content-Length must be the chunk's exact length; without one (the body reached the
// origin with Transfer-Encoding: chunked, e.g. an HTTP/2 upload re-framed by a proxy)
// the body itself must carry exactly that many bytes.
func (s *Server) putUploadChunk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	defer s.paceBody(w, r)() // also bounds net/http's drain of an unread body
	dc, err := s.domainFor(ctx, chi.URLParam(r, "domainKey"))
	if err != nil {
		s.writeErr(w, r, "put upload chunk", err)
		return
	}
	// refuse answers after reading what is left of the body (see drainRefusedBody).
	refuse := func(e *APIError, maxChunk int64) {
		drainRefusedBody(r, maxChunk+chunkRequestHeadroom)
		writeAPIError(w, e)
	}
	sess, apiErr := s.lookupSession(dc, chi.URLParam(r, "uploadId"))
	if apiErr != nil {
		refuse(apiErr, s.maxChunkSize(dc))
		return
	}
	rawIndex := chi.URLParam(r, "index")
	index, ok := parseChunkIndex(rawIndex)
	if !ok || index >= sess.chunkCount {
		refuse(NewAPIError(http.StatusBadRequest, CodeInvalidRequest,
			fmt.Sprintf("chunk index must be 0..%d", sess.chunkCount-1)).
			WithDetails(map[string]any{"index": truncate(rawIndex, 32), "chunkCount": sess.chunkCount}), sess.chunkSize)
		return
	}
	want := sess.chunkLen(index)
	lengthErr := func(msg string, got int64) *APIError {
		return NewAPIError(http.StatusBadRequest, CodeInvalidRequest, msg).
			WithDetails(map[string]any{"index": index, "expectedBytes": want, "contentLength": got})
	}
	// -1: no Content-Length (chunked transfer); writeChunk then checks the body's length.
	if r.ContentLength >= 0 && r.ContentLength != want {
		refuse(lengthErr(fmt.Sprintf("chunk %d must be exactly %d bytes (Content-Length)", index, want), r.ContentLength), sess.chunkSize)
		return
	}

	body := r.Body
	gen, apiErr, perr := sess.beginChunk(index, s.d.Now(), func() { cutBody(body) })
	if apiErr != nil {
		refuse(apiErr, sess.chunkSize)
		return
	}
	if perr != nil {
		s.d.Logger.WarnContext(ctx, "httpapi: persist upload session", "upload_id", sess.id, "err", perr)
	}
	n, werr := writeChunk(sess, index, body, want)
	end := sess.endChunk(index, gen, werr == nil, s.d.Now())
	if end.persistErr != nil {
		// The chunk is stored; only a restart would forget it.
		s.d.Logger.WarnContext(ctx, "httpapi: persist upload session", "upload_id", sess.id, "err", end.persistErr)
	}
	var clientErr *APIError
	switch {
	case end.closed || errors.Is(werr, fs.ErrNotExist):
		refuse(errUploadNotFound(), sess.chunkSize) // aborted, expired or evicted meanwhile
	case werr != nil && end.superseded && bodyWasCut(body):
		// complete cut this older PUT off: a newer PUT of the chunk had stored it.
		s.d.Logger.InfoContext(ctx, "httpapi: superseded chunk upload cut off", "upload_id", sess.id, "index", index, "bytes", n)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(werr, errChunkShort):
		writeAPIError(w, lengthErr(fmt.Sprintf("chunk %d ended after %d of %d bytes", index, n, want), r.ContentLength))
	case errors.Is(werr, errChunkLong):
		refuse(lengthErr(fmt.Sprintf("chunk %d is longer than %d bytes", index, want), r.ContentLength), sess.chunkSize)
	case errors.As(werr, &clientErr):
		s.d.Logger.InfoContext(ctx, "httpapi: chunk upload failed", "upload_id", sess.id, "index", index, "bytes", n, "err", werr)
		writeAPIError(w, clientErr)
	case werr != nil:
		if ctx.Err() == nil {
			s.d.Logger.ErrorContext(ctx, "httpapi: write upload chunk", "upload_id", sess.id, "index", index, "err", werr)
		}
		writeAPIError(w, errInternal())
	default:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

// maxChunkSize is the largest chunk a session of dc can have (sessionChunkSize of
// the domain's largest upload).
func (s *Server) maxChunkSize(dc *domain.Config) int64 {
	return s.sessionChunkSize(dc.Limits.MaxUploadBytes)
}

// drainRefusedBody reads and discards what is left of the body of a chunk PUT that
// is refused, before the answer is written: an HTTP/1.1 client (a browser, or a
// proxy hop) reads the answer only after it sent the whole body, and net/http would
// otherwise close the connection with most of it unread, so the client would see a
// connection reset instead of, say, UPLOAD_NOT_FOUND. It reads at most limit bytes
// (a chunk plus headroom) and nothing when the declared Content-Length is above that
// (not a chunk of ours); the body is paced (paceBody), so a trickling client is cut
// off as usual. A client that sent Expect: 100-continue has not sent the body yet
// and gets the answer at once.
func drainRefusedBody(r *http.Request, limit int64) {
	if r.Body == nil || r.ContentLength == 0 || r.ContentLength > limit ||
		strings.EqualFold(r.Header.Get("Expect"), "100-continue") {
		return
	}
	n := limit
	if r.ContentLength > 0 {
		n = r.ContentLength
	}
	_, _ = io.CopyN(io.Discard, r.Body, n)
}

var (
	errChunkShort = errors.New("httpapi: chunk body shorter than its Content-Length")
	errChunkLong  = errors.New("httpapi: chunk body longer than its Content-Length")
)

// writeChunk copies exactly want bytes of body into the session file at the chunk's
// offset; it reads one byte more to see that the body ends there (net/http stops at
// a declared Content-Length; a chunked body must end by itself). Errors: *APIError
// (wrapped) for a broken or too slow body, errChunkShort / errChunkLong for a wrong
// length, fs.ErrNotExist when the session file is gone, and anything else for a
// server-side failure.
func writeChunk(sess *uploadSession, index int, body io.Reader, want int64) (int64, error) {
	f, err := os.OpenFile(sess.dataPath(), os.O_WRONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open session file: %w", err)
	}
	rr := &readErrRecorder{r: io.LimitReader(body, want)}
	buf := make([]byte, uploadBufferSize)
	n, copyErr := io.CopyBuffer(io.NewOffsetWriter(f, int64(index)*sess.chunkSize), rr, buf)
	closeErr := f.Close()
	switch {
	case rr.err != nil:
		return n, fmt.Errorf("read chunk: %w (%w)", chunkBodyError(rr.err), rr.err)
	case copyErr != nil:
		return n, fmt.Errorf("write chunk: %w", copyErr)
	case closeErr != nil:
		return n, fmt.Errorf("close session file: %w", closeErr)
	case n < want:
		return n, errChunkShort
	}
	// The body must end here: at its Content-Length, or with its last chunk.
	m, err := io.ReadFull(body, buf[:1])
	switch {
	case m > 0:
		return n, errChunkLong
	case err != nil && !errors.Is(err, io.EOF):
		return n, fmt.Errorf("read chunk end: %w (%w)", chunkBodyError(err), err)
	}
	return n, nil
}

// chunkBodyError maps a failure reading a chunk body to an envelope.
func chunkBodyError(err error) *APIError {
	if errors.Is(err, errUploadTooSlow) {
		return NewAPIError(http.StatusBadRequest, CodeInvalidRequest, "the upload was too slow and was cut off")
	}
	return NewAPIError(http.StatusBadRequest, CodeInvalidRequest, "the chunk body was interrupted")
}

// parseChunkIndex accepts a plain decimal number.
func parseChunkIndex(s string) (int, bool) {
	if s == "" || len(s) > maxChunkIndexDigits || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// completeUpload is POST /v1/domains/{domainKey}/uploads/{uploadId}/complete.
func (s *Server) completeUpload(ctx context.Context, in *UploadPathInput) (*CompleteUploadOutput, error) {
	dc, err := s.domainFor(ctx, in.DomainKey)
	if err != nil {
		return nil, s.apiError(ctx, "complete upload", err)
	}
	sess, apiErr := s.lookupSession(dc, in.UploadID)
	if apiErr != nil {
		return s.replayComplete(ctx, dc, in.UploadID, apiErr)
	}
	if !sess.hold() { // no sweep or eviction while this request waits and runs
		return s.replayComplete(ctx, dc, in.UploadID, errUploadNotFound())
	}
	defer func() { sess.unhold(s.d.Now()) }()
	sess.completeMu.Lock()
	defer sess.completeMu.Unlock()
	// An older PUT of a chunk that a newer PUT stored may still be open (the client
	// gave up on it); give it a moment to end rather than calling the chunk missing.
	sess.awaitStaleWriters(s.staleWriterWait)
	closed, apiErr := sess.startCompleting()
	if closed { // a concurrent complete (or abort, or expiry) finished while this one waited
		return s.replayComplete(ctx, dc, in.UploadID, errUploadNotFound())
	}
	if apiErr != nil {
		return nil, apiErr
	}

	jobID := ids.NewJob()
	noteJob(ctx, jobID)
	err = s.ingestSession(ctx, dc, sess, jobID)
	var rej *rejection
	switch {
	case err == nil:
		s.removeSession(sess, &completedUpload{domainKey: dc.Key, jobID: jobID, at: s.d.Now()})
		return completedOutput(dc, jobID), nil
	case errors.As(err, &rej):
		s.countRejection(ctx, dc, rej.e, rej.attrs...)
		s.removeSession(sess, &completedUpload{domainKey: dc.Key, err: rej.e, at: s.d.Now()})
		return nil, rej.e
	default:
		// Storage, database or disk trouble: keep the chunks so the client can retry.
		sess.stopCompleting(s.d.Now())
		op := "ingest"
		var ie *ingestError
		if errors.As(err, &ie) {
			op, err = ie.op, ie.err
		}
		s.d.Logger.ErrorContext(ctx, "httpapi: complete upload: "+op, "domain", dc.Key, "upload_id", sess.id, "err", err)
		return nil, errInternal()
	}
}

// replayComplete answers a complete of a session that is gone: with the outcome of
// its completion when there was one, else with notFound.
func (s *Server) replayComplete(ctx context.Context, dc *domain.Config, id string, notFound *APIError) (*CompleteUploadOutput, error) {
	c, ok := s.completedOutcome(dc, id)
	switch {
	case !ok:
		return nil, notFound
	case c.err != nil:
		return nil, c.err
	}
	noteJob(ctx, c.jobID)
	return completedOutput(dc, c.jobID), nil
}

func completedOutput(dc *domain.Config, jobID string) *CompleteUploadOutput {
	return &CompleteUploadOutput{Location: jobPath(dc.Key, jobID), Body: CreateJobResponse{JobID: jobID, Status: JobQueued}}
}

// ingestSession sniffs and hashes the assembled file, then runs the ingest of POST
// /jobs on it. It is detached from the request (see completeTimeout).
func (s *Server) ingestSession(ctx context.Context, dc *domain.Config, sess *uploadSession, jobID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
	defer cancel()
	format, sum, n, err := sniffAndHash(sess.dataPath())
	switch {
	case errors.Is(err, errNotAVideo):
		return &rejection{e: errUnsupportedFormat(), attrs: []any{"declared_type", sess.contentType}}
	case err != nil:
		return &ingestError{op: "read upload", err: err}
	case n != sess.size:
		return &ingestError{op: "read upload", err: fmt.Errorf("session file has %d bytes, want %d", n, sess.size)}
	}
	return s.ingest(ctx, dc, ingestInput{
		jobID: jobID, path: sess.dataPath(), format: format, sha256: sum, size: n,
		fileName: sess.fileName, ipHash: sess.ipHash, via: "chunked",
	})
}

var errNotAVideo = errors.New("httpapi: not a supported video container")

// sniffAndHash detects the container from the first media.SniffBytes of path (like
// POST /jobs does on the stream) and, when it is a video, hashes and counts the file.
func sniffAndHash(path string) (media.Format, string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return media.Format{}, "", 0, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, media.SniffBytes)
	m, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return media.Format{}, "", 0, fmt.Errorf("read: %w", err)
	}
	format, ok := media.Sniff(head[:m])
	if !ok {
		return media.Format{}, "", 0, errNotAVideo
	}
	h := sha256.New()
	h.Write(head[:m])
	rest, err := io.Copy(h, f)
	if err != nil {
		return media.Format{}, "", 0, fmt.Errorf("hash: %w", err)
	}
	return format, hex.EncodeToString(h.Sum(nil)), int64(m) + rest, nil
}

// abortUpload is DELETE /v1/domains/{domainKey}/uploads/{uploadId}.
func (s *Server) abortUpload(ctx context.Context, in *UploadPathInput) (*struct{}, error) {
	dc, err := s.domainFor(ctx, in.DomainKey)
	if err != nil {
		return nil, s.apiError(ctx, "abort upload", err)
	}
	sess, apiErr := s.lookupSession(dc, in.UploadID)
	if apiErr != nil {
		return nil, apiErr
	}
	sess.completeMu.Lock() // never pull the file from under a running complete
	defer sess.completeMu.Unlock()
	if !s.removeSession(sess, nil) {
		return nil, errUploadNotFound()
	}
	s.d.Logger.InfoContext(ctx, "httpapi: upload session aborted", "domain", dc.Key, "upload_id", sess.id)
	return nil, nil
}
