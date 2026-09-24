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
	}), false, "all", []string{"User-Agent"}).ServeHTTP(httptest.NewRecorder(), request)

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
	requestLogger(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), false, "all", []string{"CF-Connecting-IP"}).ServeHTTP(httptest.NewRecorder(), request)

	if !strings.Contains(output.String(), `cf_connecting_ip="198.51.100.10"`) {
		t.Fatalf("expected configured header, got %q", output.String())
	}
}

func TestHTTPConcurrencyLimitReturnsRetryableOverload(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := limitHTTPRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}), 1, false)

	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(firstDone)
	}()
	<-entered
	overloaded := httptest.NewRecorder()
	handler.ServeHTTP(overloaded, httptest.NewRequest(http.MethodGet, "/", nil))
	if overloaded.Code != http.StatusServiceUnavailable || overloaded.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected retryable overload, got status=%d retry-after=%q", overloaded.Code, overloaded.Header().Get("Retry-After"))
	}
	close(release)
	<-firstDone
}

func TestRequestMetricsIncludeOutcomeAndResponseBytes(t *testing.T) {
	store := testStore(t, time.Hour)
	handler := NewHTTPServer(Config{MailDomain: "mail.test", MetricsEnabled: true, HTTPAccessLogMode: "off"}, store)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/privacy", nil))

	metrics := httptest.NewRecorder()
	NewMetricsServer().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, metric := range []string{"tmpmail_http_response_bytes_total", "tmpmail_http_request_duration_seconds_bucket", `outcome="2xx"`, `route="/privacy"`} {
		if !strings.Contains(metrics.Body.String(), metric) {
			t.Fatalf("expected %s in metrics output", metric)
		}
	}
}

func TestInboxShortcutUsesStableMetricsRoute(t *testing.T) {
	for _, inbox := range []string{"asdf", "another-inbox"} {
		if route := metricRoute("/"+inbox, "GET /{inbox}"); route != "/{inbox}" {
			t.Fatalf("expected stable inbox metric route, got %q", route)
		}
	}
	if route := metricRoute("/privacy", "GET /privacy"); route != "/privacy" {
		t.Fatalf("expected named route to remain distinct, got %q", route)
	}
}

