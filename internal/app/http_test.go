package app

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRequestLoggerLogsMillisecondsAndUserAgentWithoutRemoteAddress(t *testing.T) {
	var output bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request.RemoteAddr = "192.0.2.1:12345"
	request.Header.Set("User-Agent", "tmpmail-test/1.0")
	requestLogger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), false, []string{"User-Agent"}).ServeHTTP(httptest.NewRecorder(), request)

	entry := output.String()
	if !regexp.MustCompile(`duration_ms=\d+`).MatchString(entry) {
		t.Fatalf("expected millisecond duration, got %q", entry)
	}
	if !strings.Contains(entry, `user_agent="tmpmail-test/1.0"`) {
		t.Fatalf("expected user agent, got %q", entry)
	}
	if strings.Contains(entry, "remote=") || strings.Contains(entry, request.RemoteAddr) {
		t.Fatalf("expected no remote address, got %q", entry)
	}
}

func TestParseHTTPLogHeaders(t *testing.T) {
	headers, err := parseHTTPLogHeaders("User-Agent, CF-Connecting-IP, user-agent")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(headers, ",") != "User-Agent,Cf-Connecting-Ip" {
		t.Fatalf("unexpected headers: %#v", headers)
	}
	if _, err := parseHTTPLogHeaders("CF-Connecting-IP,not a header"); err == nil {
		t.Fatal("expected invalid header name to be rejected")
	}
}

func TestRequestLoggerLogsConfiguredHeaders(t *testing.T) {
	var output bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("CF-Connecting-IP", "198.51.100.10")
	requestLogger(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), false, []string{"CF-Connecting-IP"}).ServeHTTP(httptest.NewRecorder(), request)

	if !strings.Contains(output.String(), `cf_connecting_ip="198.51.100.10"`) {
		t.Fatalf("expected configured header, got %q", output.String())
	}
}

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
	if !strings.Contains(page, "<title>tmpmail - build@mail.test</title>") {
		t.Fatalf("expected inbox-specific page title, got %q", page)
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
	if !strings.Contains(page, "https://chandl.io/") || !strings.Contains(page, "https://github.com/chandl/tmpmail.fyi") {
		t.Fatalf("expected footer links, got %q", page)
	}
	if !strings.Contains(page, "href=\"/privacy\"") {
		t.Fatalf("expected privacy link, got %q", page)
	}
	if !strings.Contains(page, `<header class="top"><a class="brand" href="/">tmp<span>mail</span></a><span class="badge">disposable email</span></header>`) {
		t.Fatalf("expected shared wordmark and disposable-email badge, got %q", page)
	}
	if !strings.Contains(page, strconv.Itoa(time.Now().Year())) {
		t.Fatalf("expected current copyright year, got %q", page)
	}
}

func TestPrivacyPageExplainsMessageHandling(t *testing.T) {
	store := testStore(t, time.Hour)
	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/privacy", nil))

	page := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(page, "disposable email") || !strings.Contains(page, "Privacy and message handling") || !strings.Contains(page, "automatically deleted after one hour by default") || !strings.Contains(page, "does not include advertising or analytics trackers") {
		t.Fatalf("expected privacy details page, got status=%d body=%q", response.Code, page)
	}
}

func TestHTMLMessageUsesLightTheme(t *testing.T) {
	store := testStore(t, time.Hour)
	message, err := store.Save("build@mail.test", "sender@example.org", []byte("Content-Type: text/html\r\n\r\n<p>Hello</p>"))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/messages/"+message.ID+"/html", nil))

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `color-scheme" content="light"`) || !strings.Contains(response.Body.String(), "background:#fff;color:#1e293b") {
		t.Fatalf("expected light HTML message rendering, got status=%d body=%q", response.Code, response.Body.String())
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

func TestServesFaviconPack(t *testing.T) {
	store := testStore(t, time.Hour)
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)
	for _, asset := range []string{"/favicon.ico", "/favicon-16x16.png", "/favicon-32x32.png", "/apple-touch-icon.png", "/android-chrome-192x192.png", "/android-chrome-512x512.png", "/site.webmanifest"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, asset, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("expected favicon asset %s, got status=%d body length=%d", asset, response.Code, response.Body.Len())
		}
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

func TestInboxUIDefaultsToLightTheme(t *testing.T) {
	store := testStore(t, time.Hour)
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(page.Body.String(), "body{margin:0;background:#f1f5f9;color:#1e293b") {
		t.Fatalf("expected light page theme, got %q", page.Body.String())
	}

	styles := httptest.NewRecorder()
	handler.ServeHTTP(styles, httptest.NewRequest(http.MethodGet, "/ui.css", nil))
	if !strings.Contains(styles.Body.String(), ".message-list{overflow:auto;padding:9px;border-right:1px solid #cbd5e1;background:#f8fafc}") {
		t.Fatalf("expected light mailbox theme, got %q", styles.Body.String())
	}
}

func TestMetricsEndpointRequiresOptIn(t *testing.T) {
	store := testStore(t, time.Hour)
	public := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test", MetricsEnabled: true}, store).ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("the public server must not serve metrics, got %d", public.Code)
	}

	enabled := httptest.NewRecorder()
	NewMetricsServer().ServeHTTP(enabled, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if enabled.Code != http.StatusOK || !strings.Contains(enabled.Body.String(), "tmpmail_smtp_message_bytes_total") {
		t.Fatalf("expected Prometheus metrics, got status=%d body=%q", enabled.Code, enabled.Body.String())
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
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/inboxes/build@mail.test?limit=25&offset=0", nil))

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"hasMore":false`) {
		t.Fatalf("expected paged inbox response, got status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestMessageAPISeparatesHeadersAndBody(t *testing.T) {
	store := testStore(t, time.Hour)
	message, err := store.Save("build@mail.test", "sender@example.org", []byte("From: sender@example.org\r\nSubject: hello\r\n\r\nbody"))
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+message.ID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("got status %d: %q", response.Code, response.Body.String())
	}

	var result struct {
		Headers string `json:"headers"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Headers != "From: sender@example.org\r\nSubject: hello" || result.Body != "body" {
		t.Fatalf("expected separated headers and body, got %#v", result)
	}
}
