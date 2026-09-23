// Package httpapi is `v2i serve`: Huma v2 on chi. JSON routes are typed Huma
// operations; the streaming multipart upload and the binary downloads are raw chi
// routes documented manually in the same OpenAPI document (served at /openapi.json).
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"video-to-image/internal/domain"
	"video-to-image/internal/media"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

// APIVersion is the OpenAPI info.version.
const APIVersion = "1.0.0"

// JobStore is the subset of *store.Store the API uses (fakes implement it in tests).
type JobStore interface {
	Ping(ctx context.Context) error
	CreateJob(ctx context.Context, j store.NewJob, enqueue func(ctx context.Context, tx pgx.Tx) error) error
	GetJob(ctx context.Context, id string) (*store.Job, error)
	ListSteps(ctx context.Context, jobID string) ([]store.Step, error)
	ListSelectedImages(ctx context.Context, jobID string) ([]store.SelectedImage, error)
	GetSelectedImage(ctx context.Context, jobID, imageID string) (*store.SelectedImage, error)
	ExpireJob(ctx context.Context, id string) error
	UsageReport(ctx context.Context, from, to time.Time, domainKey string) ([]store.UsageRow, error)
	JobUsage(ctx context.Context, jobID string) ([]store.UsageByStep, error)
}

// Enqueuer inserts the River pipeline job inside the job-creation transaction.
type Enqueuer interface {
	EnqueuePipelineTx(ctx context.Context, tx pgx.Tx, jobID string) error
}

// Prober reads video metadata (media.FFmpeg in production).
type Prober interface {
	Probe(ctx context.Context, path string) (media.VideoInfo, error)
}

// Defaults for zero Deps fields.
const (
	defaultStatusRatePerMinute        = 120
	defaultMaxConcurrentUploads       = 4
	defaultMaxUploadsPerClient        = 2
	defaultChunkSizeBytes             = 16 << 20
	defaultUploadSessionTTL           = 15 * time.Minute
	defaultMaxUploadSessionsPerClient = 2
	// chunkRequestHeadroom is what a chunk request keeps free below MaxRequestBytes
	// (headers, and proxies that count them against the body limit).
	chunkRequestHeadroom = 1 << 20
	ipHashKeyBytes       = 32
)

type Deps struct {
	Registry *domain.Registry
	Store    JobStore
	Storage  storage.Storage
	Prober   Prober
	Enqueuer Enqueuer
	Logger   *slog.Logger

	InternalToken string // X-Internal-Token; empty disables token auth
	// IPHashSalt keys the HMAC stored as jobs.client_ip_hash. Empty → a random key per
	// process (New logs a warning): hashes are then not comparable across restarts.
	IPHashSalt string
	// TrustedProxies are the reverse proxies whose X-Forwarded-For / X-Real-IP headers
	// are believed (see clientAddr; build it with ParseTrustedProxies). Empty → the TCP
	// peer is the client.
	TrustedProxies []netip.Prefix
	// ClientIPHeader names a header carrying the visitor address set by the CDN in
	// front of the trusted proxies (e.g. "CF-Connecting-IP"). Honoured only from a
	// trusted peer; "" disables it.
	ClientIPHeader      string
	StatusRatePerMinute int // per client per domain for GET job / file routes
	// MaxConcurrentUploads caps uploads being received at once, across all clients
	// (each spools up to maxUploadBytes to ScratchDir). Beyond it: 503 SERVER_BUSY.
	MaxConcurrentUploads int
	// MaxUploadsPerClient caps one client's concurrent uploads. Beyond it: 429.
	MaxUploadsPerClient int
	// MaxRequestBytes is the largest request body the ingress in front of the API
	// accepts (Cloudflare Free/Pro: 100 MB); 0 = no cap. POST /jobs then refuses a
	// body or file above min(domain limit, MaxRequestBytes) with 413 UPLOAD_TOO_LARGE,
	// and larger videos (up to the domain limit) take the chunked upload, whose chunks
	// stay chunkRequestHeadroom below it.
	MaxRequestBytes int64
	// ChunkSizeBytes is the chunk size given to new chunked uploads (default 16 MiB),
	// clamped to MaxRequestBytes - 1 MiB when that is set.
	ChunkSizeBytes int64
	// UploadSessionTTL drops a chunked upload, with its chunks, once it has been idle
	// this long after a chunk was stored (default 15 min). Every request on the
	// session extends it. A session with no chunk stored yet is dropped after 5 min
	// idle (or this TTL when shorter).
	UploadSessionTTL time.Duration
	// MaxUploadSessionsPerClient caps one client's open chunked uploads (default 2).
	// A client at the cap that starts another gets its own longest-idle session
	// evicted when that one has been idle for a minute with no request in flight, else
	// 429 (Retry-After: until one can be evicted). Every open session also holds one
	// of MaxConcurrentUploads (disk safety); when they are all taken, the longest-idle
	// session of anyone idle for 5 minutes is evicted, else 503.
	MaxUploadSessionsPerClient int
	// ScratchDir holds the upload spool files and the chunked-upload sessions
	// (<ScratchDir>/uploads). Only when it is set does New restore the sessions a
	// previous process left there. Empty → $TMPDIR/v2i.
	ScratchDir       string
	PresignDownloads bool   // redirect downloads to storage.PresignGet URLs
	FFmpegVersion    string // reported by /healthz to X-Internal-Token callers
	Now              func() time.Time
}

