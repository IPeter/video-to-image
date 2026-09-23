package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
)

// Chunked upload sessions. A session is the directory ScratchDir/uploads/<uploadId>/
// holding the preallocated file (data) and its record (session.json, replaced
// atomically after every chunk), plus the in-memory state below. Sessions live in
// this API process (one replica, like the rate limits); the record lets the next
// process pick them up again after a restart (loadUploadSessions).

const (
	sessionDataFile      = "data"
	sessionRecordFile    = "session.json"
	sessionRecordVersion = 1
	// maxSessionRecordBytes bounds the record read on restore.
	maxSessionRecordBytes = 1 << 20
	// maxChunkCount bounds a session's bookkeeping (and its record): a tiny chunk size
	// is raised for large files so that no session has more chunks than this.
	maxChunkCount = 10_000
	// maxMissingListed is how many missing chunk indices a 409 lists.
	maxMissingListed = 20
	// uploadSweepEvery is how often idle sessions are looked for.
	uploadSweepEvery = time.Minute
	// firstChunkIdle is how long a session that has not stored any chunk yet is kept
	// idle (a tab closed right after create); from the first chunk on it is
	// Deps.UploadSessionTTL.
	firstChunkIdle = 5 * time.Minute
	// clientEvictIdle: a client at its session cap that starts another upload gets its
	// own longest-idle session evicted once that one has been idle this long (a reload
	// or a closed tab left it behind).
	clientEvictIdle = time.Minute
	// globalEvictIdle: when every upload slot is taken (or the scratch disk is short),
	// the longest-idle session of anyone is evicted once it has been idle this long.
	globalEvictIdle = 5 * time.Minute
	// defaultStaleWriterWait bounds how long complete waits for superseded PUTs of a
	// chunk (a newer PUT of the same chunk already stored it) to end.
	defaultStaleWriterWait = 5 * time.Second
)

// uploadSession is one chunked upload.
type uploadSession struct {
	// Set at creation, then read-only.
	id          string
	domainKey   string
	clientKey   string // sessionClientKey of the creator; "" for internal-token callers
	ipHash      string // stored on the job
	fileName    string // sanitised
	contentType string // as declared, for the rejection log only
	size        int64
	chunkSize   int64
	chunkCount  int
	createdAt   time.Time
	dir         string
	release     func() // the upload slot

	// completeMu is held by complete and abort for their whole run, so a second
	// complete waits for the first one's outcome and abort never removes the file
	// while it is being ingested.
	completeMu sync.Mutex

	mu           sync.Mutex
	received     []bool
	nReceived    int
	stored       bool                // a chunk was stored at least once (else the firstChunkIdle rule)
	writers      []int32             // chunk PUTs in flight, per chunk
	gen          []uint32            // chunk PUTs started, per chunk: only the latest may mark it received
	inflight     int                 // chunk PUTs in flight, in total
	holds        int                 // complete requests in flight
	puts         map[chunkPut]func() // the PUTs in flight, with what cuts their body off
	chunkEnded   chan struct{}       // closed (and replaced) whenever a chunk PUT ends
	lastActivity time.Time
	completing   bool // complete is ingesting the file: chunks can no longer change
	closed       bool // completed, aborted, expired or evicted: gone from the registry
}

// chunkPut identifies one PUT of a chunk: its index and generation.
type chunkPut struct {
	index int
	gen   uint32
}

// sessionRecord is session.json.
type sessionRecord struct {
	Version        int       `json:"version"`
	UploadID       string    `json:"uploadId"`
	DomainKey      string    `json:"domainKey"`
	ClientHash     string    `json:"clientHash,omitempty"` // sessionClientKey: an HMAC, never the address
	IPHash         string    `json:"ipHash"`
	FileName       string    `json:"fileName"`
	ContentType    string    `json:"contentType,omitempty"`
	SizeBytes      int64     `json:"sizeBytes"`
	ChunkSizeBytes int64     `json:"chunkSizeBytes"`
	ChunkCount     int       `json:"chunkCount"`
	Received       []int     `json:"received"`
	CreatedAt      time.Time `json:"createdAt"`
	LastActivity   time.Time `json:"lastActivity"`
}

