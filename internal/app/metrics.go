package app

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	smtpMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "smtp_messages_total", Help: "SMTP message delivery attempts by result.",
	}, []string{"result"})
	smtpMessageBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "smtp_message_bytes_total", Help: "Bytes accepted through SMTP.",
	})
	smtpConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "tmpmail", Name: "smtp_connections_active", Help: "SMTP sessions currently being handled.",
	})
	smtpRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "smtp_rejections_total", Help: "SMTP rejections caused by protective limits.",
	}, []string{"reason"})
	smtpDeliveryDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "smtp_delivery_duration_seconds", Help: "Time to read and persist an SMTP DATA transaction.",
	}, []string{"result"})
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "http_requests_total", Help: "HTTP requests by normalized route and status.",
	}, []string{"route", "status"})
	httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "http_request_duration_seconds", Help: "HTTP request duration by normalized route.",
	}, []string{"route"})
	cleanupMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "cleanup_messages_total", Help: "Messages removed by cleanup reason.",
	}, []string{"reason"})
	cleanupBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "cleanup_bytes_total", Help: "Bytes reclaimed by cleanup reason.",
	}, []string{"reason"})
	storageBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "tmpmail", Name: "storage_bytes", Help: "Bytes currently used by stored messages.",
	})
	storageMessages = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "tmpmail", Name: "storage_messages", Help: "Messages currently stored.",
	})
	storageErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "storage_errors_total", Help: "Storage operation errors by operation.",
	}, []string{"operation"})
	storageSaveDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "storage_save_duration_seconds", Help: "Time to persist a raw message and its metadata.",
	}, []string{"result"})
	storageWriteLockWait = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "storage_write_lock_wait_seconds", Help: "Time spent waiting for the serialized storage write lock.",
	})
	storageReadDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "storage_read_duration_seconds", Help: "Time to read inbox listings and messages from storage.",
	}, []string{"operation", "result"})
	cleanupErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "cleanup_errors_total", Help: "Cleanup runs that returned an error.",
	})
	cleanupDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "cleanup_duration_seconds", Help: "Time spent removing expired or evicted messages.",
	}, []string{"result"})
)

func init() {
	prometheus.MustRegister(smtpMessages, smtpMessageBytes, smtpConnections, smtpRejections, smtpDeliveryDuration, httpRequests, httpDuration, cleanupMessages, cleanupBytes, storageBytes, storageMessages, storageErrors, storageSaveDuration, storageWriteLockWait, storageReadDuration, cleanupErrors, cleanupDuration)
}

func metricsHandler() http.Handler { return promhttp.Handler() }

func observeHTTP(route, status string, seconds float64) {
	httpRequests.WithLabelValues(route, status).Inc()
	httpDuration.WithLabelValues(route).Observe(seconds)
}

func metricRoute(path, pattern string) string {
	switch {
	case pattern == "GET /{inbox}":
		return "/{inbox}"
	case strings.HasPrefix(path, "/api/v1/inboxes/"):
		return "/api/v1/inboxes/{inbox}"
	case strings.HasPrefix(path, "/api/v1/messages/"):
		return "/api/v1/messages/{id}"
	case strings.HasPrefix(path, "/ui/messages/") && strings.HasSuffix(path, "/html"):
		return "/ui/messages/{id}/html"
	default:
		return path
	}
}

func observeCleanup(stats cleanupStats) {
	if stats.Expired > 0 {
		cleanupMessages.WithLabelValues("expired").Add(float64(stats.Expired))
		cleanupBytes.WithLabelValues("expired").Add(float64(stats.ExpiredBytes))
	}
	if stats.Evicted > 0 {
		cleanupMessages.WithLabelValues("storage_limit").Add(float64(stats.Evicted))
		cleanupBytes.WithLabelValues("storage_limit").Add(float64(stats.EvictedBytes))
	}
}

func observeStorageUsage(bytes, messages int64) {
	storageBytes.Set(float64(bytes))
	storageMessages.Set(float64(messages))
}

func metricResult(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}
