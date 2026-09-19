package app

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"

	"golang.org/x/net/html"
)

type Attachment struct {
	Index       int    `json:"index"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

type parsedEmail struct {
	Headers     string
	Text        string
	HTML        string
	Attachments []Attachment
}

func parseEmail(raw string) parsedEmail {
	headers, fallback := splitRawMessage(raw)
	result := parsedEmail{Headers: headers, Text: fallback}
	message, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return result
	}
	parts := &mimeParts{}
	index := 0
	walkMIMEParts(textproto.MIMEHeader(message.Header), message.Body, &index, parts)
	if parts.Plain != "" {
		result.Text = parts.Plain
	}
	result.HTML = sanitizeHTML(parts.HTML)
	if parts.Plain == "" && result.HTML != "" {
		result.Text = htmlText(result.HTML)
	}
	for _, part := range parts.Attachments {
		result.Attachments = append(result.Attachments, part.Attachment)
	}
	return result
}

// AttachmentContent re-parses raw looking for the attachment at index (as
// produced by parseEmail's Attachments list) and returns its decoded bytes.
// Attachments are not persisted separately; they are re-extracted on demand
// from the message's raw bytes, the same way the text/HTML body is.
func AttachmentContent(raw string, index int) (Attachment, []byte, bool) {
	message, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return Attachment{}, nil, false
	}
	parts := &mimeParts{}
	i := 0
	walkMIMEParts(textproto.MIMEHeader(message.Header), message.Body, &i, parts)
	for _, part := range parts.Attachments {
		if part.Index == index {
			return part.Attachment, part.Content, true
		}
	}
	return Attachment{}, nil, false
}

type attachmentPart struct {
	Attachment
	Content []byte
}

type mimeParts struct {
	Plain       string
	HTML        string
	Attachments []attachmentPart
}

// walkMIMEParts recursively descends multipart bodies, keeping the first
// text/plain and text/html parts as the message body and collecting every
// other part as an attachment, in document order.
func walkMIMEParts(header textproto.MIMEHeader, body io.Reader, index *int, parts *mimeParts) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				return
			}
			if err != nil {
				return
			}
			walkMIMEParts(part.Header, part, index, parts)
		}
	}

	filename, isAttachment := attachmentFilename(header, params)
	isAttachment = isAttachment || (!strings.HasPrefix(mediaType, "text/") && mediaType != "")
	if !isAttachment {
		content, err := decodeTransfer(body, header.Get("Content-Transfer-Encoding"))
		if err != nil {
			return
		}
		switch strings.ToLower(mediaType) {
		case "text/html":
			if parts.HTML == "" {
				parts.HTML = content
			}
		case "text/plain", "":
			if parts.Plain == "" {
				parts.Plain = content
			}
		}
		return
	}
	data, err := decodeTransferBytes(body, header.Get("Content-Transfer-Encoding"))
	if err != nil {
		return
	}
	i := *index
	*index++
	if filename == "" {
		filename = fmt.Sprintf("attachment-%d", i)
	}
	parts.Attachments = append(parts.Attachments, attachmentPart{
		Attachment: Attachment{Index: i, Filename: filename, ContentType: mediaType, Size: int64(len(data))},
		Content:    data,
	})
}

// attachmentFilename reports the part's filename (from Content-Disposition,
// falling back to the Content-Type name parameter) and whether the part is
// explicitly marked as an attachment.
func attachmentFilename(header textproto.MIMEHeader, contentTypeParams map[string]string) (filename string, isAttachment bool) {
	decoder := new(mime.WordDecoder)
	decode := func(value string) string {
		if decoded, err := decoder.DecodeHeader(value); err == nil && decoded != "" {
			return decoded
		}
		return value
	}
	if dispositionType, dispositionParams, err := mime.ParseMediaType(header.Get("Content-Disposition")); err == nil {
		isAttachment = strings.EqualFold(dispositionType, "attachment")
		if name := dispositionParams["filename"]; name != "" {
			return decode(name), true
		}
	}
	if name := contentTypeParams["name"]; name != "" {
		return decode(name), isAttachment
	}
	return "", isAttachment
}

func decodeTransferBytes(body io.Reader, encoding string) ([]byte, error) {
	var reader io.Reader = body
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		reader = quotedprintable.NewReader(body)
	}
	return io.ReadAll(reader)
}

func decodeTransfer(body io.Reader, encoding string) (string, error) {
	data, err := decodeTransferBytes(body, encoding)
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
	if head := findElement(doc, "head"); head != nil {
		for child := head.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode && child.Data == "style" {
				_ = html.Render(&output, child)
			}
		}
	}
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
		if child.Type == html.ElementNode && map[string]bool{"script": true, "iframe": true, "object": true, "embed": true, "link": true, "form": true, "meta": true, "base": true}[child.Data] {
			node.RemoveChild(child)
		} else {
			if child.Type == html.ElementNode {
				attrs := child.Attr[:0]
				for _, attr := range child.Attr {
					name := strings.ToLower(attr.Key)
					if !strings.HasPrefix(name, "on") && name != "src" && name != "srcset" && name != "target" && name != "rel" && (name != "href" || child.Data == "a" && safeLink(attr.Val)) {
						attrs = append(attrs, attr)
					}
				}
				child.Attr = attrs
				if child.Data == "a" && hasLink(child) {
					child.Attr = append(child.Attr, html.Attribute{Key: "target", Val: "_blank"}, html.Attribute{Key: "rel", Val: "noopener noreferrer"})
				}
			}
			removeUnsafeHTML(child)
		}
		child = next
	}
}

func safeLink(value string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	return strings.HasPrefix(value, "#") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "mailto:")
}

func hasLink(node *html.Node) bool {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, "href") && !strings.HasPrefix(strings.TrimSpace(attr.Val), "#") {
			return true
		}
	}
	return false
}

func findElement(node *html.Node, name string) *html.Node {
	if node.Type == html.ElementNode && node.Data == name {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if element := findElement(child, name); element != nil {
			return element
		}
	}
	return nil
}

func findBody(node *html.Node) *html.Node {
	return findElement(node, "body")
}

func htmlText(source string) string {
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return ""
	}
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "style" {
			return
		}
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