// Server holds the router and the Huma API.
type Server struct {
	d      Deps
	router chi.Router
	api    huma.API

	ipKey         []byte        // HMAC key of ipHash
	healthLimiter *keyedLimiter // /healthz per client
	tokenFailures *keyedLimiter // wrong X-Internal-Token values per client
	uploads       *uploadSlots
	sessions      *uploadSessions // chunked uploads

	// Upload body pacing, the scratch free-space probe and how long complete waits for
	// superseded chunk PUTs; tests shrink/replace them.
	uploadReadWindow time.Duration
	uploadReadChunk  int64
	freeSpace        func(dir string) (uint64, error)
	staleWriterWait  time.Duration
}

// New builds the server and registers every route. It restores the chunked uploads
// a previous process left in Deps.ScratchDir; Close stops their expiry sweeper.
func New(d Deps) *Server {
	s := newServer(d)
	if d.ScratchDir != "" {
		s.loadUploadSessions()
	}
	return s
}

// Close stops the background expiry of chunked uploads (their files stay on disk for
// the next process). It does not affect requests in flight and may be called twice.
func (s *Server) Close() { s.sessions.stopSweeper() }

// newServer is New without touching the scratch directory.
func newServer(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.StatusRatePerMinute <= 0 {
		d.StatusRatePerMinute = defaultStatusRatePerMinute
	}
	if d.MaxConcurrentUploads <= 0 {
		d.MaxConcurrentUploads = defaultMaxConcurrentUploads
	}
	if d.MaxUploadsPerClient <= 0 {
		d.MaxUploadsPerClient = defaultMaxUploadsPerClient
	}
	if d.MaxUploadSessionsPerClient <= 0 {
		d.MaxUploadSessionsPerClient = defaultMaxUploadSessionsPerClient
	}
	if d.UploadSessionTTL <= 0 {
		d.UploadSessionTTL = defaultUploadSessionTTL
	}
	if d.MaxRequestBytes < 0 {
		d.MaxRequestBytes = 0
	}
	d.ChunkSizeBytes = effectiveChunkSize(d.ChunkSizeBytes, d.MaxRequestBytes)
	r := chi.NewRouter()
	s := &Server{
		d:                d,
		router:           r,
		ipKey:            []byte(d.IPHashSalt),
		healthLimiter:    newKeyedLimiter(d.Now, defaultMaxLimiterEntries),
		tokenFailures:    newKeyedLimiter(d.Now, defaultMaxLimiterEntries),
		uploads:          newUploadSlots(d.MaxConcurrentUploads, d.MaxUploadsPerClient, d.MaxUploadSessionsPerClient),
		sessions:         newUploadSessions(),
		uploadReadWindow: defaultUploadReadWindow,
		uploadReadChunk:  defaultUploadReadChunk,
		freeSpace:        diskFreeBytes,
		staleWriterWait:  defaultStaleWriterWait,
	}
	if len(s.ipKey) == 0 {
		s.ipKey = make([]byte, ipHashKeyBytes)
		_, _ = rand.Read(s.ipKey) // never fails (crypto/rand panics instead)
		d.Logger.Warn("httpapi: V2I_IP_HASH_SALT is not set; using a random per-process key, " +
			"so client_ip_hash values are not comparable across restarts or replicas")
	}
	s.installMiddleware(r)

	cfg := huma.DefaultConfig("v2i API", APIVersion)
	cfg.Info.Description = "Video-to-image backend. One API, many domain frontends. See plan/05-api.md."
	cfg.CreateHooks = nil // no $schema link transformer: keep bodies exactly as documented
	s.api = humachi.New(r, cfg)

	s.registerOperations()
	s.registerRawRoutes(r)
	return s
}

