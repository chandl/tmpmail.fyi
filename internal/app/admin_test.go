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
	}{{"1h", 3}, {"7d", 4}, {"30d", 4}} {
		var history adminOverview
		adminGet(t, h, "/api/overview?window="+check.window, &history)
		if history.Deliveries != check.deliveries {
			t.Fatalf("window %s: deliveries %d", check.window, history.Deliveries)
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
	}
	var out page
	adminGet(t, h, "/api/activity?kind=delivery&recipient=a%40mail.test", &out)
	if len(out.Events) != 50 || !out.HasMore || out.Events[0].ID != "d054" {
		t.Fatalf("incorrect first page: %+v", out)
	}
	adminGet(t, h, "/api/activity?kind=delivery&recipient=a%40mail.test&offset=50", &out)
	if len(out.Events) != 5 || out.HasMore || out.Events[0].ID != "d004" {
		t.Fatalf("incorrect second page: %+v", out)
	}
	for _, path := range []string{"/api/activity?callerIP=203.0.113.1", "/api/activity?search=100%25_", "/api/activity?kind=http&recipient=a%40mail.test"} {
		adminGet(t, h, path, &out)
		if len(out.Events) != 1 || out.Events[0].ID != "h1" {
			t.Fatalf("incorrect filter %s: %+v", path, out)
		}
	}
	adminGet(t, h, "/api/activity?senderDomain=other.org", &out)
	if len(out.Events) != 1 || out.Events[0].ID != "different" {
		t.Fatalf("sender drilldown: %+v", out)
	}
	seedAdminDelivery(t, a, "empty", now, "empty@mail.test", "", "", "192.0.2.1")
	adminGet(t, h, "/api/activity?senderDomain=", &out)
	if len(out.Events) != 1 || out.Events[0].ID != "empty" {
		t.Fatalf("empty sender domain drilldown: %+v", out)
	}
	for _, path := range []string{"/api/activity?offset=-1", "/api/activity?offset=10001", "/api/activity?kind=bad", "/api/overview?window=90d", "/api/activity?search=" + strings.Repeat("x", 257)} {
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
