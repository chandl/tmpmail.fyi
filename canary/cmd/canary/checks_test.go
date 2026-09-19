package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAPIHealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()

	result := runAPIHealthCheck(context.Background(), config{APIURL: server.URL})
	if result.Status != "healthy" {
		t.Fatalf("status = %q, message = %q", result.Status, result.Message)
	}
}

func TestVerifyInboxUI(t *testing.T) {
	body := "canary message unique-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.URL.Query().Get("inbox") != "canary-unique-token" {
			t.Fatalf("unexpected UI request %q", r.URL.String())
		}
		fmt.Fprintf(w, `<link rel="stylesheet" href="/ui.css?v=2"><script src="/ui.js?v=2"></script><pre>%s</pre>`, body)
	}))
	defer server.Close()

	if err := verifyInboxUI(context.Background(), server.URL, "canary-unique-token@mail.test", body); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyInboxUIRejectsEmptyBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<link href="/ui.css?v=2"><script src="/ui.js?v=2"></script><pre></pre>`))
	}))
	defer server.Close()

	err := verifyInboxUI(context.Background(), server.URL, "canary-token@mail.test", "canary message token")
	if err == nil || !strings.Contains(err.Error(), "does not render probe message body") {
		t.Fatalf("error = %v, want missing body", err)
	}
}

func TestRunnerMarksAFailedCheckUnhealthy(t *testing.T) {
	runner := newRunner(config{Timeout: time.Second, APIURL: "http://127.0.0.1:1", SMTPAddr: "127.0.0.1:1", MailDomain: "mail.example.com"})
	runner.runOnce(context.Background())
	if status := runner.status(); status.Status != "unhealthy" || len(status.Checks) != 3 {
		t.Fatalf("unexpected health status: %+v", status)
	}
}

func TestFindMessageDecodesAttachments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/inboxes/"):
			fmt.Fprint(w, `{"messages":[{"id":"abc123","recipient":"canary-attachment-token@mail.test","from":"canary@monitor.invalid","subject":"canary attachment token"}]}`)
		case r.URL.Path == "/api/v1/messages/abc123":
			fmt.Fprint(w, `{"id":"abc123","recipient":"canary-attachment-token@mail.test","from":"canary@monitor.invalid","subject":"canary attachment token","body":"hello","attachments":[{"index":0,"filename":"canary-token.txt","contentType":"text/plain","size":7}]}`)
		default:
			t.Fatalf("unexpected request %s", r.URL.String())
		}
	}))
	defer server.Close()

	message, found, err := findMessage(context.Background(), server.URL, "canary-attachment-token@mail.test", "canary attachment token")
	if err != nil || !found {
		t.Fatalf("findMessage() = %+v, found=%t, err=%v", message, found, err)
	}
	if len(message.Attachments) != 1 {
		t.Fatalf("expected one attachment, got %#v", message.Attachments)
	}
	if got := message.Attachments[0]; got.Filename != "canary-token.txt" || got.ContentType != "text/plain" || got.Size != 7 {
		t.Fatalf("unexpected attachment metadata: %#v", got)
	}
}

func TestDownloadAttachmentReturnsContentOrError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/messages/abc123/attachments/0" {
			_, _ = w.Write([]byte("attachment payload"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	content, err := downloadAttachment(context.Background(), server.URL, "abc123", 0)
	if err != nil || string(content) != "attachment payload" {
		t.Fatalf("downloadAttachment() = %q, err=%v", content, err)
	}
	if _, err := downloadAttachment(context.Background(), server.URL, "abc123", 9); err == nil {
		t.Fatal("expected an error for a missing attachment index")
	}
}

func TestAttachmentFlowCheckFailsWhenSMTPIsUnreachable(t *testing.T) {
	result := runAttachmentFlowCheck(context.Background(), config{Timeout: time.Second, APIURL: "http://127.0.0.1:1", SMTPAddr: "127.0.0.1:1", MailDomain: "mail.example.com", From: "canary@monitor.invalid"})
	if result.Status != "unhealthy" || result.Name != "smtp-attachment-download" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestCanaryPNGIsAValidPNG(t *testing.T) {
	data := canaryPNG()
	signature := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if len(data) < len(signature) || string(data[:len(signature)]) != string(signature) {
		t.Fatalf("embedded canary attachment does not start with a PNG signature: %x", data)
	}
}
