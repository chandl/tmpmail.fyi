package app

import (
	"strings"
	"testing"
)

func TestParseEmailPrefersPlainTextAndSanitizesHTML(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nPlain body\r\n--x\r\nContent-Type: text/html\r\n\r\n<p>Hello</p><img src=\"https://tracker.example/pixel\"><script>alert(1)</script>\r\n--x--\r\n"
	parsed := parseEmail(raw)
	if parsed.Text != "Plain body" {
		t.Fatalf("expected plain text, got %q", parsed.Text)
	}
	if !strings.Contains(parsed.HTML, "Hello") || strings.Contains(parsed.HTML, "script") || strings.Contains(parsed.HTML, "src=") {
		t.Fatalf("HTML was not sanitized: %q", parsed.HTML)
	}
}
