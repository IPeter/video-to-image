// Package metrics defines the Prometheus collectors shared by serve and work. All
// collectors register on the default registry, exposed via Handler() at /metrics.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var stepBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600}

var (
	// API side.
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "v2i_http_requests_total", Help: "HTTP requests by route pattern, method and status code.",
	}, []string{"route", "method", "code"})
	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "v2i_http_request_duration_seconds", Help: "HTTP request latency by route pattern.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method"})
	JobsCreated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "v2i_jobs_created_total", Help: "Jobs accepted (202) by domain.",
	}, []string{"domain"})
	UploadsRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "v2i_uploads_rejected_total", Help: "Uploads rejected before a job was created, by error code.",
	}, []string{"domain", "code"})

	// Worker side.
	JobsFinished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "v2i_jobs_finished_total", Help: "Jobs reaching a terminal status (completed, rejected, failed, expired).",
	}, []string{"domain", "status"})
	StepDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "v2i_step_duration_seconds", Help: "Pipeline step duration.", Buckets: stepBuckets,
	}, []string{"step"})
	ModelLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "v2i_model_latency_seconds", Help: "Model call latency.", Buckets: stepBuckets,
	}, []string{"model", "step"})
	ModelTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "v2i_model_tokens_total", Help: "Model tokens by kind (input, output, thinking).",
	}, []string{"model", "kind"})
	ModelCostUSD = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "v2i_model_cost_usd_total", Help: "Model cost in USD.",
	}, []string{"domain", "model"})
	ModelErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "v2i_model_errors_total", Help: "Model call failures by kind (unavailable, bad_output).",
	}, []string{"model", "kind"})
	DailyCostUSD = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "v2i_daily_cost_usd", Help: "Sum of usage_events.cost_usd since 00:00 UTC.",
	})
	QueuePaused = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "v2i_queue_paused", Help: "1 when a River queue is paused by the cost guard.",
	}, []string{"queue"})
	TableRows = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "v2i_table_rows", Help: "Approximate row counts of growing tables (candidate_frames etc.).",
	}, []string{"table"})
	JobsByStatus = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "v2i_jobs", Help: "Jobs in the database by status (refreshed by the stats job).",
	}, []string{"status"})
	QueueJobs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "v2i_queue_jobs", Help: "River jobs by queue and state.",
	}, []string{"queue", "state"})
)

// Handler serves the default registry.
func Handler() http.Handler { return promhttp.Handler() }