// Handler is the root http.Handler.
func (s *Server) Handler() http.Handler { return s.router }

// OpenAPI returns the generated document.
func (s *Server) OpenAPI() *huma.OpenAPI { return s.api.OpenAPI() }

// OpenAPISpec returns the indented OpenAPI 3.1 JSON without needing any dependency.
// `v2i openapi` writes it to api/openapi.json for the frontend type generation.
func OpenAPISpec() ([]byte, error) {
	s := newServer(Deps{Registry: domain.NewRegistry(), IPHashSalt: "openapi", Logger: slog.New(slog.DiscardHandler)})
	return json.MarshalIndent(s.OpenAPI(), "", "  ")
}

// ---- path/query inputs ----

type DomainPathInput struct {
	DomainKey string `path:"domainKey" doc:"Domain key, e.g. cars." example:"cars"`
}

type JobPathInput struct {
	DomainKey string `path:"domainKey" doc:"Domain key, e.g. cars." example:"cars"`
	JobID     string `path:"jobId" doc:"Job ID (job_ + ULID)." example:"job_01J8ZK3M2Q7K2M9QXA4B5C6D7E"`
}

type UsageInput struct {
	Token     string `header:"X-Internal-Token" doc:"Internal token (V2I_INTERNAL_TOKEN)."`
	From      string `query:"from" doc:"Start day (YYYY-MM-DD, UTC, inclusive). Default: 7 days ago."`
	To        string `query:"to" doc:"End day (YYYY-MM-DD, UTC, inclusive). Default: today."`
	DomainKey string `query:"domainKey" doc:"Restrict to one domain."`
}

