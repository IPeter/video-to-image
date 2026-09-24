package studio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testJPEG(t testing.TB, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil))
	return buf.Bytes()
}

func pngB64(t testing.TB, m image.Image) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, m))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// okBody is a valid sidecar answer for a w×h photo: an ellipse and its stats.
func okBody(t testing.TB, w, h int, box Box) []byte {
	t.Helper()
	alpha := EllipseMask(w, h, box)
	st := ComputeStats(alpha, box)
	st.IoU, st.RescuedFraction = 0.99, 0.001
	b, err := json.Marshal(map[string]any{
		"width": w, "height": h, "maskPng": pngB64(t, alpha), "stats": st,
		"timingsMs": map[string]int64{"birefnet": 3100, "sam": 2900, "total": 3300},
		"models":    map[string]string{"birefnet": "sha256:da5c", "sam": "sha256:f8a5"},
	})
	require.NoError(t, err)
	return b
}

var testBox = Box{0.1, 0.2, 0.9, 0.85}

func TestHTTPSegmenterMask(t *testing.T) {
	photo := testJPEG(t, 160, 120)
	var got struct {
		method, path, box, ctype string
		body                     []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.box, got.ctype = r.Method, r.URL.Path, r.URL.Query().Get("box"), r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(okBody(t, 160, 120, testBox))
	}))
	defer srv.Close()

	seg := NewHTTPSegmenter(srv.URL+"/", time.Second, nil)
	res, err := seg.Mask(context.Background(), photo, testBox)
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, got.method)
	assert.Equal(t, "/v1/mask", got.path)
	assert.Equal(t, "0.100000,0.200000,0.900000,0.850000", got.box)
	assert.Equal(t, "image/jpeg", got.ctype)
	assert.Equal(t, photo, got.body)

	require.NotNil(t, res.Alpha)
	assert.Equal(t, image.Rect(0, 0, 160, 120), res.Alpha.Bounds())
	assert.Equal(t, EllipseMask(160, 120, testBox).Pix, res.Alpha.Pix)
	assert.Equal(t, 0.99, res.Stats.IoU)
	assert.Equal(t, 0.001, res.Stats.RescuedFraction)
	assert.InDelta(t, 0.785, res.Stats.BoxFill, 0.02)
	assert.Equal(t, int64(3300), res.TimingsMS["total"])
	assert.Equal(t, "sha256:da5c", res.Models["birefnet"])
}

// TestHTTPSegmenterRetriesBusy: 503 + Retry-After is waited out, then the answer is used.
func TestHTTPSegmenterRetriesBusy(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0.05")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"busy"}`))
			return
		}
		_, _ = w.Write(okBody(t, 64, 48, testBox))
	}))
	defer srv.Close()
	start := time.Now()
	res, err := NewHTTPSegmenter(srv.URL, 5*time.Second, nil).Mask(context.Background(), testJPEG(t, 64, 48), testBox)
	require.NoError(t, err)
	assert.NotNil(t, res.Alpha)
	assert.Equal(t, int32(3), calls.Load())
	assert.GreaterOrEqual(t, time.Since(start), 100*time.Millisecond, "Retry-After was not honoured")
}

