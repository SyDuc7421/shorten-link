package metrics

import "time"

// Helpers so callers outside this package never touch Prometheus types or pass
// label values by hand. The exported vectors stay available to tests.

// CacheHit, CacheMiss and CacheError count one link cache lookup.
func CacheHit()   { CacheRequests.WithLabelValues("hit").Inc() }
func CacheMiss()  { CacheRequests.WithLabelValues("miss").Inc() }
func CacheError() { CacheRequests.WithLabelValues("error").Inc() }

// Redirect counts one redirect attempt by outcome (found, not_found, disabled, error).
func Redirect(outcome string) { Redirects.WithLabelValues(outcome).Inc() }

// LinkCreated counts one created link.
func LinkCreated() { LinksCreated.Inc() }

// Login counts one login attempt by result (ok, fail, locked, error).
func Login(result string) { Logins.WithLabelValues(result).Inc() }

// RateLimitRejected counts one request rejected by the rate limiter.
func RateLimitRejected(action string) { RateLimited.WithLabelValues(action).Inc() }

// Publish counts one RabbitMQ publish by routing key and result (ok, error).
func Publish(routingKey, result string) { Publishes.WithLabelValues(routingKey, result).Inc() }

// WorkerMessage counts n worker messages on queue by result (ack, nack, dlq).
func WorkerMessage(queue, result string, n int) {
	WorkerMessages.WithLabelValues(queue, result).Add(float64(n))
}

// ClickFlush records one click batch flush: its size, duration and result.
func ClickFlush(size int, d time.Duration, ok bool) {
	ClickBatchSize.Observe(float64(size))
	ClickFlushDuration.Observe(d.Seconds())
	result := "ok"
	if !ok {
		result = "error"
	}
	ClickFlushes.WithLabelValues(result).Inc()
}

// ExpiryScan records one expiry scan; a success also stamps the last-success gauge.
func ExpiryScan(ok bool, at time.Time) {
	if !ok {
		ExpiryScans.WithLabelValues("error").Inc()
		return
	}
	ExpiryScans.WithLabelValues("ok").Inc()
	ExpiryScanLastSuccess.Set(float64(at.Unix()))
}