func (s *Server) registerOperations() {
	huma.Register(s.api, huma.Operation{
		OperationID: "getDomain",
		Method:      http.MethodGet,
		Path:        "/v1/domains/{domainKey}",
		Summary:     "Public domain config",
		Description: "Limits, shot list, image count and copy for the frontend. Cached for 5 minutes.",
		Tags:        []string{"domains"},
		Errors:      []int{http.StatusForbidden, http.StatusNotFound},
	}, s.getDomain)

	huma.Register(s.api, huma.Operation{
		OperationID: "getJob",
		Method:      http.MethodGet,
		Path:        "/v1/domains/{domainKey}/jobs/{jobId}",
		Summary:     "Job status and result",
		Description: "Poll every 1.5 s while queued/processing. 410 EXPIRED once files are cleaned up.",
		Tags:        []string{"jobs"},
		Errors:      []int{http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusTooManyRequests},
	}, s.getJob)

	huma.Register(s.api, huma.Operation{
		OperationID:   "deleteJob",
		Method:        http.MethodDelete,
		Path:          "/v1/domains/{domainKey}/jobs/{jobId}",
		Summary:       "Delete a job's files now",
		Description:   "User-initiated early deletion. Removes files and sets status expired.",
		Tags:          []string{"jobs"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound},
	}, s.deleteJob)

	huma.Register(s.api, huma.Operation{
		OperationID: "getUsage",
		Method:      http.MethodGet,
		Path:        "/internal/usage",
		Summary:     "Token and cost totals per domain per day",
		Tags:        []string{"internal"},
		Errors:      []int{http.StatusBadRequest, http.StatusUnauthorized},
	}, s.getUsage)

	huma.Register(s.api, huma.Operation{
		OperationID: "createUpload",
		Method:      http.MethodPost,
		Path:        "/v1/domains/{domainKey}/uploads",
		Summary:     "Start a chunked upload",
		Description: "For files larger than one request may be (e.g. behind Cloudflare's 100 MB body limit). PUT the chunks, then complete. " +
			"sizeBytes may be up to the domain's maxUploadBytes (else 413 UPLOAD_TOO_LARGE). Counts against the upload rate limit. " +
			"An open upload holds an upload slot until it is completed, cancelled or idle past expiresAt (5 minutes before its first chunk, " +
			"then the session TTL after its last request). A client that already has its maximum of open uploads gets its own longest-idle one " +
			"(idle for 1 minute, no request in flight) cancelled to make room, else 429 RATE_LIMITED; when all upload slots are taken, " +
			"the longest-idle upload of anyone idle for 5 minutes is cancelled, else 503 SERVER_BUSY. Retry-After says when one can be.",
		Tags:          []string{"uploads"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusTooManyRequests, http.StatusServiceUnavailable},
	}, s.createUpload)

	huma.Register(s.api, huma.Operation{
		OperationID: "completeUpload",
		Method:      http.MethodPost,
		Path:        "/v1/domains/{domainKey}/uploads/{uploadId}/complete",
		Summary:     "Finish a chunked upload and start the job",
		Description: "Validates the assembled file exactly like POST /jobs (format, decode, duration, resolution) and enqueues the job. " +
			"409 UPLOAD_INCOMPLETE lists the missing chunks in details (an older PUT of a chunk that a newer PUT already stored is waited for, " +
			"at most a few seconds, and then cut off). Afterwards the upload is gone; calling complete again returns the same outcome, " +
			"except after a 500, which keeps the chunks so complete can be retried.",
		Tags:          []string{"uploads"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusTooManyRequests, http.StatusServiceUnavailable},
	}, s.completeUpload)

	huma.Register(s.api, huma.Operation{
		OperationID:   "abortUpload",
		Method:        http.MethodDelete,
		Path:          "/v1/domains/{domainKey}/uploads/{uploadId}",
		Summary:       "Cancel a chunked upload",
		Tags:          []string{"uploads"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound},
	}, s.abortUpload)

	s.documentRawOperations()
}

func (s *Server) registerRawRoutes(r chi.Router) {
	r.Post("/v1/domains/{domainKey}/jobs", s.createJob)
	r.Put("/v1/domains/{domainKey}/uploads/{uploadId}/chunks/{index}", s.putUploadChunk)
	r.Get("/v1/domains/{domainKey}/jobs/{jobId}/images/{imageId}", s.getImage)
	r.Get("/v1/domains/{domainKey}/jobs/{jobId}/images/{imageId}/thumb", s.getThumbnail)
	r.Get("/v1/domains/{domainKey}/jobs/{jobId}/download.zip", s.downloadZip)
	// Probes stay on the API listener (plan/05). /metrics does not: it is served on
	// the admin listener (V2I_ADMIN_ADDR), which no Ingress exposes.
	r.Get("/healthz", s.healthz)
	r.Get("/livez", s.livez)
}

