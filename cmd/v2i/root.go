package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"video-to-image/internal/app"
	"video-to-image/internal/config"
	"video-to-image/internal/httpapi"
	"video-to-image/internal/metrics"
	"video-to-image/internal/queue"
	"video-to-image/internal/store"
)

// version is set with -ldflags "-X main.version=<sha>".
var version = "dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "v2i",
		Short:         "Video-to-image backend: serve (API), work (pipeline worker), migrate, eval",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.AddCommand(newServeCmd(), newWorkCmd(), newMigrateCmd(), newEvalCmd(), newOpenAPICmd())
	return root
}

// signalContext is cancelled on SIGINT/SIGTERM (Kubernetes sends SIGTERM on rollout).
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func loadConfig(analyzerOverride string) (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if analyzerOverride != "" {
		cfg.Analyzer = analyzerOverride
	}
	return cfg, nil
}

func newServeCmd() *cobra.Command {
	var embedWorker bool
	var analyzerName string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP API (optionally with the pipeline worker in-process)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext()
			defer stop()

			cfg, err := loadConfig(analyzerName)
			if err != nil {
				return err
			}
			if err := errors.Join(cfg.RequireDatabase(), cfg.RequireStorage()); err != nil {
				return err
			}
			if embedWorker {
				if err := errors.Join(cfg.RequireAnalyzer(), cfg.RequireSafeDefaults()); err != nil {
					return err
				}
			}
			trusted, err := httpapi.ParseTrustedProxies(cfg.TrustedProxies)
			if err != nil {
				return fmt.Errorf("V2I_TRUSTED_PROXIES: %w", err)
			}
			log := app.NewLogger(cfg).With("component", "api")
			a, err := app.Open(ctx, cfg, log)
			if err != nil {
				return err
			}
			defer a.Close()

			ffmpegVersion, err := a.FFmpeg.Version(ctx)
			if err != nil {
				log.Warn("could not read ffmpeg version", "err", err)
			}

			var enqueuer httpapi.Enqueuer
			var worker *queue.Worker
			if embedWorker {
				an, err := a.Analyzer(ctx)
				if err != nil {
					return err
				}
				worker, err = a.NewWorker(an)
				if err != nil {
					return err
				}
				if err := worker.Start(ctx); err != nil {
					return fmt.Errorf("start worker: %w", err)
				}
				enqueuer = worker.Client()
				log.Info("embedded worker started", "analyzer", an.Name(), "pipeline_workers", cfg.PipelineWorkers,
					"daily_cost_ceiling_usd", cfg.DailyCostCeilingUSD)
			} else {
				c, err := queue.NewInsertClient(a.Pool, log)
				if err != nil {
					return err
				}
				enqueuer = c
			}

			srv := httpapi.New(httpapi.Deps{
				Registry:             a.Registry,
				Store:                a.Store,
				Storage:              a.Storage,
				Prober:               a.FFmpeg,
				Enqueuer:             enqueuer,
				Logger:               log,
				InternalToken:        cfg.InternalToken,
				IPHashSalt:           cfg.IPHashSalt,
				TrustedProxies:       trusted,
				ClientIPHeader:       cfg.ClientIPHeader,
				StatusRatePerMinute:  cfg.StatusRatePerMinute,
				MaxConcurrentUploads: cfg.MaxConcurrentUploads,
				MaxUploadsPerClient:  cfg.MaxUploadsPerClient,
				ScratchDir:           cfg.ScratchDir,
				PresignDownloads:     cfg.Storage.PresignDownloads,
				FFmpegVersion:        ffmpegVersion,
			})
			hs := &http.Server{
				Addr:              cfg.HTTPAddr,
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				// No Read/WriteTimeout: uploads of up to 500 MB over home upstream
				// bandwidth and ZIP downloads legitimately take minutes.
				IdleTimeout: 120 * time.Second,
				ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
			}
			// /metrics lives on the admin listener only (keep it off the public ingress).
			admin := newAdminServer(cfg.AdminAddr)
			go serveAdmin(admin, log)
			return runHTTP(ctx, hs, log, cfg.ShutdownTimeout, func(sctx context.Context) {
				_ = admin.Shutdown(sctx)
				if worker != nil {
					if err := worker.Stop(sctx); err != nil {
						log.Warn("worker stop", "err", err)
					}
				}
			})
		},
	}
	cmd.Flags().BoolVar(&embedWorker, "embed-worker", false, "also run the River pipeline worker in this process (local dev)")
	cmd.Flags().StringVar(&analyzerName, "analyzer", "", "override V2I_ANALYZER (gemini, fake, recording, recorded)")
	return cmd
}