// completedUpload is the outcome of a completed session, kept for one TTL so a
// complete that is retried (the response got lost) gets the same answer instead of
// UPLOAD_NOT_FOUND. It never makes the session usable again.
type completedUpload struct {
	domainKey string
	jobID     string    // the job created
	err       *APIError // or why the file was rejected
	at        time.Time
}

// uploadSessions is the registry of open sessions.
type uploadSessions struct {
	mu        sync.Mutex
	byID      map[string]*uploadSession
	done      map[string]completedUpload
	nextSweep time.Time

	sweeperOnce sync.Once
	stopOnce    sync.Once
	stop        chan struct{}
}

func newUploadSessions() *uploadSessions {
	return &uploadSessions{
		byID: map[string]*uploadSession{},
		done: map[string]completedUpload{},
		stop: make(chan struct{}),
	}
}

func (m *uploadSessions) stopSweeper() { m.stopOnce.Do(func() { close(m.stop) }) }

// count returns the number of open sessions (tests, logs).
func (m *uploadSessions) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID)
}

// ---- one session ----

func (u *uploadSession) dataPath() string { return filepath.Join(u.dir, sessionDataFile) }

// initChunks sets up the per-chunk bookkeeping of a new or restored session.
func (u *uploadSession) initChunks() {
	u.received = make([]bool, u.chunkCount)
	u.writers = make([]int32, u.chunkCount)
	u.gen = make([]uint32, u.chunkCount)
	u.puts = map[chunkPut]func(){}
	u.chunkEnded = make(chan struct{})
}

// chunkLen is the exact length of chunk i: chunkSize, except for the last chunk.
func (u *uploadSession) chunkLen(i int) int64 {
	if i == u.chunkCount-1 {
		return u.size - int64(i)*u.chunkSize
	}
	return u.chunkSize
}

// idleLimitLocked is how long the session is kept idle: ttl, or at most
// firstChunkIdle until a chunk was stored.
func (u *uploadSession) idleLimitLocked(ttl time.Duration) time.Duration {
	if u.stored {
		return ttl
	}
	return min(ttl, firstChunkIdle)
}

// expiresAtLocked is when the sweeper drops the session unless another request arrives.
func (u *uploadSession) expiresAtLocked(ttl time.Duration) time.Time {
	return u.lastActivity.Add(u.idleLimitLocked(ttl)).UTC()
}

// busyLocked reports a request in flight on the session (a chunk PUT or a complete):
// such a session is never swept or evicted.
func (u *uploadSession) busyLocked() bool {
	return u.inflight > 0 || u.holds > 0 || u.completing
}

// evictableLocked reports whether the session may be dropped to make room for another
// upload: nothing in flight, and idle for minIdle or past its expiry.
func (u *uploadSession) evictableLocked(now time.Time, ttl, minIdle time.Duration) bool {
	if u.busyLocked() {
		return false
	}
	idle := now.Sub(u.lastActivity)
	return idle >= minIdle || idle >= u.idleLimitLocked(ttl)
}

// evictableInLocked is how long until evictableLocked turns true (0: it is), assuming
// nothing happens meanwhile. A busy session needs at least minIdle after its request
// (a running complete frees the slot itself, within seconds).
func (u *uploadSession) evictableInLocked(now time.Time, ttl, minIdle time.Duration) time.Duration {
	switch {
	case u.completing:
		return uploadBusyRetryAfter * time.Second
	case u.busyLocked():
		return minIdle
	}
	at := u.lastActivity.Add(min(minIdle, u.idleLimitLocked(ttl)))
	return max(0, at.Sub(now))
}

