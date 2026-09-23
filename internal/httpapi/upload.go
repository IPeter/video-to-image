package httpapi

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/metrics"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

const (
	// multipartSlack is what the body may carry on top of maxUploadBytes: multipart
	// framing and small extra form fields.
	multipartSlack = 1 << 20
	// uploadBufferSize is the read buffer between the multipart part and the temp file.
	uploadBufferSize = 64 << 10
	// probeTimeout bounds ffprobe on the uploaded file.
	probeTimeout = time.Minute
	// cleanupTimeout bounds best-effort deletes after a failed request.
	cleanupTimeout = 30 * time.Second
	// maxOriginalNameRunes caps the stored client file name.
	maxOriginalNameRunes = 200
	videoField           = "video"
)

// createJob is POST /v1/domains/{domainKey}/jobs (plan/03-pipeline.md step 0). The
// upload first needs a slot (admitUpload: global and per-client concurrency, scratch
// free space); then the file is streamed from the multipart body to a scratch file
// (never held in memory, at a minimum pace) while it is hashed and counted, and
// ingest validates it with ffprobe, copies it to storage and inserts the job row +
// River job in one transaction. Videos larger than one request may carry
// (Deps.MaxRequestBytes) take the chunked upload instead (uploads_chunked.go), which
// ends in the same ingest.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dc, err := s.domainFor(ctx, chi.URLParam(r, "domainKey"))
	if err != nil {
		s.writeErr(w, r, "create job", err)
		return
	}
	reject := func(e *APIError, attrs ...any) {
		s.countRejection(ctx, dc, e, attrs...)
		writeAPIError(w, e)
	}
	fail := func(op string, err error) {
		if ctx.Err() != nil {
			// The client went away; nobody reads this response.
			s.d.Logger.InfoContext(ctx, "httpapi: create job: request cancelled", "op", op, "domain", dc.Key, "err", err)
		} else {
			s.d.Logger.ErrorContext(ctx, "httpapi: create job: "+op, "domain", dc.Key, "err", err)
		}
		writeAPIError(w, errInternal())
	}
	lim := s.requestLimit(dc)

	// 1. Size, before reading anything; the body is capped again below for clients
	//    that lie about (or omit) Content-Length.
	if r.ContentLength > dc.Limits.MaxUploadBytes+multipartSlack {
		reject(errTooLarge(dc.Limits.MaxUploadBytes), "content_length", r.ContentLength)
		return
	}
	if r.ContentLength > lim.body {
		reject(lim.tooLarge(), "content_length", r.ContentLength)
		return
	}

	// 2. A slot, before reading anything: each upload holds up to lim.file of scratch
	//    and a goroutine until it is done.
	release, busy := s.admitUpload(w, r, lim.file)
	if busy != nil {
		reject(busy)
		return
	}
	defer release()
	r.Body = http.MaxBytesReader(w, r.Body, lim.body)
	defer s.paceBody(w, r)() // also bounds net/http's drain of an unread body tail

	mr, err := r.MultipartReader()
	if err != nil {
		reject(NewAPIError(http.StatusBadRequest, CodeInvalidRequest,
			"expected a multipart/form-data body with a video field"), "err", err)
		return
	}
	part, err := nextVideoPart(mr)
	if err != nil {
		reject(bodyError(err, lim.tooLarge), "err", err)
		return
	}
	// The part is deliberately never Closed: Part.Close drains the rest of the part,
	// which would read a whole (possibly 500 MB) upload just to reject it. net/http
	// discards or cuts off whatever body is left once the handler returns.

	// 3. Magic bytes decide the container, whatever the client declared.
	br := bufio.NewReaderSize(part, uploadBufferSize)
	head, err := br.Peek(media.SniffBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		reject(bodyError(err, lim.tooLarge), "err", err)
		return
	}
	format, ok := media.Sniff(head)
	if !ok {
		reject(errUnsupportedFormat(), "declared_type", part.Header.Get("Content-Type"))
		return
	}

	// 4. Stream to scratch, hashing and counting on the way.
	jobID := ids.NewJob()
	noteJob(ctx, jobID)
	up, err := s.spool(br, jobID, format.Ext, lim.file, lim.tooLarge)
	if up != nil {
		defer up.remove(s.d.Logger)
	}
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			reject(apiErr, "err", err)
			return
		}
		fail("spool upload", err)
		return
	}

	// 5.-6. Probe, validate, store, create.
	err = s.ingest(ctx, dc, ingestInput{
		jobID: jobID, path: up.path, format: format, sha256: up.sha256, size: up.size,
		fileName: sanitizeFileName(part.FileName()), ipHash: s.ipHash(r), via: "request",
	})
	var rej *rejection
	var ie *ingestError
	switch {
	case errors.As(err, &rej):
		reject(rej.e, rej.attrs...)
		return
	case errors.As(err, &ie):
		fail(ie.op, ie.err)
		return
	case err != nil:
		fail("ingest", err)
		return
	}
	w.Header().Set("Location", jobPath(dc.Key, jobID))
	writeJSON(w, http.StatusAccepted, CreateJobResponse{JobID: jobID, Status: JobQueued})
}

