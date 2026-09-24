package studio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultSegmentTimeout bounds one Mask call (V2I_STUDIO_TIMEOUT's default).
const DefaultSegmentTimeout = 45 * time.Second

// Errors of the segmenter. Wrapped errors keep the cause (errors.Is works on both).
var (
	// ErrSegmenterUnavailable: the sidecar could not be reached (connection refused,
	// reset, DNS) or stayed busy / not ready (503) until the timeout. Nothing is wrong
	// with the photo; a later attempt may succeed.
	ErrSegmenterUnavailable = errors.New("studio: segmentation sidecar unavailable")
	// ErrSegmenterTimeout: the call did not finish within the per-photo timeout.
	ErrSegmenterTimeout = errors.New("studio: segmentation timed out")
	// ErrSegmenterRejected: the sidecar refused the input (400: bad image or box).
	ErrSegmenterRejected = errors.New("studio: segmentation input rejected")
	// ErrSegmenterBadResponse: the answer broke the contract (bad JSON, a mask of the
	// wrong size or type, another status).
	ErrSegmenterBadResponse = errors.New("studio: bad segmentation response")
)

// unavailableError is ErrSegmenterUnavailable with its cause. errors.Is matches both;
// Unavailable() serves callers that test for the method instead of the sentinel.
type unavailableError struct{ cause error }

func unavailable(cause error) error { return &unavailableError{cause: cause} }

func (e *unavailableError) Error() string {
	return ErrSegmenterUnavailable.Error() + ": " + e.cause.Error()
}
func (e *unavailableError) Unwrap() []error   { return []error{ErrSegmenterUnavailable, e.cause} }
func (e *unavailableError) Unavailable() bool { return true }

// Limits of one sidecar exchange.
const (
	maxResponseBytes = 96 << 20 // a noisy 8K mask PNG in base64 stays well below
	maxErrorBytes    = 4 << 10  // error bodies are logged, not parsed at length
	defaultRetry     = time.Second
	minRetryWait     = 50 * time.Millisecond // "Retry-After: 0" must not hammer the sidecar
	maxRetryWait     = 10 * time.Second      // one Retry-After wait, however long the sidecar asks
)

// HTTPSegmenter is the client of the segmentation sidecar (see Segmenter for the
// contract). It is safe for concurrent use; the sidecar itself serves one request at a
// time and answers 503 + Retry-After while busy, which Mask waits out within its
// timeout.
type HTTPSegmenter struct {
	base    string
	timeout time.Duration
	client  *http.Client
}

var _ Segmenter = (*HTTPSegmenter)(nil)

// NewHTTPSegmenter returns the client of the sidecar at baseURL (e.g.
// http://127.0.0.1:4100). timeout bounds one Mask call including 503 retries; client
// nil → a default client.
func NewHTTPSegmenter(baseURL string, timeout time.Duration, client *http.Client) Segmenter {
	return newHTTPSegmenter(baseURL, timeout, client)
}

func newHTTPSegmenter(baseURL string, timeout time.Duration, client *http.Client) *HTTPSegmenter {
	if timeout <= 0 {
		timeout = DefaultSegmentTimeout
	}
	if client == nil {
		client = &http.Client{} // the per-call context bounds every request
	}
	return &HTTPSegmenter{base: strings.TrimRight(baseURL, "/"), timeout: timeout, client: client}
}

// NewSegmenter interprets V2I_STUDIO_URL: "" → nil (the feature is off), "fake" → a
// FakeSegmenter, otherwise an HTTP(S) base URL of the sidecar.
func NewSegmenter(baseURL string, timeout time.Duration, client *http.Client) (Segmenter, error) {
	switch s := strings.TrimSpace(baseURL); s {
	case "":
		return nil, nil
	case "fake":
		return FakeSegmenter{}, nil
	default:
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
			return nil, fmt.Errorf("studio: segmenter URL %q is not an http(s) base URL", s)
		}
		return newHTTPSegmenter(s, timeout, client), nil
	}
}

// Readier is implemented by segmenters that can report readiness (models loaded and
// warmed up).
type Readier interface {
	Ready(ctx context.Context) error
}

// Ready asks GET /readyz: nil once the models are loaded and warmed up; an error
// wrapping ErrSegmenterUnavailable while the sidecar is starting or unreachable.
func (s *HTTPSegmenter) Ready(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/readyz", nil)
	if err != nil {
		return fmt.Errorf("studio: readiness request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return unavailable(err)
	}
	defer resp.Body.Close()
	defer discard(resp.Body) // runs first: lets the connection be reused
	if resp.StatusCode != http.StatusOK {
		return unavailable(fmt.Errorf("readyz answered %d", resp.StatusCode))
	}
	return nil
}

// maskResponse is the sidecar's 200 body.
type maskResponse struct {
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	MaskPNG   string            `json:"maskPng"`
	Stats     *MaskStats        `json:"stats"`
	TimingsMS map[string]int64  `json:"timingsMs"`
	Models    map[string]string `json:"models"`
}

// Mask posts the JPEG with the box and decodes the answer. Errors: see the Err*
// variables; a cancelled or expired caller context is returned as is.
func (s *HTTPSegmenter) Mask(ctx context.Context, jpegData []byte, box Box) (*MaskResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validBox(box) {
		return nil, fmt.Errorf("studio: invalid box %+v", box)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpegData))
	if err != nil {
		return nil, fmt.Errorf("studio: segmenter input is not a JPEG: %w", err)
	}
	cctx, cancel := context.WithTimeoutCause(ctx, s.timeout, ErrSegmenterTimeout)
	defer cancel()
	endpoint := s.base + "/v1/mask?box=" + url.QueryEscape(formatBox(box))

	for attempt := 1; ; attempt++ {
		res, wait, err := s.post(cctx, endpoint, jpegData, cfg)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil { // the caller gave up (shutdown, job budget)
			return nil, ctx.Err()
		}
		if cctx.Err() != nil {
			return nil, fmt.Errorf("%w after %s (attempt %d): %w", ErrSegmenterTimeout, s.timeout, attempt, err)
		}
		if wait < 0 { // not retryable
			return nil, err
		}
		// 503: busy or still loading. Wait as asked, but never past the deadline.
		busy := func() error {
			return unavailable(fmt.Errorf("still busy when the %s timeout ran out (attempt %d): %w", s.timeout, attempt, err))
		}
		wait = max(wait, minRetryWait)
		if dl, ok := cctx.Deadline(); ok && time.Until(dl) <= wait {
			return nil, busy()
		}
		t := time.NewTimer(wait)
		select {
		case <-cctx.Done():
			t.Stop()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, busy()
		case <-t.C:
		}
	}
}