// missingLocked returns the first maxMissingListed missing chunks and how many are
// missing in all. A chunk is missing until a PUT of it succeeded, and again while it
// is being re-sent (also while an older PUT of it, superseded by the one that stored
// it, is still in flight: see awaitStaleWriters).
func (u *uploadSession) missingLocked() (first []int, n int) {
	first = []int{}
	for i := range u.chunkCount {
		if u.received[i] && u.writers[i] == 0 {
			continue
		}
		if n++; len(first) < maxMissingListed {
			first = append(first, i)
		}
	}
	return first, n
}

func (u *uploadSession) recordLocked() sessionRecord {
	rec := sessionRecord{
		Version: sessionRecordVersion, UploadID: u.id, DomainKey: u.domainKey, ClientHash: u.clientKey,
		IPHash: u.ipHash, FileName: u.fileName, ContentType: u.contentType,
		SizeBytes: u.size, ChunkSizeBytes: u.chunkSize, ChunkCount: u.chunkCount,
		Received: make([]int, 0, u.nReceived), CreatedAt: u.createdAt, LastActivity: u.lastActivity,
	}
	for i, ok := range u.received {
		if ok {
			rec.Received = append(rec.Received, i)
		}
	}
	return rec
}

// persistLocked replaces session.json (write a temp file, then rename). A closed
// session is never written: its directory is being removed.
func (u *uploadSession) persistLocked() error {
	if u.closed {
		return nil
	}
	b, err := json.Marshal(u.recordLocked())
	if err != nil {
		return fmt.Errorf("encode session record: %w", err)
	}
	tmp, err := os.CreateTemp(u.dir, ".session-*.tmp")
	if err != nil {
		return fmt.Errorf("create session record: %w", err)
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write session record: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(u.dir, sessionRecordFile)); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("replace session record: %w", err)
	}
	return nil
}

// beginChunk registers a PUT of chunk i. Until it ends successfully the chunk counts
// as missing (a re-send overwrites what was there). cut (may be nil) cuts the PUT's
// body off when complete no longer wants to wait for it. It returns the PUT's
// generation.
func (u *uploadSession) beginChunk(i int, now time.Time, cut func()) (gen uint32, apiErr *APIError, persistErr error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return 0, errUploadNotFound(), nil
	}
	if u.completing {
		return 0, NewAPIError(http.StatusBadRequest, CodeInvalidRequest,
			"the upload is being completed; its chunks can no longer change"), nil
	}
	u.writers[i]++
	u.inflight++
	u.gen[i]++
	u.lastActivity = now
	if cut == nil {
		cut = func() {}
	}
	u.puts[chunkPut{i, u.gen[i]}] = cut
	if u.received[i] {
		u.received[i] = false
		u.nReceived--
		persistErr = u.persistLocked() // a restart must not trust a half-rewritten chunk
	}
	return u.gen[i], nil, persistErr
}

// chunkEnd is what endChunk reports.
type chunkEnd struct {
	closed     bool // the session was removed while the chunk was being written
	superseded bool // a newer PUT of the same chunk stored it (this one did not decide)
	persistErr error
}

// endChunk finishes a PUT of chunk i. ok marks the chunk received, unless a later PUT
// of the same chunk started meanwhile (that one decides).
func (u *uploadSession) endChunk(i int, gen uint32, ok bool, now time.Time) chunkEnd {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.writers[i]--
	u.inflight--
	delete(u.puts, chunkPut{i, gen})
	close(u.chunkEnded)
	u.chunkEnded = make(chan struct{})
	if u.closed {
		return chunkEnd{closed: true}
	}
	u.lastActivity = now
	var end chunkEnd
	if ok && gen == u.gen[i] && !u.received[i] {
		u.received[i] = true
		u.nReceived++
		u.stored = true
		end.persistErr = u.persistLocked()
	}
	end.superseded = u.supersededLocked(i, gen)
	return end
}

// supersededLocked reports whether PUT gen of chunk i is an older one whose chunk a
// newer PUT has stored.
func (u *uploadSession) supersededLocked(i int, gen uint32) bool {
	return u.received[i] && gen != u.gen[i]
}

