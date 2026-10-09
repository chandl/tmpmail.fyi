package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"html/template"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const adminQueryTimeout = 3 * time.Second
const adminPageSize = 50
const adminMaxOffset = 10000

var adminStarted = time.Now()
var adminCSS = mustReadUIAsset("admin.css")
var adminScript = mustReadUIAsset("admin.js")
var adminVersion = assetVersion(uiCSS, uiScript, adminCSS, adminScript)
var adminTemplate = template.Must(template.New("admin").ParseFS(uiAssets, "assets/ui/layout.html", "assets/ui/admin.html"))

type adminServer struct {
	cfg       Config
	store     *Store
	smtp      *SMTPServer
	analytics *Analytics
}
type adminRanking struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}
type adminBucket struct {
	Timestamp  time.Time `json:"timestamp"`
	Deliveries int64     `json:"deliveries"`
	HTTP       int64     `json:"http"`
	Polling    int64     `json:"polling"`
}
type adminOverview struct {
	GeneratedAt   time.Time      `json:"generatedAt"`
	Since         time.Time      `json:"since"`
	Deliveries    int64          `json:"deliveries"`
	Recipients    int64          `json:"recipients"`
	Requests      int64          `json:"requests"`
	Polling       int64          `json:"polling"`
	Callers       int64          `json:"callers"`
	Earliest      *time.Time     `json:"earliest"`
	Buckets       []adminBucket  `json:"buckets"`
	Inboxes       []adminRanking `json:"inboxes"`
	SenderDomains []adminRanking `json:"senderDomains"`
	HTTPCallers   []adminRanking `json:"httpCallers"`
}

