package export

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/storage"
)

// fixture is a set of generated source frames.
type fixture struct {
	in Input
}

// newFixture writes n JPEG frames (and 640 px thumbnails) of different sizes.
func newFixture(t *testing.T, n int) fixture {
	t.Helper()
	dir := t.TempDir()
	sizes := []image.Point{{1920, 1080}, {1280, 720}, {720, 1280}, {1024, 768}}
	shots := []struct{ id, label string }{
		{"front_three_quarter", "Front three-quarter"},
		{"rear", "Rear"},
		{"interior_dashboard", "Dashboard"},
		{"other", "Other"},
	}
	timestamps := []float64{6.25, 12.5, 3725.123456, 30}
	in := Input{
		DomainKey:   "cars",
		JobID:       testJobID,
		CreatedAt:   time.Date(2026, 9, 23, 10, 11, 12, 0, time.FixedZone("CEST", 2*3600)),
		Summary:     "A silver hatchback filmed outdoors.",
		Explanation: "Front, rear and dashboard covered; no engine bay.",
		MissingShots: []MissingShot{
			{ID: "engine_bay", Label: "Engine bay"},
		},
	}
	for i := range n {
		sz := sizes[i%len(sizes)]
		full := filepath.Join(dir, "full_"+string(rune('a'+i))+".jpg")
		small := filepath.Join(dir, "small_"+string(rune('a'+i))+".jpg")
		writeJPEG(t, full, sz.X, sz.Y, i)
		tw, th := 640, sz.Y*640/sz.X
		if sz.Y > sz.X {
			tw, th = sz.X*640/sz.Y, 640
		}
		writeJPEG(t, small, tw, th, i)
		in.Images = append(in.Images, Image{
			Index:        i + 1,
			ImageID:      "img_01J00000000000000000000" + string(rune('A'+i)),
			CandidateID:  "cf_" + string(rune('a'+i)),
			ShotID:       shots[i%len(shots)].id,
			ShotLabel:    shots[i%len(shots)].label,
			TimestampSec: timestamps[i%len(timestamps)],
			FullPath:     full,
			SmallPath:    small,
			Reason:       "reason " + string(rune('a'+i)),
			Scores: Scores{
				Relevance: 0.9 - float64(i)/10, Sharpness: 120 + float64(i), SharpnessZ: 1.5,
				ExposurePenalty: 0.05, Score: 0.8,
			},
		})
	}
	return fixture{in: in}
}

// jpegCache holds encoded fixture frames by size and seed: encoding under -race is
// slow, and every test only needs its own copies on disk.
var jpegCache sync.Map // [3]int → []byte

func writeJPEG(t *testing.T, path string, w, h, seed int) {
	t.Helper()
	k := [3]int{w, h, seed}
	data, ok := jpegCache.Load(k)
	if !ok {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				o := img.PixOffset(x, y)
				img.Pix[o+0] = uint8(x * 255 / w)
				img.Pix[o+1] = uint8(y * 255 / h)
				img.Pix[o+2] = uint8((x ^ y + seed*50) & 0xff)
				img.Pix[o+3] = 0xff
			}
		}
		var buf bytes.Buffer
		require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}))
		data, _ = jpegCache.LoadOrStore(k, buf.Bytes())
	}
	require.NoError(t, os.WriteFile(path, data.([]byte), 0o644))
}

func newStore(t *testing.T) *storage.Local {
	t.Helper()
	st, err := storage.NewLocal(t.TempDir())
	require.NoError(t, err)
	return st
}

func readObject(t *testing.T, st storage.Storage, key string) []byte {
	t.Helper()
	rc, err := st.Get(t.Context(), key)
	require.NoError(t, err, key)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(t, err, key)
	return b
}

func objectExists(t *testing.T, st storage.Storage, key string) bool {
	t.Helper()
	_, err := st.Head(t.Context(), key)
	if errors.Is(err, storage.ErrNotFound) {
		return false
	}
	require.NoError(t, err, key)
	return true
}

func TestRun(t *testing.T) {
	st := newStore(t)
	fx := newFixture(t, 3)
	checkRun(t, st, fx.in)
}

