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
	page := response.Body.String()
	if !strings.Contains(page, "build@mail.test") {
		t.Fatalf("expected resolved inbox address, got %q", page)
	}
	if !strings.Contains(page, "Message headers") || !strings.Contains(page, "body") {
		t.Fatalf("expected separate message sections, got %q", page)
	}
	if !strings.Contains(page, "tmpmail:last-inbox") {
		t.Fatalf("expected last inbox browser storage, got %q", page)
	}
	if !strings.Contains(page, "Copy address") || !strings.Contains(page, "randomInbox") {
		t.Fatalf("expected generated inbox and copy controls, got %q", page)
	}
	if !strings.Contains(page, "local-time") {
		t.Fatalf("expected browser-local timestamps, got %q", page)
	}
}

func TestServesEmbeddedOpenAPISpecification(t *testing.T) {
	store := testStore(t, time.Hour)
	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"openapi":"3.1.0"`) {
		t.Fatalf("expected OpenAPI document, got %q", response.Body.String())
	}
}