// NewAdminServer serves metadata on the separately configured private listener.
// It deliberately has no product request instrumentation or application authentication.
func NewAdminServer(cfg Config, store *Store, smtp *SMTPServer, analytics *Analytics) http.Handler {
	s := &adminServer{cfg, store, smtp, analytics}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openapi.json", serveAPISpec)
	mux.HandleFunc("GET /ui.js", adminAsset("text/javascript; charset=utf-8", uiScript))
	mux.HandleFunc("GET /admin.js", adminAsset("text/javascript; charset=utf-8", adminScript))
	mux.HandleFunc("GET /admin.css", adminAsset("text/css; charset=utf-8", adminCSS))
	for _, name := range []string{"favicon.ico", "favicon-16x16.png", "favicon-32x32.png", "apple-touch-icon.png", "android-chrome-192x192.png", "android-chrome-512x512.png", "site.webmanifest"} {
		mux.HandleFunc("GET /"+name, serveFaviconAsset(name))
	}
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/overview", s.overview)
	mux.HandleFunc("GET /api/activity", s.activity)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/activity" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		chrome := newPageChrome()
		chrome.AssetVersion = adminVersion
		_ = adminTemplate.ExecuteTemplate(w, "admin", chrome)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
		mux.ServeHTTP(w, r)
	})
}
func adminAsset(contentType, content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		if r.URL.Query().Get("v") == adminVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		_, _ = w.Write([]byte(content))
	}
}
func adminJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
func adminError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
func (s *adminServer) database(w http.ResponseWriter) *sql.DB {
	var db *sql.DB
	if s.analytics != nil {
		db = s.analytics.DB()
	}
	if db == nil {
		adminError(w, 503, "Analytics unavailable. Enable analytics to collect activity.")
	}
	return db
}
func adminWindow(r *http.Request) (time.Duration, bool) {
	switch r.URL.Query().Get("window") {
	case "", "24h":
		return 24 * time.Hour, true
	case "1h":
		return time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "30d":
		return 30 * 24 * time.Hour, true
	}
	return 0, false
}
func (s *adminServer) status(w http.ResponseWriter, r *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	result := map[string]any{"generatedAt": time.Now(), "uptimeSeconds": time.Since(adminStarted).Seconds(), "memoryBytes": mem.Alloc, "goroutines": runtime.NumGoroutine(), "retentionHours": s.cfg.AnalyticsEventTTL.Hours(), "storageBudgetBytes": s.cfg.AnalyticsMaxStorageBytes, "messageStorageBudgetBytes": s.cfg.MaxStorageBytes}
	if s.store != nil {
		result["store"] = s.store.Status()
	}
	if s.smtp != nil {
		result["smtp"] = s.smtp.RuntimeStatus()
	}
	if s.analytics != nil {
		result["analytics"] = s.analytics.Snapshot()
	}
	adminJSON(w, result)
}
func (s *adminServer) overview(w http.ResponseWriter, r *http.Request) {
	window, ok := adminWindow(r)
	if !ok {
		adminError(w, 400, "Invalid time window")
		return
	}
	db := s.database(w)
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminQueryTimeout)
	defer cancel()
	now := time.Now()
	since := now.Add(-window)
	lo, hi := since.UnixMilli(), now.UnixMilli()
	out := adminOverview{GeneratedAt: now, Since: since, Buckets: []adminBucket{}}
	err := db.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT recipient) FROM deliveries WHERE timestamp>=? AND timestamp<=?`, lo, hi).Scan(&out.Deliveries, &out.Recipients)
	if err == nil {
		err = db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(polling),0),COUNT(DISTINCT ip) FROM http_requests WHERE timestamp>=? AND timestamp<=?`, lo, hi).Scan(&out.Requests, &out.Polling, &out.Callers)
	}
	var earliest sql.NullInt64
	if err == nil {
		err = db.QueryRowContext(ctx, `SELECT MIN(t) FROM (SELECT MIN(timestamp) t FROM deliveries UNION ALL SELECT MIN(timestamp) t FROM http_requests)`).Scan(&earliest)
	}
	if earliest.Valid {
		t := time.UnixMilli(earliest.Int64)
		out.Earliest = &t
	}
	for _, q := range []struct {
		table, column string
		dst           *[]adminRanking
	}{{"deliveries", "recipient", &out.Inboxes}, {"deliveries", "sender_domain", &out.SenderDomains}, {"http_requests", "ip", &out.HTTPCallers}} {
		if err != nil {
			break
		}
		*q.dst, err = adminRank(ctx, db, q.table, q.column, lo, hi)
	}
	// All buckets share the same exact window, including empty intervals and the final partial interval.
	count := 24
	switch window {
	case time.Hour:
		count = 60
	case 7 * 24 * time.Hour:
		count = 21
	case 30 * 24 * time.Hour:
		count = 30
	}
	width := (hi - lo + int64(count) - 1) / int64(count)
	for i := 0; i < count; i++ {
		out.Buckets = append(out.Buckets, adminBucket{Timestamp: time.UnixMilli(lo + int64(i)*width)})
	}
	if err == nil {
		var rows *sql.Rows
		rows, err = db.QueryContext(ctx, `SELECT (timestamp-?)/? bucket,COUNT(*),0,0 FROM deliveries WHERE timestamp>=? AND timestamp<=? GROUP BY bucket UNION ALL SELECT (timestamp-?)/? bucket,0,COUNT(*),COALESCE(SUM(polling),0) FROM http_requests WHERE timestamp>=? AND timestamp<=? GROUP BY bucket`, lo, width, lo, hi, lo, width, lo, hi)
		if err == nil {
			for rows.Next() {
				var index int
				var d, h, p int64
				if err = rows.Scan(&index, &d, &h, &p); err != nil {
					break
				}
				if index >= count {
					index = count - 1
				}
				if index >= 0 {
					out.Buckets[index].Deliveries += d
					out.Buckets[index].HTTP += h
					out.Buckets[index].Polling += p
				}
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
		}
	}
	if err != nil {
		adminError(w, 503, "Analytics query unavailable; retry shortly.")
		return
	}
	adminJSON(w, out)
}
func adminRank(ctx context.Context, db *sql.DB, table, column string, lo, hi int64) ([]adminRanking, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+column+`,COUNT(*) n FROM `+table+` WHERE timestamp>=? AND timestamp<=? GROUP BY `+column+` ORDER BY n DESC,`+column+` ASC LIMIT 10`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []adminRanking{}
	for rows.Next() {
		var rank adminRanking
		if err = rows.Scan(&rank.Value, &rank.Count); err != nil {
			return nil, err
		}
		result = append(result, rank)
	}
	return result, rows.Err()
}

const adminDeliverySelect = `SELECT id,'delivery' kind,timestamp,recipient,sender,sender_domain,ip,size,'' user_agent,'' route,'' method,0 status,0 duration_ms,'' message_id,0 polling FROM deliveries`
const adminHTTPSelect = `SELECT id,'http' kind,timestamp,recipient,'' sender,'' sender_domain,ip,0 size,user_agent,route,method,status,duration_ms,message_id,polling FROM http_requests`

