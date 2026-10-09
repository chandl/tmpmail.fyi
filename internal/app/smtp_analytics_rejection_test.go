package app

import (
	"errors"
	"github.com/emersion/go-smtp"
	"testing"
	"time"
)

type rejectedDataReader struct{}

func (rejectedDataReader) Read([]byte) (int, error) { return 0, smtp.ErrDataTooLarge }
func TestSMTPRejectionAnalytics(t *testing.T) {
	a, h := testAdmin(t)
	server := &SMTPServer{cfg: Config{MailDomain: "mail.test"}, analytics: a}
	session := &smtpSession{server: server, sourceIP: "192.0.2.1", sender: "s@example.org"}
	if err := session.Rcpt("bad@other.test", nil); err == nil {
		t.Fatal("recipient accepted")
	}
	session.recipients = []string{"ok@mail.test"}
	if err := session.Data(rejectedDataReader{}); !errors.Is(err, smtp.ErrDataTooLarge) {
		t.Fatalf("DATA error: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := a.DB().QueryRow(`SELECT COUNT(*) FROM smtp_rejections`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rejections not ingested")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var page struct {
		Events []AnalyticsEvent `json:"events"`
		Total  int              `json:"total"`
	}
	adminGet(t, h, "/api/activity?kind=smtpRejected&status=550&senderDomain=example.org", &page)
	if page.Total != 1 || page.Events[0].Recipient != "bad@other.test" || page.Events[0].Route != "RCPT TO" || page.Events[0].IP != "192.0.2.1" {
		t.Fatalf("rejection metadata: %+v", page)
	}
	adminGet(t, h, "/api/activity?kind=smtpRejected&status=552", &page)
	if page.Total != 1 || page.Events[0].Recipient != "ok@mail.test" {
		t.Fatalf("DATA rejection: %+v", page)
	}
	var overview adminOverview
	adminGet(t, h, "/api/overview?window=1h", &overview)
	if overview.Deliveries != 0 || overview.Requests != 0 {
		t.Fatalf("rejections must not inflate traffic: %+v", overview)
	}
	// Retention applies to rejection metadata as well.
	if _, err := a.DB().Exec(`UPDATE smtp_rejections SET timestamp=?`, time.Now().Add(-31*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := a.prune(a.DB()); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := a.DB().QueryRow(`SELECT COUNT(*) FROM smtp_rejections`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 0 {
		t.Fatalf("expired rejections retained: %d", retained)
	}
}
