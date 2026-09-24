package queue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeStore is a concurrency-safe in-memory Store.
type fakeStore struct {
	mu sync.Mutex

	cost      float64
	costErr   error
	costSince []time.Time

	fails    []failCall
	failErr  error            // returned by every FailJob
	failErrs map[string]error // per job id
	failCtx  []error          // ctx.Err() seen by FailJob

	// byID holds the jobs GetJob returns; FailJob moves queued/processing ones to
	// failed and reports ErrConflict for terminal ones.
	byID    map[string]*store.Job
	getErr  error
	skipped []string
	skipErr error

	orphans     []store.JobRef
	orphanErr   error
	orphanCalls []listCall // Now is idleSince

	jobs       []store.JobRef // expiry candidates, oldest expiry first
	expired    map[string]bool
	listErr    error
	listCalls  []listCall
	expireErrs map[string]error
	expireIDs  []string

	tableRows map[string]int64
	tableErr  error
	counts    map[store.JobStatus]int64
	countErr  error
	statCalls int

	logs []failCall // AppendInternalLog calls (ID, InternalLog)

	// studio: FailStudio moves a job of studioByID from pending/running to failed and
	// reports ErrConflict otherwise (ErrNotFound for unknown jobs when studioByID is set).
	studioByID       map[string]store.StudioStatus
	studioFails      []failCall // ID, Message (the reason)
	studioFailErr    error
	studioOrphans    []store.JobRef
	studioOrphanErr  error
	studioOrphanCall []listCall
}

type failCall struct{ ID, Code, Message, InternalLog string }

type listCall struct {
	Now   time.Time
	Limit int
}

var _ Store = (*fakeStore)(nil)

func (f *fakeStore) setCost(c float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cost = c
}

func (f *fakeStore) FailJob(ctx context.Context, id, code, message, internalLog string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails = append(f.fails, failCall{id, code, message, internalLog})
	f.failCtx = append(f.failCtx, ctx.Err())
	if f.failErr != nil {
		return f.failErr
	}
	if err := f.failErrs[id]; err != nil {
		return err
	}
	if j := f.byID[id]; j != nil {
		if j.Status.Terminal() {
			return fmt.Errorf("fake: fail job %s: status %s: %w", id, j.Status, store.ErrConflict)
		}
		j.Status = store.StatusFailed
	}
	return nil
}

// addJob registers a job for GetJob/FailJob.
func (f *fakeStore) addJob(id, domainKey string, status store.JobStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byID == nil {
		f.byID = map[string]*store.Job{}
	}
	f.byID[id] = &store.Job{ID: id, DomainKey: domainKey, Status: status}
}

func (f *fakeStore) jobStatus(id string) store.JobStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.byID[id]; j != nil {
		return j.Status
	}
	return ""
}

func (f *fakeStore) GetJob(_ context.Context, id string) (*store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	j := f.byID[id]
	if j == nil {
		return nil, fmt.Errorf("fake: get job %s: %w", id, store.ErrNotFound)
	}
	cp := *j
	return &cp, nil
}

func (f *fakeStore) SkipRemainingSteps(_ context.Context, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skipped = append(f.skipped, jobID)
	return f.skipErr
}

func (f *fakeStore) skippedJobs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.skipped...)
}

// ListOrphanedJobs returns the orphans that are still queued or processing (per byID
// when the job is registered there).
func (f *fakeStore) ListOrphanedJobs(_ context.Context, idleSince time.Time, limit int) ([]store.JobRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orphanCalls = append(f.orphanCalls, listCall{idleSince, limit})
	if f.orphanErr != nil {
		return nil, f.orphanErr
	}
	var out []store.JobRef
	for _, ref := range f.orphans {
		if len(out) >= limit {
			break
		}
		if j := f.byID[ref.ID]; j != nil && j.Status.Terminal() {
			continue
		}
		out = append(out, ref)
	}
	return out, nil
}

func (f *fakeStore) orphanCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.orphanCalls)
}

func (f *fakeStore) failCalls() []failCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]failCall(nil), f.fails...)
}

func (f *fakeStore) ListExpiredJobs(_ context.Context, now time.Time, limit int) ([]store.JobRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls = append(f.listCalls, listCall{now, limit})
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []store.JobRef
	for _, j := range f.jobs {
		if len(out) >= limit {
			break
		}
		if !f.expired[j.ID] {
			out = append(out, j)
		}
	}
	return out, nil
}

