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
	smtpConnectionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "smtp_connections_total", Help: "SMTP connection admission attempts by result.",
	}, []string{"result"})
	smtpSessionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "smtp_sessions_total", Help: "SMTP sessions that completed HELO or EHLO by TLS state.",
	}, []string{"tls"})
	smtpSessionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "smtp_session_duration_seconds", Help: "Time spent handling admitted SMTP sessions.",
	}, []string{"result"})
	smtpRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "smtp_rejections_total", Help: "SMTP admission and delivery rejections by reason.",
	}, []string{"reason"})
	smtpTLSCertificateNotAfter = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "tmpmail", Name: "smtp_tls_certificate_not_after_timestamp", Help: "Unix timestamp at which the active SMTP TLS certificate expires; zero when SMTP TLS is disabled.",
	})
	smtpDeliveryDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "smtp_delivery_duration_seconds", Help: "Time to read and persist an SMTP DATA transaction.",
	}, []string{"result"})
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "http_requests_total", Help: "HTTP requests by normalized route and status.",
	}, []string{"route", "status"})
	httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "http_request_duration_seconds", Help: "HTTP request duration by normalized route and outcome class.",
	}, []string{"route", "outcome"})
	httpResponseBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "http_response_bytes_total", Help: "HTTP response body bytes by normalized route and status.",
	}, []string{"route", "status"})
	httpRequestsInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "tmpmail", Name: "http_requests_in_flight", Help: "HTTP requests currently being handled.",
	})
	httpOverloadRejections = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "http_overload_rejections_total", Help: "HTTP requests rejected by the application concurrency limit.",
	})
	httpCanceled = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "http_canceled_requests_total", Help: "HTTP requests whose context was canceled while being handled.",
	})
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
	storageSaveStageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "storage_save_stage_duration_seconds", Help: "Time spent in each message persistence stage.",
	}, []string{"stage", "result"})
	storageDBOpenConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "tmpmail", Name: "storage_db_open_connections", Help: "Open SQLite database connections.",
	})
	storageDBInUseConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "tmpmail", Name: "storage_db_in_use_connections", Help: "SQLite database connections currently in use.",
	})
	cleanupErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "tmpmail", Name: "cleanup_errors_total", Help: "Cleanup runs that returned an error.",
	})
	cleanupDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tmpmail", Name: "cleanup_duration_seconds", Help: "Time spent removing expired or evicted messages.",
	}, []string{"result"})
)

func init() {
	prometheus.MustRegister(smtpMessages, smtpMessageBytes, smtpConnections, smtpConnectionsTotal, smtpSessionsTotal, smtpSessionDuration, smtpRejections, smtpTLSCertificateNotAfter, smtpDeliveryDuration, httpRequests, httpDuration, httpResponseBytes, httpRequestsInFlight, httpOverloadRejections, httpCanceled, cleanupMessages, cleanupBytes, storageBytes, storageMessages, storageErrors, storageSaveDuration, storageWriteLockWait, storageReadDuration, storageSaveStageDuration, storageDBOpenConnections, storageDBInUseConnections, cleanupErrors, cleanupDuration)
}

func metricsHandler() http.Handler { return promhttp.Handler() }

func observeHTTP(route, status string, bytes int64, seconds float64) {
	httpRequests.WithLabelValues(route, status).Inc()
	httpDuration.WithLabelValues(route, statusClass(status)).Observe(seconds)
	httpResponseBytes.WithLabelValues(route, status).Add(float64(bytes))
}

func statusClass(status string) string {
	if len(status) == 3 {
		switch status[0] {
		case '2', '3', '4', '5':
			return status[:1] + "xx"
		}
	}
	return "unknown"
}

func metricRoute(path, pattern string) string {
	switch {
	case pattern == "GET /{inbox}":
		return "/{inbox}"
	case pattern == "" && strings.Count(strings.Trim(path, "/"), "/") == 0 && path != "/":
		// Admission control runs before the mux assigns r.Pattern. Preserve the
		// inbox shortcut's bounded metric cardinality on rejected requests.
		return "/{inbox}"
	case strings.HasPrefix(path, "/api/v1/inboxes/"):
		return "/api/v1/inboxes/{inbox}"
	case strings.HasPrefix(path, "/api/v1/messages/"):
		return "/api/v1/messages/{id}"
	case strings.HasPrefix(path, "/ui/messages/") && strings.HasSuffix(path, "/html"):
		return "/ui/messages/{id}/html"
	case strings.HasPrefix(path, "/ui/messages/"):
		return "/ui/messages/{id}"
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