func (s *adminServer) activity(w http.ResponseWriter, r *http.Request) {
	window, ok := adminWindow(r)
	if !ok {
		adminError(w, 400, "Invalid time window")
		return
	}
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind != "" && kind != "delivery" && kind != "http" && kind != "httpPoll" && kind != "httpOther" {
		adminError(w, 400, "Invalid activity type")
		return
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		var err error
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 || offset > adminMaxOffset {
			adminError(w, 400, "Offset must be between 0 and 10000")
			return
		}
	}
	search := q.Get("search")
	if len(search) > 256 {
		adminError(w, 400, "Search is too long")
		return
	}
	union := adminDeliverySelect + ` UNION ALL ` + adminHTTPSelect
	if kind == "delivery" {
		union = adminDeliverySelect
	}
	if kind == "http" || kind == "httpPoll" || kind == "httpOther" {
		union = adminHTTPSelect
	}
	now := time.Now()
	where := ` WHERE timestamp>=? AND timestamp<=?`
	args := []any{now.Add(-window).UnixMilli(), now.UnixMilli()}
	if kind == "httpPoll" {
		where += ` AND polling=1`
	} else if kind == "httpOther" {
		where += ` AND polling=0`
	}
	for _, f := range []struct{ param, column string }{{"recipient", "recipient"}, {"senderDomain", "sender_domain"}, {"callerIP", "ip"}, {"sourceIP", "ip"}} {
		v := q.Get(f.param)
		if len(v) > 320 {
			adminError(w, 400, "Filter is too long")
			return
		}
		if q.Has(f.param) {
			where += ` AND ` + f.column + `=?`
			args = append(args, v)
			if f.param == "callerIP" {
				where += ` AND kind='http'`
			}
			if f.param == "senderDomain" {
				where += ` AND kind='delivery'`
			}
		}
	}
	for _, f := range []struct{ param, column string }{{"sender", "sender"}, {"userAgent", "user_agent"}} {
		v := q.Get(f.param)
		if len(v) > 320 {
			adminError(w, 400, "Filter is too long")
			return
		}
		if v != "" {
			where += ` AND ` + f.column + ` LIKE ? ESCAPE '\'`
			args = append(args, "%"+strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(v)+"%")
			if f.param == "sender" {
				where += ` AND kind='delivery'`
			} else {
				where += ` AND kind='http'`
			}
		}
	}
	if search != "" {
		where += ` AND (recipient LIKE ? ESCAPE '\' OR sender LIKE ? ESCAPE '\' OR ip LIKE ? ESCAPE '\' OR user_agent LIKE ? ESCAPE '\')`
		term := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search) + "%"
		for i := 0; i < 4; i++ {
			args = append(args, term)
		}
	}
	db := s.database(w)
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminQueryTimeout)
	defer cancel()
	args = append(args, adminPageSize+1, offset)
	rows, err := db.QueryContext(ctx, `SELECT * FROM (`+union+`)`+where+` ORDER BY timestamp DESC,id DESC,kind DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		adminError(w, 503, "Analytics query unavailable; retry shortly.")
		return
	}
	defer rows.Close()
	events := []AnalyticsEvent{}
	for rows.Next() {
		var e AnalyticsEvent
		var ms int64
		if err = rows.Scan(&e.ID, &e.Kind, &ms, &e.Recipient, &e.Sender, &e.SenderDomain, &e.IP, &e.Size, &e.UserAgent, &e.Route, &e.Method, &e.Status, &e.DurationMS, &e.MessageID, &e.Polling); err != nil {
			break
		}
		e.Timestamp = time.UnixMilli(ms)
		events = append(events, e)
	}
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		adminError(w, 503, "Analytics query unavailable; retry shortly.")
		return
	}
	more := len(events) > adminPageSize && offset+adminPageSize <= adminMaxOffset
	if len(events) > adminPageSize {
		events = events[:adminPageSize]
	}
	adminJSON(w, map[string]any{"generatedAt": now, "events": events, "offset": offset, "pageSize": adminPageSize, "hasMore": more, "offsetLimit": adminMaxOffset})
}
