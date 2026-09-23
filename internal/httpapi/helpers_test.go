package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

const (
	testOrigin     = "http://localhost:3000"
	testToken      = "s3cret-internal-token"
	testSalt       = "test-salt"
	testMaxUpload  = 1 << 20 // 1 MiB keeps the size tests cheap
	testRemoteAddr = "192.0.2.10:54321"
)

var testNow = time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

// ---- domains ----

func testDomain(key string, origins ...string) *domain.Config {
	return &domain.Config{
		Key:            key,
		Name:           strings.ToUpper(key[:1]) + key[1:] + " Photos",
		AllowedOrigins: origins,
		Limits: domain.Limits{
			MaxUploadBytes:     testMaxUpload,
			MaxDurationSec:     180,
			MinDurationSec:     5,
			MinShortSidePx:     480,
			MaxCandidateFrames: 300,
			RatePerIPPerHour:   1000,
		},
		Model: domain.Model{Name: "secret-model-" + key, PreviewHeight: 480, PreviewFPS: 2},
		Gate: domain.Gate{
			Subject:      "a " + key,
			Instructions: "SECRET GATE INSTRUCTIONS for " + key,
		},
		Shots: []domain.Shot{
			{ID: "front_three_quarter", Label: "Front 3/4", Description: "hero", Required: true},
			{ID: "side_profile", Label: "Side profile", Description: "side", Required: true},
			{ID: "rear_three_quarter", Label: "Rear 3/4", Description: "rear", Required: true},
			{ID: "interior", Label: "Interior", Description: "inside"},
		},
		Selection: domain.Selection{
			ImageCount:   8,
			Instructions: "SECRET SELECTION PROMPT",
			MaxScenes:    12,
			SampleFPS:    4,
			Weights:      domain.Weights{Relevance: 0.5, Sharpness: 0.4, Exposure: 0.1},
		},
		RetentionHours: 24,
		Copy:           domain.Copy{UploadTitle: "Upload", UploadHint: "Walk around", RejectedTitle: "Nope"},
	}
}

// ---- clock ----

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ---- store ----

type fakeTx struct {
	pgx.Tx // nil: calling any method panics, the fakes only compare identity
	jobID  string
}

type usageCall struct {
	from, to  time.Time
	domainKey string
}

type fakeStore struct {
	mu     sync.Mutex
	now    func() time.Time
	jobs   map[string]*store.Job
	steps  map[string][]store.Step
	images map[string][]store.SelectedImage

	usageRows  []store.UsageRow
	usageCalls []usageCall
	usageErr   error

	created     []store.NewJob
	enqueueTx   pgx.Tx
	expired     []string
	getJobCalls int

	createErr error
	pingErr   error
	pingHang  bool
	getJobErr error
	expireErr error
	getPanic  bool
	// beforeExpire runs (without the lock) at the start of ExpireJob: a worker
	// finishing the job concurrently.
	beforeExpire func(id string)
}

func newFakeStore(now func() time.Time) *fakeStore {
	return &fakeStore{
		now:    now,
		jobs:   map[string]*store.Job{},
		steps:  map[string][]store.Step{},
		images: map[string][]store.SelectedImage{},
	}
}

func (f *fakeStore) Ping(ctx context.Context) error {
	f.mu.Lock()
	hang, err := f.pingHang, f.pingErr
	f.mu.Unlock()
	if hang {
		time.Sleep(10 * time.Second) // ignores ctx on purpose
	}
	return err
}