// documentRawOperations adds the chi-served routes to the OpenAPI document so the
// generated TypeScript client knows their responses and error codes.
func (s *Server) documentRawOperations() {
	oapi := s.api.OpenAPI()
	reg := oapi.Components.Schemas
	errSchema := reg.Schema(reflect.TypeOf(APIError{}), true, "Error")
	createResp := reg.Schema(reflect.TypeOf(CreateJobResponse{}), true, "CreateJobResponse")

	errResp := func(desc string) *huma.Response {
		return &huma.Response{Description: desc, Content: map[string]*huma.MediaType{
			"application/json": {Schema: errSchema},
		}}
	}
	domainParam := &huma.Param{Name: "domainKey", In: "path", Required: true, Schema: &huma.Schema{Type: huma.TypeString}, Example: "cars"}
	jobParam := &huma.Param{Name: "jobId", In: "path", Required: true, Schema: &huma.Schema{Type: huma.TypeString}}
	imageParam := &huma.Param{Name: "imageId", In: "path", Required: true, Schema: &huma.Schema{Type: huma.TypeString}}
	downloadParam := &huma.Param{Name: "download", In: "query", Schema: &huma.Schema{Type: huma.TypeString, Enum: []any{"1"}},
		Description: "1 → Content-Disposition: attachment with the listing file name."}
	jpeg := func(desc string) *huma.Response {
		return &huma.Response{Description: desc, Content: map[string]*huma.MediaType{
			"image/jpeg": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}},
		}}
	}
	redirect := &huma.Response{Description: "Redirect to a presigned storage URL (S3 deployments)."}
	notModified := &huma.Response{Description: "Not modified (If-None-Match matched the ETag)."}

	oapi.AddOperation(&huma.Operation{
		OperationID: "createJob",
		Method:      http.MethodPost,
		Path:        "/v1/domains/{domainKey}/jobs",
		Summary:     "Upload a video and start a job",
		Description: "multipart/form-data with one field `video`. Streams to storage, validates with ffprobe, enqueues. " +
			"A body above what one request may carry in this deployment is 413 UPLOAD_TOO_LARGE with details.maxRequestBytes: send such a video as a chunked upload (createUpload).",
		Tags:       []string{"jobs"},
		Parameters: []*huma.Param{domainParam},
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
			"multipart/form-data": {Schema: &huma.Schema{
				Type:       huma.TypeObject,
				Required:   []string{"video"},
				Properties: map[string]*huma.Schema{"video": {Type: huma.TypeString, Format: "binary"}},
			}},
		}},
		Responses: map[string]*huma.Response{
			"202": {Description: "Accepted", Content: map[string]*huma.MediaType{"application/json": {Schema: createResp}}},
			"400": errResp("UNSUPPORTED_FORMAT | CANNOT_DECODE | TOO_LONG | TOO_SHORT | RESOLUTION_TOO_LOW | RESOLUTION_TOO_HIGH | INVALID_REQUEST"),
			"403": errResp("ORIGIN_NOT_ALLOWED"),
			"404": errResp("DOMAIN_NOT_FOUND"),
			"413": errResp("UPLOAD_TOO_LARGE"),
			"429": errResp("RATE_LIMITED"),
			"500": errResp("INTERNAL_ERROR"),
			"503": errResp("SERVER_BUSY"),
		},
	})

	oapi.AddOperation(&huma.Operation{
		OperationID: "putUploadChunk",
		Method:      http.MethodPut,
		Path:        "/v1/domains/{domainKey}/uploads/{uploadId}/chunks/{index}",
		Summary:     "Upload one chunk",
		Description: "Raw bytes (application/octet-stream) of chunk index (0-based). Re-sending a chunk replaces it. Every chunk but the last must be exactly chunkSizeBytes: " +
			"a Content-Length must match it, and a body without one (chunked transfer) must end after exactly that many bytes (else 400 with details " +
			"{index, expectedBytes, contentLength}). Chunks may be sent in any order and in parallel. A refused chunk's body is read before the answer, " +
			"so the client can read it.",
		Tags: []string{"uploads"},
		Parameters: []*huma.Param{domainParam,
			{Name: "uploadId", In: "path", Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
			{Name: "index", In: "path", Required: true, Schema: &huma.Schema{Type: huma.TypeInteger, Minimum: ptrFloat(0)}}},
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
			"application/octet-stream": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}},
		}},
		Responses: map[string]*huma.Response{
			"204": {Description: "Chunk stored"},
			"400": errResp("INVALID_REQUEST (bad index or size)"),
			"403": errResp("ORIGIN_NOT_ALLOWED"),
			"404": errResp("DOMAIN_NOT_FOUND | UPLOAD_NOT_FOUND"),
			"413": errResp("UPLOAD_TOO_LARGE"),
			"429": errResp("RATE_LIMITED"),
			"500": errResp("INTERNAL_ERROR"),
			"503": errResp("SERVER_BUSY"),
		},
	})

	for _, op := range []struct{ id, path, summary string }{
		{"getImage", "/v1/domains/{domainKey}/jobs/{jobId}/images/{imageId}", "Full-resolution JPG"},
		{"getThumbnail", "/v1/domains/{domainKey}/jobs/{jobId}/images/{imageId}/thumb", "640 px thumbnail JPG"},
	} {
		oapi.AddOperation(&huma.Operation{
			OperationID: op.id,
			Method:      http.MethodGet,
			Path:        op.path,
			Summary:     op.summary,
			Tags:        []string{"files"},
			Parameters:  []*huma.Param{domainParam, jobParam, imageParam, downloadParam},
			Responses: map[string]*huma.Response{
				"200": jpeg("JPEG image"),
				"302": redirect,
				"304": notModified,
				"403": errResp("ORIGIN_NOT_ALLOWED"),
				"404": errResp("DOMAIN_NOT_FOUND | JOB_NOT_FOUND | IMAGE_NOT_FOUND"),
				"410": errResp("EXPIRED"),
				"429": errResp("RATE_LIMITED"),
				"500": errResp("INTERNAL_ERROR"),
			},
		})
	}

	oapi.AddOperation(&huma.Operation{
		OperationID: "downloadZip",
		Method:      http.MethodGet,
		Path:        "/v1/domains/{domainKey}/jobs/{jobId}/download.zip",
		Summary:     "All images + manifest.json as a ZIP",
		Tags:        []string{"files"},
		Parameters:  []*huma.Param{domainParam, jobParam},
		Responses: map[string]*huma.Response{
			"200": {Description: "ZIP archive", Content: map[string]*huma.MediaType{
				"application/zip": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}},
			}},
			"302": redirect,
			"304": notModified,
			"403": errResp("ORIGIN_NOT_ALLOWED"),
			"404": errResp("DOMAIN_NOT_FOUND | JOB_NOT_FOUND | NOT_FOUND"),
			"409": errResp("NOT_READY"),
			"410": errResp("EXPIRED"),
			"429": errResp("RATE_LIMITED"),
			"500": errResp("INTERNAL_ERROR"),
		},
	})
}

func ptrFloat(v float64) *float64 { return &v }

// effectiveChunkSize applies the default and the MaxRequestBytes clamp.
func effectiveChunkSize(size, maxRequest int64) int64 {
	if size <= 0 {
		size = defaultChunkSizeBytes
	}
	if maxRequest > 0 {
		limit := maxRequest - chunkRequestHeadroom
		if limit <= 0 { // not a real deployment; keep chunks below the cap anyway
			limit = max(maxRequest/2, 1)
		}
		size = min(size, limit)
	}
	return size
}