// checkRun runs the export and verifies every stored artefact.
func checkRun(t *testing.T, st storage.Storage, in Input) {
	t.Helper()
	start := time.Now()
	out, err := Run(t.Context(), st, in)
	require.NoError(t, err)

	wantNames := []string{
		"cars_7K2M9QXA_1_front_three_quarter_00-06.jpg",
		"cars_7K2M9QXA_2_rear_00-12.jpg",
		"cars_7K2M9QXA_3_interior_dashboard_1-02-05.jpg",
	}
	if in.DomainKey != "cars" { // S3 run uses a random domain key
		for i := range wantNames {
			wantNames[i] = strings.Replace(wantNames[i], "cars", in.DomainKey, 1)
		}
	}
	wantSizes := []image.Point{{1920, 1080}, {1280, 720}, {720, 1280}}

	require.Len(t, out.Images, len(in.Images))
	for i, e := range out.Images {
		src := in.Images[i]
		assert.Equal(t, src, e.Image, "input image carried through, in order")
		assert.Equal(t, wantNames[i], e.FileName)
		assert.Equal(t, storage.ImageKey(in.DomainKey, in.JobID, src.ImageID), e.ImageKey)
		assert.Equal(t, storage.ThumbKey(in.DomainKey, in.JobID, src.ImageID), e.ThumbKey)
		assert.Equal(t, wantSizes[i].X, e.Width, e.FileName)
		assert.Equal(t, wantSizes[i].Y, e.Height, e.FileName)

		full, err := os.ReadFile(src.FullPath)
		require.NoError(t, err)
		assert.EqualValues(t, len(full), e.Bytes)
		assert.True(t, bytes.Equal(full, readObject(t, st, e.ImageKey)), "full image stored byte for byte (no re-encode)")
		small, err := os.ReadFile(src.SmallPath)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(small, readObject(t, st, e.ThumbKey)), "thumbnail stored as-is")

		info, err := st.Head(t.Context(), e.ImageKey)
		require.NoError(t, err)
		assert.Equal(t, "image/jpeg", info.ContentType)
	}

	// manifest.json
	assert.Equal(t, storage.ManifestKey(in.DomainKey, in.JobID), out.ManifestKey)
	rawManifest := readObject(t, st, out.ManifestKey)
	assert.True(t, bytes.HasPrefix(rawManifest, []byte("{\n  \"jobId\"")), "indented JSON")
	var m Manifest
	require.NoError(t, json.Unmarshal(rawManifest, &m))
	assert.Equal(t, in.JobID, m.JobID)
	assert.Equal(t, in.DomainKey, m.DomainKey)
	assert.True(t, in.CreatedAt.Equal(m.CreatedAt), "createdAt %v", m.CreatedAt)
	assert.Equal(t, in.Summary, m.Summary)
	assert.Equal(t, in.Explanation, m.Explanation)
	assert.Equal(t, in.MissingShots, m.MissingShots)
	wantLabels := []string{"00:06", "00:12", "1:02:05"}
	wantTS := []float64{6.25, 12.5, 3725.123}
	require.Len(t, m.Images, 3)
	for i, mi := range m.Images {
		src := in.Images[i]
		assert.Equal(t, ManifestImage{
			Index: src.Index, File: wantNames[i], ShotID: src.ShotID, ShotLabel: src.ShotLabel,
			TimestampSec: wantTS[i], TimestampLabel: wantLabels[i],
			Width: wantSizes[i].X, Height: wantSizes[i].Y, Reason: src.Reason, Scores: src.Scores,
		}, mi)
	}

	// export.zip
	assert.Equal(t, storage.ZipKey(in.DomainKey, in.JobID), out.ZipKey)
	assert.Equal(t, ZipName(in.DomainKey, in.JobID), out.ZipName)
	zipBytes := readObject(t, st, out.ZipKey)
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	require.NoError(t, err)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	assert.Equal(t, append(append([]string{}, wantNames...), "manifest.json"), names, "exactly the expected entries, in order")
	for i, f := range zr.File {
		assert.WithinDuration(t, start, f.Modified, time.Minute, f.Name)
		assert.Equal(t, os.FileMode(0o644), f.Mode().Perm(), f.Name)
		got := readZipEntry(t, f)
		if f.Name == "manifest.json" {
			assert.Equal(t, zip.Deflate, f.Method)
			assert.Equal(t, rawManifest, got, "the ZIP carries the same manifest")
			continue
		}
		assert.Equal(t, zip.Store, f.Method, "JPEGs are stored, not recompressed")
		assert.Zero(t, f.Flags&0x8, "stored entries have sizes in the local header, no data descriptor")
		full, err := os.ReadFile(in.Images[i].FullPath)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(full, got), "%s identical to the source", f.Name)
	}

	// Cross-check with an independent implementation when available.
	if unzip, err := exec.LookPath("unzip"); err == nil {
		p := filepath.Join(t.TempDir(), "export.zip")
		require.NoError(t, os.WriteFile(p, zipBytes, 0o644))
		outp, err := exec.Command(unzip, "-t", p).CombinedOutput()
		assert.NoError(t, err, "unzip -t: %s", outp)
	}
	// A streaming reader (no central directory) must get through too.
	if bsdtar, err := exec.LookPath("bsdtar"); err == nil {
		cmd := exec.Command(bsdtar, "-tf", "-")
		cmd.Stdin = bytes.NewReader(zipBytes)
		outp, err := cmd.CombinedOutput()
		require.NoError(t, err, "bsdtar -tf -: %s", outp)
		assert.Equal(t, names, strings.Fields(string(outp)))
	}
}