// TestHTTPSegmenterBusyUntilTimeout: a sidecar that stays busy gives up at the timeout
// with ErrSegmenterUnavailable, without sleeping past it.
func TestHTTPSegmenterBusyUntilTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"busy"}`))
	}))
	defer srv.Close()
	start := time.Now()
	_, err := NewHTTPSegmenter(srv.URL, 300*time.Millisecond, nil).Mask(context.Background(), testJPEG(t, 64, 48), testBox)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSegmenterUnavailable)
	assert.Contains(t, err.Error(), "busy")
	var u interface{ Unavailable() bool }
	require.ErrorAs(t, err, &u)
	assert.True(t, u.Unavailable())
	assert.Less(t, time.Since(start), 900*time.Millisecond, "waited past the timeout")
	assert.Equal(t, int32(1), calls.Load(), "a 1 s Retry-After does not fit a 300 ms timeout")
}

// TestHTTPSegmenterRetryFloor: "Retry-After: 0" does not turn into a busy loop.
func TestHTTPSegmenterRetryFloor(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := NewHTTPSegmenter(srv.URL, 300*time.Millisecond, nil).Mask(context.Background(), testJPEG(t, 16, 16), testBox)
	assert.ErrorIs(t, err, ErrSegmenterUnavailable)
	assert.GreaterOrEqual(t, calls.Load(), int32(2))
	assert.LessOrEqual(t, calls.Load(), int32(300/50+1))
}

func TestHTTPSegmenterConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close()) // nothing listens there now
	_, err = NewHTTPSegmenter("http://"+addr, time.Second, nil).Mask(context.Background(), testJPEG(t, 32, 32), testBox)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSegmenterUnavailable)
	assert.ErrorIs(t, err, syscall.ECONNREFUSED, "the cause is kept")
	assert.NotErrorIs(t, err, ErrSegmenterTimeout)
	assert.True(t, strings.HasPrefix(err.Error(), "studio: segmentation sidecar unavailable: "), err.Error())

	err = newHTTPSegmenter("http://"+addr, time.Second, nil).Ready(context.Background())
	assert.ErrorIs(t, err, ErrSegmenterUnavailable)
}

// TestHTTPSegmenterSlowAnswer: an answer that takes longer than the timeout is
// ErrSegmenterTimeout; a caller's own cancellation is returned as is.
func TestHTTPSegmenterSlowAnswer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	seg := NewHTTPSegmenter(srv.URL, 100*time.Millisecond, nil)
	_, err := seg.Mask(context.Background(), testJPEG(t, 32, 32), testBox)
	assert.ErrorIs(t, err, ErrSegmenterTimeout)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	_, err = NewHTTPSegmenter(srv.URL, 10*time.Second, nil).Mask(ctx, testJPEG(t, 32, 32), testBox)
	assert.Equal(t, context.Canceled, err)

	_, err = seg.Mask(ctx, testJPEG(t, 32, 32), testBox) // already cancelled
	assert.Equal(t, context.Canceled, err)
}

func TestHTTPSegmenterErrors(t *testing.T) {
	grey := func(w, h int) *image.Gray { return image.NewGray(image.Rect(0, 0, w, h)) }
	body := func(fields map[string]any) []byte {
		b, _ := json.Marshal(fields)
		return b
	}
	st := MaskStats{IoU: 0.99}
	tests := []struct {
		name   string
		status int
		body   []byte
		want   error
		text   string
	}{
		{"400", http.StatusBadRequest, []byte(`{"error":"box outside the image"}`), ErrSegmenterRejected, "box outside the image"},
		{"500", http.StatusInternalServerError, []byte("boom"), ErrSegmenterBadResponse, "status 500: boom"},
		{"not JSON", http.StatusOK, []byte("<html>"), ErrSegmenterBadResponse, ""},
		{"size mismatch", http.StatusOK, body(map[string]any{"width": 10, "height": 10, "maskPng": pngB64(t, grey(10, 10)), "stats": st}), ErrSegmenterBadResponse, "answer is for 10x10"},
		{"mask size mismatch", http.StatusOK, body(map[string]any{"width": 32, "height": 24, "maskPng": pngB64(t, grey(16, 12)), "stats": st}), ErrSegmenterBadResponse, "mask is 16x12"},
		{"colour mask", http.StatusOK, body(map[string]any{"width": 32, "height": 24, "maskPng": pngB64(t, image.NewNRGBA(image.Rect(0, 0, 32, 24))), "stats": st}), ErrSegmenterBadResponse, "8-bit grayscale"},
		{"bad base64", http.StatusOK, body(map[string]any{"width": 32, "height": 24, "maskPng": "@@@", "stats": st}), ErrSegmenterBadResponse, "not base64"},
		{"not a PNG", http.StatusOK, body(map[string]any{"width": 32, "height": 24, "maskPng": base64.StdEncoding.EncodeToString([]byte("GIF89a")), "stats": st}), ErrSegmenterBadResponse, "maskPng"},
		{"no stats", http.StatusOK, body(map[string]any{"width": 32, "height": 24, "maskPng": pngB64(t, grey(32, 24))}), ErrSegmenterBadResponse, "no stats"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write(tt.body)
			}))
			defer srv.Close()
			_, err := NewHTTPSegmenter(srv.URL, time.Second, nil).Mask(context.Background(), testJPEG(t, 32, 24), testBox)
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.want)
			assert.Contains(t, err.Error(), tt.text)
			assert.NotErrorIs(t, err, ErrSegmenterUnavailable)
		})
	}

	// Input checks happen before any request.
	seg := NewHTTPSegmenter("http://127.0.0.1:1", time.Second, nil)
	_, err := seg.Mask(context.Background(), []byte("not a jpeg"), testBox)
	assert.ErrorContains(t, err, "not a JPEG")
	_, err = seg.Mask(context.Background(), testJPEG(t, 8, 8), Box{0.5, 0.5, 0.4, 0.9})
	assert.ErrorContains(t, err, "invalid box")
}

func TestHTTPSegmenterReady(t *testing.T) {
	var ready atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/readyz", r.URL.Path)
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	seg, err := NewSegmenter(srv.URL, 0, nil)
	require.NoError(t, err)
	r, ok := seg.(Readier)
	require.True(t, ok)
	assert.ErrorIs(t, r.Ready(context.Background()), ErrSegmenterUnavailable)
	ready.Store(true)
	assert.NoError(t, r.Ready(context.Background()))
	assert.Equal(t, DefaultSegmentTimeout, seg.(*HTTPSegmenter).timeout)
}

func TestNewSegmenter(t *testing.T) {
	s, err := NewSegmenter("", time.Second, nil)
	require.NoError(t, err)
	assert.Nil(t, s, "empty URL: feature off")
	s, err = NewSegmenter(" fake ", time.Second, nil)
	require.NoError(t, err)
	assert.IsType(t, FakeSegmenter{}, s)
	s, err = NewSegmenter("http://127.0.0.1:4100", 2*time.Second, nil)
	require.NoError(t, err)
	hs := s.(*HTTPSegmenter)
	assert.Equal(t, "http://127.0.0.1:4100", hs.base)
	assert.Equal(t, 2*time.Second, hs.timeout)
	for _, bad := range []string{"127.0.0.1:4100", "ftp://host", "http://", "http://h/?x=1", "::"} {
		_, err := NewSegmenter(bad, time.Second, nil)
		assert.Error(t, err, bad)
	}
}

func TestRetryAfter(t *testing.T) {
	assert.Equal(t, defaultRetry, retryAfter(""))
	assert.Equal(t, defaultRetry, retryAfter("soon"))
	assert.Equal(t, 2*time.Second, retryAfter("2"))
	assert.Equal(t, 500*time.Millisecond, retryAfter("0.5"))
	assert.Equal(t, maxRetryWait, retryAfter("3600"))
	assert.Equal(t, time.Duration(0), retryAfter(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)))
	d := retryAfter(time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat))
	assert.Greater(t, d, time.Second)
	assert.LessOrEqual(t, d, 3*time.Second)
}

func TestErrorText(t *testing.T) {
	assert.Equal(t, "busy", errorText(strings.NewReader(`{"error":"busy"}`)))
	assert.Equal(t, "plain", errorText(strings.NewReader(" plain \n")))
	assert.Equal(t, "(no message)", errorText(strings.NewReader("")))
	long := errorText(strings.NewReader(strings.Repeat("x", 10000)))
	assert.Len(t, long, maxErrorBytes)
	assert.True(t, errors.Is(ErrSegmenterTimeout, ErrSegmenterTimeout))
}

// TestFakeSegmenter: the fake's ellipse is the photo's size, inscribed in the box, and
// passes every gate; its stats are ComputeStats with IoU 1.
func TestFakeSegmenter(t *testing.T) {
	photo := testJPEG(t, 400, 300)
	box := Box{0.1, 0.15, 0.9, 0.85} // the fake analyzer's review box
	res, err := FakeSegmenter{}.Mask(context.Background(), photo, box)
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 400, 300), res.Alpha.Bounds())
	want := ComputeStats(res.Alpha, box)
	want.IoU = 1
	assert.Equal(t, want, res.Stats)
	assert.InDelta(t, 0.785, res.Stats.BoxFill, 0.01, "π/4 of the box")
	assert.Zero(t, res.Stats.OutsideBox)
	assert.True(t, CheckMask(res.Stats, DefaultGates).OK)
	assert.False(t, CheckMask(res.Stats, DefaultGates).Hard)
	assert.Equal(t, FakeModels, res.Models)
	// Centre is car, corners of the box are not, the edge is soft.
	assert.Equal(t, uint8(255), res.Alpha.GrayAt(200, 150).Y)
	assert.Zero(t, res.Alpha.GrayAt(41, 46).Y)
	soft := 0
	for _, v := range res.Alpha.Pix {
		if v != 0 && v != 255 {
			soft++
		}
	}
	assert.Greater(t, soft, 500)

	// It composites.
	img, err := jpeg.Decode(bytes.NewReader(photo))
	require.NoError(t, err)
	_, rep, err := Render(img, res.Alpha, StyleGrey)
	require.NoError(t, err)
	assert.InDelta(t, res.Stats.Coverage, rep.Coverage, 0.01)

	// Errors and cancellation.
	_, err = FakeSegmenter{}.Mask(context.Background(), []byte("x"), box)
	assert.ErrorIs(t, err, ErrSegmenterRejected)
	_, err = FakeSegmenter{}.Mask(context.Background(), photo, Box{0.9, 0, 0.1, 1})
	assert.ErrorIs(t, err, ErrSegmenterRejected)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = FakeSegmenter{Delay: time.Minute}.Mask(ctx, photo, box)
	assert.Equal(t, context.DeadlineExceeded, err)
	assert.NoError(t, FakeSegmenter{}.Ready(context.Background()))
}

func TestEllipseMask(t *testing.T) {
	g := EllipseMask(100, 50, Box{0, 0, 1, 1})
	assert.Equal(t, uint8(255), g.GrayAt(50, 25).Y)
	assert.Zero(t, g.GrayAt(0, 0).Y)
	assert.Zero(t, g.GrayAt(99, 49).Y)
	assert.Equal(t, color.Gray{}, EllipseMask(10, 10, Box{0.5, 0.5, 0.5, 0.5}).GrayAt(5, 5), "empty box: empty mask")
}
