package app

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chandl/tmpmail.fyi/internal/api"
)

func NewHTTPServer(cfg Config, store *Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(uiCSS))
	})
	mux.HandleFunc("GET /ui.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write([]byte(uiScript))
	})
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
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'none'; style-src 'unsafe-inline'")
		_, _ = w.Write([]byte(content))
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
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		renderInbox(w, store, cfg.MailDomain, strings.TrimSpace(r.URL.Query().Get("inbox")), pageOffset(r.URL.Query().Get("offset")))
	})
	return requestLogger(securityHeaders(mux))
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
	writeJSON(w, api.Message{
		Body:      message.Body,
		ExpiresAt: message.ExpiresAt,
		From:      message.From,
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

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf("[web] method=%s path=%s status=%d remote=%s duration=%s", r.Method, r.URL.Path, status, r.RemoteAddr, time.Since(started).Round(time.Millisecond))
	})
}

var inboxTemplate = template.Must(template.New("inbox").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/ui.css"><script defer src="/ui.js"></script><title>tmpmail</title>
<style>
*{box-sizing:border-box}body{margin:0;background:#0b1020;color:#e8edf7;font:15px/1.5 ui-sans-serif,system-ui,sans-serif}.shell{width:min(880px,calc(100% - 32px));margin:7vh auto}.top{display:flex;align-items:center;justify-content:space-between;margin-bottom:30px}.brand{font-weight:750;font-size:22px;letter-spacing:-.04em}.brand b{color:#7c9cff}.badge{color:#9aa7bf;font-size:12px;border:1px solid #263554;border-radius:999px;padding:4px 9px}.panel{background:#121a2d;border:1px solid #263554;border-radius:16px;padding:22px;box-shadow:0 18px 50px #0003}label{display:block;color:#9aa7bf;font-size:12px;font-weight:700;letter-spacing:.06em;text-transform:uppercase;margin-bottom:8px}.lookup{display:flex;gap:8px;align-items:center}.lookup input{min-width:0;flex:1;background:#0b1020;border:1px solid #334365;border-radius:9px;color:#f4f7ff;font:inherit;padding:11px 12px;outline:none}.lookup input:focus{border-color:#7c9cff;box-shadow:0 0 0 3px #7c9cff22}.domain{white-space:nowrap;color:#9aa7bf}button{border:0;border-radius:9px;background:#7c9cff;color:#0b1020;font:700 14px inherit;padding:11px 15px;cursor:pointer}button:hover{background:#a4b8ff}.copy{margin-left:auto;background:#263554;color:#dce5f8;padding:6px 10px;font-size:12px}.copy:hover{background:#334365}.meta{display:flex;align-items:center;flex-wrap:wrap;gap:7px 14px;color:#9aa7bf;font-size:13px;margin:18px 0 4px}.meta a{color:#aebeff;text-decoration:none}.meta a:hover{text-decoration:underline}.notice{color:#ffb6b6;margin:16px 0 0}.message{border-top:1px solid #263554;padding:17px 0}.message:first-of-type{margin-top:10px}.subject{font-weight:700;color:#f7f9ff}.details{color:#9aa7bf;font-size:13px;margin-top:3px}.section-label{margin-top:14px;color:#9aa7bf;font-size:11px;font-weight:700;letter-spacing:.08em;text-transform:uppercase}details{margin-top:14px}summary{cursor:pointer;color:#aebeff;font-size:13px}pre{white-space:pre-wrap;overflow-wrap:anywhere;margin:7px 0 0;padding:13px;background:#0b1020;border:1px solid #202d48;border-radius:8px;color:#cbd6ec;font:12px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace}.empty{color:#9aa7bf;padding:20px 0 4px}.footer{display:flex;align-items:center;justify-content:space-between;gap:16px;margin-top:20px;padding:14px 2px 0;border-top:1px solid #202d48;color:#71809e;font-size:12px}.footer a{color:#aebeff;text-decoration:none}.footer a:hover{text-decoration:underline}.credit{display:flex;align-items:center;gap:8px}.credit:before{content:"";width:4px;height:4px;background:#526584;border-radius:50%}.copyright{color:#526584}@media(max-width:560px){.shell{margin:28px auto}.top{margin-bottom:20px}.panel{padding:16px}.lookup{flex-wrap:wrap}.domain{width:100%}button{width:100%}.copy{margin-left:0;width:auto}.footer{align-items:flex-start;flex-direction:column;gap:6px}}
</style></head><body><main class="shell"><header class="top"><div class="brand">tmp<span>mail</span></div><div class="badge">disposable inboxes</div></header><section class="panel"><form id="inbox-form"><label for="inbox">Open inbox</label><div class="lookup"><input id="inbox" name="inbox" value="{{.InboxName}}" placeholder="build-482" autocomplete="off" autofocus><span class="domain">@{{.Domain}}</span><button>Open</button></div></form>{{if .Error}}<p class="notice">{{.Error}}</p>{{end}}{{if .Address}}<div class="meta"><span>{{.Address}}</span><span>messages expire in 1 hour</span><a href="/api/inboxes/{{.Address}}">Inbox JSON</a><button class="copy" type="button" id="copy-inbox" data-address="{{.Address}}">Copy address</button></div>{{if .Messages}}{{range .Messages}}<article class="message"><div class="subject">{{.Subject}}</div><div class="details">From {{.From}} · <time class="local-time" datetime="{{.Received.Format "2006-01-02T15:04:05Z07:00"}}">{{.Received.Format "02 Jan, 15:04 UTC"}}</time> · expires <time class="local-time" datetime="{{.ExpiresAt.Format "2006-01-02T15:04:05Z07:00"}}">{{.ExpiresAt.Format "15:04 UTC"}}</time></div><div class="section-label">Body</div><pre>{{.Body}}</pre><details><summary>Message headers</summary><pre>{{.Headers}}</pre></details></article>{{end}}{{else}}<p class="empty">No active messages in this inbox.</p>{{end}}{{end}}</section><footer class="footer"><a href="/openapi.json">OpenAPI 3.1 specification</a><span class="credit">Created by <a href="https://chandl.io">chandl.io</a><span class="copyright">© 2026</span></span></footer></main><script>(()=>{const key="tmpmail:last-inbox",form=document.getElementById("inbox-form"),input=document.getElementById("inbox"),copy=document.getElementById("copy-inbox"),randomInbox=()=>{const adjectives=["amber","brisk","calm","daring","fuzzy","golden","lucky","mellow","nimble","solar","swift","velvet"],nouns=["badger","comet","falcon","fern","otter","panda","raven","river","tiger","willow","wren","zebra"],pick=words=>words[Math.floor(Math.random()*words.length)],crypto=globalThis.crypto;if(crypto?.getRandomValues){const bytes=new Uint32Array(2);crypto.getRandomValues(bytes);return pick(adjectives)+"-"+pick(nouns)+"-"+Array.from(bytes,x=>x.toString(36)).join("")}return pick(adjectives)+"-"+pick(nouns)+"-"+Math.random().toString(36).slice(2,12)};document.querySelectorAll(".local-time").forEach(element=>{const time=new Date(element.dateTime);if(!Number.isNaN(time.valueOf()))element.textContent=time.toLocaleString([], {month:"short",day:"2-digit",hour:"2-digit",minute:"2-digit"})});try{form.addEventListener("submit",()=>{const value=input.value.trim();if(value&&!/[\/@\\]/.test(value))localStorage.setItem(key,value)});if(!input.value){input.value=localStorage.getItem(key)||randomInbox();form.requestSubmit()}if(copy)copy.addEventListener("click",async()=>{const text=copy.dataset.address;try{if(navigator.clipboard)await navigator.clipboard.writeText(text);else{const area=document.createElement("textarea");area.value=text;document.body.append(area);area.select();document.execCommand("copy");area.remove()}copy.textContent="Copied";setTimeout(()=>copy.textContent="Copy address",1500)}catch(_){copy.textContent="Copy failed"}})}catch(_){}})()</script></body></html>`))

func renderInbox(w http.ResponseWriter, store *Store, domain, inboxName string, offset int) {
	data := struct {
		InboxName string
		Domain    string
		Address   string
		Error     string
		Messages  []inboxMessage
	}{InboxName: inboxName, Domain: domain}
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
				data.Messages = append(data.Messages, inboxMessage{Message: messages[i], Headers: parsed.Headers, Body: parsed.Text})
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
