package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestLocal(t *testing.T) *Local {
	t.Helper()
	st, err := NewLocal(t.TempDir())
	require.NoError(t, err)
	return st
}

func TestLocalConformance(t *testing.T) {
	runConformance(t, newTestLocal(t))
}

func TestNewLocal(t *testing.T) {
	_, err := NewLocal("")
	require.Error(t, err)

	root := filepath.Join(t.TempDir(), "not", "yet", "there")
	st, err := NewLocal(root)
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(st.Root()))
	fi, err := os.Stat(root)
	require.NoError(t, err)
	assert.True(t, fi.IsDir())

	// A relative root is resolved once, so a later chdir cannot move the store.
	wd := t.TempDir()
	t.Chdir(wd)
	st, err = NewLocal("data/storage")
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(filepath.Join(wd, "data", "storage"))
	require.NoError(t, err)
	got, err := filepath.EvalSymlinks(st.Root())
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestLocalLayout(t *testing.T) {
	st := newTestLocal(t)
	key := ImageKey("cars", "job_1", "img_1")
	putBytes(t, st, key, []byte("jpeg"), "image/jpeg")

	p := filepath.Join(st.Root(), "cars", "jobs", "job_1", "images", "img_1.jpg")
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "jpeg", string(data))
	fi, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())

	rc, err := st.Get(t.Context(), key)
	require.NoError(t, err)
	defer rc.Close()
	_, isFile := rc.(*os.File)
	assert.True(t, isFile, "Get returns the *os.File so the API can use sendfile / ServeContent")
}

func TestLocalFailedPutLeavesNoTempFiles(t *testing.T) {
	st := newTestLocal(t)
	key := "cars/jobs/job_1/export.zip"
	putBytes(t, st, key, []byte("v1"), "application/zip")

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(bytes.Repeat([]byte("z"), 1<<20))
		pw.CloseWithError(errors.New("zip writer failed"))
	}()
	require.Error(t, st.Put(t.Context(), key, pr, "application/zip"))

	assert.Equal(t, "v1", string(getBytes(t, st, key)))
	entries, err := os.ReadDir(filepath.Join(st.Root(), "cars", "jobs", "job_1"))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"export.zip"}, names, "no temp files left behind")
}

func TestLocalDeletePrunesEmptyDirectories(t *testing.T) {
	st := newTestLocal(t)
	root := st.Root()
	putBytes(t, st, "cars/jobs/job_1/images/a.jpg", []byte("a"), "")
	putBytes(t, st, "cars/jobs/job_1/thumbs/a.jpg", []byte("a"), "")
	putBytes(t, st, "cars/jobs/job_2/original.mp4", []byte("v"), "")

	require.NoError(t, st.DeletePrefix(t.Context(), JobPrefix("cars", "job_1")))
	assert.NoDirExists(t, filepath.Join(root, "cars", "jobs", "job_1"))
	assert.DirExists(t, filepath.Join(root, "cars", "jobs", "job_2"))

	// Removing the last object of a name-prefix delete prunes up to the root.
	require.NoError(t, st.DeletePrefix(t.Context(), "cars/jobs/job_2/orig"))
	assert.NoDirExists(t, filepath.Join(root, "cars"))
	assert.DirExists(t, root, "the root itself is never removed")

	// Delete prunes too.
	putBytes(t, st, "real/jobs/job_3/manifest.json", []byte("{}"), "")
	require.NoError(t, st.Delete(t.Context(), "real/jobs/job_3/manifest.json"))
	assert.NoDirExists(t, filepath.Join(root, "real"))
	assert.DirExists(t, root)

	// Deleting everything by domain prefix keeps the root.
	putBytes(t, st, "cars/jobs/job_4/manifest.json", []byte("{}"), "")
	require.NoError(t, st.DeletePrefix(t.Context(), "cars/"))
	assert.NoDirExists(t, filepath.Join(root, "cars"))
	assert.DirExists(t, root)
}

func TestLocalPrefixMatchingAnObject(t *testing.T) {
	st := newTestLocal(t)
	putBytes(t, st, "a/b/c.jpg", []byte("c"), "")
	// "a/b/c.jpg/" names a directory that does not exist; the file must survive.
	require.NoError(t, st.DeletePrefix(t.Context(), "a/b/c.jpg/"))
	assert.True(t, exists(t, st, "a/b/c.jpg"))
	// A key below a file is simply missing.
	_, err := st.Head(t.Context(), "a/b/c.jpg/d.jpg")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = st.Get(t.Context(), "a/b/c.jpg/d.jpg")
	assert.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, st.DeletePrefix(t.Context(), "a/b/c.jpg/d"))
	assert.True(t, exists(t, st, "a/b/c.jpg"))
}

func TestLocalPresignUnsupported(t *testing.T) {
	st := newTestLocal(t)
	_, err := st.PresignGet(t.Context(), "cars/jobs/job_1/export.zip", time.Minute, "x.zip")
	assert.ErrorIs(t, err, ErrPresignUnsupported)
}

func TestLocalPingUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	st := newTestLocal(t)
	require.NoError(t, os.Chmod(st.Root(), 0o555))
	t.Cleanup(func() { _ = os.Chmod(st.Root(), 0o755) })
	assert.Error(t, st.Ping(t.Context()))
}

func TestLocalPingLeavesNothing(t *testing.T) {
	st := newTestLocal(t)
	require.NoError(t, st.Ping(t.Context()))
	entries, err := os.ReadDir(st.Root())
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestLocalHeadContentTypeFromExtension(t *testing.T) {
	st := newTestLocal(t)
	// The content type given to Put is not stored locally; the extension decides.
	putBytes(t, st, "x/photo.JPG", []byte("x"), "text/plain")
	info, err := st.Head(t.Context(), "x/photo.JPG")
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", info.ContentType)
}

func TestLocalNamePrefixDeleteKeepsInFlightPuts(t *testing.T) {
	st := newTestLocal(t)
	dir := filepath.Join(st.Root(), "a")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	inFlight := filepath.Join(dir, ".v2i-tmp-123") // another Put's temp file
	require.NoError(t, os.WriteFile(inFlight, []byte("in flight"), 0o600))
	putBytes(t, st, "a/img_1.jpg", []byte("x"), "")
	require.NoError(t, st.DeletePrefix(t.Context(), "a/img_"))
	assert.FileExists(t, inFlight)
	assert.False(t, exists(t, st, "a/img_1.jpg"))
}
