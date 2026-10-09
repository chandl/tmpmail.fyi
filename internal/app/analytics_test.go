package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func analyticsTestConfig(t *testing.T) Config {
	t.Helper()
	return Config{DataDir: t.TempDir(), AnalyticsEnabled: true, AnalyticsEventTTL: 720 * time.Hour, AnalyticsMaxStorageBytes: 1 << 30}
}
func waitAnalytics(t *testing.T, a *Analytics, want int) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if db := a.DB(); db != nil {
			var count int
			if db.QueryRow("SELECT COUNT(*) FROM deliveries").Scan(&count) == nil && count == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("analytics did not reach %d deliveries: %+v", want, a.Snapshot())
}

func TestAnalyticsMetadataAndShutdownDrain(t *testing.T) {
	cfg := analyticsTestConfig(t)
	a := OpenAnalytics(cfg)
	for i := 0; i < 500; i++ {
		a.Record(AnalyticsEvent{Kind: "delivery", Recipient: "inbox@mail.test", Sender: "sender@example.test", SenderDomain: "example.test", IP: "192.0.2.1", Size: 100})
	}
	a.Close()
	db, err := sql.Open("sqlite3", filepath.Join(cfg.DataDir, "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM deliveries").Scan(&count); err != nil || count != 500 {
		t.Fatalf("drain count=%d err=%v", count, err)
	}
	for i := 0; i < reflect.TypeOf(AnalyticsEvent{}).NumField(); i++ {
		name := strings.ToLower(reflect.TypeOf(AnalyticsEvent{}).Field(i).Name)
		if strings.Contains(name, "body") || strings.Contains(name, "attachment") || strings.Contains(name, "header") {
			t.Fatalf("content field %s in analytics contract", name)
		}
	}
	var columns string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE name='deliveries'").Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(columns, "body") || strings.Contains(columns, "subject") {
		t.Fatalf("message content in analytics schema: %s", columns)
	}
}