func TestInboxUIAppendsConfiguredDomain(t *testing.T) {
	store := testStore(t, time.Hour)
	saveOne(t, store, "build@mail.test", "sender@example.org", []byte("Subject: hello\r\n\r\nbody"))

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
	if !strings.Contains(page, `class="has-inbox"`) || !strings.Contains(page, `class="mailbox"`) || !strings.Contains(page, `role="listbox"`) || !strings.Contains(page, `aria-selected="true"`) {
		t.Fatalf("expected populated inboxes to use the two-pane mailbox layout, got %q", page)
	}
	if !strings.Contains(page, "<title>tmpmail - build@mail.test</title>") {
		t.Fatalf("expected inbox-specific page title, got %q", page)
	}
	if !strings.Contains(page, `/ui.js?v=`+uiVersion) || !strings.Contains(page, "<style>/* tmpmail UI.") {
		t.Fatalf("expected inlined CSS and cache-busted UI script, got %q", page)
	}
	if !strings.Contains(page, "Show original headers") || !strings.Contains(page, `data-message-id="`) {
		t.Fatalf("expected message metadata and loading targets, got %q", page)
	}
	if !strings.Contains(page, ">body</pre>") {
		t.Fatalf("expected the initial render to contain the message body, got %q", page)
	}
	if !strings.Contains(uiScript, "tmpmail:last-inbox") {
		t.Fatal("expected last inbox browser storage")
	}
	if !strings.Contains(page, `data-copy="build@mail.test"`) || !strings.Contains(page, "data-new-address") || !strings.Contains(uiScript, "randomInbox") {
		t.Fatalf("expected generated inbox and copy controls, got %q", page)
	}
	if !strings.Contains(page, "local-time") {
		t.Fatalf("expected browser-local timestamps, got %q", page)
	}
	if !strings.Contains(page, "https://chandl.io/") || !strings.Contains(page, "https://github.com/chandl/tmpmail.fyi") {
		t.Fatalf("expected footer links, got %q", page)
	}
	if !strings.Contains(page, "href=\"/privacy\"") || !strings.Contains(page, `href="/openapi.json"`) {
		t.Fatalf("expected privacy link, got %q", page)
	}
	if !strings.Contains(page, `<a class="wordmark" href="/">tmpmail</a>`) || !strings.Contains(page, "Disposable email · deleted after 1 hour") {
		t.Fatalf("expected shared wordmark and disposable-email note, got %q", page)
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
	if response.Code != http.StatusOK || !strings.Contains(page, "Disposable email") || !strings.Contains(page, "Privacy and message handling") || !strings.Contains(page, "automatically deleted after one hour by default") || !strings.Contains(page, "does not include advertising or analytics trackers") {
		t.Fatalf("expected privacy details page, got status=%d body=%q", response.Code, page)
	}
}

func TestInboxShortcutRedirectsSinglePathSegment(t *testing.T) {
	store := testStore(t, time.Hour)
	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/asdf?offset=25", nil))

	if response.Code != http.StatusFound {
		t.Fatalf("expected inbox shortcut redirect, got status=%d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/?inbox=asdf&offset=25" {
		t.Fatalf("expected inbox shortcut location, got %q", location)
	}
}

func TestInboxShortcutDoesNotOverrideRegisteredPaths(t *testing.T) {
	store := testStore(t, time.Hour)
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)

	for _, path := range []string{"/privacy", "/ui.css", "/openapi.json"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code == http.StatusFound {
			t.Fatalf("expected registered path %s not to redirect as an inbox", path)
		}
	}
}

func TestHTMLMessageUsesLightTheme(t *testing.T) {
	store := testStore(t, time.Hour)
	message := saveOne(t, store, "build@mail.test", "sender@example.org", []byte("Content-Type: text/html\r\n\r\n<style>p{color:red}</style><p>Hello <a href=\"https://example.com\">world</a></p>"))
	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/messages/"+message.ID+"/html", nil))

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `color-scheme" content="light"`) || !strings.Contains(response.Body.String(), "background:#fff;color:#1e293b") {
		t.Fatalf("expected light HTML message rendering, got status=%d body=%q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `<style>p{color:red}</style>`) || !strings.Contains(response.Body.String(), `target="_blank" rel="noopener noreferrer"`) {
		t.Fatalf("expected formatted HTML and clickable external links, got %q", response.Body.String())
	}
	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "img-src 'none'") || !strings.Contains(policy, "form-action 'none'") {
		t.Fatalf("expected isolated HTML message policy, got %q", policy)
	}
}

func TestInboxLoadsSelectedMessageOnDemand(t *testing.T) {
	store := testStore(t, time.Hour)
	message := saveOne(t, store, "build@mail.test", "sender@example.org", []byte("Content-Type: multipart/alternative; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nPlain body\r\n--x\r\nContent-Type: text/html\r\n\r\n<p>HTML body</p>\r\n--x--\r\n"))

	page := httptest.NewRecorder()
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/?inbox=build", nil))
	if !strings.Contains(page.Body.String(), "data-email-dark-toggle") || !strings.Contains(uiCSS, "filter:invert(.93) hue-rotate(180deg)") {
		t.Fatal("expected a dark preview toggle for HTML emails")
	}
	if !strings.Contains(page.Body.String(), `data-message-id="`+message.ID+`"`) || strings.Contains(page.Body.String(), "HTML body") {
		t.Fatalf("expected page to contain only message metadata, got %q", page.Body.String())
	}
	details := httptest.NewRecorder()
	handler.ServeHTTP(details, httptest.NewRequest(http.MethodGet, "/ui/messages/"+message.ID, nil))
	if details.Code != http.StatusOK || !strings.Contains(details.Body.String(), `"text":"Plain body"`) || !strings.Contains(details.Body.String(), `"hasHtml":true`) {
		t.Fatalf("expected selected-message details, got status=%d body=%q", details.Code, details.Body.String())
	}

	script := httptest.NewRecorder()
	handler.ServeHTTP(script, httptest.NewRequest(http.MethodGet, "/ui.js", nil))
	contents := script.Body.String()
	if !strings.Contains(contents, "fetch('/ui/messages/'") || !strings.Contains(contents, "show-plain") || !strings.Contains(contents, "allow-popups allow-popups-to-escape-sandbox") {
		t.Fatalf("expected HTML-first reader with safe external navigation, got %q", contents)
	}
}

func TestMessageAttachmentEndpointServesContentAndRejectsBadIndex(t *testing.T) {
	store := testStore(t, time.Hour)
	raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--x\r\nContent-Type: text/csv; name=\"report.csv\"\r\nContent-Disposition: attachment; filename=\"report.csv\"\r\n\r\na,b,c\r\n" +
		"--x--\r\n"
	message := saveOne(t, store, "build@mail.test", "sender@example.org", []byte(raw))
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+message.ID+"/attachments/0", nil))
	if response.Code != http.StatusOK || response.Body.String() != "a,b,c" {
		t.Fatalf("expected attachment content, got status=%d body=%q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Disposition"), `filename="report.csv"`) {
		t.Fatalf("expected attachment filename in Content-Disposition, got %q", response.Header().Get("Content-Disposition"))
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+message.ID+"/attachments/5", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an out-of-range attachment index, got %d", missing.Code)
	}

	unknownMessage := httptest.NewRecorder()
	handler.ServeHTTP(unknownMessage, httptest.NewRequest(http.MethodGet, "/api/v1/messages/00000000000000000000000000000000/attachments/0", nil))
	if unknownMessage.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown message, got %d", unknownMessage.Code)
	}
}

