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
	if status := runner.status(); status.Status != "unhealthy" || len(status.Checks) != 2 {
		t.Fatalf("unexpected health status: %+v", status)
	}
}