func TestAnalyticsOverflowAndStartupRecovery(t *testing.T) {
	cfg := analyticsTestConfig(t)
	blocked := filepath.Join(cfg.DataDir, "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = blocked
	a := OpenAnalytics(cfg)
	defer a.Close()
	start := time.Now()
	for i := 0; i < analyticsQueueSize+analyticsBatchSize+100; i++ {
		a.Record(AnalyticsEvent{Kind: "delivery", Recipient: "inbox@mail.test"})
	}
	if time.Since(start) > time.Second {
		t.Fatal("recording waited for unavailable disk")
	}
	if a.Snapshot().DroppedEvents == 0 || a.Snapshot().Available {
		t.Fatalf("overflow/unavailability not surfaced: %+v", a.Snapshot())
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocked, 0750); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if a.pending.Load() == 0 && a.Snapshot().Available {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !a.Snapshot().Available || a.pending.Load() != 0 {
		t.Fatalf("startup recovery failed: %+v", a.Snapshot())
	}
}

func TestAnalyticsWriteFailureRecovery(t *testing.T) {
	a := OpenAnalytics(analyticsTestConfig(t))
	defer a.Close()
	// Closing only the analytics pool must be recoverable without touching mail.
	if err := a.DB().Close(); err != nil {
		t.Fatal(err)
	}
	a.Record(AnalyticsEvent{Kind: "delivery", Recipient: "recovered@mail.test"})
	waitAnalytics(t, a, 1)
	if a.Snapshot().WriteErrors == 0 {
		t.Fatal("write failure was not counted")
	}
}

func TestAnalyticsRetentionAndEarlyEviction(t *testing.T) {
	cfg := analyticsTestConfig(t)
	cfg.AnalyticsEventTTL = time.Hour
	a := OpenAnalytics(cfg)
	defer a.Close()
	a.Record(AnalyticsEvent{Kind: "delivery", Timestamp: time.Now().Add(-2 * time.Hour), Recipient: "old@mail.test"})
	a.Record(AnalyticsEvent{Kind: "delivery", Recipient: "current@mail.test"})
	waitAnalytics(t, a, 1)
	var recipient string
	if err := a.DB().QueryRow("SELECT recipient FROM deliveries").Scan(&recipient); err != nil || recipient != "current@mail.test" {
		t.Fatalf("retention: %s %v", recipient, err)
	}
	// Budget smaller than schema overhead demonstrates an honest best-effort
	// budget: every detailed event is evicted, but SQLite itself still uses space.
	cfg2 := analyticsTestConfig(t)
	cfg2.AnalyticsMaxStorageBytes = 1
	b := OpenAnalytics(cfg2)
	defer b.Close()
	b.Record(AnalyticsEvent{Kind: "delivery", Recipient: "evict@mail.test"})
	b.Record(AnalyticsEvent{Kind: "http", UserAgent: strings.Repeat("x", 2000), IP: "192.0.2.2"})
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if b.Snapshot().EarlyEvictedEvents == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b.Snapshot().EarlyEvictedEvents != 2 || b.Snapshot().EarliestEvent != nil {
		t.Fatalf("budget eviction: %+v", b.Snapshot())
	}
}

func TestAnalyticsOldestEvictionAcrossTables(t *testing.T) {
	a := OpenAnalytics(analyticsTestConfig(t))
	defer a.Close()
	db := a.DB()
	events := make([]AnalyticsEvent, 0, 400)
	for i := 0; i < 400; i++ {
		kind := "delivery"
		if i%2 == 1 {
			kind = "http"
		}
		events = append(events, AnalyticsEvent{ID: newID(), Kind: kind, Timestamp: time.Now().Add(time.Duration(i-400) * time.Minute), Recipient: strings.Repeat("a", 300), UserAgent: strings.Repeat("u", 500)})
	}
	if err := a.writeBatch(db, events); err != nil {
		t.Fatal(err)
	}
	db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	used, err := a.usage(db)
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.AnalyticsMaxStorageBytes = used - 4096
	if err := a.maintain(db); err != nil {
		t.Fatal(err)
	}
	if a.Snapshot().EarlyEvictedEvents == 0 {
		t.Fatal("budget failed to evict")
	}
	var first int64
	if err := db.QueryRow("SELECT MIN(timestamp) FROM (SELECT timestamp FROM deliveries UNION ALL SELECT timestamp FROM http_requests)").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first < events[256].Timestamp.UnixMilli() {
		t.Fatal("eviction did not remove oldest events across both tables")
	}
}

func TestAnalyticsPersistentWriteFailureDropsBoundedBatch(t *testing.T) {
	a := OpenAnalytics(analyticsTestConfig(t))
	defer a.Close()
	locker, err := sql.Open("sqlite3", a.path+"?_busy_timeout=100")
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	tx, err := locker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO deliveries VALUES ('lock', ?, '', '', '', '', 0)", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	a.Record(AnalyticsEvent{Kind: "delivery", Recipient: "drop@mail.test"})
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if a.Snapshot().DroppedEvents > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if a.Snapshot().DroppedEvents != 1 {
		t.Fatalf("failed batch retries not bounded: %+v", a.Snapshot())
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	a.Record(AnalyticsEvent{Kind: "delivery", Recipient: "recovered@mail.test"})
	waitAnalytics(t, a, 1)
}

func TestAnalyticsBoundsCallerControlledMetadata(t *testing.T) {
	a := OpenAnalytics(analyticsTestConfig(t))
	defer a.Close()
	a.Record(AnalyticsEvent{Kind: "http", ID: strings.Repeat("i", 1024), UserAgent: strings.Repeat("界", 20000) + "\xff", Recipient: strings.Repeat("r", 10000)})
	until := time.Now().Add(3 * time.Second)
	var id, ua, recipient string
	for time.Now().Before(until) {
		if a.DB().QueryRow("SELECT id,user_agent,recipient FROM http_requests").Scan(&id, &ua, &recipient) == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" || len(id) > 128 || len(ua) > 512 || len(recipient) > 320 || !utf8.ValidString(ua) {
		t.Fatalf("unbounded or malformed stored metadata: id=%d ua=%d recipient=%d", len(id), len(ua), len(recipient))
	}
}