func (f *fakeStore) CreateJob(ctx context.Context, j store.NewJob, enqueue func(ctx context.Context, tx pgx.Tx) error) error {
	f.mu.Lock()
	err := f.createErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	tx := &fakeTx{jobID: j.ID}
	if err := enqueue(ctx, tx); err != nil {
		return fmt.Errorf("fake store: enqueue: %w", err) // the real store rolls back
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueueTx = tx
	f.created = append(f.created, j)
	f.jobs[j.ID] = &store.Job{
		ID: j.ID, DomainKey: j.DomainKey, Status: store.StatusQueued,
		CreatedAt: f.now(), UpdatedAt: f.now(), ExpiresAt: j.ExpiresAt,
		ClientIPHash: j.ClientIPHash, VideoPath: j.VideoPath, VideoInfo: j.VideoInfo, ModelName: j.ModelName,
	}
	for _, n := range store.StepNames {
		f.steps[j.ID] = append(f.steps[j.ID], store.Step{JobID: j.ID, Name: n, Status: store.StepPending, Meta: map[string]any{}})
	}
	return nil
}

func (f *fakeStore) GetJob(ctx context.Context, id string) (*store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getJobCalls++
	if f.getPanic {
		panic("fake store: boom")
	}
	if f.getJobErr != nil {
		return nil, f.getJobErr
	}
	j, ok := f.jobs[id]
	if !ok {
		return nil, fmt.Errorf("fake store: get job %s: %w", id, store.ErrNotFound)
	}
	cp := *j
	return &cp, nil
}

func (f *fakeStore) ListSteps(ctx context.Context, jobID string) ([]store.Step, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Step(nil), f.steps[jobID]...), nil
}

func (f *fakeStore) ListSelectedImages(ctx context.Context, jobID string) ([]store.SelectedImage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.SelectedImage(nil), f.images[jobID]...), nil
}

func (f *fakeStore) GetSelectedImage(ctx context.Context, jobID, imageID string) (*store.SelectedImage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, img := range f.images[jobID] {
		if img.ID == imageID {
			cp := img
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("fake store: image %s: %w", imageID, store.ErrNotFound)
}

func (f *fakeStore) ExpireJob(ctx context.Context, id string) error {
	f.mu.Lock()
	hook := f.beforeExpire
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.expireErr != nil {
		return f.expireErr
	}
	j, ok := f.jobs[id]
	if !ok {
		return fmt.Errorf("fake store: expire %s: %w", id, store.ErrNotFound)
	}
	f.expired = append(f.expired, id)
	j.Status, j.VideoPath, j.ZipPath = store.StatusExpired, "", ""
	for i := range f.images[id] {
		f.images[id][i].ImagePath, f.images[id][i].ThumbPath = "", ""
	}
	return nil
}

func (f *fakeStore) UsageReport(ctx context.Context, from, to time.Time, domainKey string) ([]store.UsageRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usageCalls = append(f.usageCalls, usageCall{from: from, to: to, domainKey: domainKey})
	if f.usageErr != nil {
		return nil, f.usageErr
	}
	return append([]store.UsageRow(nil), f.usageRows...), nil
}

// addJob inserts a job row (with pending steps unless steps are given).
func (f *fakeStore) addJob(j store.Job, steps ...store.Step) *store.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j.ID == "" {
		j.ID = ids.NewJob()
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = testNow
		j.UpdatedAt = testNow
		j.ExpiresAt = testNow.Add(24 * time.Hour)
	}
	if len(steps) == 0 {
		for _, n := range store.StepNames {
			steps = append(steps, store.Step{JobID: j.ID, Name: n, Status: store.StepPending})
		}
	}
	f.jobs[j.ID] = &j
	f.steps[j.ID] = steps
	return &j
}

func (f *fakeStore) addImages(jobID string, imgs ...store.SelectedImage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[jobID] = append(f.images[jobID], imgs...)
}

func (f *fakeStore) createdJobs() []store.NewJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.NewJob(nil), f.created...)
}

// ---- enqueuer ----

type enqueueCall struct {
	tx    pgx.Tx
	jobID string
}

type fakeEnqueuer struct {
	mu    sync.Mutex
	calls []enqueueCall
	err   error
}

func (e *fakeEnqueuer) EnqueuePipelineTx(ctx context.Context, tx pgx.Tx, jobID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, enqueueCall{tx: tx, jobID: jobID})
	return e.err
}

// ---- prober ----

type fakeProber struct {
	mu    sync.Mutex
	info  media.VideoInfo
	err   error
	delay time.Duration // a slow ffprobe (honours ctx)
	paths []string
	seen  [][]byte // file contents at probe time
}

func (p *fakeProber) Probe(ctx context.Context, path string) (media.VideoInfo, error) {
	p.mu.Lock()
	delay := p.delay
	p.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return media.VideoInfo{}, fmt.Errorf("fake prober: %w", ctx.Err())
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paths = append(p.paths, path)
	b, err := os.ReadFile(path)
	if err != nil {
		return media.VideoInfo{}, fmt.Errorf("fake prober: %w", err)
	}
	p.seen = append(p.seen, b)
	if p.err != nil {
		return media.VideoInfo{}, p.err
	}
	info := p.info
	info.SizeBytes = int64(len(b))
	return info, nil
}

