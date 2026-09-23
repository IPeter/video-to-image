package gemini

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genai"
)

// errKind is how the retry loop treats an error.
type errKind int

const (
	// kindTransient: retry; after the last attempt the error wraps ErrUnavailable.
	kindTransient errKind = iota + 1
	// kindFatal: the API refused the request or the file never became usable; retrying
	// cannot help. Not retried, wraps ErrUnavailable.
	kindFatal
	// kindLocal: a problem on our side (bad request construction, unreadable file). Not
	// retried and not ErrUnavailable.
	kindLocal
)

// kindError tags an error produced by this package with its errKind.
type kindError struct {
	kind errKind
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }
func (e *kindError) Unwrap() error { return e.err }

func localErrorf(format string, args ...any) error {
	return &kindError{kind: kindLocal, err: fmt.Errorf(format, args...)}
}

func fatalErrorf(format string, args ...any) error {
	return &kindError{kind: kindFatal, err: fmt.Errorf(format, args...)}
}

func transientErrorf(format string, args ...any) error {
	return &kindError{kind: kindTransient, err: fmt.Errorf(format, args...)}
}

// apiErrorText matches genai.APIError.Error() output. Files.Upload formats the error of
// its first request with %s, which drops the type, so the status code is recovered
// from the text.
var apiErrorText = regexp.MustCompile(`Error (\d{3}), Message:`)

// apiError extracts the SDK's APIError (returned by value; a pointer is accepted too).
func apiError(err error) (genai.APIError, bool) {
	var v genai.APIError
	if errors.As(err, &v) {
		return v, true
	}
	var p *genai.APIError
	if errors.As(err, &p) && p != nil {
		return *p, true
	}
	return genai.APIError{}, false
}

// httpStatus returns the HTTP status carried by err, if any.
func httpStatus(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	if ae, ok := apiError(err); ok && ae.Code != 0 {
		return ae.Code, true
	}
	if m := apiErrorText.FindStringSubmatch(err.Error()); m != nil {
		code, convErr := strconv.Atoi(m[1])
		if convErr == nil {
			return code, true
		}
	}
	return 0, false
}

// classify decides whether err is worth retrying. Errors without an HTTP status
// (network failures, timeouts, truncated or garbled responses) are transient.
func classify(err error) errKind {
	var ke *kindError
	if errors.As(err, &ke) {
		return ke.kind
	}
	if code, ok := httpStatus(err); ok {
		switch {
		case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests, code >= 500:
			return kindTransient
		case code >= 400:
			return kindFatal
		}
	}
	return kindTransient
}

// isGone reports whether a Files API error means the file no longer exists. The Gemini
// API answers 403 PERMISSION_DENIED ("... or it may not exist") for unknown or expired
// files as well as 404.
func isGone(err error) bool {
	code, ok := httpStatus(err)
	return ok && (code == http.StatusNotFound || code == http.StatusForbidden)
}

// describe is a short human label for an error's HTTP status, for messages.
func describe(err error) string {
	code, ok := httpStatus(err)
	if !ok {
		return "no HTTP status"
	}
	s := "HTTP " + strconv.Itoa(code)
	if ae, ok := apiError(err); ok && ae.Status != "" && !strings.Contains(ae.Status, strconv.Itoa(code)) {
		s += " " + ae.Status
	}
	return s
}

// retryHint returns the server's google.rpc.RetryInfo delay (sent with most 429s).
func retryHint(err error) time.Duration {
	ae, ok := apiError(err)
	if !ok {
		return 0
	}
	for _, d := range ae.Details {
		t, _ := d["@type"].(string)
		if !strings.HasSuffix(t, "google.rpc.RetryInfo") {
			continue
		}
		s, _ := d["retryDelay"].(string)
		if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
			return dur
		}
	}
	return 0
}

// backoff is the delay before retry number n (1-based): BackoffBase·2^(n-1) capped at
// MaxBackoff, with "equal jitter" (half fixed, half random), raised to the server's
// retry hint when that is longer (still capped at MaxBackoff).
func (c *Client) backoff(n int, hint time.Duration) time.Duration {
	d := c.backoffBase
	for i := 1; i < n && d < c.maxBackoff; i++ {
		d *= 2
	}
	d = min(d, c.maxBackoff)
	if half := d / 2; half > 0 {
		d = half + rand.N(half+1)
	}
	if hint > d {
		d = min(hint, c.maxBackoff)
	}
	return d
}

// sleepCtx waits for d or until ctx ends (returning ctx.Err()).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