// hold registers a complete request, which keeps the session from being swept or
// evicted while it waits and runs. It is false when the session is already gone.
func (u *uploadSession) hold() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return false
	}
	u.holds++
	return true
}

// unhold ends a hold; the request counts as activity.
func (u *uploadSession) unhold(now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.holds--
	if !u.closed {
		u.lastActivity = now
	}
}

// awaitStaleWriters waits, at most maxWait, until no chunk that has been stored still
// has an older PUT in flight. Such a PUT was superseded (the client gave up on it and
// sent the chunk again, but the old request is still open at the origin, e.g. held by
// a proxy or trickling); until it ends it may still write to the file, so complete
// cannot freeze the chunks. Its bytes are not needed any more, so its body is cut off
// at once, which normally ends it within milliseconds.
func (u *uploadSession) awaitStaleWriters(maxWait time.Duration) {
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	cut := map[chunkPut]bool{}
	for {
		u.mu.Lock()
		stale := false
		var cuts []func()
		for p, c := range u.puts {
			if !u.supersededLocked(p.index, p.gen) {
				continue
			}
			stale = true
			if !cut[p] {
				cut[p] = true
				cuts = append(cuts, c)
			}
		}
		ended := u.chunkEnded
		closed := u.closed
		u.mu.Unlock()
		if closed || !stale {
			return
		}
		for _, c := range cuts {
			c()
		}
		select {
		case <-ended:
		case <-timer.C:
			return
		}
	}
}

// startCompleting freezes the chunks for complete, or reports why it cannot: closed
// (the session is gone), or the 409 listing the missing chunks.
func (u *uploadSession) startCompleting() (closed bool, apiErr *APIError) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return true, nil
	}
	missing, n := u.missingLocked()
	if n > 0 {
		return false, NewAPIError(http.StatusConflict, CodeUploadIncomplete,
			fmt.Sprintf("%d of %d chunks have not been received yet", n, u.chunkCount)).
			WithDetails(map[string]any{"missing": missing, "received": u.chunkCount - n, "chunkCount": u.chunkCount})
	}
	u.completing = true
	return false, nil
}

// stopCompleting lets a session whose complete failed on the server side be
// completed again.
func (u *uploadSession) stopCompleting(now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.completing = false
	u.lastActivity = now
}

func errUploadNotFound() *APIError {
	return NewAPIError(http.StatusNotFound, CodeUploadNotFound,
		"upload not found: it was completed, cancelled or expired; start the upload again")
}

// ---- registry operations ----