func readZipEntry(t *testing.T, f *zip.File) []byte {
	t.Helper()
	rc, err := f.Open()
	require.NoError(t, err, f.Name)
	defer rc.Close()
	b, err := io.ReadAll(rc) // also verifies the CRC
	require.NoError(t, err, f.Name)
	return b
}

func TestRunNoImages(t *testing.T) {
	st := newStore(t)
	in := Input{DomainKey: "cars", JobID: testJobID, CreatedAt: time.Now(), Explanation: "Nothing usable.",
		MissingShots: []MissingShot{{ID: "front", Label: "Front"}}}
	out, err := Run(t.Context(), st, in)
	require.NoError(t, err)
	assert.Empty(t, out.Images)
	assert.Empty(t, out.ZipKey)
	assert.Empty(t, out.ZipName)
	assert.False(t, objectExists(t, st, storage.ZipKey("cars", testJobID)), "no ZIP without images")

	raw := readObject(t, st, out.ManifestKey)
	assert.Contains(t, string(raw), `"images": []`, "an empty list, not null")
	var m Manifest
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, in.MissingShots, m.MissingShots)
	assert.Equal(t, "Nothing usable.", m.Explanation)

	// Nil missing shots also encode as [].
	out, err = Run(t.Context(), st, Input{DomainKey: "cars", JobID: testJobID})
	require.NoError(t, err)
	raw = readObject(t, st, out.ManifestKey)
	assert.Contains(t, string(raw), `"missingShots": []`)
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.False(t, m.CreatedAt.IsZero(), "a zero CreatedAt falls back to the export time")
}

func TestRunNonFiniteScores(t *testing.T) {
	st := newStore(t)
	fx := newFixture(t, 1)
	fx.in.Images[0].Scores.SharpnessZ = math.NaN()
	fx.in.Images[0].Scores.Score = math.Inf(1)
	out, err := Run(t.Context(), st, fx.in)
	require.NoError(t, err)
	var m Manifest
	require.NoError(t, json.Unmarshal(readObject(t, st, out.ManifestKey), &m))
	assert.Zero(t, m.Images[0].Scores.SharpnessZ)
	assert.Zero(t, m.Images[0].Scores.Score)
	assert.InDelta(t, 0.9, m.Images[0].Scores.Relevance, 1e-9)
}

