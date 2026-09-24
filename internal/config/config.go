// Package config reads process configuration from the environment. Domain behaviour
// lives in internal/domain (YAML); this is only deployment wiring.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Analyzer implementations selectable via V2I_ANALYZER / --analyzer.
const (
	AnalyzerGemini    = "gemini"    // real model calls
	AnalyzerFake      = "fake"      // canned, deterministic, free (tests, e2e, local UI work)
	AnalyzerRecording = "recording" // real Gemini calls, responses written to V2I_RECORDINGS_DIR
	AnalyzerRecorded  = "recorded"  // replays responses from V2I_RECORDINGS_DIR, no network
)

type Config struct {
	HTTPAddr string `env:"V2I_HTTP_ADDR"         envDefault:":4000"`
	// AdminAddr serves /livez and /metrics (serve and work). Keep it off the public
	// Ingress: metrics expose cost and traffic figures.
	AdminAddr       string        `env:"V2I_ADMIN_ADDR" envDefault:":4001"`
	DatabaseURL     string        `env:"DATABASE_URL"`
	DomainsDir      string        `env:"V2I_DOMAINS_DIR"       envDefault:"configs/domains"`
	ScratchDir      string        `env:"V2I_SCRATCH_DIR"` // default: $TMPDIR/v2i
	LogLevel        string        `env:"V2I_LOG_LEVEL"         envDefault:"info"`
	LogFormat       string        `env:"V2I_LOG_FORMAT"        envDefault:"json"` // json | text
	ShutdownTimeout time.Duration `env:"V2I_SHUTDOWN_TIMEOUT"  envDefault:"30s"`

	Storage StorageConfig

	Analyzer            string        `env:"V2I_ANALYZER"               envDefault:"gemini"`
	GeminiAPIKey        string        `env:"GEMINI_API_KEY"`
	GeminiBaseURL       string        `env:"GEMINI_BASE_URL"` // override for tests / proxies
	RecordingsDir       string        `env:"V2I_RECORDINGS_DIR"         envDefault:"testdata/recordings"`
	FakeResponseFile    string        `env:"V2I_FAKE_RESPONSE_FILE"`         // optional canned JSON for the fake analyzer
	FakeDelay           time.Duration `env:"V2I_FAKE_DELAY" envDefault:"0s"` // simulated model latency (UI work)
	ModelCallsPerMinute int           `env:"V2I_MODEL_CALLS_PER_MINUTE" envDefault:"30"`
	// DailyCostCeilingUSD pauses the pipeline once today's model spend reaches it. The
	// default is deliberately non-zero (fail safe); set 0 only to disable on purpose.
	DailyCostCeilingUSD float64 `env:"DAILY_COST_CEILING_USD" envDefault:"5"`

	PipelineWorkers int `env:"V2I_PIPELINE_WORKERS" envDefault:"2"`

	InternalToken string `env:"V2I_INTERNAL_TOKEN"` // X-Internal-Token for eval CLI and /internal
	// IPHashSalt keys the HMAC of client IPs stored in jobs.client_ip_hash. Empty → a
	// random per-process key (hashes are then not comparable across restarts).
	IPHashSalt string `env:"V2I_IP_HASH_SALT"`
	// TrustedProxies lists CIDRs of reverse proxies (e.g. the Traefik pods / node
	// network). Forwarding headers are honoured only when the peer is inside one of
	// them, walking X-Forwarded-For from the right. Empty → use the TCP peer address.
	TrustedProxies []string `env:"V2I_TRUSTED_PROXIES" envSeparator:","`
	// ClientIPHeader is set by a CDN in front of the trusted proxies with the visitor
	// address (Cloudflare: CF-Connecting-IP). Honoured only from a trusted peer.
	ClientIPHeader string `env:"V2I_CLIENT_IP_HEADER"`
	// MaxRequestBytes is the largest request body the ingress in front of the API
	// accepts (e.g. 95 MiB behind Cloudflare, which rejects bodies over 100 MB). The
	// single-request upload is capped at it; larger videos go through chunked uploads,
	// whose chunks are kept below it. 0 = no cap.
	MaxRequestBytes int64 `env:"V2I_MAX_REQUEST_BYTES"`
	// UploadChunkBytes is the chunk size handed to chunked-upload clients.
	UploadChunkBytes int64 `env:"V2I_UPLOAD_CHUNK_BYTES" envDefault:"16777216"`
	// UploadSessionTTL is how long a chunked-upload session is kept after its last chunk.
	UploadSessionTTL time.Duration `env:"V2I_UPLOAD_SESSION_TTL" envDefault:"15m"`
	// MaxUploadSessionsPerClient caps open chunked-upload sessions per client.
	MaxUploadSessionsPerClient int `env:"V2I_MAX_UPLOAD_SESSIONS_PER_CLIENT" envDefault:"2"`
	StatusRatePerMinute        int `env:"V2I_STATUS_RATE_PER_MINUTE"  envDefault:"120"`
	// Upload concurrency caps: each upload spools up to maxUploadBytes to scratch.
	MaxConcurrentUploads int `env:"V2I_MAX_CONCURRENT_UPLOADS"  envDefault:"4"`
	MaxUploadsPerClient  int `env:"V2I_MAX_UPLOADS_PER_CLIENT"  envDefault:"2"`

	FFmpegPath  string `env:"FFMPEG_PATH"  envDefault:"ffmpeg"`
	FFprobePath string `env:"FFPROBE_PATH" envDefault:"ffprobe"`
	// FFmpegThreads bounds -threads/-filter_threads per ffmpeg process: memory and CPU
	// otherwise scale with the node's core count, not the pod's CPU limit. 0 = ffmpeg default.
	FFmpegThreads int `env:"FFMPEG_THREADS" envDefault:"2"`
	// ExtractConcurrency is how many ffmpeg processes one job's extract step runs at once.
	// 0 = auto: GOMAXPROCS / (FFMPEG_THREADS × V2I_PIPELINE_WORKERS), at least 1 (see
	// pipeline.ExtractConcurrency). Each 4K HEVC process holds about 450 MiB at 2 threads.
	ExtractConcurrency int `env:"V2I_EXTRACT_CONCURRENCY" envDefault:"0"`

	// StudioURL is the base URL of the studio-background segmentation sidecar (e.g.
	// http://127.0.0.1:4100, in the worker pod). "" turns studio backgrounds off in this
	// process; StudioFake uses the built-in fake segmenter (tests, e2e).
	StudioURL string `env:"V2I_STUDIO_URL"`
	// StudioTimeout bounds the segmentation of one photo (the sidecar call, including
	// its busy retries).
	StudioTimeout time.Duration `env:"V2I_STUDIO_TIMEOUT" envDefault:"45s"`
	// StudioWorkers is the studio queue's MaxWorkers in this process. The sidecar runs
	// one request at a time, so more than 1 only queues requests there.
	StudioWorkers int `env:"V2I_STUDIO_WORKERS" envDefault:"1"`
}

