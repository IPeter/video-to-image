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
// (never held in memory, at a minimum pace) while it is hashed and counted, validated
// with ffprobe, copied to storage, and the job row + River job are inserted in one
// transaction.
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
	maxBytes := dc.Limits.MaxUploadBytes

	// 1. Size, before reading anything; the body is capped again below for clients
	//    that lie about (or omit) Content-Length.
	if r.ContentLength > maxBytes+multipartSlack {
		reject(errTooLarge(maxBytes), "content_length", r.ContentLength)
		return
	}

	// 2. A slot, before reading anything: each upload holds up to maxBytes of scratch
	//    and a goroutine until it is done.
	release, busy := s.admitUpload(w, r, maxBytes)
	if busy != nil {
		reject(busy)
		return
	}
	defer release()
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+multipartSlack)
	defer s.paceBody(w, r)() // also bounds net/http's drain of an unread body tail

	mr, err := r.MultipartReader()
	if err != nil {
		reject(NewAPIError(http.StatusBadRequest, CodeInvalidRequest,
			"expected a multipart/form-data body with a video field"), "err", err)
		return
	}
	part, err := nextVideoPart(mr)
	if err != nil {
		reject(bodyError(err, maxBytes), "err", err)
		return
	}
	// The part is deliberately never Closed: Part.Close drains the rest of the part,
	// which would read a whole (possibly 500 MB) upload just to reject it. net/http
	// discards or cuts off whatever body is left once the handler returns.

	// 3. Magic bytes decide the container, whatever the client declared.
	br := bufio.NewReaderSize(part, uploadBufferSize)
	head, err := br.Peek(media.SniffBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		reject(bodyError(err, maxBytes), "err", err)
		return
	}
	format, ok := media.Sniff(head)
	if !ok {
		reject(NewAPIError(http.StatusBadRequest, CodeUnsupportedFormat,
			"unsupported file type: upload an MP4, MOV, WebM or MKV video"), "declared_type", part.Header.Get("Content-Type"))
		return
	}

	// 4. Stream to scratch, hashing and counting on the way.
	jobID := ids.NewJob()
	noteJob(ctx, jobID)
	up, err := s.spool(br, jobID, format.Ext, maxBytes)
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

	// 5. ffprobe: decodable, duration, resolution and frame rate within the domain's
	//    limits (media.FFmpeg also caps the decoder at maxPixels, so a decode bomb is
	//    CANNOT_DECODE before it can use much memory).
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	info, err := s.d.Prober.Probe(pctx, up.path)
	cancel()
	if errors.Is(err, media.ErrCannotDecode) {
		reject(NewAPIError(http.StatusBadRequest, CodeCannotDecode, "the video could not be decoded"), "err", err)
		return
	}
	if err != nil {
		fail("probe", err)
		return
	}
	if e := validateVideo(dc, info); e != nil {
		reject(e, "duration_sec", info.DurationSec, "width", info.Width, "height", info.Height, "fps", info.FPS)
		return
	}
	info.MIME = format.MIME
	info.SHA256 = up.sha256
	info.SizeBytes = up.size
	info.OriginalName = sanitizeFileName(part.FileName())

	// 6. Store the original, then job row + River job in one transaction.
	key := storage.OriginalKey(dc.Key, jobID, format.Ext)
	if err := s.putFile(ctx, key, up.path, format.MIME); err != nil {
		s.deleteBestEffort(ctx, key)
		fail("store original", err)
		return
	}
	nj := store.NewJob{
		ID:           jobID,
		DomainKey:    dc.Key,
		ExpiresAt:    s.d.Now().UTC().Add(time.Duration(dc.RetentionHours) * time.Hour),
		ClientIPHash: s.ipHash(r),
		VideoPath:    key,
		VideoInfo:    info,
		ModelName:    dc.Model.Name,
	}
	err = s.d.Store.CreateJob(ctx, nj, func(ctx context.Context, tx pgx.Tx) error {
		return s.d.Enqueuer.EnqueuePipelineTx(ctx, tx, jobID)
	})
	if err != nil {
		s.deleteBestEffort(ctx, key)
		fail("insert job", err)
		return
	}

	metrics.JobsCreated.WithLabelValues(dc.Key).Inc()
	s.d.Logger.InfoContext(ctx, "httpapi: job created", "domain", dc.Key, "job_id", jobID,
		"bytes", up.size, "mime", format.MIME, "duration_sec", info.DurationSec, "width", info.Width, "height", info.Height)
	w.Header().Set("Location", jobPath(dc.Key, jobID))
	writeJSON(w, http.StatusAccepted, CreateJobResponse{JobID: jobID, Status: JobQueued})
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

// bodyError maps a failure reading the request body to an envelope.
func bodyError(err error, maxBytes int64) *APIError {
	var apiErr *APIError
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &apiErr):
		return apiErr
	case errors.As(err, &mbe):
		return errTooLarge(maxBytes)
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
// remove it. Errors are *APIError for client-side problems (too large, broken body)
// and plain errors for server-side ones.
func (s *Server) spool(src io.Reader, jobID, ext string, maxBytes int64) (*spooled, error) {
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
		return up, fmt.Errorf("read upload: %w (%w)", bodyError(rr.err, maxBytes), rr.err)
	case copyErr != nil:
		return up, fmt.Errorf("write upload file: %w", copyErr)
	case closeErr != nil:
		return up, fmt.Errorf("close upload file: %w", closeErr)
	case n > maxBytes:
		return up, errTooLarge(maxBytes)
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
