package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMessageLifetimeLabel(t *testing.T) {
	for _, tc := range []struct {
		ttl  time.Duration
		want string
	}{
		{time.Hour, "1 hour"}, {30 * time.Minute, "30 minutes"}, {2 * time.Hour, "2 hours"},
		{24 * time.Hour, "1 day"}, {90 * time.Minute, "1 hour 30 minutes"},
		{time.Minute + time.Second, "1 minute 1 second"}, {1500 * time.Millisecond, "1.5 seconds"},
	} {
		if got := messageLifetimeLabel(tc.ttl); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.ttl, got, tc.want)
		}
	}
}

func TestPublicPagesShowStoredMessageLifetime(t *testing.T) {
	store := testStore(t, 90*time.Minute)
	server := NewHTTPServer(Config{MailDomain: "mail.test"}, store)
	for _, path := range []string{"/", "/?inbox=example", "/privacy"} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		page := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(page, "deleted after 1 hour 30 minutes") || strings.Contains(page, "deleted after 1 hour.") {
			t.Fatalf("%s: expected configured lifetime, status=%d", path, response.Code)
		}
		if path != "/privacy" && strings.Count(page, "deleted after 1 hour 30 minutes") != 2 {
			t.Fatalf("%s: expected both header and introduction to show configured lifetime", path)
		}
	}
}