// sessionClientKey is the per-client key of the session cap: an HMAC of the rate key,
// so the record on disk never holds a client address.
func (s *Server) sessionClientKey(r *http.Request) string {
	m := hmac.New(sha256.New, s.ipKey)
	_, _ = m.Write([]byte("upload-session\x00" + s.rateKey(r)))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// sessionChunkSize is the chunk size of a new session of size bytes: the configured
// one, raised when the file would need more than maxChunkCount chunks.
func (s *Server) sessionChunkSize(size int64) int64 {
	return max(s.d.ChunkSizeBytes, (size+maxChunkCount-1)/maxChunkCount)
}

// newSessionParams are what createUpload admitted.
type newSessionParams struct {
	dc          *domain.Config
	clientKey   string
	ipHash      string
	fileName    string
	contentType string
	size        int64
	release     func()
}

// openUploadSession creates the session directory with the preallocated data file
// and the record, and registers the session. On error nothing is left on disk (the
// caller still owns p.release).
func (s *Server) openUploadSession(p newSessionParams) (*uploadSession, error) {
	id := ids.NewUpload()
	chunkSize := s.sessionChunkSize(p.size)
	chunkCount := int((p.size + chunkSize - 1) / chunkSize)
	now := s.d.Now()
	sess := &uploadSession{
		id: id, domainKey: p.dc.Key, clientKey: p.clientKey, ipHash: p.ipHash,
		fileName: p.fileName, contentType: p.contentType,
		size: p.size, chunkSize: chunkSize, chunkCount: chunkCount,
		createdAt: now, dir: filepath.Join(s.uploadDir(), id), release: p.release,
		lastActivity: now,
	}
	sess.initChunks()
	if err := os.MkdirAll(s.uploadDir(), 0o755); err != nil {
		return nil, fmt.Errorf("create upload dir: %w", err)
	}
	if err := os.Mkdir(sess.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	err := func() error {
		f, err := os.OpenFile(sess.dataPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create session file: %w", err)
		}
		// Sparse: the disk space is accounted for by the slot's reservation.
		terr := f.Truncate(p.size)
		if err := errors.Join(terr, f.Close()); err != nil {
			return fmt.Errorf("size session file: %w", err)
		}
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.persistLocked()
	}()
	if err != nil {
		s.removeSessionDir(sess.dir)
		return nil, err
	}
	s.sessions.mu.Lock()
	s.sessions.byID[id] = sess
	s.sessions.mu.Unlock()
	s.startSweeper()
	return sess, nil
}

// lookupSession returns the open session id of domain dc. A malformed ID, an
// unknown one and one of another domain are all UPLOAD_NOT_FOUND.
func (s *Server) lookupSession(dc *domain.Config, id string) (*uploadSession, *APIError) {
	if !ids.Valid(ids.PrefixUpload, id) {
		return nil, errUploadNotFound()
	}
	s.sessions.mu.Lock()
	sess := s.sessions.byID[id]
	s.sessions.mu.Unlock()
	if sess == nil || sess.domainKey != dc.Key {
		return nil, errUploadNotFound()
	}
	return sess, nil
}

// completedOutcome returns the recorded outcome of a completed session of dc.
func (s *Server) completedOutcome(dc *domain.Config, id string) (completedUpload, bool) {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	c, ok := s.sessions.done[id]
	if !ok || c.domainKey != dc.Key {
		return completedUpload{}, false
	}
	return c, true
}

// removeSession unregisters sess, deletes its directory and frees its slot. outcome,
// when set, is remembered for a retried complete. It returns false when the session
// was already gone.
func (s *Server) removeSession(sess *uploadSession, outcome *completedUpload) bool {
	m := s.sessions
	m.mu.Lock()
	if m.byID[sess.id] != sess {
		m.mu.Unlock()
		return false
	}
	delete(m.byID, sess.id)
	sess.mu.Lock()
	sess.closed = true
	sess.mu.Unlock()
	if outcome != nil {
		m.done[sess.id] = *outcome
	}
	m.mu.Unlock()
	s.removeSessionDir(sess.dir)
	sess.release()
	return true
}

func (s *Server) removeSessionDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		s.d.Logger.Warn("httpapi: remove upload session files", "dir", dir, "err", err)
	}
}

// ---- expiry ----

// startSweeper starts the expiry goroutine once, when the first session exists.
func (s *Server) startSweeper() {
	s.sessions.sweeperOnce.Do(func() {
		go func() {
			t := time.NewTicker(uploadSweepEvery)
			defer t.Stop()
			for {
				select {
				case <-s.sessions.stop:
					return
				case <-t.C:
					s.sweepUploadSessions()
				}
			}
		}()
	})
}

// maybeSweepUploadSessions sweeps when the last sweep is uploadSweepEvery old, so
// expired sessions free their slots before a new upload is admitted.
func (s *Server) maybeSweepUploadSessions() {
	now := s.d.Now()
	s.sessions.mu.Lock()
	due := !now.Before(s.sessions.nextSweep)
	s.sessions.mu.Unlock()
	if due {
		s.sweepUploadSessions()
	}
}