func (f *fakeStore) ExpireJob(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expireIDs = append(f.expireIDs, id)
	if err := f.expireErrs[id]; err != nil {
		return err
	}
	if f.expired == nil {
		f.expired = map[string]bool{}
	}
	f.expired[id] = true
	return nil
}

func (f *fakeStore) CostSince(_ context.Context, since time.Time) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.costSince = append(f.costSince, since)
	return f.cost, f.costErr
}

func (f *fakeStore) costCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.costSince)
}

func (f *fakeStore) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.listCalls)
}

func (f *fakeStore) statsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statCalls
}

func (f *fakeStore) TableRowEstimates(context.Context) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statCalls++
	return f.tableRows, f.tableErr
}

func (f *fakeStore) CountJobsByStatus(context.Context) (map[store.JobStatus]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts, f.countErr
}

func (f *fakeStore) AppendInternalLog(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, failCall{ID: id, InternalLog: text})
	return nil
}

func (f *fakeStore) internalLogs() []failCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]failCall(nil), f.logs...)
}

func (f *fakeStore) FailStudio(_ context.Context, jobID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.studioFails = append(f.studioFails, failCall{ID: jobID, Message: reason})
	if f.studioFailErr != nil {
		return f.studioFailErr
	}
	if f.studioByID == nil {
		return nil
	}
	st, ok := f.studioByID[jobID]
	switch {
	case !ok:
		return fmt.Errorf("fake: fail studio %s: %w", jobID, store.ErrNotFound)
	case !st.Active():
		return fmt.Errorf("fake: fail studio %s: studio %s: %w", jobID, st, store.ErrConflict)
	}
	f.studioByID[jobID] = store.StudioFailed
	return nil
}

func (f *fakeStore) studioFailCalls() []failCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]failCall(nil), f.studioFails...)
}

func (f *fakeStore) ListOrphanedStudioJobs(_ context.Context, idleSince time.Time, limit int) ([]store.JobRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.studioOrphanCall = append(f.studioOrphanCall, listCall{idleSince, limit})
	if f.studioOrphanErr != nil {
		return nil, f.studioOrphanErr
	}
	var out []store.JobRef
	for _, ref := range f.studioOrphans {
		if len(out) >= limit {
			break
		}
		if st, ok := f.studioByID[ref.ID]; ok && !st.Active() {
			continue
		}
		out = append(out, ref)
	}
	return out, nil
}

// fakeStorage records DeletePrefix calls; prefixes containing a failOn substring fail.
// objects is an optional key set that DeletePrefix removes from, and afterDelete (if
// set) runs after every successful DeletePrefix, outside the lock, with the call's
// 1-based number (to simulate a concurrent worker uploading).
type fakeStorage struct {
	mu          sync.Mutex
	failOn      []string
	deleted     []string
	deleteCtx   []error // ctx.Err() seen by DeletePrefix
	objects     map[string]bool
	afterDelete func(call int, prefix string)
}

var _ storage.Storage = (*fakeStorage)(nil)

var errNotImplemented = errors.New("fake storage: not implemented")

func (s *fakeStorage) DeletePrefix(ctx context.Context, prefix string) error {
	s.mu.Lock()
	s.deleteCtx = append(s.deleteCtx, ctx.Err())
	for _, f := range s.failOn {
		if strings.Contains(prefix, f) {
			s.mu.Unlock()
			return errors.New("fake storage: delete failed for " + prefix)
		}
	}
	s.deleted = append(s.deleted, prefix)
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			delete(s.objects, k)
		}
	}
	call, hook := len(s.deleted), s.afterDelete
	s.mu.Unlock()
	if hook != nil {
		hook(call, prefix)
	}
	return nil
}

// put adds an object key (a worker upload).
func (s *fakeStorage) put(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = map[string]bool{}
	}
	s.objects[key] = true
}

func (s *fakeStorage) objectKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for k := range s.objects {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (s *fakeStorage) deletedPrefixes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

func (s *fakeStorage) Put(context.Context, string, io.Reader, string) error { return errNotImplemented }
func (s *fakeStorage) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errNotImplemented
}

func (s *fakeStorage) PresignGet(context.Context, string, time.Duration, string) (string, error) {
	return "", storage.ErrPresignUnsupported
}
func (s *fakeStorage) Delete(context.Context, string) error { return nil }
func (s *fakeStorage) Head(context.Context, string) (*storage.ObjectInfo, error) {
	return nil, storage.ErrNotFound
}
func (s *fakeStorage) Ping(context.Context) error { return nil }

// fakeRunner calls fn (nil → success) and reports every jobID on calls.
type fakeRunner struct {
	fn    func(ctx context.Context, jobID string) error
	calls chan string
}