// requestLimits bounds one POST /jobs: the video file and the whole multipart body.
type requestLimits struct {
	file, body int64
	tooLarge   func() *APIError // the 413 for a body or file over the limits
}

// requestLimit is the domain's upload limit, lowered to Deps.MaxRequestBytes: the
// ingress refuses larger bodies anyway, and those videos take the chunked upload.
func (s *Server) requestLimit(dc *domain.Config) requestLimits {
	maxUpload := dc.Limits.MaxUploadBytes
	l := requestLimits{file: maxUpload, body: maxUpload + multipartSlack,
		tooLarge: func() *APIError { return errTooLarge(maxUpload) }}
	if m := s.d.MaxRequestBytes; m > 0 && m < l.body {
		l.body, l.file = m, min(maxUpload, m)
		l.tooLarge = func() *APIError { return errRequestTooLarge(dc, m) }
	}
	return l
}

// errRequestTooLarge: the video may be fine, but not in one request.
func errRequestTooLarge(dc *domain.Config, maxRequest int64) *APIError {
	return NewAPIError(http.StatusRequestEntityTooLarge, CodeUploadTooLarge,
		fmt.Sprintf("the upload is larger than the %s one request may carry: send it as a chunked upload (POST %s%s/uploads)",
			formatBytes(maxRequest), domainsPrefix, dc.Key)).
		WithDetails(map[string]any{"maxUploadBytes": dc.Limits.MaxUploadBytes, "maxRequestBytes": maxRequest})
}

func errUnsupportedFormat() *APIError {
	return NewAPIError(http.StatusBadRequest, CodeUnsupportedFormat,
		"unsupported file type: upload an MP4, MOV, WebM or MKV video")
}

// ingestInput is a complete upload on scratch: sniffed, hashed and counted.
type ingestInput struct {
	jobID    string
	path     string
	format   media.Format
	sha256   string
	size     int64
	fileName string // sanitised
	ipHash   string
	via      string // "request" or "chunked", for the log
}

// rejection is a client-side problem with an upload: the envelope to answer with and
// the attributes to log it with.
type rejection struct {
	e     *APIError
	attrs []any
}

func (r *rejection) Error() string { return r.e.Error() }

// ingestError is a server-side failure of step op.
type ingestError struct {
	op  string
	err error
}

func (e *ingestError) Error() string { return e.op + ": " + e.err.Error() }
func (e *ingestError) Unwrap() error { return e.err }

