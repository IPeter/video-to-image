package storage

import (
	"context"
	"io"
	"mime"
	"path"
	"strings"
)

// validKey reports whether key is a well-formed object key: non-empty, relative
// ("a/b.jpg", not "/a/b.jpg"), without "..", backslashes, NUL bytes, empty segments
// ("a//b"), "." segments or a trailing slash. Both backends apply the same rules so a key
// that works on local disk also works on S3 and vice versa.
func validKey(key string) bool {
	if key == "" || strings.HasSuffix(key, "/") {
		return false
	}
	return validPath(key)
}

// validPrefix reports whether prefix is usable with DeletePrefix. It follows the key
// rules except that it may (and usually does) end in "/".
func validPrefix(prefix string) bool {
	if prefix == "" {
		return false
	}
	return validPath(strings.TrimSuffix(prefix, "/"))
}

func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") ||
		strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." {
			return false
		}
	}
	return true
}

// knownTypes covers the extensions this service stores. They are checked before
// mime.TypeByExtension because minimal container images ship no /etc/mime.types, in
// which case the stdlib only knows a handful of web types (no .mp4, .zip, .mkv).
var knownTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".json": "application/json",
	".zip":  "application/zip",
	".mp4":  "video/mp4",
	".mov":  "video/quicktime",
	".webm": "video/webm",
	".mkv":  "video/x-matroska",
}

// ContentTypeForKey returns the media type implied by the key's extension, or
// application/octet-stream when it is unknown.
func ContentTypeForKey(key string) string {
	ext := strings.ToLower(path.Ext(key))
	if ext == "" {
		return "application/octet-stream"
	}
	if ct, ok := knownTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// AttachmentDisposition returns a Content-Disposition value that makes browsers save
// the response as name: `attachment; filename="<name>"`. Path separators become "_".
// Characters that cannot appear safely in the quoted string (double quotes, control
// characters, non-ASCII runes) are replaced by "_" there, and an RFC 8187 filename*
// parameter then carries the exact UTF-8 name as well. An empty name yields plain
// "attachment". Used for presigned S3 URLs and by the API when it streams a download.
func AttachmentDisposition(name string) string {
	if name == "" {
		return "attachment"
	}
	// Path separators never belong in a saved file name: replace them everywhere,
	// including the exact filename* form.
	name = strings.NewReplacer("/", "_", "\\", "_").Replace(name)
	var b strings.Builder
	ascii := true
	for _, r := range name {
		if r == '"' || r < 0x20 || r >= 0x7f {
			b.WriteByte('_')
			ascii = false
			continue
		}
		b.WriteRune(r)
	}
	v := `attachment; filename="` + b.String() + `"`
	if !ascii {
		v += "; filename*=UTF-8''" + pctEncodeAttr(name)
	}
	return v
}

// pctEncodeAttr percent-encodes s per RFC 8187 attr-char.
func pctEncodeAttr(s string) string {
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

// ctxReader stops a copy loop once ctx is done, so a Put of a long local stream can be
// cancelled between reads.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
