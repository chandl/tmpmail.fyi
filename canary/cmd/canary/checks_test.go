package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestRunnerMarksAFailedCheckUnhealthy(t *testing.T) {
	runner := newRunner(config{Timeout: time.Second, APIURL: "http://127.0.0.1:1", SMTPAddr: "127.0.0.1:1", MailDomain: "mail.example.com"})
	runner.runOnce(context.Background())
	if status := runner.status(); status.Status != "unhealthy" || len(status.Checks) != 2 {
		t.Fatalf("unexpected health status: %+v", status)
	}
}