// ingest turns a complete upload on scratch into a job — the part both upload paths
// share. ffprobe checks it is decodable, and its duration, resolution and frame rate
// against the domain's limits (media.FFmpeg also caps the decoder at maxPixels, so a
// decode bomb is CANNOT_DECODE before it can use much memory); then the original is
// stored and the job row + River job are inserted in one transaction. It returns a
// *rejection for a video the domain does not accept and an *ingestError for a
// server-side failure (anything already stored is deleted again).
func (s *Server) ingest(ctx context.Context, dc *domain.Config, in ingestInput) error {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	info, err := s.d.Prober.Probe(pctx, in.path)
	cancel()
	if errors.Is(err, media.ErrCannotDecode) {
		return &rejection{e: NewAPIError(http.StatusBadRequest, CodeCannotDecode, "the video could not be decoded"), attrs: []any{"err", err}}
	}
	if err != nil {
		return &ingestError{op: "probe", err: err}
	}
	if e := validateVideo(dc, info); e != nil {
		return &rejection{e: e, attrs: []any{"duration_sec", info.DurationSec, "width", info.Width, "height", info.Height, "fps", info.FPS}}
	}
	info.MIME = in.format.MIME
	info.SHA256 = in.sha256
	info.SizeBytes = in.size
	info.OriginalName = in.fileName

	key := storage.OriginalKey(dc.Key, in.jobID, in.format.Ext)
	if err := s.putFile(ctx, key, in.path, in.format.MIME); err != nil {
		s.deleteBestEffort(ctx, key)
		return &ingestError{op: "store original", err: err}
	}
	nj := store.NewJob{
		ID:           in.jobID,
		DomainKey:    dc.Key,
		ExpiresAt:    s.d.Now().UTC().Add(time.Duration(dc.RetentionHours) * time.Hour),
		ClientIPHash: in.ipHash,
		VideoPath:    key,
		VideoInfo:    info,
		ModelName:    dc.Model.Name,
	}
	err = s.d.Store.CreateJob(ctx, nj, func(ctx context.Context, tx pgx.Tx) error {
		return s.d.Enqueuer.EnqueuePipelineTx(ctx, tx, in.jobID)
	})
	if err != nil {
		s.deleteBestEffort(ctx, key)
		return &ingestError{op: "insert job", err: err}
	}

	metrics.JobsCreated.WithLabelValues(dc.Key).Inc()
	s.d.Logger.InfoContext(ctx, "httpapi: job created", "domain", dc.Key, "job_id", in.jobID, "upload", in.via,
		"bytes", in.size, "mime", in.format.MIME, "duration_sec", info.DurationSec, "width", info.Width, "height", info.Height)
	return nil
}

// nextVideoPart returns the first part named "video", draining any other part
// (the body is size-capped, so draining is bounded).
func nextVideoPart(mr *multipart.Reader) (*multipart.Part, error) {
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, NewAPIError(http.StatusBadRequest, CodeInvalidRequest, "missing video field")
		}
		if err != nil {
			return nil, err
		}
		if p.FormName() == videoField {
			return p, nil
		}
		_, err = io.Copy(io.Discard, p)
		_ = p.Close()
		if err != nil {
			return nil, err
		}
	}
}

// bodyError maps a failure reading the request body to an envelope (tooLarge: the
// 413 for a body over its cap).
func bodyError(err error, tooLarge func() *APIError) *APIError {
	var apiErr *APIError
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &apiErr):
		return apiErr
	case errors.As(err, &mbe):
		return tooLarge()
	case errors.Is(err, errUploadTooSlow):
		// The connection read side is dead by now; the client may still read this.
		return NewAPIError(http.StatusBadRequest, CodeInvalidRequest, "the upload was too slow and was cut off")
	default:
		return NewAPIError(http.StatusBadRequest, CodeInvalidRequest, "malformed or interrupted multipart body")
	}
}

func errTooLarge(maxBytes int64) *APIError {
	return NewAPIError(http.StatusRequestEntityTooLarge, CodeUploadTooLarge,
		fmt.Sprintf("the video is larger than the %s limit", formatBytes(maxBytes))).
		WithDetails(map[string]any{"maxUploadBytes": maxBytes})
}

// validateVideo applies the domain's duration, resolution and frame-rate limits.
// The upper bounds stop decode bombs: a flat-colour 16384x12288 clip is under 1 MB
// but needs gigabytes of RAM in every later ffmpeg step.
func validateVideo(dc *domain.Config, v media.VideoInfo) *APIError {
	l := dc.Limits
	maxPixels, maxFPS := l.EffectiveMaxPixels(), l.EffectiveMaxFPS()
	switch {
	case v.DurationSec > float64(l.MaxDurationSec):
		return NewAPIError(http.StatusBadRequest, CodeTooLong,
			fmt.Sprintf("the video is longer than %d seconds", l.MaxDurationSec)).
			WithDetails(map[string]any{"durationSec": v.DurationSec, "maxDurationSec": l.MaxDurationSec})
	case v.DurationSec < float64(l.MinDurationSec):
		return NewAPIError(http.StatusBadRequest, CodeTooShort,
			fmt.Sprintf("the video is shorter than %d seconds", l.MinDurationSec)).
			WithDetails(map[string]any{"durationSec": v.DurationSec, "minDurationSec": l.MinDurationSec})
	case v.ShortSide() < l.MinShortSidePx:
		return NewAPIError(http.StatusBadRequest, CodeResolutionTooLow,
			fmt.Sprintf("the video resolution is too low: the short side must be at least %d px", l.MinShortSidePx)).
			WithDetails(map[string]any{"width": v.Width, "height": v.Height, "minShortSidePx": l.MinShortSidePx})
	case int64(v.Width)*int64(v.Height) > maxPixels || v.FPS > maxFPS:
		return NewAPIError(http.StatusBadRequest, CodeResolutionTooHigh,
			fmt.Sprintf("the video resolution or frame rate is too high: at most %s pixels per frame and %s fps",
				strconv.FormatInt(maxPixels, 10), strconv.FormatFloat(maxFPS, 'f', -1, 64))).
			WithDetails(map[string]any{"width": v.Width, "height": v.Height, "fps": v.FPS, "maxPixels": maxPixels, "maxFps": maxFPS})
	}
	return nil
}

