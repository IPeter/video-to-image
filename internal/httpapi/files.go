package httpapi

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"video-to-image/internal/ids"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

const (
	// presignTTL is how long a redirect target stays valid.
	presignTTL = 15 * time.Minute
	// fileCacheControl: image and ZIP bytes never change for a given ID.
	fileCacheControl = "private, max-age=86400, immutable"
)

func errImageNotFound() *APIError {
	return NewAPIError(http.StatusNotFound, CodeImageNotFound, "image not found")
}

// getImage serves the full-resolution JPG.
func (s *Server) getImage(w http.ResponseWriter, r *http.Request) { s.serveImage(w, r, false) }

// getThumbnail serves the 640 px JPG.
func (s *Server) getThumbnail(w http.ResponseWriter, r *http.Request) { s.serveImage(w, r, true) }

func (s *Server) serveImage(w http.ResponseWriter, r *http.Request, thumb bool) {
	ctx := r.Context()
	_, job, err := s.loadJob(ctx, chi.URLParam(r, "domainKey"), chi.URLParam(r, "jobId"))
	if err != nil {
		s.writeErr(w, r, "get image", err)
		return
	}
	if job.Status == store.StatusExpired {
		writeAPIError(w, errExpired())
		return
	}
	imageID := chi.URLParam(r, "imageId")
	if !ids.Valid(ids.PrefixImage, imageID) {
		writeAPIError(w, errImageNotFound())
		return
	}
	img, err := s.d.Store.GetSelectedImage(ctx, job.ID, imageID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, errImageNotFound())
		return
	}
	if err != nil {
		s.writeErr(w, r, "get image", fmt.Errorf("get selected image %s: %w", imageID, err))
		return
	}

	key, name, etag := img.ImagePath, img.FileName, imageID
	if name == "" {
		name = imageID + ".jpg"
	}
	if thumb {
		key, name, etag = img.ThumbPath, "thumb_"+name, imageID+"-thumb"
	}
	if key == "" {
		writeAPIError(w, errExpired())
		return
	}
	s.serveObject(w, r, object{
		key:         key,
		contentType: "image/jpeg",
		etag:        `"` + etag + `"`,
		fileName:    name,
		attachment:  r.URL.Query().Get("download") == "1",
	})
}

// downloadZip serves all images + manifest.json.
func (s *Server) downloadZip(w http.ResponseWriter, r *http.Request) {
	dc, job, err := s.loadJob(r.Context(), chi.URLParam(r, "domainKey"), chi.URLParam(r, "jobId"))
	if err != nil {
		s.writeErr(w, r, "download zip", err)
		return
	}
	switch {
	case job.Status == store.StatusExpired:
		writeAPIError(w, errExpired())
		return
	case job.Status != store.StatusCompleted:
		writeAPIError(w, NewAPIError(http.StatusConflict, CodeNotReady, "the job has not completed yet"))
		return
	case job.ZipPath == "":
		writeAPIError(w, NewAPIError(http.StatusNotFound, CodeNotFound, "no images to download"))
		return
	}
	s.serveObject(w, r, object{
		key:         job.ZipPath,
		contentType: "application/zip",
		etag:        `"` + job.ID + `-zip"`,
		fileName:    export.ZipName(dc.Key, job.ID),
		attachment:  true,
	})
}

// object is a stored file served by serveObject.
type object struct {
	key         string
	contentType string
	etag        string // quoted strong ETag; the bytes behind an ID never change
	fileName    string
	attachment  bool // Content-Disposition: attachment instead of inline
}

// serveObject answers a conditional request with 304, otherwise redirects to a
// presigned URL (Deps.PresignDownloads) or streams the object through the API.
func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, o object) {
	ctx := r.Context()
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")

	if etagMatches(r.Header.Get("If-None-Match"), o.etag) {
		h.Set("ETag", o.etag)
		h.Set("Cache-Control", fileCacheControl)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if s.d.PresignDownloads {
		downloadName := ""
		if o.attachment {
			downloadName = o.fileName
		}
		u, err := s.d.Storage.PresignGet(ctx, o.key, presignTTL, downloadName)
		switch {
		case err == nil:
			h.Set("Cache-Control", "no-store") // the target URL expires
			http.Redirect(w, r, u, http.StatusFound)
			return
		case !errors.Is(err, storage.ErrPresignUnsupported):
			s.d.Logger.WarnContext(ctx, "httpapi: presign failed, streaming instead", "key", o.key, "err", err)
		}
	}

	info, err := s.d.Storage.Head(ctx, o.key)
	if errors.Is(err, storage.ErrNotFound) {
		writeAPIError(w, errExpired())
		return
	}
	if err != nil {
		s.writeErr(w, r, "serve file", fmt.Errorf("head %s: %w", o.key, err))
		return
	}
	rc, err := s.d.Storage.Get(ctx, o.key)
	if errors.Is(err, storage.ErrNotFound) {
		writeAPIError(w, errExpired())
		return
	}
	if err != nil {
		s.writeErr(w, r, "serve file", fmt.Errorf("get %s: %w", o.key, err))
		return
	}
	defer func() { _ = rc.Close() }()

	disposition := "inline"
	if o.attachment {
		disposition = "attachment"
	}
	h.Set("Content-Type", o.contentType)
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	h.Set("Content-Disposition", contentDisposition(disposition, o.fileName))
	h.Set("Cache-Control", fileCacheControl)
	h.Set("ETag", o.etag)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if n, err := io.Copy(w, rc); err != nil {
		// Headers are gone; the short body (vs Content-Length) tells the client.
		s.d.Logger.WarnContext(ctx, "httpapi: streaming file failed", "key", o.key, "written", n, "err", err)
	}
}

// etagMatches implements If-None-Match's weak comparison against a strong ETag.
func etagMatches(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	for part := range strings.SplitSeq(header, ",") {
		p := strings.TrimSpace(part)
		if p == "*" || strings.TrimPrefix(p, "W/") == etag {
			return true
		}
	}
	return false
}

// contentDisposition builds an RFC 6266 value: an ASCII filename parameter for old
// clients plus an RFC 5987 filename* parameter (UTF-8, percent-encoded) with the exact
// name. Quotes, backslashes, control characters and path separators never reach the
// header unencoded.
func contentDisposition(disposition, name string) string {
	if name == "" {
		return disposition
	}
	v := mime.FormatMediaType(disposition, map[string]string{
		"filename":  asciiFileName(name),
		"filename*": "UTF-8''" + rfc5987Encode(name),
	})
	if v == "" {
		return disposition
	}
	return v
}

// asciiFileName replaces everything outside printable ASCII, and the characters that
// would need escaping or could act as path separators, with "_".
func asciiFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r >= 0x7f || r == '"' || r == '\\' || r == '/' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// rfc5987Encode percent-encodes every byte that is not an RFC 5987 attr-char.
func rfc5987Encode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isAttrChar(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

func isAttrChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", c) >= 0
}
