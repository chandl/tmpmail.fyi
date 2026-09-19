package app

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseEmailPrefersPlainTextAndSanitizesHTML(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nPlain body\r\n--x\r\nContent-Type: text/html\r\n\r\n<style>p{color:red}</style><p style=\"font-weight:bold\">Hello</p><a href=\"https://example.com/reset\" target=\"_self\" rel=\"opener\">Reset</a><a href=\"javascript:alert(1)\" onclick=\"alert(1)\">Bad</a><img src=\"https://tracker.example/pixel\"><script>alert(1)</script>\r\n--x--\r\n"
	parsed := parseEmail(raw)
	if parsed.Text != "Plain body" {
		t.Fatalf("expected plain text, got %q", parsed.Text)
	}
	if !strings.Contains(parsed.HTML, "Hello") || !strings.Contains(parsed.HTML, "<style>") || !strings.Contains(parsed.HTML, `style="font-weight:bold"`) {
		t.Fatalf("expected email formatting to be preserved: %q", parsed.HTML)
	}
	if !strings.Contains(parsed.HTML, `href="https://example.com/reset"`) || !strings.Contains(parsed.HTML, `target="_blank"`) || !strings.Contains(parsed.HTML, `rel="noopener noreferrer"`) {
		t.Fatalf("expected safe links to open in a new tab: %q", parsed.HTML)
	}
	if strings.Contains(parsed.HTML, "javascript:") || strings.Contains(parsed.HTML, "onclick") || strings.Contains(parsed.HTML, "script") || strings.Contains(parsed.HTML, "src=") {
		t.Fatalf("HTML was not sanitized: %q", parsed.HTML)
	}
}

func TestParseEmailExtractsAttachmentsAndRoundTripsContent(t *testing.T) {
	binary := "\x00\x01\x02\x03binary-content\xff"
	encoded := base64.StdEncoding.EncodeToString([]byte(binary))
	raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nPlain body\r\n" +
		"--x\r\nContent-Type: application/octet-stream; name=\"data.bin\"\r\nContent-Disposition: attachment; filename=\"data.bin\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" + encoded + "\r\n" +
		"--x--\r\n"

	parsed := parseEmail(raw)
	if parsed.Text != "Plain body" {
		t.Fatalf("expected plain body to remain the message text, got %q", parsed.Text)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("expected one attachment, got %#v", parsed.Attachments)
	}
	attachment := parsed.Attachments[0]
	if attachment.Filename != "data.bin" || attachment.ContentType != "application/octet-stream" || attachment.Size != int64(len(binary)) {
		t.Fatalf("unexpected attachment metadata: %#v", attachment)
	}

	meta, content, ok := AttachmentContent(raw, attachment.Index)
	if !ok {
		t.Fatal("expected AttachmentContent to find the attachment")
	}
	if meta != attachment {
		t.Fatalf("expected consistent attachment metadata, got %#v vs %#v", meta, attachment)
	}
	if string(content) != binary {
		t.Fatalf("expected decoded attachment bytes to round-trip, got %q", content)
	}

	if _, _, ok := AttachmentContent(raw, attachment.Index+1); ok {
		t.Fatal("expected an out-of-range attachment index to be reported as not found")
	}
}

func TestParseHTMLEmailBuildsReadablePlainTextPreview(t *testing.T) {
	parsed := parseEmail("Content-Type: text/html; charset=UTF-8\r\n\r\n<!doctype html><html><head><style>p{color:red}</style></head><body><h1>Welcome</h1><p>Open your account</p></body></html>")
	if parsed.Text != "Welcome Open your account" {
		t.Fatalf("expected readable HTML preview text, got %q", parsed.Text)
	}
	if strings.Contains(parsed.Text, "<html") || strings.Contains(parsed.Text, "color:red") {
		t.Fatalf("preview contains HTML or CSS: %q", parsed.Text)
	}
}
