// Package storage abstracts object storage: local disk for dev/tests and S3-compatible
// (MinIO on k3s, any S3 later). Keys are slash-separated and never start with "/".
// Layout (plan/06-data-model.md):
//
//	<domainKey>/jobs/<jobId>/original.<ext>
//	<domainKey>/jobs/<jobId>/preview.mp4        (deleted after analyze)
//	<domainKey>/jobs/<jobId>/images/<imgId>.jpg
//	<domainKey>/jobs/<jobId>/thumbs/<imgId>.jpg
//	<domainKey>/jobs/<jobId>/manifest.json
//	<domainKey>/jobs/<jobId>/export.zip
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNotFound is returned by Get and Head for a missing key.
	ErrNotFound = errors.New("storage: object not found")
	// ErrPresignUnsupported is returned by PresignGet when the backend cannot hand out
	// browser-usable URLs (local disk, or S3 with presigning disabled). Callers then
	// stream the object through the API instead.
	ErrPresignUnsupported = errors.New("storage: presign not supported")
	// ErrInvalidKey is returned for empty keys, absolute keys or keys containing "..".
	ErrInvalidKey = errors.New("storage: invalid key")
)

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	ContentType  string
	LastModified time.Time
}

// Storage is implemented by Local and S3. All methods are safe for concurrent use.
type Storage interface {
	// Put stores r under key, replacing any existing object. r may be a non-seekable
	// stream of unknown length (e.g. the read side of an io.Pipe for ZIP streaming).
	Put(ctx context.Context, key string, r io.Reader, contentType string) error
	// Get opens the object. The caller must Close the reader. Missing → ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// PresignGet returns a time-limited URL a browser can GET directly. When
	// downloadName is not empty the response carries
	// Content-Disposition: attachment; filename="<downloadName>".
	PresignGet(ctx context.Context, key string, ttl time.Duration, downloadName string) (string, error)
	// Delete removes one object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes every object whose key starts with prefix. prefix must be
	// non-empty and should end with "/". Deleting nothing is not an error.
	DeletePrefix(ctx context.Context, prefix string) error
	// Head returns object metadata. Missing → ErrNotFound.
	Head(ctx context.Context, key string) (*ObjectInfo, error)
	// Ping checks the backend is reachable (bucket exists / directory writable). Used by /healthz.
	Ping(ctx context.Context) error
}

// JobPrefix is the key prefix of everything belonging to one job, ending in "/".
func JobPrefix(domainKey, jobID string) string { return domainKey + "/jobs/" + jobID + "/" }

// OriginalKey is the uploaded video. ext includes no dot ("mp4", "mov", "webm", "mkv").
func OriginalKey(domainKey, jobID, ext string) string {
	return JobPrefix(domainKey, jobID) + "original." + ext
}

func PreviewKey(domainKey, jobID string) string { return JobPrefix(domainKey, jobID) + "preview.mp4" }

func ImageKey(domainKey, jobID, imageID string) string {
	return JobPrefix(domainKey, jobID) + "images/" + imageID + ".jpg"
}

func ThumbKey(domainKey, jobID, imageID string) string {
	return JobPrefix(domainKey, jobID) + "thumbs/" + imageID + ".jpg"
}

func ManifestKey(domainKey, jobID string) string {
	return JobPrefix(domainKey, jobID) + "manifest.json"
}

func ZipKey(domainKey, jobID string) string { return JobPrefix(domainKey, jobID) + "export.zip" }