// sweepUploadSessions removes the sessions past their expiry (idle for
// UploadSessionTTL, or firstChunkIdle before the first chunk; never one with a chunk
// PUT or a complete in flight), with their files, and forgets outcomes of completed
// sessions older than UploadSessionTTL.
func (s *Server) sweepUploadSessions() {
	now := s.d.Now()
	ttl := s.d.UploadSessionTTL
	m := s.sessions
	var expired []*uploadSession
	m.mu.Lock()
	m.nextSweep = now.Add(uploadSweepEvery)
	for id, sess := range m.byID {
		sess.mu.Lock()
		if !sess.busyLocked() && !now.Before(sess.expiresAtLocked(ttl)) {
			sess.closed = true
			delete(m.byID, id)
			expired = append(expired, sess)
		}
		sess.mu.Unlock()
	}
	for id, c := range m.done {
		if now.Sub(c.at) >= ttl {
			delete(m.done, id)
		}
	}
	open := len(m.byID)
	m.mu.Unlock()

	var bytes int64
	for _, sess := range expired {
		s.removeSessionDir(sess.dir)
		sess.release()
		bytes += sess.size
	}
	if len(expired) > 0 {
		s.d.Logger.Info("httpapi: expired upload sessions removed", "count", len(expired), "bytes", bytes, "open", open)
	}
}

// ---- eviction ----

// evictReasons name a busyReason in the eviction log.
var evictReasons = map[busyReason]string{busyClient: "client_session_cap", busyGlobal: "upload_slots_full", busyDisk: "scratch_disk_low"}

// evictSession drops the longest-idle session that match accepts (nil: any) and that
// is evictable (evictableLocked with minIdle) to make room for another upload, with
// its files and its slot. It returns the session, or nil when none qualifies.
func (s *Server) evictSession(ctx context.Context, reason busyReason, match func(*uploadSession) bool, minIdle time.Duration) *uploadSession {
	now := s.d.Now()
	ttl := s.d.UploadSessionTTL
	m := s.sessions
	m.mu.Lock()
	var victim *uploadSession
	var victimSince time.Time
	for _, sess := range m.byID {
		if match != nil && !match(sess) {
			continue
		}
		sess.mu.Lock()
		if sess.evictableLocked(now, ttl, minIdle) &&
			(victim == nil || sess.lastActivity.Before(victimSince) ||
				(sess.lastActivity.Equal(victimSince) && sess.createdAt.Before(victim.createdAt))) {
			victim, victimSince = sess, sess.lastActivity
		}
		sess.mu.Unlock()
	}
	if victim == nil {
		m.mu.Unlock()
		return nil
	}
	// Decide and close under the session lock: a chunk PUT starting now either sees
	// the session closed (404) or keeps it.
	victim.mu.Lock()
	ok := victim.evictableLocked(now, ttl, minIdle)
	received := victim.nReceived
	if ok {
		victim.closed = true
		delete(m.byID, victim.id)
	}
	victim.mu.Unlock()
	m.mu.Unlock()
	if !ok {
		return nil
	}
	s.removeSessionDir(victim.dir)
	victim.release()
	s.d.Logger.InfoContext(ctx, "httpapi: upload session evicted", "reason", evictReasons[reason],
		"domain", victim.domainKey, "upload_id", victim.id, "idle", now.Sub(victimSince).Round(time.Second).String(),
		"received_chunks", received, "chunks", victim.chunkCount, "bytes", victim.size)
	return victim
}

// sessionsWait is how long until the first session that match accepts (nil: any)
// can be evicted with minIdle (evictableInLocked). ok is false when there is none.
func (s *Server) sessionsWait(match func(*uploadSession) bool, minIdle time.Duration) (wait time.Duration, ok bool) {
	now := s.d.Now()
	ttl := s.d.UploadSessionTTL
	m := s.sessions
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sess := range m.byID {
		if match != nil && !match(sess) {
			continue
		}
		sess.mu.Lock()
		in := sess.evictableInLocked(now, ttl, minIdle)
		sess.mu.Unlock()
		if !ok || in < wait {
			wait, ok = in, true
		}
	}
	return wait, ok
}

// ---- restore after a restart ----

