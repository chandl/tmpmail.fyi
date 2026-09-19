package app

import (
	"net"
	"net/http"
	"strings"

	"github.com/didip/tollbooth/v7"
	"github.com/didip/tollbooth/v7/limiter"
)

// Fallback defaults for callers that build a Config directly instead of via LoadConfig (which
// always fills these in with a positive value). Kept in sync with config.go's env defaults.
const (
	defaultRateLimitRPS   = 10
	defaultRateLimitBurst = 50
)

// newRateLimiter builds a tollbooth limiter keyed on RemoteAddr only. Tollbooth's own
// default IPLookups also trusts X-Forwarded-For/X-Real-IP; we deliberately don't, since
// those headers are spoofable unless a trusted proxy sets them. HTTP_RATE_LIMIT_IP_HEADER
// support is layered on top by rewriting RemoteAddr before this limiter runs.
func newRateLimiter(rps float64, burst int) *limiter.Limiter {
	if rps <= 0 {
		rps = defaultRateLimitRPS
	}
	if burst <= 0 {
		burst = defaultRateLimitBurst
	}
	limit := tollbooth.NewLimiter(rps, nil).
		SetBurst(burst).
		SetIPLookups([]string{"RemoteAddr"}).
		SetIgnoreURL(true) // limit per IP across all routes; tollbooth otherwise keys by IP+path,
		// which would give every distinct inbox/message-ID path its own fresh bucket.
	limit.SetOnLimitReached(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
	})
	return limit
}

// rateLimitPerIP rate-limits requests per client IP to rps requests/second (with the given
// burst). When ipHeader is set (for example "CF-Connecting-IP"), the client IP is read from
// that header instead of RemoteAddr, since every request otherwise arrives from the same
// reverse-proxy address. Only enable this for a header a trusted proxy actually sets/overwrites
// itself.
func rateLimitPerIP(next http.Handler, rps float64, burst int, ipHeader string) http.Handler {
	limited := tollbooth.LimitHandler(newRateLimiter(rps, burst), next)
	if ipHeader == "" {
		return limited
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := firstHeaderValue(r.Header.Get(ipHeader)); ip != "" {
			clone := r.Clone(r.Context())
			clone.RemoteAddr = net.JoinHostPort(ip, "0")
			r = clone
		}
		limited.ServeHTTP(w, r)
	})
}

// firstHeaderValue returns the first entry of a comma-separated header value (as used by
// headers like X-Forwarded-For), trimmed of whitespace.
func firstHeaderValue(value string) string {
	first, _, _ := strings.Cut(value, ",")
	return strings.TrimSpace(first)
}