// StudioFake is the V2I_STUDIO_URL value that selects the fake segmenter.
const StudioFake = "fake"

// StudioEnabled reports whether this process makes studio backgrounds (a sidecar URL or
// the fake segmenter is configured).
func (c *Config) StudioEnabled() bool { return c.StudioURL != "" }

type StorageConfig struct {
	Backend          string `env:"V2I_STORAGE"           envDefault:"local"` // local | s3
	LocalDir         string `env:"V2I_STORAGE_DIR"       envDefault:"./data/storage"`
	S3Endpoint       string `env:"S3_ENDPOINT"`        // e.g. http://localhost:9000 (MinIO)
	S3PublicEndpoint string `env:"S3_PUBLIC_ENDPOINT"` // host used in presigned URLs; defaults to S3Endpoint
	S3Region         string `env:"S3_REGION"             envDefault:"us-east-1"`
	S3Bucket         string `env:"S3_BUCKET"             envDefault:"v2i"`
	S3AccessKeyID    string `env:"S3_ACCESS_KEY_ID"`
	S3SecretKey      string `env:"S3_SECRET_ACCESS_KEY"`
	S3UsePathStyle   bool   `env:"S3_USE_PATH_STYLE"     envDefault:"true"`
	// PresignDownloads redirects image/ZIP downloads to presigned S3 URLs instead of
	// streaming through the API. Only useful when S3PublicEndpoint is reachable by browsers.
	PresignDownloads bool `env:"V2I_PRESIGN_DOWNLOADS" envDefault:"false"`
}

