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

func TestServesMailClientUIAssets(t *testing.T) {
	store := testStore(t, time.Hour)
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui.js", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "message-list") || !strings.Contains(response.Body.String(), "New random") || !strings.Contains(response.Body.String(), "Refresh") {
		t.Fatalf("expected mail client UI script, got status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestContentSecurityPolicyAllowsUIAssets(t *testing.T) {
	store := testStore(t, time.Hour)
	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("UI scripts are blocked by CSP: %q", response.Header().Get("Content-Security-Policy"))
	}
}

func TestInboxAPIUsesPageResponse(t *testing.T) {
	store := testStore(t, time.Hour)
	if _, err := store.Save("build@mail.test", "sender@example.org", []byte("Subject: hello\r\n\r\nbody")); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/inboxes/build@mail.test?limit=25&offset=0", nil))

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"hasMore":false`) {
		t.Fatalf("expected paged inbox response, got status=%d body=%q", response.Code, response.Body.String())
	}
}
