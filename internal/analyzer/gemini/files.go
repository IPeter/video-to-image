package gemini

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/genai"

	"video-to-image/internal/analyzer"
)

// resolveFile makes part i reference an ACTIVE file: it reuses the known handle when the
// file is still usable, otherwise uploads the local file and waits for processing.
func (k *call) resolveFile(ctx context.Context, i int) error {
	p := &k.parts[i]
	if p.uri != "" {
		return nil // resolved by an earlier attempt
	}
	if p.name != "" {
		f, err := k.c.api.getFile(ctx, p.name)
		switch {
		case err != nil && isGone(err):
			k.log.InfoContext(ctx, "gemini: file handle no longer usable, re-uploading", "file", p.name, "part", i)
			k.forget(p)
		case err != nil:
			return fmt.Errorf("get file %s: %w", p.name, err)
		case f.State == genai.FileStateActive:
			if err := k.activate(p, f); err != nil {
				return err
			}
			k.log.DebugContext(ctx, "gemini: reusing uploaded file", "file", p.name, "part", i)
			return nil
		case f.State == genai.FileStateFailed:
			k.log.WarnContext(ctx, "gemini: file processing failed, re-uploading", "file", p.name, "part", i, "reason", fileError(f))
			k.discard(ctx, p)
		default: // PROCESSING (or unspecified): an earlier upload is still being processed
			return k.waitActive(ctx, i)
		}
	}

	if p.path == "" {
		return localErrorf("part %d: file handle is not usable and the part has no FilePath to upload", i)
	}
	if p.mime == "" {
		return localErrorf("part %d: unknown MIME type", i)
	}
	start := time.Now()
	f, err := k.c.api.upload(ctx, p.path, p.mime, displayName(k.req))
	if err != nil {
		return fmt.Errorf("part %d: %w", i, err)
	}
	if f == nil || f.Name == "" {
		return transientErrorf("part %d: upload returned no file name", i)
	}
	p.name, p.ours = f.Name, true
	k.uploaded = append(k.uploaded, f.Name)
	k.log.InfoContext(ctx, "gemini: uploaded file", "file", f.Name, "part", i, "mime", p.mime,
		"state", f.State, "elapsed", time.Since(start))
	if f.State == genai.FileStateActive && f.URI != "" {
		return k.activate(p, f)
	}
	return k.waitActive(ctx, i)
}

// waitActive polls part i's file until it is ACTIVE, FAILED or FileActiveTimeout passes.
func (k *call) waitActive(ctx context.Context, i int) error {
	p := &k.parts[i]
	deadline := k.c.now().Add(k.c.activeTimeout)
	for {
		if err := sleepCtx(ctx, k.c.pollInterval); err != nil {
			return err
		}
		f, err := k.c.api.getFile(ctx, p.name)
		if err != nil {
			if isGone(err) {
				name := p.name
				k.forget(p)
				return transientErrorf("file %s disappeared while processing: %w", name, err)
			}
			return fmt.Errorf("get file %s: %w", p.name, err)
		}
		switch f.State {
		case genai.FileStateActive:
			return k.activate(p, f)
		case genai.FileStateFailed:
			name := p.name
			k.discard(ctx, p)
			// A fresh upload may succeed; the retry loop re-uploads on the next attempt.
			return transientErrorf("file %s processing failed: %s", name, fileError(f))
		}
		if !k.c.now().Before(deadline) {
			return fatalErrorf("file %s not ACTIVE after %s (state %s)", p.name, k.c.activeTimeout, f.State)
		}
	}
}

// activate records an ACTIVE file on the part.
func (k *call) activate(p *partState, f *genai.File) error {
	if f.URI == "" {
		return transientErrorf("file %s is ACTIVE but has no URI", f.Name)
	}
	p.uri = f.URI
	if p.mime == "" {
		p.mime = f.MIMEType
	}
	if p.mime == "" {
		return localErrorf("file %s has no MIME type", f.Name)
	}
	return nil
}

// forget drops the part's handle without deleting anything (the file is gone).
func (k *call) forget(p *partState) {
	if p.ours {
		k.uploaded = slices.DeleteFunc(k.uploaded, func(n string) bool { return n == p.name })
	}
	p.name, p.uri, p.ours = "", "", false
}

// discard deletes a FAILED file and drops the part's handle. This includes a file the
// caller passed in: it is being replaced by a fresh upload whose handle the caller gets
// back, so nobody would release the old one.
func (k *call) discard(ctx context.Context, p *partState) {
	k.deleteQuietly(ctx, p.name)
	k.forget(p)
}