func TestRunValidation(t *testing.T) {
	st := newStore(t)
	fx := newFixture(t, 2)
	pngPath := filepath.Join(t.TempDir(), "frame.png")
	{
		f, err := os.Create(pngPath)
		require.NoError(t, err)
		img := image.NewRGBA(image.Rect(0, 0, 8, 8))
		img.Set(1, 1, color.White)
		require.NoError(t, png.Encode(f, img))
		require.NoError(t, f.Close())
	}

	for _, tc := range []struct {
		name   string
		st     storage.Storage
		mutate func(in *Input)
		want   string
	}{
		{"nil storage", nil, func(*Input) {}, "storage is nil"},
		{"no domain", st, func(in *Input) { in.DomainKey = "" }, "domain key and job ID"},
		{"no job", st, func(in *Input) { in.JobID = "" }, "domain key and job ID"},
		{"bad image id", st, func(in *Input) { in.Images[0].ImageID = "img/../x" }, "invalid image ID"},
		{"empty image id", st, func(in *Input) { in.Images[0].ImageID = "" }, "invalid image ID"},
		{"zero index", st, func(in *Input) { in.Images[0].Index = 0 }, "not 1-based"},
		{"duplicate id", st, func(in *Input) { in.Images[1].ImageID = in.Images[0].ImageID }, "duplicate image ID"},
		{"duplicate file name", st, func(in *Input) { in.Images[1] = sameName(in.Images[0], in.Images[1]) }, "duplicate file name"},
		{"no small path", st, func(in *Input) { in.Images[1].SmallPath = "" }, "paths are required"},
		{"missing file", st, func(in *Input) { in.Images[1].FullPath = filepath.Join(t.TempDir(), "gone.jpg") }, "no such file"},
		{"not a jpeg", st, func(in *Input) { in.Images[1].FullPath = pngPath }, "want a JPEG, got png"},
		{"thumb not a jpeg", st, func(in *Input) { in.Images[1].SmallPath = pngPath }, "want a JPEG, got png"},
		{"garbage", st, func(in *Input) { in.Images[1].FullPath = writeFile(t, "junk") }, "read image header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fx.in
			in.Images = append([]Image(nil), fx.in.Images...)
			tc.mutate(&in)
			_, err := Run(t.Context(), tc.st, in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func sameName(a, b Image) Image {
	b.Index, b.ShotID, b.TimestampSec = a.Index, a.ShotID, a.TimestampSec
	return b
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file.jpg")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

// hookStorage lets a test take over Put for chosen keys.
type hookStorage struct {
	storage.Storage
	put func(ctx context.Context, key string, r io.Reader) (handled bool, err error)
}

func (h *hookStorage) Put(ctx context.Context, key string, r io.Reader, contentType string) error {
	if handled, err := h.put(ctx, key, r); handled {
		return err
	}
	return h.Storage.Put(ctx, key, r, contentType)
}

// runWithDeadline fails the test instead of hanging when Run deadlocks.
func runWithDeadline(t *testing.T, ctx context.Context, st storage.Storage, in Input) (*Output, error) {
	t.Helper()
	type result struct {
		out *Output
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := Run(ctx, st, in)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return: the ZIP pipe is deadlocked")
		return nil, nil
	}
}

// assertNoGoroutineLeak waits briefly for the goroutine count to return to baseline.
func assertNoGoroutineLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.LessOrEqual(t, runtime.NumGoroutine(), baseline, "goroutines leaked")
}

func TestRunZipPutFailures(t *testing.T) {
	errBoom := errors.New("s3 went away")
	for _, tc := range []struct {
		name string
		put  func(r io.Reader) error
		want func(t *testing.T, err error)
	}{
		{
			name: "fails mid-stream without draining",
			put: func(r io.Reader) error {
				_, _ = io.CopyN(io.Discard, r, 100<<10)
				return errBoom
			},
			want: func(t *testing.T, err error) { assert.ErrorIs(t, err, errBoom) },
		},
		{
			name: "fails before reading",
			put:  func(io.Reader) error { return errBoom },
			want: func(t *testing.T, err error) { assert.ErrorIs(t, err, errBoom) },
		},
		{
			name: "returns success without reading everything",
			put: func(r io.Reader) error {
				_, _ = io.CopyN(io.Discard, r, 10<<10)
				return nil
			},
			want: func(t *testing.T, err error) { assert.ErrorContains(t, err, "before reading the whole archive") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newStore(t)
			fx := newFixture(t, 3)
			zipKey := storage.ZipKey(fx.in.DomainKey, fx.in.JobID)
			st := &hookStorage{Storage: local, put: func(_ context.Context, key string, r io.Reader) (bool, error) {
				if key != zipKey {
					return false, nil
				}
				return true, tc.put(r)
			}}
			baseline := runtime.NumGoroutine()
			out, err := runWithDeadline(t, t.Context(), st, fx.in)
			require.Error(t, err)
			assert.Nil(t, out)
			tc.want(t, err)
			assertNoGoroutineLeak(t, baseline)
		})
	}
}

func TestRunZipWriterFails(t *testing.T) {
	local := newStore(t)
	fx := newFixture(t, 3)
	manifestKey := storage.ManifestKey(fx.in.DomainKey, fx.in.JobID)
	// The manifest is stored after every image upload and before the ZIP starts:
	// removing a source frame there makes the ZIP writer fail deterministically.
	st := &hookStorage{Storage: local, put: func(_ context.Context, key string, _ io.Reader) (bool, error) {
		if key == manifestKey {
			require.NoError(t, os.Remove(fx.in.Images[1].FullPath))
		}
		return false, nil
	}}
	baseline := runtime.NumGoroutine()
	_, err := runWithDeadline(t, t.Context(), st, fx.in)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Contains(t, err.Error(), "write zip")
	assert.Contains(t, err.Error(), "cars_7K2M9QXA_2_rear_00-12.jpg")
	assert.False(t, objectExists(t, local, storage.ZipKey(fx.in.DomainKey, fx.in.JobID)), "no partial ZIP")
	assertNoGoroutineLeak(t, baseline)
}

func TestRunSourceChangedBeforeZip(t *testing.T) {
	local := newStore(t)
	fx := newFixture(t, 2)
	manifestKey := storage.ManifestKey(fx.in.DomainKey, fx.in.JobID)
	st := &hookStorage{Storage: local, put: func(_ context.Context, key string, _ io.Reader) (bool, error) {
		if key == manifestKey { // same length, different bytes
			b, err := os.ReadFile(fx.in.Images[0].FullPath)
			require.NoError(t, err)
			b[len(b)/2] ^= 0xff
			require.NoError(t, os.WriteFile(fx.in.Images[0].FullPath, b, 0o644))
		}
		return false, nil
	}}
	_, err := runWithDeadline(t, t.Context(), st, fx.in)
	assert.ErrorContains(t, err, "changed after upload")
	assert.False(t, objectExists(t, local, storage.ZipKey(fx.in.DomainKey, fx.in.JobID)))
}

func TestRunCancelDuringZip(t *testing.T) {
	local := newStore(t)
	fx := newFixture(t, 3)
	zipKey := storage.ZipKey(fx.in.DomainKey, fx.in.JobID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var read atomic.Int64
	// A slow consumer that ignores ctx: only closing the pipe can stop it.
	st := &hookStorage{Storage: local, put: func(_ context.Context, key string, r io.Reader) (bool, error) {
		if key != zipKey {
			return false, nil
		}
		buf := make([]byte, 1024)
		for {
			n, err := r.Read(buf)
			if read.Add(int64(n)) > 4096 {
				cancel()
			}
			if err != nil {
				return true, err
			}
			time.Sleep(time.Millisecond)
		}
	}}
	baseline := runtime.NumGoroutine()
	_, err := runWithDeadline(t, ctx, st, fx.in)
	require.ErrorIs(t, err, context.Canceled)
	assertNoGoroutineLeak(t, baseline)
}

func TestRunImageUploadFails(t *testing.T) {
	local := newStore(t)
	fx := newFixture(t, 3)
	errBoom := errors.New("disk full")
	badKey := storage.ThumbKey(fx.in.DomainKey, fx.in.JobID, fx.in.Images[2].ImageID)
	st := &hookStorage{Storage: local, put: func(_ context.Context, key string, _ io.Reader) (bool, error) {
		return key == badKey, errBoom
	}}
	_, err := Run(t.Context(), st, fx.in)
	require.ErrorIs(t, err, errBoom)
	assert.Contains(t, err.Error(), fx.in.Images[2].ImageID)
	assert.False(t, objectExists(t, local, storage.ManifestKey(fx.in.DomainKey, fx.in.JobID)), "nothing after a failed upload")
	assert.False(t, objectExists(t, local, storage.ZipKey(fx.in.DomainKey, fx.in.JobID)))
}

func TestRunIsIdempotent(t *testing.T) {
	st := newStore(t)
	fx := newFixture(t, 2)
	first, err := Run(t.Context(), st, fx.in)
	require.NoError(t, err)
	second, err := Run(t.Context(), st, fx.in)
	require.NoError(t, err)
	assert.Equal(t, first.Images, second.Images)
	assert.Equal(t, first.ZipKey, second.ZipKey)
}

// TestRunS3 streams the export into MinIO when V2I_TEST_S3_ENDPOINT is set.
func TestRunS3(t *testing.T) {
	endpoint := os.Getenv("V2I_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("V2I_TEST_S3_ENDPOINT not set")
	}
	st, err := storage.NewS3(t.Context(), storage.S3Options{
		Endpoint: endpoint, Region: "us-east-1", Bucket: envOr("V2I_TEST_S3_BUCKET", "v2i"),
		AccessKeyID: envOr("V2I_TEST_S3_ACCESS_KEY_ID", "v2i-minio"),
		SecretKey:   envOr("V2I_TEST_S3_SECRET_ACCESS_KEY", "v2i-minio-secret"), UsePathStyle: true,
	})
	require.NoError(t, err)
	require.NoError(t, st.Ping(t.Context()))

	fx := newFixture(t, 3)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	fx.in.DomainKey = "exporttest" + hex.EncodeToString(b)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		assert.NoError(t, st.DeletePrefix(ctx, fx.in.DomainKey+"/"))
	})
	checkRun(t, st, fx.in)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
