package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chandl/tmpmail.fyi/internal/api"
)

// Exercise the boundaries between SMTP canonicalization, persistence, the
// API, MIME previews and byte-preserving attachment downloads.
func TestSMTPToHTTPNormalizesDomainsAndDecodesPreview(t *testing.T) {
	store := testStore(t, time.Hour)
	server := mustNewSMTPServer(t, store.cfg, store)
	client, reader := startSMTPServer(t, server)
	readSMTPResponse(t, reader, 220)
	for _, command := range []string{
		"EHLO integration-client",
		"MAIL FROM:<sender@example.org>",
		"RCPT TO:<Build@MAIL.TEST>",
		"RCPT TO:<Build@mail.test>",
		"RCPT TO:<build@MAIL.TEST>",
	} {
		writeSMTPCommand(t, client, command)
		readSMTPResponse(t, reader, 250)
	}
	writeSMTPCommand(t, client, "DATA")
	readSMTPResponse(t, reader, 354)
	raw := "From: sender@example.org\r\nSubject: charset flow\r\nContent-Type: multipart/mixed; boundary=flow\r\n\r\n" +
		"--flow\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ncaf=E9\r\n" +
		"--flow\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Disposition: attachment; filename=cafe.txt\r\nContent-Transfer-Encoding: base64\r\n\r\nY2Fm6Q==\r\n--flow--\r\n"
	if _, err := fmt.Fprint(client, raw+".\r\n"); err != nil {
		t.Fatal(err)
	}
	readSMTPResponse(t, reader, 250)
	writeSMTPCommand(t, client, "QUIT")
	readSMTPResponse(t, reader, 221)

	handler := NewHTTPServer(store.cfg, store)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, response.Code, response.Body.String())
		}
		return response
	}
	var ids []string
	for _, inbox := range []string{"Build@MAIL.TEST", "build@MAIL.TEST"} {
		response := get("/api/v1/inboxes/" + inbox)
		var page api.InboxPage
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Messages) != 1 || string(page.Messages[0].Recipient) != normalizeRecipient(inbox) {
			t.Fatalf("inbox %q: unexpected page %#v", inbox, page)
		}
		ids = append(ids, page.Messages[0].Id)
	}
	if ids[0] == ids[1] {
		t.Fatal("local-part case was collapsed")
	}
	var preview struct{ Text string }
	if err := json.Unmarshal(get("/ui/messages/"+ids[0]).Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Text != "café" {
		t.Fatalf("preview text=%q", preview.Text)
	}
	if body := get("/?inbox=Build").Body.String(); !strings.Contains(body, "café") {
		t.Fatal("initial server-rendered message is missing decoded text")
	}
	if body := get("/api/v1/messages/" + ids[0] + "/attachments/0").Body.String(); body != "caf\xe9" {
		t.Fatalf("attachment bytes changed: %x", body)
	}
}
