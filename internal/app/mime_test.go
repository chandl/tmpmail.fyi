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

func TestContentTypeFilenameMakesTextPartAnAttachment(t *testing.T) {
	for _, mediaType := range []string{"text/plain", "text/calendar"} {
		for _, disposition := range []string{"", "Content-Disposition: inline\r\n"} {
			t.Run(mediaType+"/"+disposition, func(t *testing.T) {
				content := "BEGIN:VCALENDAR\r\nEND:VCALENDAR"
				raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n" +
					"--x\r\nContent-Type: " + mediaType + "; name=invite.ics\r\n" + disposition +
					"Content-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(content)) + "\r\n" +
					"--x\r\nContent-Type: text/plain\r\n\r\nPlain body\r\n--x--\r\n"
				parsed := parseEmail(raw)
				if parsed.Text != "Plain body" {
					t.Fatalf("expected unnamed text part as body, got %q", parsed.Text)
				}
				if len(parsed.Attachments) != 1 {
					t.Fatalf("expected named text attachment, got %#v", parsed.Attachments)
				}
				attachment := parsed.Attachments[0]
				if attachment.Filename != "invite.ics" || attachment.ContentType != mediaType || attachment.Size != int64(len(content)) {
					t.Fatalf("unexpected attachment metadata: %#v", attachment)
				}
				meta, data, ok := AttachmentContent(raw, attachment.Index)
				if !ok || meta != attachment || string(data) != content {
					t.Fatalf("attachment failed to round-trip: metadata=%#v content=%q found=%t", meta, data, ok)
				}
			})
		}
	}
}

func TestParseEmailDecodesBodyCharset(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, transfer, body, want string
	}{
		{"latin1 quoted printable", "text/plain; charset=iso-8859-1", "quoted-printable", "caf=E9", "café"},
		{"windows1252 base64", "text/plain; charset=windows-1252", "base64", base64.StdEncoding.EncodeToString([]byte("\x93hello\x94")), "“hello”"},
		{"latin1 html", "text/html; charset=ISO-8859-1", "quoted-printable", "<p>caf=E9</p>", "café"},
		{"missing charset UTF8", "text/plain", "8bit", "café", "café"},
		{"unknown charset UTF8", "text/plain; charset=unknown", "8bit", "café", "café"},
		{"unknown charset invalid bytes", "text/plain; charset=unknown", "8bit", "caf\xe9", "caf\ufffd"},
		{"invalid UTF8", "text/plain; charset=UTF-8", "8bit", "caf\xe9", "caf\ufffd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "Content-Type: " + tc.contentType + "\r\nContent-Transfer-Encoding: " + tc.transfer + "\r\n\r\n" + tc.body
			parsed := parseEmail(raw)
			if parsed.Text != tc.want {
				t.Fatalf("text=%q, want %q", parsed.Text, tc.want)
			}
			if strings.HasPrefix(tc.contentType, "text/html") && parsed.HTML != "<p>café</p>" {
				t.Fatalf("HTML=%q, want decoded body", parsed.HTML)
			}
		})
	}
}

func TestParseMultipartConvertsBodiesButPreservesTextAttachments(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ncaf=E9\r\n" +
		"--x\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Disposition: attachment; filename=cafe.txt\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte("caf\xe9")) + "\r\n--x--\r\n"
	parsed := parseEmail(raw)
	if parsed.Text != "café" || len(parsed.Attachments) != 1 {
		t.Fatalf("parsed=%#v, want decoded body and attachment", parsed)
	}
	meta, content, ok := AttachmentContent(raw, parsed.Attachments[0].Index)
	if !ok || string(content) != "caf\xe9" || meta.Size != 4 {
		t.Fatalf("text attachment was transcoded: metadata=%#v content=%q found=%t", meta, content, ok)
	}
}
