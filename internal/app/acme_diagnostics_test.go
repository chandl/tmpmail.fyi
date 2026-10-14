package app

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestACMEDiagnosticInvalidAuthorization(t *testing.T) {
	body := `{"status":"invalid","identifier":{"type":"dns","value":"ohpp.fyi"},"token":"token-secret","privateKey":"key-secret","challenges":[{"type":"tls-alpn-01","status":"invalid","url":"url-secret","token":"challenge-secret","error":{"type":"urn:ietf:params:acme:error:connection","detail":"Connection refused on port 443"}}]}`
	var logs strings.Builder
	reader := &acmeDiagnosticBody{ReadCloser: io.NopCloser(strings.NewReader(body)), endpoint: "ca.example/authz/1", logf: func(format string, args ...any) { fmt.Fprintf(&logs, format+"\n", args...) }}
	returned, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if string(returned) != body {
		t.Fatal("response body modified")
	}
	for _, want := range []string{`status="invalid"`, `identifier="ohpp.fyi"`, `type="tls-alpn-01"`, `detail="Connection refused on port 443"`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %s: %s", want, logs.String())
		}
	}
	for _, secret := range []string{"token-secret", "key-secret", "url-secret", "challenge-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
	if strings.Count(logs.String(), "ACME state:") != 1 {
		t.Fatal("diagnostics logged twice")
	}
}

func TestACMEDiagnosticsIgnoreMalformedAndOversizedBodies(t *testing.T) {
	for _, body := range []string{"not-json", `{"status":"invalid","padding":"` + strings.Repeat("x", acmeDiagnosticLimit) + `"}`} {
		var logs strings.Builder
		reader := &acmeDiagnosticBody{ReadCloser: io.NopCloser(strings.NewReader(body)), logf: func(format string, args ...any) { fmt.Fprintf(&logs, format, args...) }}
		returned, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		reader.Close()
		if string(returned) != body || logs.Len() != 0 || len(reader.captured) > acmeDiagnosticLimit {
			t.Fatal("invalid diagnostic body handling")
		}
	}
}
