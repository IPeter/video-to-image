package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// tempPattern names in-flight Put files. They live next to the final file so the
// closing rename is atomic (same directory, same file system).
const tempPattern = ".v2i-tmp-*"

// Local stores objects as files under a root directory, mirroring the key layout
// (key "cars/jobs/job_x/manifest.json" → <root>/cars/jobs/job_x/manifest.json). It is
// meant for development, tests and single-node deployments. Content types are derived
// from the key's extension; the value passed to Put is not persisted.
//
// A key cannot be both an object and a "directory" of other objects on local disk
// ("a/b" and "a/b/c"); the service's key layout never needs that.
type Local struct {
	root string // absolute, cleaned
}

var _ Storage = (*Local)(nil)

// NewLocal returns a Local rooted at root, creating the directory when needed.
func NewLocal(root string) (*Local, error) {
	if root == "" {
		return nil, errors.New("storage: local: root directory is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage: local: resolve root %q: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("storage: local: create root: %w", err)
	}
	return &Local{root: abs}, nil
}

// Root returns the absolute root directory.
func (l *Local) Root() string { return l.root }

// path maps a validated key to its file path.
func (l *Local) path(key string) (string, error) {
	if !validKey(key) {
		return "", ErrInvalidKey
	}
	return l.join(key)
}

func (l *Local) join(rel string) (string, error) {
	rel = filepath.FromSlash(rel)
	if !filepath.IsLocal(rel) { // belt and braces on top of validPath
		return "", ErrInvalidKey
	}
	return filepath.Join(l.root, rel), nil
}

// Put writes r to a temp file in the target directory and renames it into place, so
// readers never see a partial object and a failed Put leaves any previous version
// intact.
func (l *Local) Put(ctx context.Context, key string, r io.Reader, _ string) error {
	p, err := l.path(key)
	if err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	if r == nil {
		return fmt.Errorf("storage: put %q: nil reader", key)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	tmp, err := l.createTemp(filepath.Dir(p))
	if err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	tmpName := tmp.Name()
	_, err = io.Copy(tmp, ctxReader{ctx: ctx, r: r})
	if err == nil {
		err = tmp.Chmod(0o644) // CreateTemp uses 0600
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpName, p)
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	return nil
}

// createTemp creates the temp file, recreating dir when a concurrent DeletePrefix
// pruned it between MkdirAll and CreateTemp.
func (l *Local) createTemp(dir string) (*os.File, error) {
	for attempt := 0; ; attempt++ {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		f, err := os.CreateTemp(dir, tempPattern)
		if err == nil || !errors.Is(err, fs.ErrNotExist) || attempt == 2 {
			return f, err
		}
	}
}

// Get opens the file. The returned reader is an *os.File.
func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	f, err := os.Open(p)
	if err != nil {
		if isNotExist(err) {
			return nil, fmt.Errorf("storage: get %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("storage: get %q: %w", key, ErrNotFound)
	}
	return f, nil
}

// PresignGet always fails with ErrPresignUnsupported (after key validation): local
// files are streamed through the API.
func (l *Local) PresignGet(_ context.Context, key string, _ time.Duration, _ string) (string, error) {
	if !validKey(key) {
		return "", fmt.Errorf("storage: presign %q: %w", key, ErrInvalidKey)
	}
	return "", ErrPresignUnsupported
}

// Delete removes one file and prunes directories it leaves empty. A missing key, or a
// key that names a directory rather than an object, is not an error.
func (l *Local) Delete(ctx context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	fi, err := os.Lstat(p)
	switch {
	case isNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("storage: delete %q: %w", key, err)
	case fi.IsDir():
		return nil
	}
	if err := os.Remove(p); err != nil && !isNotExist(err) {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	l.pruneEmpty(filepath.Dir(p))
	return nil
}

// DeletePrefix removes every object whose key starts with prefix. A prefix ending in
// "/" removes that directory tree; any other prefix removes the entries of its parent
// directory whose names start with the last segment ("a/b/img_1" removes
// a/b/img_1.jpg and a/b/img_10.jpg). Directories left empty are pruned up to, but not
// including, the root.
func (l *Local) DeletePrefix(ctx context.Context, prefix string) error {
	if !validPrefix(prefix) {
		return fmt.Errorf("storage: delete prefix %q: %w", prefix, ErrInvalidKey)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storage: delete prefix %q: %w", prefix, err)
	}
	if dirKey, ok := strings.CutSuffix(prefix, "/"); ok {
		dir, err := l.join(dirKey)
		if err != nil {
			return fmt.Errorf("storage: delete prefix %q: %w", prefix, err)
		}
		fi, err := os.Lstat(dir)
		switch {
		case isNotExist(err):
			return nil
		case err != nil:
			return fmt.Errorf("storage: delete prefix %q: %w", prefix, err)
		case !fi.IsDir(): // an object named like the prefix without its slash is not under it
			return nil
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("storage: delete prefix %q: %w", prefix, err)
		}
		l.pruneEmpty(filepath.Dir(dir))
		return nil
	}

	parentKey, base := path.Split(prefix) // parentKey is "" or ends in "/"
	parent := l.root
	if parentKey != "" {
		var err error
		if parent, err = l.join(strings.TrimSuffix(parentKey, "/")); err != nil {
			return fmt.Errorf("storage: delete prefix %q: %w", prefix, err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return fmt.Errorf("storage: delete prefix %q: %w", prefix, err)
	}
	// base has no "/", so a key below entry e starts with prefix iff e's name starts
	// with base: whole matching entries go, nothing else needs walking.
	var errs []error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), base) {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := os.RemoveAll(filepath.Join(parent, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	l.pruneEmpty(parent)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("storage: delete prefix %q: %w", prefix, err)
	}
	return nil
}

// pruneEmpty removes dir and its ancestors while they are empty, stopping at the root.
func (l *Local) pruneEmpty(dir string) {
	for dir != l.root && strings.HasPrefix(dir, l.root+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil && !isNotExist(err) {
			return // not empty (or not ours to remove)
		}
		dir = filepath.Dir(dir)
	}
}

// Head stats the file. ContentType comes from the key's extension.
func (l *Local) Head(ctx context.Context, key string) (*ObjectInfo, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, fmt.Errorf("storage: head %q: %w", key, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("storage: head %q: %w", key, err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		if isNotExist(err) {
			return nil, fmt.Errorf("storage: head %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("storage: head %q: %w", key, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("storage: head %q: %w", key, ErrNotFound)
	}
	return &ObjectInfo{
		Key:          key,
		Size:         fi.Size(),
		ContentType:  ContentTypeForKey(key),
		LastModified: fi.ModTime().UTC(),
	}, nil
}

// Ping checks the root exists and is writable.
func (l *Local) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storage: ping: %w", err)
	}
	if err := os.MkdirAll(l.root, 0o755); err != nil {
		return fmt.Errorf("storage: ping: %w", err)
	}
	f, err := os.CreateTemp(l.root, ".v2i-ping-*")
	if err != nil {
		return fmt.Errorf("storage: ping: %w", err)
	}
	name := f.Name()
	_, err = f.WriteString("ok")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if rerr := os.Remove(name); err == nil {
		err = rerr
	}
	if err != nil {
		return fmt.Errorf("storage: ping: %w", err)
	}
	return nil
}

// isNotExist also treats ENOTDIR as missing: "a/b.jpg/c" cannot exist when a/b.jpg is
// a file.
func isNotExist(err error) bool {
	return err != nil && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR))
}