// spooled is an upload written to scratch.
type spooled struct {
	path   string
	size   int64
	sha256 string
}

func (u *spooled) remove(log *slog.Logger) {
	if err := os.Remove(u.path); err != nil && !errors.Is(err, os.ErrNotExist) { //nolint:gosec // G703: u.path is built by the server (scratch dir + job ID), never from client input
		log.Warn("httpapi: remove upload temp file", "path", u.path, "err", err)
	}
}

// spool copies at most maxBytes from src to ScratchDir/uploads/<jobID>.<ext>. It
// returns a non-nil *spooled whenever the file was created, so the caller can always
// remove it. Errors are *APIError for client-side problems (too large: tooLarge(),
// broken body) and plain errors for server-side ones.
func (s *Server) spool(src io.Reader, jobID, ext string, maxBytes int64, tooLarge func() *APIError) (*spooled, error) {
	dir := s.uploadDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create upload dir: %w", err)
	}
	up := &spooled{path: filepath.Join(dir, jobID+"."+ext)}
	f, err := os.OpenFile(up.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create upload file: %w", err)
	}
	h := sha256.New()
	rr := &readErrRecorder{r: io.LimitReader(src, maxBytes+1)}
	n, copyErr := io.Copy(io.MultiWriter(f, h), rr)
	closeErr := f.Close()
	up.size = n
	switch {
	case rr.err != nil:
		// Keep the cause for the log; errors.As still finds the envelope.
		return up, fmt.Errorf("read upload: %w (%w)", bodyError(rr.err, tooLarge), rr.err)
	case copyErr != nil:
		return up, fmt.Errorf("write upload file: %w", copyErr)
	case closeErr != nil:
		return up, fmt.Errorf("close upload file: %w", closeErr)
	case n > maxBytes:
		return up, tooLarge()
	}
	up.sha256 = hex.EncodeToString(h.Sum(nil))
	return up, nil
}

// readErrRecorder remembers the first non-EOF read error, telling client-side
// (body) failures apart from server-side (disk) ones after io.Copy.
type readErrRecorder struct {
	r   io.Reader
	err error
}

func (e *readErrRecorder) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && e.err == nil {
		e.err = err
	}
	return n, err
}

func (s *Server) scratchDir() string {
	if s.d.ScratchDir != "" {
		return s.d.ScratchDir
	}
	return filepath.Join(os.TempDir(), "v2i")
}

// uploadDir is where uploads are spooled.
func (s *Server) uploadDir() string { return filepath.Join(s.scratchDir(), "uploads") }

// putFile uploads a local file to storage.
func (s *Server) putFile(ctx context.Context, key, path, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := s.d.Storage.Put(ctx, key, f, contentType); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// deleteBestEffort removes an object after a failed request, even when the request
// context is already cancelled.
func (s *Server) deleteBestEffort(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := s.d.Storage.Delete(ctx, key); err != nil {
		s.d.Logger.WarnContext(ctx, "httpapi: cleanup of stored original failed", "key", key, "err", err)
	}
}

// sanitizeFileName keeps the base name of a client file name, without control
// characters or invalid UTF-8, capped at maxOriginalNameRunes.
func sanitizeFileName(name string) string {
	name = strings.ToValidUTF8(name, "")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "." || name == ".." {
		return ""
	}
	if utf8.RuneCountInString(name) > maxOriginalNameRunes {
		name = string([]rune(name)[:maxOriginalNameRunes])
	}
	return name
}

// formatBytes renders a byte limit for messages ("500 MB", "1.5 MB", "900 bytes").
func formatBytes(n int64) string {
	const mb = 1 << 20
	switch {
	case n >= mb && n%mb == 0:
		return fmt.Sprintf("%d MB", n/mb)
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// writeJSON writes a JSON body on a raw route.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
