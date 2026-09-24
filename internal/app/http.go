package app

import (
	"database/sql"
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chandl/tmpmail.fyi/internal/api"
)

//go:embed assets/favicon/*
var faviconAssets embed.FS

func NewHTTPServer(cfg Config, store *Store) http.Handler {
	mux := http.NewServeMux()
	for _, name := range []string{"favicon.ico", "favicon-16x16.png", "favicon-32x32.png", "apple-touch-icon.png", "android-chrome-192x192.png", "android-chrome-512x512.png", "site.webmanifest"} {
		mux.HandleFunc("GET /"+name, serveFaviconAsset(name))
	}
	mux.HandleFunc("GET /ui.css", serveUIAsset("text/css; charset=utf-8", uiCSS))
	mux.HandleFunc("GET /ui.js", serveUIAsset("application/javascript; charset=utf-8", uiScript))
	mux.HandleFunc("GET /metrics", http.NotFound)
	mux.HandleFunc("GET /ui/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		message, err := store.GetContext(r.Context(), r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		parsed := parseEmail(message.Body)
		images := countBlockedImages(parsed.HTML)
		setSensitiveResponseHeaders(w)
		writeJSON(w, struct {
			Headers        string       `json:"headers"`
			Text           string       `json:"text"`
			HasHTML        bool         `json:"hasHtml"`
			Attachments    []Attachment `json:"attachments"`
			BlockedImages  int          `json:"blockedImages"`
			TrackingPixels int          `json:"trackingPixels"`
		}{Headers: parsed.Headers, Text: parsed.Text, HasHTML: parsed.HTML != "", Attachments: parsed.Attachments, BlockedImages: images.Total, TrackingPixels: images.TrackingPixels})
	})
	mux.HandleFunc("GET /ui/messages/{id}/html", func(w http.ResponseWriter, r *http.Request) {
		message, err := store.GetContext(r.Context(), r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		content := parseEmail(message.Body).HTML
		if content == "" {
			http.NotFound(w, r)
			return
		}
		setSensitiveResponseHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
		_, _ = w.Write([]byte(renderHTMLMessage(content)))
	})
	api.HandlerFromMux(&apiServer{store: store}, mux)
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		specification, err := api.GetSpecJSON()
		if err != nil {
			http.Error(w, "API specification unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oai.openapi+json;version=3.1")
		_, _ = w.Write(specification)
	})
	mux.HandleFunc("GET /privacy", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = privacyTemplate.Execute(w, newPageChrome())
	})
	// A single path segment is an inbox shortcut (for example, /build-482).
	// More-specific registered routes above take precedence over this pattern.
	mux.HandleFunc("GET /{inbox}", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		query.Set("inbox", r.PathValue("inbox"))
		location := &url.URL{Path: "/", RawQuery: query.Encode()}
		http.Redirect(w, r, location.String(), http.StatusFound)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		renderInbox(w, store, cfg.MailDomain, strings.TrimSpace(r.URL.Query().Get("inbox")), pageOffset(r.URL.Query().Get("offset")))
	})
	return requestLogger(
		rateLimitPerIP(limitHTTPRequests(securityHeaders(mux), cfg.MaxHTTPRequests, cfg.MetricsEnabled), cfg.HTTPRateLimitRPS, cfg.HTTPRateLimitBurst, cfg.HTTPRateLimitIPHeader),
		cfg.MetricsEnabled,
		cfg.HTTPAccessLogMode,
		cfg.HTTPLogHeaders,
	)
}

func serveFaviconAsset(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		asset, err := faviconAssets.ReadFile("assets/favicon/" + name)
		if err != nil {
			http.Error(w, "favicon unavailable", http.StatusInternalServerError)
			return
		}
		switch name {
		case "favicon.ico":
			w.Header().Set("Content-Type", "image/x-icon")
		case "site.webmanifest":
			w.Header().Set("Content-Type", "application/manifest+json")
		default:
			w.Header().Set("Content-Type", "image/png")
		}
		_, _ = w.Write(asset)
	}
}

