// Package gemini implements analyzer.Analyzer and analyzer.Sweeper on the Gemini
// Developer API through the official google.golang.org/genai SDK.
//
// Media. Video parts always go through the Files API: the file is uploaded, polled
// until ACTIVE and referenced as fileData (with videoMetadata.fps when requested).
// A Part.FileHandle from an earlier attempt is reused while the file is still ACTIVE,
// so retries and worker restarts do not re-upload the preview. Image parts without a
// handle are sent inline when small (Options.InlineImageMaxBytes, 4 MiB by default, and
// at most 12 MiB of raw inline data per request, which base64 grows to ~16 MB): inline
// costs the same tokens as a file reference but saves an upload round trip, the ACTIVE
// polling and the cleanup. Larger images fall back to the Files API.
//
// Errors and retries. HTTP 408/429/5xx, network errors and timeouts are retried up to
// Options.MaxRetries times with exponential backoff and jitter (honouring a server
// RetryInfo hint), reusing files uploaded by earlier attempts. After the last attempt
// the error wraps analyzer.ErrUnavailable. Other 4xx answers (400 bad request, 401/403
// key problems, 404 unknown model) are not retried, because repeating the same request
// cannot succeed, but they also wrap analyzer.ErrUnavailable so the job fails with
// MODEL_UNAVAILABLE instead of being retried by River; the message names the HTTP
// status. Problems on our side (unreadable media file, invalid schema) are returned
// without ErrUnavailable. Context cancellation returns ctx.Err() unwrapped. Files
// uploaded by a Generate call that ultimately fails are deleted before it returns,
// because the caller never learns their handles.
//
// Output limit. Request.MaxOutputTokens (when > 0) becomes generationConfig.
// maxOutputTokens, so a runaway or prompt-injected answer cannot be billed beyond it.
// An answer cut off at the limit (FinishReason MAX_TOKENS) is logged and returned as
// is; its incomplete JSON fails the pipeline's validation like any other bad output.
//
// Privacy. Request.SourceName is never sent. Uploads are streamed from an open file
// with a neutral display name ("v2i-<step>-<jobID>"), so not even the local file name
// leaves the process. The display-name prefix also scopes SweepFiles to this app's
// uploads.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/genai"

	"video-to-image/internal/analyzer"
)

// Defaults for Options fields left zero.
const (
	DefaultMaxRetries          = 2
	DefaultBackoffBase         = time.Second
	DefaultMaxBackoff          = 30 * time.Second
	DefaultFileActiveTimeout   = 3 * time.Minute
	DefaultPollInterval        = time.Second
	DefaultInlineImageMaxBytes = 4 << 20
)

// maxInlineBytesPerRequest bounds the raw inline image data of one request; images
// beyond it are uploaded instead. The Gemini API caps whole requests at ~20 MB and
// inline bytes travel base64-encoded (+33%), so 12 MiB leaves room for the prompt.
const maxInlineBytesPerRequest = 12 << 20

// cleanupTimeout bounds best-effort deletes that run after the caller's context ended.
const cleanupTimeout = 15 * time.Second

// DisplayNamePrefix starts the display name of every file this package uploads.
// SweepFiles only deletes files carrying it.
const DisplayNamePrefix = "v2i-"

// Options configures New.
type Options struct {
	APIKey string // required
	// BaseURL overrides the API endpoint (tests, proxies). Empty → SDK default
	// (https://generativelanguage.googleapis.com/, or GOOGLE_GEMINI_BASE_URL).
	BaseURL string
	// HTTPClient is used for all requests; nil → a plain *http.Client. Per-call timeouts
	// come from the caller's context.
	HTTPClient *http.Client
	Logger     *slog.Logger

	// MaxRetries is the number of retries after the first attempt for transient errors
	// (HTTP 408/429/5xx, network errors). 0 → DefaultMaxRetries; negative → no retries.
	MaxRetries int
	// BackoffBase is the first retry delay; it doubles per retry (with jitter) up to
	// MaxBackoff. 0 → DefaultBackoffBase / DefaultMaxBackoff.
	BackoffBase time.Duration
	MaxBackoff  time.Duration
	// FileActiveTimeout bounds the wait for an uploaded file to become ACTIVE.
	// 0 → DefaultFileActiveTimeout.
	FileActiveTimeout time.Duration
	// PollInterval is the delay between file state checks. 0 → DefaultPollInterval.
	PollInterval time.Duration
	// InlineImageMaxBytes: image parts up to this size are sent inline instead of via the
	// Files API. 0 → DefaultInlineImageMaxBytes; negative → always upload.
	InlineImageMaxBytes int64
}

// Client is the Gemini analyzer. It is safe for concurrent use.
type Client struct {
	api           api
	log           *slog.Logger
	maxRetries    int
	backoffBase   time.Duration
	maxBackoff    time.Duration
	activeTimeout time.Duration
	pollInterval  time.Duration
	inlineMax     int64 // -1 → never inline
	inlineBudget  int64 // raw inline bytes allowed per request
	now           func() time.Time
}

var (
	_ analyzer.Analyzer = (*Client)(nil)
	_ analyzer.Sweeper  = (*Client)(nil)
)

// New builds a client for the Gemini Developer API. It does not contact the network.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("gemini: API key is required")
	}
	cc := &genai.ClientConfig{
		APIKey:     opts.APIKey,
		Backend:    genai.BackendGeminiAPI,
		HTTPClient: opts.HTTPClient,
	}
	if opts.BaseURL != "" {
		cc.HTTPOptions.BaseURL = opts.BaseURL
	}
	gc, err := genai.NewClient(ctx, cc)
	if err != nil {
		// The SDK prints the whole ClientConfig (API key included) in some errors.
		return nil, fmt.Errorf("gemini: new client: %s", strings.ReplaceAll(err.Error(), opts.APIKey, "[redacted]"))
	}
	return newClient(&genaiAPI{c: gc}, opts), nil
}

// newClient applies option defaults around an API implementation (the SDK, or a fake
// in tests).
func newClient(a api, opts Options) *Client {
	c := &Client{
		api:           a,
		log:           opts.Logger,
		maxRetries:    opts.MaxRetries,
		backoffBase:   opts.BackoffBase,
		maxBackoff:    opts.MaxBackoff,
		activeTimeout: opts.FileActiveTimeout,
		pollInterval:  opts.PollInterval,
		inlineMax:     opts.InlineImageMaxBytes,
		inlineBudget:  maxInlineBytesPerRequest,
		now:           time.Now,
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	switch {
	case c.maxRetries == 0:
		c.maxRetries = DefaultMaxRetries
	case c.maxRetries < 0:
		c.maxRetries = 0
	}
	if c.backoffBase <= 0 {
		c.backoffBase = DefaultBackoffBase
	}
	if c.maxBackoff <= 0 {
		c.maxBackoff = max(DefaultMaxBackoff, c.backoffBase)
	}
	if c.activeTimeout <= 0 {
		c.activeTimeout = DefaultFileActiveTimeout
	}
	if c.pollInterval <= 0 {
		c.pollInterval = DefaultPollInterval
	}
	switch {
	case c.inlineMax == 0:
		c.inlineMax = DefaultInlineImageMaxBytes
	case c.inlineMax < 0:
		c.inlineMax = -1
	}
	return c
}

// Name returns "gemini".
func (c *Client) Name() string { return "gemini" }
