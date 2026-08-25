package app

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"

	"golang.org/x/net/html"
)

type parsedEmail struct {
	Headers string
	Text    string
	HTML    string
}

func parseEmail(raw string) parsedEmail {
	headers, fallback := splitRawMessage(raw)
	result := parsedEmail{Headers: headers, Text: fallback}
	message, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return result
	}
	plain, htmlBody := parseMIMEPart(textproto.MIMEHeader(message.Header), message.Body)
	if plain != "" {
		result.Text = plain
	}
	result.HTML = sanitizeHTML(htmlBody)
	if result.Text == "" && result.HTML != "" {
		result.Text = htmlText(result.HTML)
	}
	return result
}

func parseMIMEPart(header textproto.MIMEHeader, body io.Reader) (string, string) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(body, params["boundary"])
		var plain, htmlBody string
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return plain, htmlBody
			}
			partPlain, partHTML := parseMIMEPart(part.Header, part)
			if plain == "" {
				plain = partPlain
			}
			if htmlBody == "" {
				htmlBody = partHTML
			}
		}
		return plain, htmlBody
	}
	content, err := decodeTransfer(body, header.Get("Content-Transfer-Encoding"))
	if err != nil {
		return "", ""
	}
	switch strings.ToLower(mediaType) {
	case "text/html":
		return "", content
	case "text/plain", "":
		return content, ""
	default:
		return "", ""
	}
}

func decodeTransfer(body io.Reader, encoding string) (string, error) {
	var reader io.Reader = body
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		reader = quotedprintable.NewReader(body)
	}
	data, err := io.ReadAll(reader)
	return string(data), err
}

func sanitizeHTML(source string) string {
	if source == "" {
		return ""
	}
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return ""
	}
	removeUnsafeHTML(doc)
	var output bytes.Buffer
	if body := findBody(doc); body != nil {
		for child := body.FirstChild; child != nil; child = child.NextSibling {
			_ = html.Render(&output, child)
		}
	}
	return output.String()
}

func removeUnsafeHTML(node *html.Node) {
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		if child.Type == html.ElementNode && map[string]bool{"script": true, "style": true, "iframe": true, "object": true, "embed": true, "link": true, "form": true, "meta": true, "base": true}[child.Data] {
			node.RemoveChild(child)
		} else {
			if child.Type == html.ElementNode {
				attrs := child.Attr[:0]
				for _, attr := range child.Attr {
					name := strings.ToLower(attr.Key)
					if !strings.HasPrefix(name, "on") && name != "style" && name != "src" && name != "srcset" {
						attrs = append(attrs, attr)
					}
				}
				child.Attr = attrs
			}
			removeUnsafeHTML(child)
		}
		child = next
	}
}

func findBody(node *html.Node) *html.Node {
	if node.Type == html.ElementNode && node.Data == "body" {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if body := findBody(child); body != nil {
			return body
		}
	}
	return nil
}

func htmlText(source string) string {
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return ""
	}
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			text.WriteString(node.Data)
			text.WriteByte(' ')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return strings.TrimSpace(text.String())
}
