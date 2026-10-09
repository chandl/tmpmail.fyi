package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAdmin(t *testing.T) (*Analytics, http.Handler) {
	t.Helper()
	cfg := Config{AnalyticsEnabled: true, AnalyticsEventTTL: 720 * time.Hour, AnalyticsMaxStorageBytes: 1 << 30, DataDir: t.TempDir()}
	a := OpenAnalytics(cfg)
	t.Cleanup(func() { a.Close() })
	if a.DB() == nil {
		t.Fatal("analytics database unavailable")
	}
	return a, NewAdminServer(cfg, nil, nil, a)
}
func seedAdminDelivery(t *testing.T, a *Analytics, id string, when time.Time, recipient, sender, domain, ip string) {
	t.Helper()
	_, err := a.DB().Exec(`INSERT INTO deliveries(id,timestamp,recipient,sender,sender_domain,ip,size) VALUES (?,?,?,?,?,?,?)`, id, when.UnixMilli(), recipient, sender, domain, ip, 123)
	if err != nil {
		t.Fatal(err)
	}
}
func seedAdminHTTP(t *testing.T, a *Analytics, id string, when time.Time, recipient, ip, ua string, poll bool) {
	t.Helper()
	_, err := a.DB().Exec(`INSERT INTO http_requests(id,timestamp,recipient,ip,user_agent,route,method,status,duration_ms,message_id,polling) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, id, when.UnixMilli(), recipient, ip, ua, "/v1/inboxes/{recipient}", "GET", 200, 1.25, "", poll)
	if err != nil {
		t.Fatal(err)
	}
}
func adminGet(t *testing.T, h http.Handler, path string, out any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	return w
}
func TestAdminOverviewCountsBucketsAndRanking(t *testing.T) {
	a, h := testAdmin(t)
	now := time.Now().Add(-time.Minute)
	// Two SMTP transactions, three accepted recipients: delivery count must be three.
	seedAdminDelivery(t, a, "d1", now, "a@mail.test", "s@example.org", "example.org", "192.0.2.1")
	seedAdminDelivery(t, a, "d2", now, "b@mail.test", "s@example.org", "example.org", "192.0.2.1")
	seedAdminDelivery(t, a, "d3", now, "a@mail.test", "other@example.net", "example.net", "192.0.2.2")
	seedAdminDelivery(t, a, "old", now.Add(-48*time.Hour), "old@mail.test", "old@example.org", "example.org", "192.0.2.1")
	seedAdminHTTP(t, a, "h1", now, "a@mail.test", "203.0.113.1", "ui", true)
	seedAdminHTTP(t, a, "h2", now, "a@mail.test", "203.0.113.1", "api", false)
	seedAdminHTTP(t, a, "h3", now, "b@mail.test", "203.0.113.2", "api", false)
	var out adminOverview
	adminGet(t, h, "/api/overview?window=24h", &out)
	if out.Deliveries != 3 || out.Recipients != 2 || out.Requests != 3 || out.Polling != 1 || out.Callers != 2 {
		t.Fatalf("incorrect counts: %+v", out)
	}
	if len(out.Inboxes) != 2 || out.Inboxes[0].Value != "a@mail.test" || out.Inboxes[0].Count != 2 {
		t.Fatalf("incorrect rankings: %+v", out.Inboxes)
	}
	if out.SenderDomains[0].Value != "example.org" || out.SenderDomains[0].Count != 2 {
		t.Fatalf("incorrect sender ranking: %+v", out.SenderDomains)
	}
	if out.Earliest == nil || out.Earliest.UnixMilli() != now.Add(-48*time.Hour).UnixMilli() {
		t.Fatalf("incorrect available history: %v", out.Earliest)
	}
	for _, check := range []struct {
		window     string
		deliveries int64
		buckets    int
		interval   time.Duration
	}{{"1h", 3, 60, time.Minute}, {"7d", 4, 21, 8 * time.Hour}, {"30d", 4, 30, 24 * time.Hour}} {
		var history adminOverview
		adminGet(t, h, "/api/overview?window="+check.window, &history)
		if history.Deliveries != check.deliveries {
			t.Fatalf("window %s: deliveries %d", check.window, history.Deliveries)
		}
		if len(history.Buckets) != check.buckets || history.Buckets[1].Timestamp.Sub(history.Buckets[0].Timestamp) != check.interval {
			t.Fatalf("window %s: incorrect bucket intervals: %+v", check.window, history.Buckets)
		}
		var bucketDeliveries int64
		for _, bucket := range history.Buckets {
			bucketDeliveries += bucket.Deliveries
		}
		if bucketDeliveries != history.Deliveries {
			t.Fatalf("window %s: bucket count %d, total %d", check.window, bucketDeliveries, history.Deliveries)
		}
	}
	var d, requests, p int64
	for _, b := range out.Buckets {
		d += b.Deliveries
		requests += b.HTTP
		p += b.Polling
	}
	if len(out.Buckets) != 24 || d != 3 || requests != 3 || p != 1 {
		t.Fatalf("incorrect buckets: %+v", out.Buckets)
	}
}
func TestAdminActivityFiltersPaginationAndLiteralSearch(t *testing.T) {
	a, h := testAdmin(t)
	now := time.Now().Add(-time.Minute)
	for i := 0; i < 55; i++ {
		seedAdminDelivery(t, a, fmt.Sprintf("d%03d", i), now.Add(time.Duration(i)*time.Millisecond), "a@mail.test", "s@example.org", "example.org", "203.0.113.1")
	}
	seedAdminDelivery(t, a, "different", now.Add(-time.Second), "b@mail.test", "s@other.org", "other.org", "203.0.113.2")
	seedAdminHTTP(t, a, "h1", now.Add(time.Second), "a@mail.test", "203.0.113.1", "literal 100%_agent", true)
	seedAdminHTTP(t, a, "h2", now.Add(2*time.Second), "b@mail.test", "203.0.113.2", "ordinary", false)
	type page struct {
		Events  []AnalyticsEvent `json:"events"`
		HasMore bool             `json:"hasMore"`
		Total   int64            `json:"total"`
	}
	var out page
	adminGet(t, h, "/api/activity?kind=delivery&recipient=a%40mail.test", &out)
	if out.Total != 55 || len(out.Events) != 50 || !out.HasMore || out.Events[0].ID != "d054" {
		t.Fatalf("incorrect first page: %+v", out)
	}
	adminGet(t, h, "/api/activity?kind=delivery&recipient=a%40mail.test&offset=50", &out)
	if out.Total != 55 || len(out.Events) != 5 || out.HasMore || out.Events[0].ID != "d004" {
		t.Fatalf("incorrect second page: %+v", out)
	}
	for _, path := range []string{"/api/activity?callerIP=203.0.113.1", "/api/activity?search=100%25_", "/api/activity?userAgent=100%25_&sourceIP=203.0.113.1", "/api/activity?kind=http&recipient=a%40mail.test"} {
		adminGet(t, h, path, &out)
		if out.Total != 1 || len(out.Events) != 1 || out.Events[0].ID != "h1" {
			t.Fatalf("incorrect filter %s: %+v", path, out)
		}
	}
	for _, check := range []struct{ kind, id string }{{"httpPoll", "h1"}, {"httpOther", "h2"}} {
		adminGet(t, h, "/api/activity?kind="+check.kind, &out)
		if out.Total != 1 || len(out.Events) != 1 || out.Events[0].ID != check.id {
			t.Fatalf("HTTP polling filter %s: %+v", check.kind, out)
		}
	}
	if _, err := a.DB().Exec(`UPDATE http_requests SET status=503,duration_ms=80 WHERE id='h2'`); err != nil {
		t.Fatal(err)
	}
	adminGet(t, h, "/api/activity?status=503", &out)
	if out.Total != 1 || out.Events[0].ID != "h2" {
		t.Fatalf("status filter: %+v", out)
	}
	adminGet(t, h, "/api/activity?kind=delivery&status=250", &out)
	if out.Total != 56 {
		t.Fatalf("SMTP accepted status filter: %+v", out)
	}
	for _, check := range []struct{ sort, id string }{{"slowest", "h2"}, {"fastest", "h1"}} {
		adminGet(t, h, "/api/activity?kind=http&sort="+check.sort, &out)
		if out.Total != 2 || out.Events[0].ID != check.id {
			t.Fatalf("HTTP duration sort: %+v", out)
		}
	}
	adminGet(t, h, "/api/activity?status=503&sort=fastest", &out)
	if out.Total != 1 || out.Events[0].ID != "h2" {
		t.Fatalf("combined status and sort: %+v", out)
	}
	adminGet(t, h, "/api/activity?senderDomain=other.org", &out)
	if out.Total != 1 || len(out.Events) != 1 || out.Events[0].ID != "different" {
		t.Fatalf("sender drilldown: %+v", out)
	}
	adminGet(t, h, "/api/activity?sender=other&sourceIP=203.0.113.2", &out)
	if out.Total != 1 || len(out.Events) != 1 || out.Events[0].ID != "different" {
		t.Fatalf("combined sender and IP filter: %+v", out)
	}
	adminGet(t, h, "/api/activity?sourceIP=203.0.113.2", &out)
	if out.Total != 2 || len(out.Events) != 2 {
		t.Fatalf("IP must match SMTP and HTTP: %+v", out)
	}
	adminGet(t, h, "/api/activity?sender=other&userAgent=ordinary", &out)
	if out.Total != 0 || len(out.Events) != 0 {
		t.Fatalf("field filters must combine with AND: %+v", out)
	}
	seedAdminDelivery(t, a, "empty", now, "empty@mail.test", "", "", "192.0.2.1")
	adminGet(t, h, "/api/activity?senderDomain=", &out)
	if out.Total != 1 || len(out.Events) != 1 || out.Events[0].ID != "empty" {
		t.Fatalf("empty sender domain drilldown: %+v", out)
	}
	for _, path := range []string{"/api/activity?sender=" + strings.Repeat("x", 321), "/api/activity?userAgent=" + strings.Repeat("x", 321), "/api/activity?sourceIP=" + strings.Repeat("x", 321), "/api/activity?sort=invalid", "/api/activity?status=abc", "/api/activity?status=99", "/api/activity?status=600", "/api/activity?offset=-1", "/api/activity?offset=10001", "/api/activity?kind=bad", "/api/overview?window=90d", "/api/activity?search=" + strings.Repeat("x", 257)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 400 {
			t.Fatalf("expected invalid filter rejection for %s: %d", path, w.Code)
		}
	}
}
func TestAdminUnavailableRuntimeAndEmbeddedPages(t *testing.T) {
	h := NewAdminServer(Config{}, nil, nil, nil)
	for _, path := range []string{"/", "/activity", "/admin.css", "/admin.js", "/ui.js", "/favicon.ico", "/api/status"} {
		w := adminGet(t, h, path, nil)
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("missing response hardening")
		}
	}
	for _, path := range []string{"/api/overview", "/api/activity"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 503 {
			t.Fatalf("unavailable analytics: %s %d", path, w.Code)
		}
	}
}

// BenchmarkAdminQueries measures direct event-table queries with retained metadata.
func BenchmarkAdminQueries(b *testing.B) {
	cfg := Config{AnalyticsEnabled: true, AnalyticsEventTTL: 720 * time.Hour, AnalyticsMaxStorageBytes: 1 << 30, DataDir: b.TempDir()}
	a := OpenAnalytics(cfg)
	defer a.Close()
	if a.DB() == nil {
		b.Fatal("analytics unavailable")
	}
	tx, err := a.DB().Begin()
	if err != nil {
		b.Fatal(err)
	}
	deliveries, err := tx.Prepare(`INSERT INTO deliveries(id,timestamp,recipient,sender,sender_domain,ip,size) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		b.Fatal(err)
	}
	requests, err := tx.Prepare(`INSERT INTO http_requests(id,timestamp,recipient,ip,user_agent,route,method,status,duration_ms,message_id,polling) VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	for i := 0; i < 10000; i++ {
		recipient := fmt.Sprintf("inbox%d@mail.test", i%100)
		ip := fmt.Sprintf("192.0.2.%d", i%100)
		timestamp := now.Add(-time.Duration(i) * time.Minute).UnixMilli()
		if _, err = deliveries.Exec(fmt.Sprint(i), timestamp, recipient, "sender@example.org", "example.org", ip, 1000); err != nil {
			b.Fatal(err)
		}
		if _, err = requests.Exec(fmt.Sprint(i), timestamp, recipient, ip, "benchmark", "/v1/inboxes/{recipient}", "GET", 200, 1.25, "", i%2 == 0); err != nil {
			b.Fatal(err)
		}
	}
	deliveries.Close()
	requests.Close()
	if err = tx.Commit(); err != nil {
		b.Fatal(err)
	}
	h := NewAdminServer(cfg, nil, nil, a)
	for _, path := range []string{"/api/overview?window=30d", "/api/activity?window=30d", "/api/activity?window=30d&search=inbox99&offset=50"} {
		b.Run(path, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				if w.Code != 200 {
					b.Fatalf("%d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestAdminEffectiveStartupSettings(t *testing.T) {
	cfg := Config{HTTPRateLimitRPS: 17.5, HTTPRateLimitBurst: 80, MaxHTTPRequests: 123, HTTPRateLimitIPHeader: "X-Real-IP", MaxSMTPConnectionsPerIP: 4, MaxSMTPRecipients: 7, MaxMessageBytes: 4096, MessageTTL: 2 * time.Hour, HTTPAccessLogMode: "errors", HTTPLogHeaders: []string{"User-Agent"}, MetricsEnabled: true, MetricsAddr: "127.0.0.1:9090", SMTPTLSKeyFile: "private-key-path"}
	var out struct {
		Settings struct {
			Rate       float64  `json:"rateLimitRPS"`
			Burst      int      `json:"rateLimitBurst"`
			Concurrent int      `json:"httpConcurrency"`
			IPHeader   string   `json:"clientIPHeader"`
			PerIP      int      `json:"smtpPerIP"`
			Recipients int      `json:"smtpRecipients"`
			MaxBytes   int64    `json:"maxMessageBytes"`
			TTL        float64  `json:"messageTTLSeconds"`
			Logs       string   `json:"accessLogMode"`
			Headers    []string `json:"logHeaders"`
			Metrics    bool     `json:"metricsEnabled"`
			Addr       string   `json:"metricsAddr"`
		} `json:"settings"`
	}
	w := adminGet(t, NewAdminServer(cfg, nil, nil, nil), "/api/status", &out)
	s := out.Settings
	if s.Rate != 17.5 || s.Burst != 80 || s.Concurrent != 123 || s.IPHeader != "X-Real-IP" || s.PerIP != 4 || s.Recipients != 7 || s.MaxBytes != 4096 || s.TTL != 7200 || s.Logs != "errors" || len(s.Headers) != 1 || !s.Metrics || s.Addr != "127.0.0.1:9090" {
		t.Fatalf("startup settings: %+v", s)
	}
	if strings.Contains(w.Body.String(), "private-key-path") {
		t.Fatal("private key configuration exposed")
	}
	adminGet(t, NewAdminServer(Config{}, nil, nil, nil), "/api/status", &out)
	if out.Settings.Rate != defaultRateLimitRPS || out.Settings.Burst != defaultRateLimitBurst || out.Settings.Recipients != defaultMaxSMTPRecipients || out.Settings.Concurrent != 0 || out.Settings.PerIP != 0 || out.Settings.Logs != "off" {
		t.Fatalf("runtime fallback settings: %+v", out.Settings)
	}
}