func TestMessageAndUIResponsesListAttachments(t *testing.T) {
	store := testStore(t, time.Hour)
	raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--x\r\nContent-Type: text/csv; name=\"report.csv\"\r\nContent-Disposition: attachment; filename=\"report.csv\"\r\n\r\na,b,c\r\n" +
		"--x--\r\n"
	message := saveOne(t, store, "build@mail.test", "sender@example.org", []byte(raw))
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)

	apiResponse := httptest.NewRecorder()
	handler.ServeHTTP(apiResponse, httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+message.ID, nil))
	if !strings.Contains(apiResponse.Body.String(), `"filename":"report.csv"`) {
		t.Fatalf("expected attachments in message API response, got %q", apiResponse.Body.String())
	}

	uiResponse := httptest.NewRecorder()
	handler.ServeHTTP(uiResponse, httptest.NewRequest(http.MethodGet, "/ui/messages/"+message.ID, nil))
	if !strings.Contains(uiResponse.Body.String(), `"filename":"report.csv"`) {
		t.Fatalf("expected attachments in UI message response, got %q", uiResponse.Body.String())
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/?inbox=build", nil))
	if !strings.Contains(page.Body.String(), "curl -sOJ &#39;{origin}/api/v1/messages/{id}/attachments/0&#39;") {
		t.Fatalf("expected a copyable attachment download command, got %q", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "report.csv") || !strings.Contains(page.Body.String(), "/api/v1/messages/"+message.ID+"/attachments/0") {
		t.Fatalf("expected server-rendered inbox to link the attachment, got %q", page.Body.String())
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
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "message-list") || !strings.Contains(response.Body.String(), "data-new-address") || !strings.Contains(response.Body.String(), "data-refresh") {
		t.Fatalf("expected mail client UI script, got status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestInboxUIFollowsSystemThemeWithLightDefault(t *testing.T) {
	store := testStore(t, time.Hour)
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	body := page.Body.String()
	if !strings.Contains(body, "--bg:#fafaf7") || !strings.Contains(body, "@media (prefers-color-scheme:dark)") || !strings.Contains(body, `localStorage.getItem("tmpmail:theme")`) {
		t.Fatalf("expected light defaults, system dark theme, and a pre-paint theme script, got %q", body)
	}

	styles := httptest.NewRecorder()
	handler.ServeHTTP(styles, httptest.NewRequest(http.MethodGet, "/ui.css?v="+uiVersion, nil))
	if styles.Body.String() != uiCSS || !strings.Contains(styles.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("expected versioned stylesheet to be cacheable, got cache-control=%q", styles.Header().Get("Cache-Control"))
	}
	unversioned := httptest.NewRecorder()
	handler.ServeHTTP(unversioned, httptest.NewRequest(http.MethodGet, "/ui.js", nil))
	if unversioned.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("expected unversioned script to revalidate, got %q", unversioned.Header().Get("Cache-Control"))
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
	saveOne(t, store, "build@mail.test", "sender@example.org", []byte("Subject: hello\r\n\r\nbody"))
	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/inboxes/build@mail.test?limit=25&offset=0", nil))

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"hasMore":false`) {
		t.Fatalf("expected paged inbox response, got status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestMessageAPISeparatesHeadersAndBody(t *testing.T) {
	store := testStore(t, time.Hour)
	message := saveOne(t, store, "build@mail.test", "sender@example.org", []byte("From: sender@example.org\r\nSubject: hello\r\n\r\nbody"))

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

func TestMessageResponsesDoNotAllowCaching(t *testing.T) {
	store := testStore(t, time.Hour)
	message := saveOne(t, store, "build@mail.test", "sender@example.org", []byte("Content-Type: text/html\r\n\r\n<p>body</p>"))
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)
	for _, path := range []string{"/?inbox=build", "/api/v1/inboxes/build@mail.test", "/api/v1/messages/" + message.ID, "/ui/messages/" + message.ID, "/ui/messages/" + message.ID + "/html"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Header().Get("Cache-Control") != "no-store, private" || response.Header().Get("Pragma") != "no-cache" {
			t.Fatalf("expected no-store headers for %s, got %#v", path, response.Header())
		}
	}
}

func TestInboxUIPaginatesWithNewerAndOlderLinks(t *testing.T) {
	store := testStore(t, time.Hour)
	for i := 0; i < defaultPageSize+2; i++ {
		saveOne(t, store, "busy@mail.test", "ci@example.org", []byte("From: CI Bot <ci@example.org>\r\nSubject: build\r\n\r\nok"))
	}
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/?inbox=busy", nil))
	page := first.Body.String()
	if !strings.Contains(page, `href="/?inbox=busy&amp;offset=25"`) || strings.Contains(page, `rel="prev"`) || !strings.Contains(page, `aria-disabled="true">← Newer`) || !strings.Contains(page, "Page 1") || !strings.Contains(page, "Messages 1–25") {
		t.Fatalf("expected an Older link and a disabled Newer button on the first page, got %q", page)
	}
	if !strings.Contains(page, `<span class="row-from" title="ci@example.org">CI Bot</span>`) {
		t.Fatalf("expected sender display name in the message list, got %q", page)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/?inbox=busy&offset=25", nil))
	page = second.Body.String()
	if !strings.Contains(page, `href="/?inbox=busy"`) || !strings.Contains(page, "← Newer") || strings.Contains(page, `rel="next"`) || !strings.Contains(page, `aria-disabled="true">Older →`) || !strings.Contains(page, "Page 2") || !strings.Contains(page, "Messages 26–27") {
		t.Fatalf("expected a Newer link and a disabled Older button on the last page, got %q", page)
	}
}

func TestEmptyInboxShowsStartCard(t *testing.T) {
	store := testStore(t, time.Hour)
	response := httptest.NewRecorder()
	NewHTTPServer(Config{MailDomain: "mail.test"}, store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?inbox=quiet", nil))

	page := response.Body.String()
	for _, want := range []string{"Get a temporary email address", "No messages yet", `data-copy="quiet@mail.test"`, "New address", "Use with the API", "until curl -sf &#39;{origin}/api/v1/inboxes/{inbox}?limit=1&#39;"} {
		if !strings.Contains(page, want) {
			t.Fatalf("expected empty inbox page to contain %q, got %q", want, page)
		}
	}
}

func TestSenderFromHeaders(t *testing.T) {
	for _, tc := range []struct{ headers, envelope, name, address string }{
		{"From: \"Jamie from QA\" <jamie@qa.example>\r\nSubject: hi", "bounce@qa.example", "Jamie from QA", "jamie@qa.example"},
		{"From: =?utf-8?q?Ren=C3=A9e?= <renee@example.org>", "x@example.org", "Renée", "renee@example.org"},
		{"From: plain@example.org", "x@example.org", "", "plain@example.org"},
		{"Subject: no from header", "envelope@example.org", "", "envelope@example.org"},
	} {
		name, address := senderFromHeaders(tc.headers, tc.envelope)
		if name != tc.name || address != tc.address {
			t.Errorf("senderFromHeaders(%q) = %q, %q; want %q, %q", tc.headers, name, address, tc.name, tc.address)
		}
	}
}

func TestBlockedImagesAreCountedAndMarked(t *testing.T) {
	for _, tc := range []struct {
		html  string
		want  blockedImages
		label string
	}{
		{`<p>no images</p>`, blockedImages{}, ""},
		{`<img alt="logo" width="120"><img alt="hero">`, blockedImages{Total: 2}, "2 images blocked for privacy"},
		{`<img width="1" height="1">`, blockedImages{Total: 1, TrackingPixels: 1}, "Tracking pixel blocked"},
		{`<img alt="logo"><img width="1px" height="1px">`, blockedImages{Total: 2, TrackingPixels: 1}, "2 images blocked, incl. 1 tracking pixel"},
	} {
		got := countBlockedImages(sanitizeHTML(tc.html))
		if got != tc.want || got.Label() != tc.label {
			t.Errorf("countBlockedImages(%q) = %+v %q; want %+v %q", tc.html, got, got.Label(), tc.want, tc.label)
		}
	}

	store := testStore(t, time.Hour)
	message := saveOne(t, store, "build@mail.test", "sender@example.org", []byte("Content-Type: text/html\r\n\r\n<p>Hi</p><img src=\"https://tracker.example/p.gif\" width=\"1\" height=\"1\">"))
	handler := NewHTTPServer(Config{MailDomain: "mail.test"}, store)

	frame := httptest.NewRecorder()
	handler.ServeHTTP(frame, httptest.NewRequest(http.MethodGet, "/ui/messages/"+message.ID+"/html", nil))
	if !strings.Contains(frame.Body.String(), blockedImageCSS) || strings.Contains(frame.Body.String(), "tracker.example") {
		t.Fatalf("expected blocked image placeholders without remote sources, got %q", frame.Body.String())
	}
	details := httptest.NewRecorder()
	handler.ServeHTTP(details, httptest.NewRequest(http.MethodGet, "/ui/messages/"+message.ID, nil))
	if !strings.Contains(details.Body.String(), `"blockedImages":1,"trackingPixels":1`) {
		t.Fatalf("expected blocked image counts, got %q", details.Body.String())
	}
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/?inbox=build", nil))
	if !strings.Contains(page.Body.String(), "Tracking pixel blocked") {
		t.Fatalf("expected blocked image note in the reader, got %q", page.Body.String())
	}
}
