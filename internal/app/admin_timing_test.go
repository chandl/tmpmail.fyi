package app

import (
	"database/sql"
	"testing"
	"time"
)

func TestAdminSortResponseTimeAcrossProtocols(t *testing.T) {
	a, h := testAdmin(t)
	now := time.Now().Add(-time.Minute)
	seedAdminDelivery(t, a, "legacy", now, "a@mail.test", "s@example.org", "example.org", "192.0.2.1")
	seedAdminDelivery(t, a, "smtp", now, "a@mail.test", "s@example.org", "example.org", "192.0.2.1")
	if _, err := a.DB().Exec(`UPDATE deliveries SET duration_ms=0 WHERE id='smtp'`); err != nil {
		t.Fatal(err)
	}
	seedAdminHTTP(t, a, "http", now, "a@mail.test", "192.0.2.1", "test", false)
	if _, err := a.DB().Exec(`INSERT INTO smtp_rejections VALUES(?,?,?,?,?,?,?,?,?)`, "reject", now.UnixMilli(), "a@mail.test", "s@example.org", "example.org", "192.0.2.1", 550, 40, "RCPT TO"); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Events []AnalyticsEvent `json:"events"`
		Total  int              `json:"total"`
	}
	for _, check := range []struct{ sort, first string }{{"fastest", "smtp"}, {"slowest", "reject"}} {
		adminGet(t, h, "/api/activity?sort="+check.sort, &page)
		if page.Total != 4 || len(page.Events) != 4 || page.Events[0].ID != check.first || page.Events[3].ID != "legacy" {
			t.Fatalf("cross-protocol order %s: %+v", check.sort, page)
		}
		if !page.Events[0].DurationKnown || page.Events[3].DurationKnown {
			t.Fatalf("unknown and zero durations: %+v", page)
		}
	}
	adminGet(t, h, "/api/activity?kind=delivery&sort=slowest", &page)
	if page.Total != 2 || page.Events[0].ID != "smtp" || page.Events[1].ID != "legacy" {
		t.Fatalf("SMTP delivery sort: %+v", page)
	}
}
func TestLegacyDeliveryTimingMigration(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE deliveries(id TEXT PRIMARY KEY); INSERT INTO deliveries(id) VALUES('legacy')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateDeliveryTiming(db); err != nil {
			t.Fatal(err)
		}
	}
	var timing sql.NullFloat64
	if err := db.QueryRow(`SELECT duration_ms FROM deliveries`).Scan(&timing); err != nil {
		t.Fatal(err)
	}
	if timing.Valid {
		t.Fatal("legacy timing must remain unknown")
	}
}