func newWorkCmd() *cobra.Command {
	var analyzerName string
	cmd := &cobra.Command{
		Use:   "work",
		Short: "Run the River pipeline worker and periodic maintenance jobs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext()
			defer stop()

			cfg, err := loadConfig(analyzerName)
			if err != nil {
				return err
			}
			if err := errors.Join(cfg.RequireDatabase(), cfg.RequireStorage(), cfg.RequireAnalyzer(), cfg.RequireSafeDefaults()); err != nil {
				return err
			}
			log := app.NewLogger(cfg).With("component", "worker")
			a, err := app.Open(ctx, cfg, log)
			if err != nil {
				return err
			}
			defer a.Close()

			an, err := a.Analyzer(ctx)
			if err != nil {
				return err
			}
			worker, err := a.NewWorker(an)
			if err != nil {
				return err
			}
			if err := worker.Start(ctx); err != nil {
				return fmt.Errorf("start worker: %w", err)
			}
			log.Info("worker started", "analyzer", an.Name(), "pipeline_workers", cfg.PipelineWorkers,
				"daily_cost_ceiling_usd", cfg.DailyCostCeilingUSD)

			// Liveness + metrics for the worker pod (no readiness: it serves no traffic).
			hs := newAdminServer(cfg.AdminAddr)
			return runHTTP(ctx, hs, log, cfg.ShutdownTimeout, func(sctx context.Context) {
				if err := worker.Stop(sctx); err != nil {
					log.Warn("worker stop", "err", err)
				}
			})
		},
	}
	cmd.Flags().StringVar(&analyzerName, "analyzer", "", "override V2I_ANALYZER (gemini, fake, recording, recorded)")
	return cmd
}

// newAdminServer serves /livez and /metrics. It must not be exposed through the
// public ingress: metrics carry cost and traffic figures.
func newAdminServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle("/metrics", metrics.Handler())
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

// serveAdmin runs the admin listener next to the API; a failure is logged, not fatal.
func serveAdmin(hs *http.Server, log *slog.Logger) {
	log.Info("admin listening", "addr", hs.Addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("admin listener failed", "addr", hs.Addr, "err", err)
	}
}

// runHTTP serves until ctx is cancelled or the server fails, then shuts down
// gracefully: HTTP first (stop accepting work), then onShutdown (the worker).
func runHTTP(ctx context.Context, hs *http.Server, log *slog.Logger, timeout time.Duration, onShutdown func(context.Context)) error {
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", hs.Addr)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()

	var serveErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case serveErr = <-errc:
		log.Error("http server failed", "err", serveErr)
	}

	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	if onShutdown != nil {
		onShutdown(sctx)
	}
	return serveErr
}

func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Run River + goose migrations (idempotent; runs as a Kubernetes Job before rollout)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext()
			defer stop()
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if err := cfg.RequireDatabase(); err != nil {
				return err
			}
			log := app.NewLogger(cfg).With("component", "migrate")
			pool, err := app.OpenPool(ctx, cfg.DatabaseURL, 2*time.Minute, log)
			if err != nil {
				return err
			}
			defer pool.Close()
			if err := store.Migrate(ctx, pool, log); err != nil {
				return err
			}
			log.Info("migrations complete")
			return nil
		},
	}
}

func newEvalCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "eval",
		Short: "Run the pipeline over a folder of videos and write a report (Phase 2)",
		Long: "Planned for Phase 2 (plan/08-roadmap.md): v2i eval --dir <videos> --analyzer=gemini|recorded --out report/.\n" +
			"Until then, use `serve --embed-worker --analyzer=recording` to record model responses and\n" +
			"`--analyzer=recorded` to replay them for free.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errors.New("eval is not implemented yet (Phase 2 in plan/08-roadmap.md)")
		},
	}
}

func newOpenAPICmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "openapi",
		Short: "Write the OpenAPI 3.1 document (default: stdout)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := httpapi.OpenAPISpec()
			if err != nil {
				return err
			}
			spec = append(spec, '\n')
			if out == "" || out == "-" {
				_, err = cmd.OutOrStdout().Write(spec)
				return err
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(out, spec, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", out, err)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "output file (default stdout)")
	return cmd
}