// releaseUploads deletes the files this call uploaded; used when Generate fails, since
// the caller never learns their handles. It runs even when ctx has ended.
func (k *call) releaseUploads(ctx context.Context) {
	for _, name := range k.uploaded {
		k.deleteQuietly(ctx, name)
	}
	k.uploaded = nil
}

func (k *call) deleteQuietly(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := k.c.api.deleteFile(ctx, name); err != nil && !isGone(err) {
		k.log.WarnContext(ctx, "gemini: could not delete uploaded file; the sweeper will retry", "file", name, "err", err)
		return
	}
	k.log.DebugContext(ctx, "gemini: deleted uploaded file", "file", name)
}

func fileError(f *genai.File) string {
	if f.Error != nil && f.Error.Message != "" {
		return f.Error.Message
	}
	return "no reason given"
}

// displayName is the neutral name uploads carry: "v2i-<step>-<jobID>".
func displayName(req *analyzer.Request) string {
	var b strings.Builder
	b.WriteString(DisplayNamePrefix)
	clean := func(s string) {
		for _, r := range s {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
				b.WriteRune(r)
			}
		}
	}
	step := req.Step
	if step == "" {
		step = "file"
	}
	clean(step)
	if req.JobID != "" {
		b.WriteByte('-')
		clean(req.JobID)
	}
	s := b.String()
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

// ReleaseFile deletes an uploaded file. Empty, unknown and expired handles are a no-op.
// Transient failures are retried like model calls.
func (c *Client) ReleaseFile(ctx context.Context, handle string) error {
	if handle == "" {
		return nil
	}
	attempts := c.maxRetries + 1
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, c.backoff(attempt-1, retryHint(lastErr))); err != nil {
				return err
			}
		}
		err := c.api.deleteFile(ctx, handle)
		if err == nil || isGone(err) {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if classify(err) != kindTransient {
			return fmt.Errorf("gemini: release file %s: %w", handle, err)
		}
		lastErr = err
	}
	return fmt.Errorf("gemini: release file %s: %w: %w", handle, analyzer.ErrUnavailable, lastErr)
}

// maxSweepErrors caps how many individual failures a sweep error reports.
const maxSweepErrors = 5

// SweepFiles deletes this app's uploads (display name starting with DisplayNamePrefix)
// created more than olderThan ago, and returns how many it deleted. Files of other
// applications sharing the API key are never touched. The listing is read before
// deleting so pagination is not disturbed; if listing fails midway, the stale files
// found so far are still deleted. Individual delete failures do not stop the sweep;
// all failures are reported together.
func (c *Client) SweepFiles(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, fmt.Errorf("gemini: sweep files: olderThan must be positive, got %s", olderThan)
	}
	cutoff := c.now().Add(-olderThan)
	var stale []string
	var listErr error
	for f, err := range c.api.listFiles(ctx) {
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, ctxErr
			}
			listErr = fmt.Errorf("list: %w", err)
			break
		}
		if f == nil || !strings.HasPrefix(f.DisplayName, DisplayNamePrefix) || f.CreateTime.IsZero() || !f.CreateTime.Before(cutoff) {
			continue
		}
		stale = append(stale, f.Name)
	}

	deleted, failed := 0, 0
	var errs []error
	for _, name := range stale {
		err := c.api.deleteFile(ctx, name)
		switch {
		case err == nil:
			deleted++
		case isGone(err):
			// Expired or deleted concurrently: nothing left to do.
		default:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return deleted, ctxErr
			}
			failed++
			if len(errs) < maxSweepErrors {
				errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
			}
		}
	}
	c.log.InfoContext(ctx, "gemini: swept uploaded files", "candidates", len(stale), "deleted", deleted,
		"failed", failed, "older_than", olderThan, "list_complete", listErr == nil)
	switch {
	case listErr != nil && failed > 0:
		return deleted, fmt.Errorf("gemini: sweep files: %w; %d of %d deletes failed: %w", listErr, failed, len(stale), errors.Join(errs...))
	case listErr != nil:
		return deleted, fmt.Errorf("gemini: sweep files: %w", listErr)
	case failed > 0:
		return deleted, fmt.Errorf("gemini: sweep files: %d of %d deletes failed: %w", failed, len(stale), errors.Join(errs...))
	}
	return deleted, nil
}
