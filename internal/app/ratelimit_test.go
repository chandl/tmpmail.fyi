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