func (p *fakeProber) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.paths)
}

// ---- storage ----

type memObject struct {
	data        []byte
	contentType string
}

type presignCall struct {
	key          string
	ttl          time.Duration
	downloadName string
}

// memStorage is an in-memory storage.Storage.
type memStorage struct {
	mu              sync.Mutex
	objects         map[string]memObject
	presign         func(key string, ttl time.Duration, downloadName string) (string, error) // nil → unsupported
	presignCalls    []presignCall
	getOverride     func(key string) (io.ReadCloser, error)
	putErr          error
	pingErr         error
	deleteErr       error
	deletedPrefixes []string
	deletedKeys     []string
	// deletePrefixErr, when set, can fail the n-th (0-based) DeletePrefix call.
	deletePrefixErr func(n int) error
}

var _ storage.Storage = (*memStorage)(nil)

func newMemStorage() *memStorage { return &memStorage{objects: map[string]memObject{}} }

func (m *memStorage) Put(ctx context.Context, key string, r io.Reader, contentType string) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		return m.putErr
	}
	m.objects[key] = memObject{data: data, contentType: contentType}
	return nil
}

func (m *memStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getOverride != nil {
		return m.getOverride(key)
	}
	o, ok := m.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data)), nil
}

func (m *memStorage) PresignGet(ctx context.Context, key string, ttl time.Duration, downloadName string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.presignCalls = append(m.presignCalls, presignCall{key: key, ttl: ttl, downloadName: downloadName})
	if m.presign == nil {
		return "", storage.ErrPresignUnsupported
	}
	return m.presign(key, ttl, downloadName)
}

func (m *memStorage) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedKeys = append(m.deletedKeys, key)
	delete(m.objects, key)
	return nil
}

func (m *memStorage) DeletePrefix(ctx context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	n := len(m.deletedPrefixes)
	m.deletedPrefixes = append(m.deletedPrefixes, prefix)
	if m.deletePrefixErr != nil {
		if err := m.deletePrefixErr(n); err != nil {
			return err
		}
	}
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			delete(m.objects, k)
		}
	}
	return nil
}

func (m *memStorage) Head(ctx context.Context, key string) (*storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.ObjectInfo{Key: key, Size: int64(len(o.data)), ContentType: o.contentType}, nil
}

func (m *memStorage) Ping(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pingErr
}

func (m *memStorage) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objects))
	for k := range m.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *memStorage) object(key string) (memObject, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	return o, ok
}

func (m *memStorage) put(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{data: data, contentType: "application/octet-stream"}
}

// ---- logs ----

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// lines returns the JSON log records.
func (s *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &m), l)
		out = append(out, m)
	}
	return out
}

// ---- environment ----

type testEnv struct {
	t       *testing.T
	srv     *Server
	deps    Deps
	store   *fakeStore
	storage *memStorage
	prober  *fakeProber
	enq     *fakeEnqueuer
	clock   *fakeClock
	scratch string
	logs    *syncBuffer
	cars    *domain.Config
	homes   *domain.Config
}

// newEnv builds a Server over fakes. mutate may adjust the domains and Deps before
// the server is constructed.
func newEnv(t *testing.T, mutate ...func(e *testEnv, d *Deps)) *testEnv {
	t.Helper()
	e := &testEnv{
		t:       t,
		clock:   &fakeClock{t: testNow},
		storage: newMemStorage(),
		prober:  &fakeProber{info: media.VideoInfo{DurationSec: 30, Width: 1920, Height: 1080, FPS: 29.97, Codec: "h264"}},
		enq:     &fakeEnqueuer{},
		scratch: t.TempDir(),
		logs:    &syncBuffer{},
		cars:    testDomain("cars", testOrigin, "https://cars.example.com"),
		homes:   testDomain("homes", "https://homes.example.com"),
	}
	e.store = newFakeStore(e.clock.Now)
	d := Deps{
		Store:               e.store,
		Storage:             e.storage,
		Prober:              e.prober,
		Enqueuer:            e.enq,
		Logger:              slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		InternalToken:       testToken,
		IPHashSalt:          testSalt,
		StatusRatePerMinute: 10_000,
		ScratchDir:          e.scratch,
		FFmpegVersion:       "9.0.1",
		Now:                 e.clock.Now,
	}
	for _, m := range mutate {
		m(e, &d)
	}
	d.Registry = domain.NewRegistry(e.cars, e.homes)
	e.deps = d
	e.srv = New(d)
	return e
}

