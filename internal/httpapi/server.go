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
	defaultStatusRatePerMinute  = 120
	defaultMaxConcurrentUploads = 4
	defaultMaxUploadsPerClient  = 2
	ipHashKeyBytes              = 32
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
	ScratchDir          string
	PresignDownloads    bool   // redirect downloads to storage.PresignGet URLs
	FFmpegVersion       string // reported by /healthz to X-Internal-Token callers
	Now                 func() time.Time
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

	// Upload body pacing and the scratch free-space probe; tests shrink/replace them.
	uploadReadWindow time.Duration
	uploadReadChunk  int64
	freeSpace        func(dir string) (uint64, error)
}

// New builds the server and registers every route.
func New(d Deps) *Server {
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
	r := chi.NewRouter()
	s := &Server{
		d:                d,
		router:           r,
		ipKey:            []byte(d.IPHashSalt),
		healthLimiter:    newKeyedLimiter(d.Now, defaultMaxLimiterEntries),
		tokenFailures:    newKeyedLimiter(d.Now, defaultMaxLimiterEntries),
		uploads:          newUploadSlots(d.MaxConcurrentUploads, d.MaxUploadsPerClient),
		uploadReadWindow: defaultUploadReadWindow,
		uploadReadChunk:  defaultUploadReadChunk,
		freeSpace:        diskFreeBytes,
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
	s := New(Deps{Registry: domain.NewRegistry(), IPHashSalt: "openapi", Logger: slog.New(slog.DiscardHandler)})
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

	s.documentRawOperations()
}

func (s *Server) registerRawRoutes(r chi.Router) {
	r.Post("/v1/domains/{domainKey}/jobs", s.createJob)
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
		Description: "multipart/form-data with one field `video`. Streams to storage, validates with ffprobe, enqueues.",
		Tags:        []string{"jobs"},
		Parameters:  []*huma.Param{domainParam},
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
