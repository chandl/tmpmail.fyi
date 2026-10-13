package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type acmeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f acmeRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestACMELoggingVisibilityAndRedaction(t *testing.T) {
	for _, tt := range []struct {
		name   string
		debug  bool
		status int
		err    error
		want   string
		quiet  bool
	}{
		{name: "normal success", status: 200, quiet: true},
		{name: "debug success", debug: true, status: 200, want: "status=200"},
		{name: "HTTP failure", status: 403, want: "status=403"},
		{name: "network failure", err: errors.New("connection refused"), want: "connection refused"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs strings.Builder
			req, err := http.NewRequest("POST", "https://ca.example/order?token=query-secret", strings.NewReader("body-secret"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer header-secret")
			transport := &acmeLogTransport{debug: tt.debug, logf: func(format string, args ...any) { fmt.Fprintf(&logs, format+"\n", args...) }, base: acmeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r != req {
					t.Fatal("request was replaced")
				}
				if tt.err != nil {
					return nil, tt.err
				}
				return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader("response-secret"))}, nil
			})}
			response, gotErr := transport.RoundTrip(req)
			if gotErr != tt.err {
				t.Fatalf("error changed: %v", gotErr)
			}
			if response != nil {
				response.Body.Close()
			}
			if tt.quiet {
				if logs.Len() != 0 {
					t.Fatalf("normal requests logged: %s", logs.String())
				}
				return
			}
			if !strings.Contains(logs.String(), tt.want) || !strings.Contains(logs.String(), "endpoint=ca.example/order") {
				t.Fatalf("missing diagnostics: %s", logs.String())
			}
			for _, secret := range []string{"query-secret", "header-secret", "body-secret", "response-secret"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("secret exposed: %s", secret)
				}
			}
		})
	}
}

func TestACMEDebugConfig(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")
	t.Setenv("ACME_DEBUG", "true")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ACMEDebug {
		t.Fatal("ACME_DEBUG not enabled")
	}
	t.Setenv("ACME_DEBUG", "invalid")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("invalid ACME_DEBUG accepted")
	}
}