func (e *testEnv) do(r *http.Request) *httptest.ResponseRecorder {
	e.t.Helper()
	if r.RemoteAddr == "" || r.RemoteAddr == "192.0.2.1:1234" {
		r.RemoteAddr = testRemoteAddr
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, r)
	return rec
}

// get sends a browser GET from the allowed origin.
func (e *testEnv) get(path string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Origin", testOrigin)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	return e.do(r)
}

// uploadsDirEntries lists what is left in the upload scratch directory.
func (e *testEnv) uploadsDirEntries() []string {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.scratch, "uploads"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(e.t, err)
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	return names
}

// ---- request bodies ----

type formPart struct {
	name, fileName, contentType string
	data                        []byte
}

func multipartBody(t *testing.T, parts ...formPart) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		cd := fmt.Sprintf(`form-data; name=%q`, p.name)
		if p.fileName != "" {
			cd += fmt.Sprintf(`; filename=%q`, p.fileName)
		}
		h.Set("Content-Disposition", cd)
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		w, err := mw.CreatePart(h)
		require.NoError(t, err)
		_, err = w.Write(p.data)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return buf.Bytes(), mw.FormDataContentType()
}

// uploadRequest is a browser upload of data as the "video" field.
func uploadRequest(t *testing.T, domainKey string, data []byte, fileName string) *http.Request {
	t.Helper()
	body, ct := multipartBody(t, formPart{name: "video", fileName: fileName, contentType: "video/mp4", data: data})
	r := httptest.NewRequest(http.MethodPost, "/v1/domains/"+domainKey+"/jobs", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Origin", testOrigin)
	return r
}

// fakeMP4 returns n bytes that sniff as MP4 (an ftyp box, then filler).
func fakeMP4(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 2, 0, 'i', 's', 'o', 'm', 'm', 'p', '4', '2'})
	for i := 24; i < n; i++ {
		b[i] = byte(i * 31)
	}
	return b
}

// realMP4 encodes a short H.264 MP4 with ffmpeg (skips the test without ffmpeg).
func realMP4(t *testing.T) []byte {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	out := filepath.Join(t.TempDir(), "clip.mp4")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10", "-t", "1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "ultrafast", out)
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, string(b))
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	return data
}

// testIPHash is the client_ip_hash the test server stores for ip.
func testIPHash(ip string) string {
	m := hmac.New(sha256.New, []byte(testSalt))
	m.Write([]byte(ip))
	return hex.EncodeToString(m.Sum(nil))
}

// ---- assertions ----

// requireError asserts an error envelope {"error":{"code","message"}} and returns it.
func requireError(t *testing.T, rec *httptest.ResponseRecorder, status int, code ErrorCode) ErrorDetail {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw), rec.Body.String())
	require.Len(t, raw, 1, "envelope has exactly one key: %s", rec.Body.String())
	var inner map[string]any
	require.NoError(t, json.Unmarshal(raw["error"], &inner))
	msg, ok := inner["message"].(string)
	require.True(t, ok, "message must be a string: %s", rec.Body.String())
	assert.NotEmpty(t, msg)
	for k := range inner {
		assert.Contains(t, []string{"code", "message", "details"}, k)
	}
	var env APIError
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, code, env.Err.Code, rec.Body.String())
	return env.Err
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), rec.Body.String())
	return v
}

// counterValue reads a counter from the default registry (0 when absent).
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metric:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue metric
				}
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
			return float64(m.GetHistogram().GetSampleCount())
		}
	}
	return 0
}

func rejected(t *testing.T, domainKey string, code ErrorCode) float64 {
	t.Helper()
	return counterValue(t, "v2i_uploads_rejected_total", map[string]string{"domain": domainKey, "code": string(code)})
}
