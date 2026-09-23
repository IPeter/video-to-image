package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runConformance checks the behaviour every Storage must share. All keys live under a
// random prefix that is deleted when the test ends, so it can run against a shared
// bucket.
func runConformance(t *testing.T, st Storage) {
	t.Helper()
	base := "conformance-" + randomHex(t, 6) + "/"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		assert.NoError(t, st.DeletePrefix(ctx, base), "cleanup")
	})
	k := func(rel string) string { return base + rel }
	ctx := t.Context()

	t.Run("PutGetHead", func(t *testing.T) {
		data := pseudoRandom(1, 1234)
		before := time.Now()
		putBytes(t, st, k("a/b/c.jpg"), data, "image/jpeg")
		assert.Equal(t, data, getBytes(t, st, k("a/b/c.jpg")))

		info, err := st.Head(ctx, k("a/b/c.jpg"))
		require.NoError(t, err)
		assert.Equal(t, k("a/b/c.jpg"), info.Key)
		assert.EqualValues(t, len(data), info.Size)
		assert.Equal(t, "image/jpeg", info.ContentType)
		// Generous window: MinIO runs in a container with its own clock.
		assert.WithinRange(t, info.LastModified, before.Add(-10*time.Minute), time.Now().Add(10*time.Minute))
		assert.Equal(t, time.UTC, info.LastModified.Location())
	})

	t.Run("ContentTypes", func(t *testing.T) {
		for _, tc := range []struct{ rel, put, want string }{
			{"ct/manifest.json", "application/json", "application/json"},
			{"ct/export.zip", "application/zip", "application/zip"},
			{"ct/original.mp4", "video/mp4", "video/mp4"},
			{"ct/thumb.jpg", "image/jpeg", "image/jpeg"},
			{"ct/noext", "", "application/octet-stream"},
		} {
			putBytes(t, st, k(tc.rel), []byte("x"), tc.put)
			info, err := st.Head(ctx, k(tc.rel))
			require.NoError(t, err, tc.rel)
			assert.Equal(t, tc.want, info.ContentType, tc.rel)
		}
	})

	t.Run("Overwrite", func(t *testing.T) {
		putBytes(t, st, k("ow/x.json"), []byte(`{"version":"first and longer"}`), "application/json")
		putBytes(t, st, k("ow/x.json"), []byte(`{"v":2}`), "application/json")
		assert.Equal(t, `{"v":2}`, string(getBytes(t, st, k("ow/x.json"))))
		info, err := st.Head(ctx, k("ow/x.json"))
		require.NoError(t, err)
		assert.EqualValues(t, 7, info.Size)
	})

	t.Run("EmptyObject", func(t *testing.T) {
		putBytes(t, st, k("empty/x.json"), nil, "application/json")
		assert.Empty(t, getBytes(t, st, k("empty/x.json")))
		info, err := st.Head(ctx, k("empty/x.json"))
		require.NoError(t, err)
		assert.Zero(t, info.Size)
	})

	t.Run("NotFound", func(t *testing.T) {
		_, err := st.Get(ctx, k("missing/x.jpg"))
		assert.ErrorIs(t, err, ErrNotFound)
		_, err = st.Head(ctx, k("missing/x.jpg"))
		assert.ErrorIs(t, err, ErrNotFound)
		assert.NoError(t, st.Delete(ctx, k("missing/x.jpg")))

		// A "directory" of objects is not itself an object.
		putBytes(t, st, k("dir/obj.jpg"), []byte("obj"), "image/jpeg")
		_, err = st.Get(ctx, k("dir"))
		assert.ErrorIs(t, err, ErrNotFound)
		_, err = st.Head(ctx, k("dir"))
		assert.ErrorIs(t, err, ErrNotFound)
		assert.NoError(t, st.Delete(ctx, k("dir")))
		assert.True(t, exists(t, st, k("dir/obj.jpg")), "Delete of a directory-like key must not remove its children")
	})

	t.Run("InvalidKey", func(t *testing.T) {
		for _, key := range []string{
			"", "/abs.jpg", "../x.jpg", "a/../b.jpg", "a/..", "a..b.jpg", `a\b.jpg`, "a\x00b",
			"a//b.jpg", "./a.jpg", "a/./b.jpg", "dir/",
		} {
			assert.ErrorIs(t, st.Put(ctx, key, bytes.NewReader([]byte("x")), ""), ErrInvalidKey, "Put %q", key)
			_, err := st.Get(ctx, key)
			assert.ErrorIs(t, err, ErrInvalidKey, "Get %q", key)
			_, err = st.Head(ctx, key)
			assert.ErrorIs(t, err, ErrInvalidKey, "Head %q", key)
			assert.ErrorIs(t, st.Delete(ctx, key), ErrInvalidKey, "Delete %q", key)
			_, err = st.PresignGet(ctx, key, time.Minute, "")
			assert.ErrorIs(t, err, ErrInvalidKey, "PresignGet %q", key)
		}
		for _, prefix := range []string{"", "/", "/abs/", "../", "a/../", `a\b/`, "a//", "a/./"} {
			assert.ErrorIs(t, st.DeletePrefix(ctx, prefix), ErrInvalidKey, "DeletePrefix %q", prefix)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		putBytes(t, st, k("del/x.jpg"), []byte("x"), "image/jpeg")
		require.NoError(t, st.Delete(ctx, k("del/x.jpg")))
		_, err := st.Head(ctx, k("del/x.jpg"))
		assert.ErrorIs(t, err, ErrNotFound)
		assert.NoError(t, st.Delete(ctx, k("del/x.jpg")), "second delete")
	})

	t.Run("DeletePrefix", func(t *testing.T) {
		dp := k("dp/")
		keys := []string{
			"job1/original.mp4", "job1/images/img_1.jpg", "job1/images/img_2.jpg", "job1/thumbs/img_1.jpg",
			"job10/keep.jpg",
			"job2/images/img_1.jpg", "job2/images/img_10.jpg", "job2/images/img_2.jpg", "job2/manifest.json",
		}
		for _, rel := range keys {
			putBytes(t, st, dp+rel, []byte(rel), "")
		}
		present := func(rels ...string) {
			t.Helper()
			for _, rel := range keys {
				want := false
				for _, r := range rels {
					want = want || r == rel
				}
				assert.Equal(t, want, exists(t, st, dp+rel), rel)
			}
		}

		// Trailing slash: exactly one job, not job10.
		require.NoError(t, st.DeletePrefix(ctx, dp+"job1/"))
		present("job10/keep.jpg", "job2/images/img_1.jpg", "job2/images/img_10.jpg",
			"job2/images/img_2.jpg", "job2/manifest.json")

		// No trailing slash: a name prefix inside one directory.
		require.NoError(t, st.DeletePrefix(ctx, dp+"job2/images/img_1"))
		present("job10/keep.jpg", "job2/images/img_2.jpg", "job2/manifest.json")

		// Deleting nothing is fine.
		require.NoError(t, st.DeletePrefix(ctx, dp+"job2/nothing/"))
		require.NoError(t, st.DeletePrefix(ctx, dp+"zzz"))
		require.NoError(t, st.DeletePrefix(ctx, k("never-existed/")))
		present("job10/keep.jpg", "job2/images/img_2.jpg", "job2/manifest.json")

		// No trailing slash spanning several directories.
		require.NoError(t, st.DeletePrefix(ctx, dp+"job"))
		present()

		// The emptied prefix is usable again.
		putBytes(t, st, dp+"job1/images/img_3.jpg", []byte("again"), "image/jpeg")
		assert.Equal(t, "again", string(getBytes(t, st, dp+"job1/images/img_3.jpg")))
	})

	t.Run("ConcurrentPuts", func(t *testing.T) {
		const distinct, sameKey = 16, 8
		contents := make([][]byte, sameKey)
		for i := range contents {
			contents[i] = bytes.Repeat([]byte{byte('a' + i)}, 1000+100*i)
		}
		var wg sync.WaitGroup
		errs := make(chan error, distinct+sameKey)
		for i := range distinct {
			wg.Go(func() {
				key := k(fmt.Sprintf("conc/distinct/%02d.bin", i))
				errs <- st.Put(ctx, key, bytes.NewReader(pseudoRandom(uint64(i), 4096+i)), "")
			})
		}
		for i := range sameKey {
			wg.Go(func() {
				errs <- st.Put(ctx, k("conc/same.bin"), bytes.NewReader(contents[i]), "")
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		for i := range distinct {
			key := k(fmt.Sprintf("conc/distinct/%02d.bin", i))
			assert.Equal(t, pseudoRandom(uint64(i), 4096+i), getBytes(t, st, key), key)
		}
		got := getBytes(t, st, k("conc/same.bin"))
		assert.Contains(t, contents, got, "the same-key object must be exactly one writer's content")
	})

	t.Run("PutFromPipeUnknownLength", func(t *testing.T) {
		// 20 MiB crosses the S3 transfer manager's 16 MiB multipart threshold (8 MiB parts).
		const size = 20 << 20
		pr, pw := io.Pipe()
		sum := make(chan []byte, 1)
		go func() {
			h := sha256.New()
			w := io.MultiWriter(pw, h)
			src := mrand.NewChaCha8([32]byte{7})
			buf := make([]byte, 64<<10)
			for written := 0; written < size; written += len(buf) {
				_, _ = src.Read(buf)
				if _, err := w.Write(buf); err != nil {
					pw.CloseWithError(err)
					sum <- nil
					return
				}
			}
			pw.Close()
			sum <- h.Sum(nil)
		}()
		err := st.Put(ctx, k("pipe/big.bin"), pr, "application/zip")
		_ = pr.Close()
		require.NoError(t, err)
		want := <-sum
		require.NotNil(t, want)

		info, err := st.Head(ctx, k("pipe/big.bin"))
		require.NoError(t, err)
		assert.EqualValues(t, size, info.Size)
		rc, err := st.Get(ctx, k("pipe/big.bin"))
		require.NoError(t, err)
		h := sha256.New()
		n, err := io.Copy(h, rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		assert.EqualValues(t, size, n)
		assert.Equal(t, hex.EncodeToString(want), hex.EncodeToString(h.Sum(nil)))
		require.NoError(t, st.Delete(ctx, k("pipe/big.bin")))
	})

	t.Run("PutStreamError", func(t *testing.T) {
		boom := errors.New("producer failed")
		for _, tc := range []struct {
			name string
			size int
		}{
			{"single part", 1 << 20},
			{"multipart", 17 << 20}, // past the S3 multipart threshold: the upload is aborted
		} {
			key := k("pipeerr/" + tc.name + ".bin")
			putBytes(t, st, key, []byte("v1"), "")
			fresh := k("pipeerr/" + tc.name + "-fresh.bin")
			for _, target := range []string{key, fresh} {
				pr, pw := io.Pipe()
				go func() {
					_, err := pw.Write(pseudoRandom(3, tc.size))
					if err == nil {
						err = boom
					}
					pw.CloseWithError(err)
				}()
				err := st.Put(ctx, target, pr, "")
				_ = pr.Close()
				require.ErrorIs(t, err, boom, "%s → %s", tc.name, target)
			}
			assert.Equal(t, "v1", string(getBytes(t, st, key)), "%s: a failed Put must keep the previous version", tc.name)
			assert.False(t, exists(t, st, fresh), "%s: a failed Put must not create the object", tc.name)
		}
	})

	t.Run("ContextCanceled", func(t *testing.T) {
		putBytes(t, st, k("ctx/x.jpg"), []byte("x"), "image/jpeg")
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		assert.ErrorIs(t, st.Put(cctx, k("ctx/y.jpg"), bytes.NewReader([]byte("y")), ""), context.Canceled)
		_, err := st.Get(cctx, k("ctx/x.jpg"))
		assert.ErrorIs(t, err, context.Canceled)
		_, err = st.Head(cctx, k("ctx/x.jpg"))
		assert.ErrorIs(t, err, context.Canceled)
		assert.False(t, exists(t, st, k("ctx/y.jpg")))
	})

	t.Run("CancelDuringStreamingPut", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		pr, pw := io.Pipe()
		producerDone := make(chan struct{})
		go func() {
			defer close(producerDone)
			chunk := pseudoRandom(5, 64<<10)
			for written := 0; ; written += len(chunk) {
				if written == 2<<20 {
					cancel() // the consumer is mid-stream now
				}
				if _, err := pw.Write(chunk); err != nil {
					return
				}
			}
		}()
		putErr := make(chan error, 1)
		go func() { putErr <- st.Put(cctx, k("ctx/stream.bin"), pr, "") }()
		select {
		case err := <-putErr:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(30 * time.Second):
			t.Fatal("Put did not return after cancel")
		}
		_ = pr.CloseWithError(errors.New("test: consumer gone")) // release the producer
		<-producerDone
		assert.False(t, exists(t, st, k("ctx/stream.bin")))
	})

	t.Run("Ping", func(t *testing.T) {
		assert.NoError(t, st.Ping(ctx))
	})
}

func putBytes(t *testing.T, st Storage, key string, data []byte, contentType string) {
	t.Helper()
	require.NoError(t, st.Put(t.Context(), key, bytes.NewReader(data), contentType), "put %s", key)
}

func getBytes(t *testing.T, st Storage, key string) []byte {
	t.Helper()
	rc, err := st.Get(t.Context(), key)
	require.NoError(t, err, "get %s", key)
	defer rc.Close()
	data, err := io.ReadAll(rc)
	require.NoError(t, err, "read %s", key)
	return data
}

func exists(t *testing.T, st Storage, key string) bool {
	t.Helper()
	_, err := st.Head(t.Context(), key)
	if errors.Is(err, ErrNotFound) {
		return false
	}
	require.NoError(t, err, "head %s", key)
	return true
}

// pseudoRandom returns n deterministic bytes for seed.
func pseudoRandom(seed uint64, n int) []byte {
	var s [32]byte
	s[0] = byte(seed)
	s[1] = byte(seed >> 8)
	b := make([]byte, n)
	_, _ = mrand.NewChaCha8(s).Read(b)
	return b
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}
