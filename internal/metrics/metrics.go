package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// HTTPRequestsTotal counts all incoming HTTP requests.
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aiproxy_http_requests_total",
		Help: "Total number of HTTP requests received.",
	}, []string{"method", "path", "status"})

	// HTTPRequestDuration observes the duration of HTTP requests.
	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "aiproxy_http_request_duration_seconds",
		Help:    "Duration of HTTP requests in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	// ProviderRequestsTotal counts requests to upstream providers.
	ProviderRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aiproxy_provider_requests_total",
		Help: "Total number of requests to upstream providers.",
	}, []string{"provider", "status", "model"})

	// ProviderRequestDuration observes the duration of upstream provider requests.
	ProviderRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "aiproxy_provider_request_duration_seconds",
		Help:    "Duration of upstream provider requests in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"provider", "model"})

	// ProviderRetriesTotal counts retries to upstream providers.
	ProviderRetriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aiproxy_provider_retries_total",
		Help: "Total number of retries to upstream providers.",
	}, []string{"provider", "reason"})

	// ConfigReloadsTotal counts config reload attempts.
	ConfigReloadsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aiproxy_config_reloads_total",
		Help: "Total number of config reload attempts.",
	}, []string{"status"})
)
