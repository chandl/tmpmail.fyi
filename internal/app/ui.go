package app

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// The web UI is plain server-rendered HTML. Templates, CSS, and JS live in
// assets/ui and are embedded in the binary; there is no frontend build step.
//
//go:embed assets/ui
var uiAssets embed.FS

var (
	uiCSS     = mustReadUIAsset("ui.css")
	uiScript  = mustReadUIAsset("ui.js")
	uiVersion = assetVersion(uiCSS, uiScript)

	uiTemplates = template.Must(template.New("ui").Funcs(template.FuncMap{
		"rowData":      func(m inboxMessage, selected bool) messageView { return messageView{Message: m, Selected: selected} },
		"emptyMessage": func() inboxMessage { return inboxMessage{} },
		"snippet":      func(label, command string) snippet { return snippet{Label: label, Command: command} },
	}).ParseFS(uiAssets, "assets/ui/*.html"))
	inboxTemplate   = uiTemplates.Lookup("inbox")
	privacyTemplate = uiTemplates.Lookup("privacy")
)

func mustReadUIAsset(name string) string {
	content, err := uiAssets.ReadFile("assets/ui/" + name)
	if err != nil {
		panic(err)
	}
	return string(content)
}

func assetVersion(assets ...string) string {
	hash := sha256.New()
	for _, asset := range assets {
		hash.Write([]byte(asset))
	}
	return hex.EncodeToString(hash.Sum(nil))[:12]
}

// serveUIAsset serves embedded CSS/JS. Requests carrying the current content
// hash (?v=...) are immutable and cached for a year.
func serveUIAsset(contentType, content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if r.URL.Query().Get("v") == uiVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		_, _ = w.Write([]byte(content))
	}
}

// pageChrome is shared by every server-rendered page.
type pageChrome struct {
	CSS          template.CSS
	AssetVersion string
	Year         int
}

func newPageChrome() pageChrome {
	return pageChrome{CSS: template.CSS(uiCSS), AssetVersion: uiVersion, Year: time.Now().Year()}
}

// snippet is a copyable command; ui.js fills in {origin}, {inbox}, and {id}.
type snippet struct {
	Label   string
	Command string
}

type messageView struct {
	Message  inboxMessage
	Selected bool
}

// senderFromHeaders returns the display name and address from the message's
// From header, falling back to the envelope sender.
func senderFromHeaders(headers, envelope string) (name, address string) {
	address = envelope
	parsed, err := mail.ReadMessage(strings.NewReader(headers + "\r\n\r\n"))
	if err != nil {
		return "", address
	}
	from, err := mail.ParseAddress(parsed.Header.Get("From"))
	if err != nil {
		return "", address
	}
	return strings.TrimSpace(from.Name), from.Address
}

// blockedImageCSS marks where an image would have been. Images are never
// loaded (the message CSP sets img-src 'none'), so each <img> is drawn as a
// striped placeholder showing its alt text.
const blockedImageCSS = `img{display:inline-block;background:repeating-linear-gradient(135deg,#f4f4f1 0 6px,#ebebe7 6px 12px);outline:1px dashed #b9b9c0;outline-offset:-1px;color:#6c6c75;font-size:12px}`

type blockedImages struct {
	Total          int
	TrackingPixels int
}

// countBlockedImages counts <img> elements in sanitized message HTML. Images
// sized 2px or smaller are counted as tracking pixels.
func countBlockedImages(sanitized string) blockedImages {
	var result blockedImages
	if sanitized == "" {
		return result
	}
	doc, err := html.Parse(strings.NewReader(sanitized))
	if err != nil {
		return result
	}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "img" {
			result.Total++
			for _, attr := range node.Attr {
				if key := strings.ToLower(attr.Key); key == "width" || key == "height" {
					if size, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(attr.Val), "px")); err == nil && size <= 2 {
						result.TrackingPixels++
						break
					}
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return result
}

// Label is the short note shown above an HTML email; ui.js mirrors it.
func (b blockedImages) Label() string {
	plural := func(n int, word string) string {
		if n == 1 {
			return "1 " + word
		}
		return strconv.Itoa(n) + " " + word + "s"
	}
	switch {
	case b.Total == 0:
		return ""
	case b.TrackingPixels == b.Total && b.Total == 1:
		return "Tracking pixel blocked"
	case b.TrackingPixels == b.Total:
		return plural(b.Total, "tracking pixel") + " blocked"
	case b.TrackingPixels == 0:
		return plural(b.Total, "image") + " blocked for privacy"
	default:
		return plural(b.Total, "image") + " blocked, incl. " + plural(b.TrackingPixels, "tracking pixel")
	}
}