func renderHTMLMessage(content string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="color-scheme" content="light"><style>:root{color-scheme:light}html,body{margin:0;background:#fff;color:#1e293b;font:15px/1.5 ui-sans-serif,system-ui,sans-serif}` + blockedImageCSS + `</style></head><body>` + content + `</body></html>`
}

// NewMetricsServer serves only the Prometheus scrape endpoint.
func NewMetricsServer() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metricsHandler())
	return mux
}

type apiServer struct{ store *Store }

var _ api.ServerInterface = (*apiServer)(nil)

func (s *apiServer) Healthcheck(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

const (
	defaultPageSize = 25
	maxPageSize     = 100
)

func (s *apiServer) ListInboxMessages(w http.ResponseWriter, r *http.Request, inbox api.Inbox, params api.ListInboxMessagesParams) {
	limit := defaultPageSize
	if params.Limit != nil {
		limit = min(max(*params.Limit, 1), maxPageSize)
	}
	offset := 0
	if params.Offset != nil {
		offset = max(*params.Offset, 0)
	}
	messages, hasMore, err := s.store.ListPageContext(r.Context(), string(inbox), limit, offset)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	result := make([]api.MessageSummary, 0, len(messages))
	for _, message := range messages {
		result = append(result, toAPISummary(message))
	}
	setSensitiveResponseHeaders(w)
	writeJSON(w, api.InboxPage{HasMore: hasMore, Limit: limit, Messages: result, Offset: offset})
}

func (s *apiServer) GetMessage(w http.ResponseWriter, r *http.Request, id string) {
	message, err := s.store.GetContext(r.Context(), id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	headers, body := splitRawMessage(message.Body)
	parsed := parseEmail(message.Body)
	attachments := make([]api.Attachment, 0, len(parsed.Attachments))
	for _, a := range parsed.Attachments {
		attachments = append(attachments, api.Attachment{Index: a.Index, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size})
	}
	setSensitiveResponseHeaders(w)
	writeJSON(w, api.Message{
		Attachments: attachments,
		Body:        body,
		ExpiresAt:   message.ExpiresAt,
		From:        message.From,
		Headers:     headers,
		Id:          message.ID,
		Received:    message.Received,
		Recipient:   api.Inbox(message.Recipient),
		Size:        message.Size,
		Subject:     message.Subject,
	})
}

func (s *apiServer) GetMessageAttachment(w http.ResponseWriter, r *http.Request, id string, index int) {
	message, err := s.store.GetContext(r.Context(), id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	attachment, content, ok := AttachmentContent(message.Body, index)
	if !ok {
		http.NotFound(w, r)
		return
	}
	setSensitiveResponseHeaders(w)
	w.Header().Set("Content-Type", attachment.ContentType)
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(attachment.Filename))
	_, _ = w.Write(content)
}

func toAPISummary(message Message) api.MessageSummary {
	return api.MessageSummary{
		ExpiresAt: message.ExpiresAt,
		From:      message.From,
		Id:        message.ID,
		Received:  message.Received,
		Recipient: api.Inbox(message.Recipient),
		Size:      message.Size,
		Subject:   message.Subject,
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(value)
}

func setSensitiveResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(body)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func requestLogger(next http.Handler, metricsEnabled bool, accessLogMode string, loggedHeaders []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		if metricsEnabled {
			httpRequestsInFlight.Inc()
			defer httpRequestsInFlight.Dec()
		}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		elapsed := time.Since(started)
		if r.Context().Err() != nil && metricsEnabled {
			httpCanceled.Inc()
		}
		if accessLogMode == "all" || (accessLogMode == "errors" && status >= http.StatusBadRequest) {
			fields := []string{
				"[web]",
				"method=" + r.Method,
				"path=" + r.URL.Path,
				"status=" + strconv.Itoa(status),
				"duration_ms=" + strconv.FormatInt(elapsed.Milliseconds(), 10),
			}
			for _, header := range loggedHeaders {
				field := strings.ToLower(strings.ReplaceAll(header, "-", "_"))
				fields = append(fields, field+"="+strconv.Quote(r.Header.Get(header)))
			}
			log.Print(strings.Join(fields, " "))
		}
		if metricsEnabled {
			observeHTTP(metricRoute(r.URL.Path, r.Pattern), strconv.Itoa(status), recorder.bytes, elapsed.Seconds())
		}
	})
}

func limitHTTPRequests(next http.Handler, limit int, metricsEnabled bool) http.Handler {
	if limit == 0 {
		return next
	}
	requests := make(chan struct{}, limit)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- struct{}{}:
			defer func() { <-requests }()
			next.ServeHTTP(w, r)
		default:
			if metricsEnabled {
				httpOverloadRejections.Inc()
			}
			w.Header().Set("Retry-After", "1")
			http.Error(w, "service temporarily overloaded", http.StatusServiceUnavailable)
		}
	})
}

