package app

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/example/tmpmail/internal/api"
)

func NewHTTPServer(cfg Config, store *Store) http.Handler {
	mux := http.NewServeMux()
	api.HandlerFromMux(&apiServer{store: store}, mux)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		renderInbox(w, store, cfg.MailDomain, strings.TrimSpace(r.URL.Query().Get("inbox")))
	})
	return requestLogger(securityHeaders(mux))
}

type apiServer struct{ store *Store }

var _ api.ServerInterface = (*apiServer)(nil)

func (s *apiServer) Healthcheck(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) ListInboxMessages(w http.ResponseWriter, _ *http.Request, inbox api.Inbox) {
	messages, err := s.store.List(string(inbox))
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	result := make([]api.MessageSummary, 0, len(messages))
	for _, message := range messages {
		result = append(result, toAPISummary(message))
	}
	writeJSON(w, result)
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
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'")
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
		log.Printf("web request method=%s path=%s status=%d remote=%s duration=%s", r.Method, r.URL.Path, status, r.RemoteAddr, time.Since(started).Round(time.Millisecond))
	})
}

var inboxTemplate = template.Must(template.New("inbox").Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>tmpmail</title><style>body{font:16px system-ui,sans-serif;max-width:900px;margin:3rem auto;padding:0 1rem;color:#17212b}input{padding:.6rem;width:min(28rem,80%)}button{padding:.6rem}article{border-top:1px solid #ddd;padding:1rem 0}small{color:#667}pre{white-space:pre-wrap;overflow-wrap:anywhere;background:#f4f5f6;padding:1rem}a{color:#0755a0}</style></head><body><h1>tmpmail</h1><form><input name="inbox" value="{{.InboxName}}" placeholder="build-482" autofocus><span>@{{.Domain}}</span><button>Open inbox</button></form>{{if .Error}}<p>{{.Error}}</p>{{end}}{{if .Address}}<p><small>Inbox: {{.Address}} · Messages expire after one hour. API: <a href="/api/inboxes/{{.Address}}">inbox JSON</a></small></p>{{if .Messages}}{{range .Messages}}<article><strong>{{.Subject}}</strong><br><small>From {{.From}} · {{.Received.Format "2006-01-02 15:04:05 UTC"}} · expires {{.ExpiresAt.Format "15:04:05 UTC"}}</small><details><summary>View original message</summary><pre>{{.Body}}</pre></details></article>{{end}}{{else}}<p>No active messages.</p>{{end}}{{end}}</body></html>`))

func renderInbox(w http.ResponseWriter, store *Store, domain, inboxName string) {
	data := struct {
		InboxName string
		Domain    string
		Address   string
		Error     string
		Messages  []Message
	}{InboxName: inboxName, Domain: domain}
	if inboxName != "" {
		if strings.ContainsAny(inboxName, "@/\\") {
			data.Error = "Enter only the inbox name, without the domain."
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = inboxTemplate.Execute(w, data)
			return
		}
		data.Address = inboxName + "@" + domain
		messages, err := store.List(data.Address)
		if err != nil {
			http.Error(w, "storage error", 500)
			return
		}
		for i := range messages {
			full, err := store.Get(messages[i].ID)
			if err == nil {
				messages[i].Body = full.Body
			}
		}
		data.Messages = messages
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = inboxTemplate.Execute(w, data)
}
