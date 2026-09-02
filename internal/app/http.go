package app

import (
	"database/sql"
	"embed"
	"encoding/json"
	"html/template"
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
	mux.HandleFunc("GET /ui.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(uiCSS))
	})
	mux.HandleFunc("GET /ui.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write([]byte(uiScript))
	})
	mux.HandleFunc("GET /metrics", http.NotFound)
	mux.HandleFunc("GET /ui/messages/{id}/html", func(w http.ResponseWriter, r *http.Request) {
		message, err := store.Get(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		content := parseEmail(message.Body).HTML
		if content == "" {
			http.NotFound(w, r)
			return
		}
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
		_ = privacyTemplate.Execute(w, struct{ Year int }{Year: time.Now().Year()})
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
	return requestLogger(securityHeaders(mux), cfg.MetricsEnabled, cfg.HTTPLogHeaders)
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
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="color-scheme" content="light"><style>:root{color-scheme:light}html,body{margin:0;background:#fff;color:#1e293b;font:15px/1.5 ui-sans-serif,system-ui,sans-serif}</style></head><body>` + content + `</body></html>`
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

func (s *apiServer) ListInboxMessages(w http.ResponseWriter, _ *http.Request, inbox api.Inbox, params api.ListInboxMessagesParams) {
	limit := defaultPageSize
	if params.Limit != nil {
		limit = min(max(*params.Limit, 1), maxPageSize)
	}
	offset := 0
	if params.Offset != nil {
		offset = max(*params.Offset, 0)
	}
	messages, hasMore, err := s.store.ListPage(string(inbox), limit, offset)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	result := make([]api.MessageSummary, 0, len(messages))
	for _, message := range messages {
		result = append(result, toAPISummary(message))
	}
	writeJSON(w, api.InboxPage{HasMore: hasMore, Limit: limit, Messages: result, Offset: offset})
}

func (s *apiServer) GetMessage(w http.ResponseWriter, r *http.Request, id string) {
	message, err := s.store.Get(id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	headers, body := splitRawMessage(message.Body)
	writeJSON(w, api.Message{
		Body:      body,
		ExpiresAt: message.ExpiresAt,
		From:      message.From,
		Headers:   headers,
		Id:        message.ID,
		Received:  message.Received,
		Recipient: api.Inbox(message.Recipient),
		Size:      message.Size,
		Subject:   message.Subject,
	})
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

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(body)
}

func requestLogger(next http.Handler, metricsEnabled bool, loggedHeaders []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		elapsed := time.Since(started)
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
		if metricsEnabled {
			observeHTTP(metricRoute(r.URL.Path), strconv.Itoa(status), elapsed.Seconds())
		}
	})
}

var inboxTemplate = template.Must(template.New("inbox").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="icon" href="/favicon.ico" sizes="any"><link rel="icon" type="image/png" sizes="32x32" href="/favicon-32x32.png"><link rel="icon" type="image/png" sizes="16x16" href="/favicon-16x16.png"><link rel="apple-touch-icon" href="/apple-touch-icon.png"><link rel="manifest" href="/site.webmanifest"><link rel="stylesheet" href="/ui.css"><script defer src="/ui.js"></script><title>tmpmail{{if .Address}} - {{.Address}}{{end}}</title>
<style>
*{box-sizing:border-box}body{margin:0;background:#f1f5f9;color:#1e293b;font:15px/1.5 ui-sans-serif,system-ui,sans-serif}.shell{width:min(880px,calc(100% - 32px));margin:7vh auto}.top{display:flex;align-items:center;gap:10px;margin-bottom:30px}.brand{color:#0f172a;font-weight:750;font-size:26px;letter-spacing:-.04em;text-decoration:none}.brand span{color:#2563eb}.badge{color:#64748b;font-size:12px;border:1px solid #cbd5e1;border-radius:999px;padding:4px 9px}.panel{background:#fff;border:1px solid #cbd5e1;border-radius:16px;padding:22px;box-shadow:0 18px 50px #0f172a14}label{display:block;color:#64748b;font-size:12px;font-weight:700;letter-spacing:.06em;text-transform:uppercase;margin-bottom:8px}.lookup{display:flex;gap:8px;align-items:center}.lookup input{min-width:0;flex:1;background:#fff;border:1px solid #94a3b8;border-radius:9px;color:#1e293b;font:inherit;padding:11px 12px;outline:none}.lookup input:focus{border-color:#2563eb;box-shadow:0 0 0 3px #2563eb22}.domain{white-space:nowrap;color:#64748b}button{border:0;border-radius:9px;background:#2563eb;color:#fff;font:700 14px inherit;padding:11px 15px;cursor:pointer}button:hover{background:#1d4ed8}.copy{margin-left:auto;background:#e2e8f0;color:#1e293b;padding:6px 10px;font-size:12px}.copy:hover{background:#cbd5e1}.meta{display:flex;align-items:center;flex-wrap:wrap;gap:7px 14px;color:#64748b;font-size:13px;margin:18px 0 4px}.meta a{color:#2563eb;text-decoration:none}.meta a:hover{text-decoration:underline}.notice{color:#b91c1c;margin:16px 0 0}.message{border-top:1px solid #cbd5e1;padding:17px 0}.message:first-of-type{margin-top:10px}.subject{font-weight:700;color:#0f172a}.details{color:#64748b;font-size:13px;margin-top:3px}.section-label{margin-top:14px;color:#64748b;font-size:11px;font-weight:700;letter-spacing:.08em;text-transform:uppercase}details{margin-top:14px}summary{cursor:pointer;color:#2563eb;font-size:13px}pre{white-space:pre-wrap;overflow-wrap:anywhere;margin:7px 0 0;padding:13px;background:#f8fafc;border:1px solid #e2e8f0;border-radius:8px;color:#334155;font:12px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace}.empty{color:#64748b;padding:20px 0 4px}.footer{display:flex;align-items:center;justify-content:space-between;gap:20px;margin-top:24px;padding:16px 2px 0;border-top:1px solid #cbd5e1;color:#64748b;font-size:12px}.footer a{text-decoration:none}.footer-resources{display:flex;flex-direction:column;gap:4px}.footer-resources small{color:#64748b;font-size:10px;font-weight:700;letter-spacing:.08em;text-transform:uppercase}.footer-links{display:flex;gap:12px}.footer-links a{color:#2563eb}.footer-links a:hover{color:#1d4ed8;text-decoration:underline}.copyright{color:#64748b;white-space:nowrap}.copyright a{color:#2563eb}.copyright a:hover{color:#1d4ed8;text-decoration:underline}@media(max-width:560px){.shell{margin:28px auto}.top{margin-bottom:20px}.panel{padding:16px}.lookup{flex-wrap:wrap}.domain{width:100%}button{width:100%}.copy{margin-left:0;width:auto}.footer{align-items:flex-start;flex-direction:column;gap:12px}}
</style></head><body><main class="shell"><header class="top"><a class="brand" href="/">tmp<span>mail</span></a><span class="badge">disposable email</span></header><section class="panel"><form id="inbox-form"><label for="inbox">Open inbox</label><div class="lookup"><input id="inbox" name="inbox" value="{{.InboxName}}" placeholder="build-482" autocomplete="off" autofocus><span class="domain">@{{.Domain}}</span><button>Open</button></div></form>{{if .Error}}<p class="notice">{{.Error}}</p>{{end}}{{if .Address}}<div class="meta"><span>{{.Address}}</span><span>messages expire in 1 hour</span><a href="/api/v1/inboxes/{{.Address}}">Inbox JSON</a><button class="copy" type="button" id="copy-inbox" data-address="{{.Address}}">Copy address</button></div>{{if .Messages}}{{range .Messages}}<article class="message" data-message-id="{{.ID}}" data-has-html="{{.HasHTML}}"><div class="subject">{{.Subject}}</div><div class="details">From {{.From}} · <time class="local-time" datetime="{{.Received.Format "2006-01-02T15:04:05Z07:00"}}">{{.Received.Format "02 Jan, 15:04 UTC"}}</time> · expires <time class="local-time" datetime="{{.ExpiresAt.Format "2006-01-02T15:04:05Z07:00"}}">{{.ExpiresAt.Format "15:04 UTC"}}</time></div><div class="plain-body"><div class="section-label">Body</div><pre>{{.Body}}</pre></div><details><summary>Message headers</summary><pre>{{.Headers}}</pre></details></article>{{end}}{{else}}<p class="empty">No active messages in this inbox.</p>{{end}}{{end}}</section><footer class="footer"><div class="footer-resources"><small>Developer resources</small><nav class="footer-links" aria-label="Developer resources"><a href="/openapi.json">OpenAPI 3.1 specification</a><a href="https://github.com/chandl/tmpmail.fyi" target="_blank" rel="noopener noreferrer">GitHub</a><a href="/privacy">Privacy</a></nav></div><span class="copyright">Copyright © <a href="https://chandl.io/" target="_blank" rel="noopener noreferrer">chandl.io</a> {{.Year}}</span></footer></main><script>(()=>{const key="tmpmail:last-inbox",form=document.getElementById("inbox-form"),input=document.getElementById("inbox"),copy=document.getElementById("copy-inbox"),randomInbox=()=>{const adjectives=["amber","brisk","calm","daring","fuzzy","golden","lucky","mellow","nimble","solar","swift","velvet"],nouns=["badger","comet","falcon","fern","otter","panda","raven","river","tiger","willow","wren","zebra"],pick=words=>words[Math.floor(Math.random()*words.length)],crypto=globalThis.crypto;if(crypto?.getRandomValues){const bytes=new Uint32Array(2);crypto.getRandomValues(bytes);return pick(adjectives)+"-"+pick(nouns)+"-"+Array.from(bytes,x=>x.toString(36)).join("")}return pick(adjectives)+"-"+pick(nouns)+"-"+Math.random().toString(36).slice(2,12)};document.querySelectorAll(".local-time").forEach(element=>{const time=new Date(element.dateTime);if(!Number.isNaN(time.valueOf()))element.textContent=time.toLocaleString([], {month:"short",day:"2-digit",hour:"2-digit",minute:"2-digit"})});try{form.addEventListener("submit",()=>{const value=input.value.trim();if(value&&!/[\/@\\]/.test(value))localStorage.setItem(key,value)});if(!input.value){input.value=localStorage.getItem(key)||randomInbox();form.requestSubmit()}if(copy)copy.addEventListener("click",async()=>{const text=copy.dataset.address;try{if(navigator.clipboard)await navigator.clipboard.writeText(text);else{const area=document.createElement("textarea");area.value=text;document.body.append(area);area.select();document.execCommand("copy");area.remove()}copy.textContent="Copied";setTimeout(()=>copy.textContent="Copy address",1500)}catch(_){copy.textContent="Copy failed"}})}catch(_){}})()</script></body></html>`))

var privacyTemplate = template.Must(template.New("privacy").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Privacy · tmpmail</title><style>*{box-sizing:border-box}body{margin:0;background:#f1f5f9;color:#1e293b;font:15px/1.6 ui-sans-serif,system-ui,sans-serif}.shell{width:min(760px,calc(100% - 32px));margin:7vh auto}.top{display:flex;align-items:center;gap:10px}.brand{color:#0f172a;font-weight:750;font-size:26px;letter-spacing:-.04em;text-decoration:none}.brand span{color:#2563eb}.badge{color:#64748b;font-size:12px;border:1px solid #cbd5e1;border-radius:999px;padding:4px 9px}.panel{margin-top:28px;background:#fff;border:1px solid #cbd5e1;border-radius:16px;padding:28px;box-shadow:0 18px 50px #0f172a14}h1{margin:0 0 8px;font-size:28px;letter-spacing:-.03em}h2{margin:28px 0 8px;font-size:17px}p,li{color:#475569}ul{padding-left:20px}a{color:#2563eb}.back{display:inline-block;margin-top:24px;font-weight:700;text-decoration:none}@media(max-width:560px){.shell{margin:28px auto}.panel{padding:20px}}</style></head><body><main class="shell"><header class="top"><a class="brand" href="/">tmp<span>mail</span></a><span class="badge">disposable email</span></header><section class="panel"><h1>Privacy and message handling</h1><p>tmpmail is a disposable inbox service for development and testing. Do not use it for sensitive, personal, or production information.</p><h2>What is stored</h2><ul><li>Messages sent to an inbox, including their contents, headers, sender, recipient, subject, and receive time.</li><li>Message metadata in a local SQLite database and the original message file on the server’s local storage.</li><li>The inbox name you last opened in your browser’s local storage; it stays in your browser and is not sent as analytics data.</li></ul><h2>How messages are used</h2><ul><li>Messages are displayed to anyone who knows an inbox address. Inbox addresses are not protected by accounts or passwords.</li><li>tmpmail does not send outbound email and does not load remote images when rendering HTML messages.</li><li>The service logs operational events such as successful receives and HTTP requests, without logging message bodies.</li></ul><h2>Retention</h2><p>Messages are automatically deleted after one hour by default. The operator can configure a different retention period or storage limits, which may remove messages sooner.</p><h2>Third parties</h2><p>tmpmail does not include advertising or analytics trackers. Deployments may still be subject to the hosting provider, reverse proxy, and network operator’s own logging and privacy practices.</p><a class="back" href="/">← Back to inbox</a></section></main></body></html>`))

func renderInbox(w http.ResponseWriter, store *Store, domain, inboxName string, offset int) {
	data := struct {
		InboxName string
		Domain    string
		Address   string
		Error     string
		Messages  []inboxMessage
		Year      int
	}{InboxName: inboxName, Domain: domain, Year: time.Now().Year()}
	if inboxName != "" {
		if strings.ContainsAny(inboxName, "@/\\") {
			data.Error = "Enter only the inbox name, without the domain."
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = inboxTemplate.Execute(w, data)
			return
		}
		data.Address = inboxName + "@" + domain
		messages, _, err := store.ListPage(data.Address, defaultPageSize, offset)
		if err != nil {
			http.Error(w, "storage error", 500)
			return
		}
		for i := range messages {
			full, err := store.Get(messages[i].ID)
			if err == nil {
				parsed := parseEmail(full.Body)
				data.Messages = append(data.Messages, inboxMessage{Message: messages[i], Headers: parsed.Headers, Body: parsed.Text, HasHTML: parsed.HTML != ""})
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = inboxTemplate.Execute(w, data)
}

func pageOffset(value string) int {
	offset, err := strconv.Atoi(value)
	if err != nil || offset < 0 {
		return 0
	}
	return offset
}

type inboxMessage struct {
	Message
	Headers string
	Body    string
	HasHTML bool
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