func newFakeRunner(fn func(ctx context.Context, jobID string) error) *fakeRunner {
	return &fakeRunner{fn: fn, calls: make(chan string, 100)}
}

func (r *fakeRunner) Run(ctx context.Context, jobID string) error {
	r.calls <- jobID
	if r.fn == nil {
		return nil
	}
	return r.fn(ctx, jobID)
}

type fakeSweeper struct {
	mu        sync.Mutex
	olderThan time.Duration
	deleted   int
	err       error
	calls     int
}

func (s *fakeSweeper) SweepFiles(_ context.Context, olderThan time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.olderThan = olderThan
	return s.deleted, s.err
}

func (s *fakeSweeper) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// fakeQueues is an in-memory QueueController.
type fakeQueues struct {
	missing bool // QueueGet/QueuePause/QueueResume → rivertype.ErrNotFound
	paused  bool
	getErr  error
	pauses  int
	resumes int
}

var _ QueueController = (*fakeQueues)(nil)

func (q *fakeQueues) QueueGet(_ context.Context, name string) (*rivertype.Queue, error) {
	if q.getErr != nil {
		return nil, q.getErr
	}
	if q.missing {
		return nil, rivertype.ErrNotFound
	}
	out := &rivertype.Queue{Name: name}
	if q.paused {
		now := time.Now()
		out.PausedAt = &now
	}
	return out, nil
}

func (q *fakeQueues) QueuePause(context.Context, string, *river.QueuePauseOpts) error {
	if q.missing {
		return rivertype.ErrNotFound
	}
	q.pauses++
	q.paused = true
	return nil
}

func (q *fakeQueues) QueueResume(context.Context, string, *river.QueuePauseOpts) error {
	if q.missing {
		return rivertype.ErrNotFound
	}
	q.resumes++
	q.paused = false
	return nil
}

// fakeQueueSet dispatches to one fakeQueues per queue name; a name without an entry is
// not registered (rivertype.ErrNotFound).
type fakeQueueSet map[string]*fakeQueues

var _ QueueController = fakeQueueSet(nil)

func (s fakeQueueSet) QueueGet(ctx context.Context, name string) (*rivertype.Queue, error) {
	if q := s[name]; q != nil {
		return q.QueueGet(ctx, name)
	}
	return nil, rivertype.ErrNotFound
}

func (s fakeQueueSet) QueuePause(ctx context.Context, name string, opts *river.QueuePauseOpts) error {
	if q := s[name]; q != nil {
		return q.QueuePause(ctx, name, opts)
	}
	return rivertype.ErrNotFound
}

func (s fakeQueueSet) QueueResume(ctx context.Context, name string, opts *river.QueuePauseOpts) error {
	if q := s[name]; q != nil {
		return q.QueueResume(ctx, name, opts)
	}
	return rivertype.ErrNotFound
}

type failingQuerier struct{}

func (failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("query failed")
}

// pipelineJob builds the job River hands to PipelineWorker.Work.
func pipelineJob(jobID string, attempt, maxAttempts int) *river.Job[PipelineArgs] {
	return &river.Job[PipelineArgs]{
		JobRow: &rivertype.JobRow{
			ID: 42, Attempt: attempt, MaxAttempts: maxAttempts,
			Kind: PipelineArgs{}.Kind(), Queue: QueuePipeline,
		},
		Args: PipelineArgs{JobID: jobID},
	}
}

// studioJob builds the job River hands to StudioWorker.Work.
func studioJob(jobID string, attempt, maxAttempts int) *river.Job[StudioArgs] {
	return &river.Job[StudioArgs]{
		JobRow: &rivertype.JobRow{
			ID: 43, Attempt: attempt, MaxAttempts: maxAttempts,
			Kind: StudioArgs{}.Kind(), Queue: QueueStudio,
		},
		Args: StudioArgs{JobID: jobID},
	}
}

// metricValue reads a gauge or counter.
func metricValue(t *testing.T, m prometheus.Metric) float64 {
	t.Helper()
	var out dto.Metric
	require.NoError(t, m.Write(&out))
	switch {
	case out.Gauge != nil:
		return out.Gauge.GetValue()
	case out.Counter != nil:
		return out.Counter.GetValue()
	}
	t.Fatalf("metric %s is neither a gauge nor a counter", m.Desc())
	return 0
}

// seriesCount is the number of series c currently exports.
func seriesCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 4096)
	c.Collect(ch)
	close(ch)
	return len(ch)
}