// post makes one request. wait ≥ 0 means "retry after wait" (503); wait < 0 means the
// error is final.
func (s *HTTPSegmenter) post(ctx context.Context, endpoint string, body []byte, cfg image.Config) (*MaskResult, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, -1, fmt.Errorf("studio: segmenter request: %w", err)
	}
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, -1, ctx.Err()
		}
		return nil, -1, unavailable(err)
	}
	defer resp.Body.Close()
	defer discard(resp.Body) // runs first: lets the connection be reused
	switch resp.StatusCode {
	case http.StatusOK:
		res, err := decodeMaskResponse(io.LimitReader(resp.Body, maxResponseBytes+1), cfg)
		if err != nil {
			if ctx.Err() != nil {
				return nil, -1, ctx.Err()
			}
			return nil, -1, err
		}
		return res, 0, nil
	case http.StatusServiceUnavailable:
		return nil, retryAfter(resp.Header.Get("Retry-After")),
			fmt.Errorf("segmenter answered 503: %s", errorText(resp.Body))
	case http.StatusBadRequest:
		return nil, -1, fmt.Errorf("%w: %s", ErrSegmenterRejected, errorText(resp.Body))
	default:
		return nil, -1, fmt.Errorf("%w: status %d: %s", ErrSegmenterBadResponse, resp.StatusCode, errorText(resp.Body))
	}
}

// decodeMaskResponse parses and checks a 200 body against the input's size.
func decodeMaskResponse(r io.Reader, cfg image.Config) (*MaskResult, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %w", ErrSegmenterBadResponse, err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: body larger than %d bytes", ErrSegmenterBadResponse, maxResponseBytes)
	}
	var mr maskResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSegmenterBadResponse, err)
	}
	if mr.Width != cfg.Width || mr.Height != cfg.Height {
		return nil, fmt.Errorf("%w: answer is for %dx%d, the photo is %dx%d",
			ErrSegmenterBadResponse, mr.Width, mr.Height, cfg.Width, cfg.Height)
	}
	if mr.Stats == nil {
		return nil, fmt.Errorf("%w: no stats", ErrSegmenterBadResponse)
	}
	raw, err := base64.StdEncoding.DecodeString(mr.MaskPNG)
	if err != nil {
		return nil, fmt.Errorf("%w: maskPng is not base64: %w", ErrSegmenterBadResponse, err)
	}
	mc, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: maskPng: %w", ErrSegmenterBadResponse, err)
	}
	if mc.Width != cfg.Width || mc.Height != cfg.Height {
		return nil, fmt.Errorf("%w: mask is %dx%d, the photo is %dx%d",
			ErrSegmenterBadResponse, mc.Width, mc.Height, cfg.Width, cfg.Height)
	}
	m, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: maskPng: %w", ErrSegmenterBadResponse, err)
	}
	g, ok := m.(*image.Gray)
	if !ok {
		return nil, fmt.Errorf("%w: mask is %T, want an 8-bit grayscale PNG", ErrSegmenterBadResponse, m)
	}
	return &MaskResult{Alpha: g, Stats: *mr.Stats, TimingsMS: mr.TimingsMS, Models: mr.Models}, nil
}

// retryAfter parses a Retry-After header (seconds or an HTTP date), capped at
// maxRetryWait; missing or unparsable → defaultRetry.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultRetry
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
		return min(time.Duration(n*float64(time.Second)), maxRetryWait)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), maxRetryWait)
	}
	return defaultRetry
}

// errorText is the "error" field of a sidecar error body, or the start of the body.
func errorText(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxErrorBytes))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return s
	}
	return "(no message)"
}

// discard reads (a bounded part of) what is left of a body, so the connection can be
// reused.
func discard(r io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, maxErrorBytes))
}

func formatBox(b Box) string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
	return f(b.X0) + "," + f(b.Y0) + "," + f(b.X1) + "," + f(b.Y1)
}

// validBox reports whether b is a normalised rectangle: 0 ≤ X0 < X1 ≤ 1, 0 ≤ Y0 < Y1 ≤ 1.
func validBox(b Box) bool {
	return finite01(b.X0, b.Y0, b.X1, b.Y1) && b.X1 > b.X0 && b.Y1 > b.Y0
}
