// Package app wires configuration into the shared dependencies (logger, Postgres
// pool, storage, ffmpeg, domain registry, analyzer, runner, River worker) used by the
// serve, work and migrate subcommands.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/analyzer/setup"
	"video-to-image/internal/config"
	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/pipeline"
	"video-to-image/internal/queue"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

// NewLogger returns a JSON (or text) slog logger writing to stdout.
func NewLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.EqualFold(cfg.LogFormat, "text") {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// OpenPool connects to Postgres, retrying for up to wait so an init container or a
// freshly started compose stack does not fail the first attempt.
func OpenPool(ctx context.Context, url string, wait time.Duration, log *slog.Logger) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("app: parse DATABASE_URL: %w", err)
	}
	deadline := time.Now().Add(wait)
	backoff := 500 * time.Millisecond
	for {
		pool, err := pgxpool.NewWithConfig(ctx, pcfg)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err = pool.Ping(pingCtx)
			cancel()
			if err == nil {
				return pool, nil
			}
			pool.Close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("app: connect to postgres: %w", err)
		}
		log.Warn("postgres not ready; retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// App holds the dependencies shared by serve and work.
type App struct {
	Cfg      *config.Config
	Log      *slog.Logger
	Pool     *pgxpool.Pool
	Store    *store.Store
	Storage  storage.Storage
	FFmpeg   *media.FFmpeg
	Registry *domain.Registry
}

// Open validates the environment-dependent pieces and connects to Postgres and storage.
func Open(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	reg, err := domain.LoadDir(cfg.DomainsDir)
	if err != nil {
		return nil, err
	}
	log.Info("domains loaded", "keys", reg.Keys(), "dir", cfg.DomainsDir)

	for _, bin := range []string{cfg.FFmpegPath, cfg.FFprobePath} {
		if _, err := exec.LookPath(bin); err != nil {
			return nil, fmt.Errorf("app: %s not found: %w", bin, err)
		}
	}
	if err := os.MkdirAll(cfg.ScratchDir, 0o755); err != nil {
		return nil, fmt.Errorf("app: scratch dir: %w", err)
	}
	CleanScratch(cfg.ScratchDir, log)

	pool, err := OpenPool(ctx, cfg.DatabaseURL, 60*time.Second, log)
	if err != nil {
		return nil, err
	}
	st, err := storage.Open(ctx, cfg.Storage)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("app: storage: %w", err)
	}
	return &App{
		Cfg:      cfg,
		Log:      log,
		Pool:     pool,
		Store:    store.New(pool),
		Storage:  st,
		FFmpeg:   newFFmpeg(cfg, reg, log),
		Registry: reg,
	}, nil
}

// newFFmpeg applies process-wide decode limits: the largest maxPixels of any domain
// (upload validation enforces each domain's own cap) and the configured thread count.
func newFFmpeg(cfg *config.Config, reg *domain.Registry, log *slog.Logger) *media.FFmpeg {
	ff := media.New(cfg.FFmpegPath, cfg.FFprobePath, log)
	for _, k := range reg.Keys() {
		d, _ := reg.Get(k)
		ff.MaxPixels = max(ff.MaxPixels, d.Limits.EffectiveMaxPixels())
	}
	ff.Threads = cfg.FFmpegThreads
	return ff
}

// staleScratchAge is well above queue.PipelineTimeout and any realistic upload, so the
// sweep never removes files of work that is still running.
const staleScratchAge = 2 * time.Hour

// CleanScratch removes leftovers of crashed uploads and jobs (<scratch>/uploads/* and
// <scratch>/jobs/*) older than staleScratchAge, except chunked-upload session
// directories (upl_*), whose lifetime the API manages (see httpapi upload sessions). Normal runs delete their own files; this
// only catches SIGKILL/OOM leftovers so the emptyDir volume cannot fill up.
func CleanScratch(dir string, log *slog.Logger) {
	cutoff := time.Now().Add(-staleScratchAge)
	for _, sub := range []string{"uploads", "jobs"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if sub == "uploads" && e.IsDir() && strings.HasPrefix(e.Name(), ids.PrefixUpload+"_") {
				continue // chunked-upload sessions: the API restores or expires them itself
			}
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			p := filepath.Join(dir, sub, e.Name())
			if err := os.RemoveAll(p); err != nil {
				log.Warn("scratch cleanup", "path", p, "err", err)
				continue
			}
			log.Info("removed stale scratch entry", "path", p)
		}
	}
}

// Close releases the pool.
func (a *App) Close() { a.Pool.Close() }

// Analyzer builds the configured model provider.
func (a *App) Analyzer(ctx context.Context) (analyzer.Analyzer, error) {
	an, err := setup.FromConfig(ctx, a.Cfg, a.Log)
	if err != nil {
		return nil, err
	}
	a.Log.Info("analyzer ready", "analyzer", an.Name())
	return an, nil
}

// Runner builds the pipeline runner.
func (a *App) Runner(an analyzer.Analyzer) *pipeline.Runner {
	workers := a.Cfg.PipelineWorkers
	if workers == 0 {
		workers = queue.DefaultPipelineWorkers
	}
	// V2I_EXTRACT_CONCURRENCY, or auto from the CPUs this process may use (GOMAXPROCS
	// follows the cgroup CPU limit).
	procs := runtime.GOMAXPROCS(0)
	perJob := pipeline.ExtractConcurrency(a.Cfg.ExtractConcurrency, procs, a.Cfg.FFmpegThreads, workers)
	a.Log.Info("extract concurrency", "per_job", perJob, "configured", a.Cfg.ExtractConcurrency,
		"gomaxprocs", procs, "ffmpeg_threads", a.Cfg.FFmpegThreads, "pipeline_workers", workers)
	return &pipeline.Runner{
		Store:              a.Store,
		Storage:            a.Storage,
		FFmpeg:             a.FFmpeg,
		Analyzer:           an,
		Registry:           a.Registry,
		ScratchDir:         a.Cfg.ScratchDir,
		Logger:             a.Log,
		ExtractConcurrency: perJob,
	}
}

// NewWorker builds (but does not start) the River worker.
func (a *App) NewWorker(an analyzer.Analyzer) (*queue.Worker, error) {
	var sweeper analyzer.Sweeper
	if s, ok := an.(analyzer.Sweeper); ok {
		sweeper = s
	}
	return queue.NewWorker(a.Pool, queue.WorkerConfig{
		Runner:              a.Runner(an),
		Store:               a.Store,
		Storage:             a.Storage,
		Sweeper:             sweeper,
		PipelineWorkers:     a.Cfg.PipelineWorkers,
		DailyCostCeilingUSD: a.Cfg.DailyCostCeilingUSD,
		Logger:              a.Log,
	})
}