func renderInbox(w http.ResponseWriter, store *Store, domain, inboxName string, offset int) {
	data := struct {
		pageChrome
		InboxName   string
		Domain      string
		Address     string
		Error       string
		Messages    []inboxMessage
		Offset      int
		HasMore     bool
		NewerOffset int
		Page        int
		OlderOffset int
		CountLabel  string
	}{pageChrome: newPageChrome(), InboxName: inboxName, Domain: domain}
	if inboxName != "" {
		if strings.ContainsAny(inboxName, "@/\\") {
			data.Error = "Enter only the inbox name, without the domain."
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = inboxTemplate.Execute(w, data)
			return
		}
		data.Address = inboxName + "@" + domain
		messages, hasMore, err := store.ListPage(data.Address, defaultPageSize, offset)
		if err != nil {
			http.Error(w, "storage error", 500)
			return
		}
		for _, message := range messages {
			full, err := store.Get(message.ID)
			if err != nil {
				continue
			}
			parsed := parseEmail(full.Body)
			fromName, fromAddress := senderFromHeaders(parsed.Headers, message.From)
			data.Messages = append(data.Messages, inboxMessage{
				Message:     message,
				Headers:     parsed.Headers,
				Body:        parsed.Text,
				HasHTML:     parsed.HTML != "",
				Images:      countBlockedImages(parsed.HTML),
				Attachments: parsed.Attachments,
				FromName:    fromName,
				FromAddress: fromAddress,
			})
		}
		data.Offset = offset
		data.HasMore = hasMore
		data.NewerOffset = max(0, offset-defaultPageSize)
		// Round up so an offset between pages (e.g. 10) still reads as a later page.
		data.Page = (offset+defaultPageSize-1)/defaultPageSize + 1
		data.OlderOffset = offset + defaultPageSize
		data.CountLabel = messageCountLabel(len(data.Messages), offset, hasMore)
	}
	setSensitiveResponseHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = inboxTemplate.Execute(w, data)
}

type inboxMessage struct {
	Message
	Headers     string
	Body        string
	HasHTML     bool
	Images      blockedImages
	Attachments []Attachment
	FromName    string
	FromAddress string
}

// FromLabel is the short sender shown in the message list.
func (m inboxMessage) FromLabel() string {
	if m.FromName != "" {
		return m.FromName
	}
	if m.FromAddress != "" {
		return m.FromAddress
	}
	return "Unknown sender"
}

func (m inboxMessage) SubjectLabel() string {
	if strings.TrimSpace(m.Subject) == "" {
		return "(no subject)"
	}
	return m.Subject
}

// messageCountLabel counts every message up to and including this page;
// "+" means older pages hold more.
func messageCountLabel(count, offset int, hasMore bool) string {
	total := offset + count
	switch {
	case hasMore:
		return strconv.Itoa(total) + "+ messages"
	case total == 1:
		return "1 message"
	default:
		return strconv.Itoa(total) + " messages"
	}
}

func pageOffset(value string) int {
	offset, err := strconv.Atoi(value)
	if err != nil || offset < 0 {
		return 0
	}
	return offset
}

func splitRawMessage(raw string) (headers, body string) {
	if parts := strings.SplitN(raw, "\r\n\r\n", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	if parts := strings.SplitN(raw, "\n\n", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	return raw, ""
}