// loadUploadSessions restores the sessions a previous process left in
// ScratchDir/uploads, newest first while upload slots last. Broken, expired and
// surplus sessions are deleted. Other entries there (single-request spool files) are
// left alone.
func (s *Server) loadUploadSessions() {
	dir := s.uploadDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		s.d.Logger.Warn("httpapi: cannot read upload sessions", "dir", dir, "err", err)
		return
	}
	now := s.d.Now()
	var restored []*uploadSession
	dropped := 0
	drop := func(path, id, reason string) {
		s.removeSessionDir(path)
		dropped++
		s.d.Logger.Info("httpapi: upload session dropped on restore", "upload_id", id, "reason", reason)
	}
	for _, en := range entries {
		id := en.Name()
		if !en.IsDir() || !ids.Valid(ids.PrefixUpload, id) {
			continue
		}
		path := filepath.Join(dir, id)
		sess, reason := s.readUploadSession(path, id, now)
		if sess == nil {
			drop(path, id, reason)
			continue
		}
		restored = append(restored, sess)
	}
	slices.SortFunc(restored, func(a, b *uploadSession) int { return b.lastActivity.Compare(a.lastActivity) })
	n := 0
	for _, sess := range restored {
		_, release, reason := s.uploads.take(slotSession, sess.clientKey, 2*sess.size, false)
		if reason != notBusy {
			drop(sess.dir, sess.id, "no free upload slot")
			continue
		}
		sess.release = release
		s.sessions.mu.Lock()
		s.sessions.byID[sess.id] = sess
		s.sessions.mu.Unlock()
		n++
	}
	if n > 0 {
		s.startSweeper()
	}
	if n > 0 || dropped > 0 {
		s.d.Logger.Info("httpapi: upload sessions restored", "restored", n, "dropped", dropped)
	}
}

// readUploadSession validates one session directory against its record, the domain
// registry and the TTL. It returns nil and the reason when the session is unusable.
func (s *Server) readUploadSession(dir, id string, now time.Time) (*uploadSession, string) {
	f, err := os.Open(filepath.Join(dir, sessionRecordFile))
	if err != nil {
		return nil, "no session record"
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSessionRecordBytes))
	_ = f.Close()
	var rec sessionRecord
	if err != nil || json.Unmarshal(raw, &rec) != nil {
		return nil, "unreadable session record"
	}
	var dc *domain.Config
	if s.d.Registry != nil {
		dc, _ = s.d.Registry.Get(rec.DomainKey)
	}
	switch {
	case rec.Version != sessionRecordVersion || rec.UploadID != id:
		return nil, "unknown record version or ID"
	case dc == nil:
		return nil, "unknown domain"
	case rec.SizeBytes <= 0 || rec.SizeBytes > dc.Limits.MaxUploadBytes || rec.ChunkSizeBytes <= 0 ||
		rec.ChunkCount <= 0 || rec.ChunkCount > maxChunkCount ||
		int64(rec.ChunkCount) != (rec.SizeBytes+rec.ChunkSizeBytes-1)/rec.ChunkSizeBytes:
		return nil, "inconsistent sizes"
	}
	st, err := os.Stat(filepath.Join(dir, sessionDataFile))
	if err != nil || !st.Mode().IsRegular() || st.Size() != rec.SizeBytes {
		return nil, "data file missing or of the wrong size"
	}
	sess := &uploadSession{
		id: id, domainKey: dc.Key, clientKey: rec.ClientHash, ipHash: rec.IPHash,
		fileName: sanitizeFileName(rec.FileName), contentType: truncate(rec.ContentType, maxContentTypeLen),
		size: rec.SizeBytes, chunkSize: rec.ChunkSizeBytes, chunkCount: rec.ChunkCount,
		createdAt: rec.CreatedAt, dir: dir, lastActivity: rec.LastActivity,
	}
	sess.initChunks()
	for _, i := range rec.Received {
		if i < 0 || i >= rec.ChunkCount {
			return nil, "chunk index out of range"
		}
		if !sess.received[i] {
			sess.received[i] = true
			sess.nReceived++
			sess.stored = true
		}
	}
	if !now.Before(sess.expiresAtLocked(s.d.UploadSessionTTL)) {
		return nil, "expired"
	}
	return sess, ""
}
