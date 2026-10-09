package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A small, fast-to-exhaust burst used across tests so they don't depend on the production
// defaults and stay quick regardless of what those defaults are tuned to.
const testRateLimitBurst = 5

func TestRateLimitPerIPRejectsBurstFromSameIP(t *testing.T) {
	handler := rateLimitPerIP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), 5, testRateLimitBurst, "")

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "203.0.113.10:5555"

	var last *httptest.ResponseRecorder
	for i := 0; i < testRateLimitBurst+1; i++ {
		last = httptest.NewRecorder()
		handler.ServeHTTP(last, request)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after exceeding burst, got %d", last.Code)
	}
	if last.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected Retry-After header, got %q", last.Header().Get("Retry-After"))
	}

	other := httptest.NewRequest(http.MethodGet, "/", nil)
	other.RemoteAddr = "203.0.113.20:5555"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, other)
	if response.Code != http.StatusNoContent {
		t.Fatalf("expected a different IP to be unaffected, got %d", response.Code)
	}
}

func TestRateLimitPerIPAppliesAcrossRoutesNotPerPath(t *testing.T) {
	handler := rateLimitPerIP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), 5, testRateLimitBurst, "")

	// Exhaust the burst on one path, then confirm a different path from the same IP is
	// already limited too: the bucket must be keyed by IP alone, not (IP, path), otherwise
	// routes with per-resource paths (e.g. /ui/messages/{id}) would each get a fresh bucket.
	request := httptest.NewRequest(http.MethodGet, "/build-482", nil)
	request.RemoteAddr = "203.0.113.40:5555"
	for i := 0; i < testRateLimitBurst; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}

	other := httptest.NewRequest(http.MethodGet, "/ui/messages/some-id", nil)
	other.RemoteAddr = "203.0.113.40:5555"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, other)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the limit to apply across routes for the same IP, got %d", response.Code)
	}
}

func TestRateLimitPerIPUsesConfiguredHeader(t *testing.T) {
	handler := rateLimitPerIP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), 5, testRateLimitBurst, "CF-Connecting-IP")

	requestFor := func(headerValue string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = "198.51.100.1:1" // shared proxy address for every client
		if headerValue != "" {
			request.Header.Set("CF-Connecting-IP", headerValue)
		}
		return request
	}

	var last *httptest.ResponseRecorder
	for i := 0; i < testRateLimitBurst+1; i++ {
		last = httptest.NewRecorder()
		handler.ServeHTTP(last, requestFor("203.0.113.30"))
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("expected first client to be rate-limited, got %d", last.Code)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, requestFor("203.0.113.31"))
	if response.Code != http.StatusNoContent {
		t.Fatalf("expected a different header value to be unaffected, got %d", response.Code)
	}
}

func TestRateLimitPerIPDefaultsWhenUnconfigured(t *testing.T) {
	// Config{} built directly (bypassing LoadConfig) leaves RPS/burst at their zero values;
	// newRateLimiter must fall back to sane defaults rather than blocking every request.
	handler := rateLimitPerIP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), 0, 0, "")

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "203.0.113.50:5555"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("expected the first request to pass through on defaults, got %d", response.Code)
	}
}

func TestFirstHeaderValue(t *testing.T) {
	cases := map[string]string{
		"":                        "",
		"203.0.113.1":             "203.0.113.1",
		"203.0.113.1, 10.0.0.1":   "203.0.113.1",
		"  203.0.113.1 ,10.0.0.1": "203.0.113.1",
	}
	for input, want := range cases {
		if got := firstHeaderValue(input); got != want {
			t.Fatalf("firstHeaderValue(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHealthcheckBypassesExhaustedClientBucket(t *testing.T) {
	for _, ipHeader := range []string{"", "CF-Connecting-IP"} {
		t.Run("header="+ipHeader, func(t *testing.T) {
			calls := 0
			downstreamStatus := http.StatusNoContent
			handler := rateLimitPerIP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(downstreamStatus)
			}), 0.00001, 1, ipHeader)
			request := func(method, path string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, nil)
				r.RemoteAddr = "127.0.0.1:5555"
				if ipHeader != "" {
					r.Header.Set(ipHeader, "203.0.113.42")
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			if got := request(http.MethodGet, "/").Code; got != http.StatusNoContent {
				t.Fatalf("first request = %d", got)
			}
			if got := request(http.MethodGet, "/").Code; got != http.StatusTooManyRequests {
				t.Fatalf("exhausted bucket = %d", got)
			}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				before := calls
				if got := request(method, "/healthz?probe=1").Code; got != http.StatusNoContent || calls != before+1 {
					t.Fatalf("%s healthcheck = %d, downstream calls = %d; want 204 and one forwarded call", method, got, calls-before)
				}
			}
			// Bypassing the client bucket must still respect downstream admission.
			downstreamStatus = http.StatusServiceUnavailable
			before := calls
			if got := request(http.MethodGet, "/healthz").Code; got != http.StatusServiceUnavailable || calls != before+1 {
				t.Fatalf("downstream health failure = %d, calls = %d", got, calls-before)
			}
			for _, tc := range []struct{ method, path string }{
				{http.MethodGet, "/ui/messages/id"},
				{http.MethodGet, "/healthz/other"},
				{http.MethodPost, "/healthz"},
			} {
				before := calls
				if got := request(tc.method, tc.path).Code; got != http.StatusTooManyRequests || calls != before {
					t.Fatalf("%s %s = %d, calls = %d; want rate limit without downstream call", tc.method, tc.path, got, calls-before)
				}
			}
		})
	}
}
