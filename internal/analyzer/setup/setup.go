// Package setup builds the configured analyzer.Analyzer (V2I_ANALYZER) for the serve,
// work and eval commands. It lives outside package analyzer so that package stays free
// of provider dependencies.
package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/analyzer/fake"
	"video-to-image/internal/analyzer/gemini"
	"video-to-image/internal/analyzer/recording"
	"video-to-image/internal/config"
)

// FromConfig returns the analyzer selected by cfg.Analyzer:
//
//   - gemini:    real Gemini calls
//   - fake:      deterministic canned answers (V2I_FAKE_RESPONSE_FILE, V2I_FAKE_DELAY)
//   - recording: real Gemini calls, each response written to V2I_RECORDINGS_DIR
//   - recorded:  replays V2I_RECORDINGS_DIR, no network
//
// The two real-call modes are wrapped in the global rate limit
// (V2I_MODEL_CALLS_PER_MINUTE); fake and recorded are free and not limited. Nothing
// here contacts the network. A nil logger means slog.Default().
func FromConfig(ctx context.Context, cfg *config.Config, logger *slog.Logger) (analyzer.Analyzer, error) {
	if cfg == nil {
		return nil, errors.New("setup: nil config")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := cfg.RequireAnalyzer(); err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}
	log := logger.With("analyzer", cfg.Analyzer)

	switch cfg.Analyzer {
	case config.AnalyzerGemini:
		g, err := newGemini(ctx, cfg, log)
		if err != nil {
			return nil, err
		}
		log.Info("analyzer ready", "calls_per_minute", cfg.ModelCallsPerMinute)
		return analyzer.WithRateLimit(g, cfg.ModelCallsPerMinute), nil

	case config.AnalyzerRecording:
		g, err := newGemini(ctx, cfg, log)
		if err != nil {
			return nil, err
		}
		rec, err := recording.NewRecorder(g, cfg.RecordingsDir, log)
		if err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
		log.Info("analyzer ready", "recordings_dir", cfg.RecordingsDir, "calls_per_minute", cfg.ModelCallsPerMinute)
		return analyzer.WithRateLimit(rec, cfg.ModelCallsPerMinute), nil

	case config.AnalyzerRecorded:
		rp, err := recording.NewReplayer(cfg.RecordingsDir, log)
		if err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
		log.Info("analyzer ready", "recordings_dir", cfg.RecordingsDir)
		return rp, nil

	case config.AnalyzerFake:
		f, err := fake.New(fake.Options{ResponseFile: cfg.FakeResponseFile, Delay: cfg.FakeDelay, Logger: log})
		if err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
		log.Info("analyzer ready", "response_file", cfg.FakeResponseFile, "delay", cfg.FakeDelay)
		return f, nil
	}
	// Unreachable while RequireAnalyzer and this switch agree.
	return nil, fmt.Errorf("setup: unknown analyzer %q", cfg.Analyzer)
}

func newGemini(ctx context.Context, cfg *config.Config, log *slog.Logger) (*gemini.Client, error) {
	g, err := gemini.New(ctx, gemini.Options{APIKey: cfg.GeminiAPIKey, BaseURL: cfg.GeminiBaseURL, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}
	return g, nil
}
