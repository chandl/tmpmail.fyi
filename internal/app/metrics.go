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
	cleanupErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "cleanup_errors_total", Help: "Cleanup runs that returned an error.",
	})
)

func init() {
	prometheus.MustRegister(smtpMessages, smtpMessageBytes, smtpConnections, smtpRejections, httpRequests, httpDuration, cleanupMessages, cleanupBytes, storageBytes, storageMessages, storageErrors, cleanupErrors)
}

func metricsHandler() http.Handler { return promhttp.Handler() }

func observeHTTP(route, status string, seconds float64) {
	httpRequests.WithLabelValues(route, status).Inc()
	httpDuration.WithLabelValues(route).Observe(seconds)
}

func metricRoute(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/inboxes/"):
		return "/api/inboxes/{inbox}"
	case strings.HasPrefix(path, "/api/messages/"):
		return "/api/messages/{id}"
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