// Load parses the environment. It does not check requirements that depend on the
// subcommand; call Require* for those.
func Load() (*Config, error) {
	trimEnv()
	var c Config
	if err := env.Parse(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.ScratchDir == "" {
		c.ScratchDir = filepath.Join(os.TempDir(), "v2i")
	}
	return &c, nil
}

// trimEnv strips surrounding whitespace from this program's variables. GNU make's
// `include .env` keeps the spaces before an inline "# comment", and a value like
// "false   " would otherwise fail to parse.
func trimEnv() {
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !ownVar(k) {
			continue
		}
		if t := strings.TrimSpace(v); t != v {
			_ = os.Setenv(k, t)
		}
	}
}

func ownVar(k string) bool {
	for _, p := range []string{"V2I_", "S3_", "GEMINI_", "DAILY_COST_", "FFMPEG_", "FFPROBE_"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return k == "DATABASE_URL"
}

// MinInternalTokenLen is the shortest V2I_INTERNAL_TOKEN accepted with a paid analyzer.
const MinInternalTokenLen = 24

// RequireSafeDefaults refuses settings that are fine on a laptop but dangerous on an
// internet-facing deployment with a paid model: a short internal token (it bypasses
// Origin checks and rate limits) or the example value from .env.example.
func (c *Config) RequireSafeDefaults() error {
	paid := c.Analyzer == AnalyzerGemini || c.Analyzer == AnalyzerRecording
	if !paid {
		return nil
	}
	if c.InternalToken != "" && (len(c.InternalToken) < MinInternalTokenLen || c.InternalToken == "dev-internal-token") {
		return fmt.Errorf("config: V2I_INTERNAL_TOKEN must be a random value of at least %d characters with analyzer %q (e.g. `openssl rand -hex 24`)", MinInternalTokenLen, c.Analyzer)
	}
	if c.StudioURL == StudioFake {
		// The fake segmenter cuts an ellipse out of the photo: real listings would get
		// studio versions with parts of the car missing.
		return fmt.Errorf("config: V2I_STUDIO_URL=%s is for tests only and must not be used with analyzer %q", StudioFake, c.Analyzer)
	}
	return nil
}

// RequireStudio validates the studio-background settings of a worker process:
// V2I_STUDIO_URL empty, StudioFake or an absolute http(s) URL, a positive timeout and at
// least one worker.
func (c *Config) RequireStudio() error {
	if c.StudioURL != "" && c.StudioURL != StudioFake {
		u, err := url.Parse(c.StudioURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: V2I_STUDIO_URL %q is not an http(s) URL (or %q, or empty to turn studio backgrounds off)", c.StudioURL, StudioFake)
		}
	}
	if c.StudioTimeout <= 0 {
		return fmt.Errorf("config: V2I_STUDIO_TIMEOUT must be positive (got %s)", c.StudioTimeout)
	}
	if c.StudioWorkers < 1 {
		return fmt.Errorf("config: V2I_STUDIO_WORKERS must be at least 1 (got %d)", c.StudioWorkers)
	}
	return nil
}

// RequireDatabase fails fast when DATABASE_URL is missing.
func (c *Config) RequireDatabase() error {
	if c.DatabaseURL == "" {
		return errors.New("config: DATABASE_URL is required")
	}
	return nil
}

// RequireAnalyzer validates the analyzer selection and its credentials.
func (c *Config) RequireAnalyzer() error {
	switch c.Analyzer {
	case AnalyzerGemini, AnalyzerRecording:
		if c.GeminiAPIKey == "" {
			return fmt.Errorf("config: GEMINI_API_KEY is required for analyzer %q", c.Analyzer)
		}
	case AnalyzerFake, AnalyzerRecorded:
	default:
		return fmt.Errorf("config: unknown analyzer %q (want gemini, fake, recording or recorded)", c.Analyzer)
	}
	return nil
}

// RequireStorage validates the storage backend settings.
func (c *Config) RequireStorage() error {
	switch c.Storage.Backend {
	case "local":
		if c.Storage.LocalDir == "" {
			return errors.New("config: V2I_STORAGE_DIR is required for local storage")
		}
	case "s3":
		if c.Storage.S3Bucket == "" {
			return errors.New("config: S3_BUCKET is required for s3 storage")
		}
	default:
		return fmt.Errorf("config: unknown storage backend %q (want local or s3)", c.Storage.Backend)
	}
	return nil
}
