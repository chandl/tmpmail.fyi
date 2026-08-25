package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInboxUIAppendsConfiguredDomain(t *testing.T) {
	store := testStore(t, time.Hour)
	if _, err := store.Save("build@mail.test", "sender@example.org", []byte("Subject: hello\r\n\r\nbody")); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/?inbox=build", nil)
	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "build@mail.test") {
		t.Fatalf("expected resolved inbox address, got %q", response.Body.String())
	}
}
