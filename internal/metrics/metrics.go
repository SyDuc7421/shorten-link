// Package metrics holds the Prometheus registry and every metric the server
// and worker export. Updating a metric never blocks and never returns an error.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry is served on /metrics. It is separate from the default registry so
// only metrics declared here are exposed.
var Registry = prometheus.NewRegistry()

var (
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_http_requests_total",
		Help: "HTTP requests by method, route pattern and status.",
	}, []string{"method", "route", "status"})
	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "shortlink_http_request_duration_seconds",
		Help:    "HTTP request duration by method and route pattern.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"method", "route"})
	HTTPInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "shortlink_http_requests_in_flight",
		Help: "HTTP requests currently being served.",
	})

	CacheRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_link_cache_requests_total",
		Help: "Link cache lookups by result (hit, miss, error).",
	}, []string{"result"})
	Redirects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_redirects_total",
		Help: "Redirect attempts by outcome (found, not_found, disabled, error).",
	}, []string{"outcome"})
	LinksCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "shortlink_links_created_total",
		Help: "Links created.",
	})
	Logins = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_auth_logins_total",
		Help: "Login attempts by result (ok, fail, locked).",
	}, []string{"result"})
	RateLimited = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_ratelimit_rejected_total",
		Help: "Requests rejected by the rate limiter, by action.",
	}, []string{"action"})

	Publishes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_rabbitmq_publish_total",
		Help: "RabbitMQ publishes by routing key and result (ok, error).",
	}, []string{"routing_key", "result"})
	WorkerMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_worker_messages_total",
		Help: "Worker messages by queue and result (ack, nack, dlq).",
	}, []string{"queue", "result"})
	ClickBatchSize = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "shortlink_worker_click_batch_size",
		Help:    "Number of click events per flush.",
		Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500},
	})
	ClickFlushDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "shortlink_worker_click_flush_duration_seconds",
		Help:    "Time to write one click batch.",
		Buckets: prometheus.DefBuckets,
	})
	ClickFlushes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_worker_click_flush_total",
		Help: "Click batch flushes by result.",
	}, []string{"result"})
	ExpiryScans = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_worker_expiry_scan_total",
		Help: "Expiry scans by result.",
	}, []string{"result"})
	ExpiryScanLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "shortlink_worker_expiry_scan_last_success_timestamp_seconds",
		Help: "Unix time of the last successful expiry scan.",
	})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration, HTTPInFlight,
		CacheRequests, Redirects, LinksCreated, Logins, RateLimited,
		Publishes, WorkerMessages, ClickBatchSize, ClickFlushDuration,
		ClickFlushes, ExpiryScans, ExpiryScanLastSuccess,
	)
}

// RegisterPublisherConnected exposes shortlink_rabbitmq_publisher_connected
// (1 when connected, 0 otherwise). Call it once per process.
func RegisterPublisherConnected(connected func() bool) {
	Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "shortlink_rabbitmq_publisher_connected",
		Help: "1 if the RabbitMQ publisher is connected, else 0.",
	}, func() float64 {
		if connected() {
			return 1
		}
		return 0
	}))
}
