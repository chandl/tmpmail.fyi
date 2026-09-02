package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
)

type checkResult struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Message   string    `json:"message,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	LatencyMS int64     `json:"latency_ms"`
}
type healthStatus struct {
	Status        string        `json:"status"`
	LastCheckedAt time.Time     `json:"last_checked_at,omitempty"`
	Checks        []checkResult `json:"checks"`
}
type runner struct {
	cfg     config
	mu      sync.RWMutex
	current healthStatus
}

func newRunner(cfg config) *runner {
	return &runner{cfg: cfg, current: healthStatus{Status: "starting", Checks: []checkResult{}}}
}
func (r *runner) run(ctx context.Context) {
	r.runOnce(ctx)
	ticker := time.NewTicker(r.cfg.Interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.runOnce(ctx)
			}
		}
	}()
}
func (r *runner) runOnce(parent context.Context) {
	log.Printf("canary run started")
	ctx, cancel := context.WithTimeout(parent, r.cfg.Timeout)
	defer cancel()
	results := []checkResult{runAPIHealthCheck(ctx, r.cfg), runMailFlowCheck(ctx, r.cfg)}
	status := "healthy"
	for _, result := range results {
		if result.Message == "" {
			log.Printf("canary check=%s status=%s latency_ms=%d", result.Name, result.Status, result.LatencyMS)
		} else {
			log.Printf("canary check=%s status=%s latency_ms=%d error=%q", result.Name, result.Status, result.LatencyMS, result.Message)
		}
	}
	for _, result := range results {
		if result.Status != "healthy" {
			status = "unhealthy"
			break
		}
	}
	r.mu.Lock()
	r.current = healthStatus{Status: status, LastCheckedAt: time.Now().UTC(), Checks: results}
	r.mu.Unlock()
	log.Printf("canary run completed status=%s", status)
}
func (r *runner) status() healthStatus { r.mu.RLock(); defer r.mu.RUnlock(); return r.current }

func runAPIHealthCheck(ctx context.Context, cfg config) checkResult {
	return timedCheck("api-health", func() error { return expectStatus(ctx, cfg.APIURL+"/healthz", http.StatusNoContent) })
}
func runMailFlowCheck(ctx context.Context, cfg config) checkResult {
	return timedCheck("smtp-to-api-and-ui-message-flow", func() error {
		token, err := randomToken()
		if err != nil {
			return err
		}
		recipient, subject, body := "canary-"+token+"@"+cfg.MailDomain, "canary "+token, "canary message "+token
		if err := smtpSend(ctx, cfg.SMTPAddr, cfg.From, recipient, subject, body); err != nil {
			return fmt.Errorf("send SMTP message: %w", err)
		}
		message, err := waitForMessage(ctx, cfg.APIURL, recipient, subject)
		if err != nil {
			return err
		}
		if message.Recipient != recipient {
			return fmt.Errorf("message recipient = %q, want %q", message.Recipient, recipient)
		}
		if message.From != cfg.From {
			return fmt.Errorf("message from = %q, want %q", message.From, cfg.From)
		}
		if message.Subject != subject {
			return fmt.Errorf("message subject = %q, want %q", message.Subject, subject)
		}
		if !strings.Contains(message.Body, body) {
			return fmt.Errorf("message body does not contain probe token")
		}
		return verifyInboxUI(ctx, cfg.APIURL, recipient, body)
	})
}

// verifyInboxUI checks the rendered inbox page rather than only its JSON API.
// It detects a deployment or cache mismatch that leaves an otherwise valid
// message body empty in the browser.
func verifyInboxUI(ctx context.Context, apiURL, recipient, body string) error {
	inbox, _, ok := strings.Cut(recipient, "@")
	if !ok || inbox == "" {
		return fmt.Errorf("invalid probe recipient %q", recipient)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/?inbox="+url.QueryEscape(inbox), nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("load inbox UI: got HTTP %d", response.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(response.Body, 3<<20))
	if err != nil {
		return fmt.Errorf("read inbox UI: %w", err)
	}
	contents := string(page)
	if !strings.Contains(contents, body) {
		return fmt.Errorf("inbox UI does not render probe message body")
	}
	if !strings.Contains(contents, "/ui.js?v=") || !strings.Contains(contents, "/ui.css?v=") {
		return fmt.Errorf("inbox UI does not reference cache-busted assets")
	}
	return nil
}
func timedCheck(name string, fn func() error) checkResult {
	start := time.Now()
	result := checkResult{Name: name, CheckedAt: start.UTC()}
	if err := fn(); err != nil {
		result.Status = "unhealthy"
		result.Message = err.Error()
	} else {
		result.Status = "healthy"
	}
	result.LatencyMS = time.Since(start).Milliseconds()
	return result
}
func expectStatus(ctx context.Context, target string, expected int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != expected {
		return fmt.Errorf("GET %s: got HTTP %d, want %d", target, response.StatusCode, expected)
	}
	return nil
}

type inboxPage struct {
	Messages []messageSummary `json:"messages"`
}
type messageSummary struct {
	ID        string `json:"id"`
	Recipient string `json:"recipient"`
	From      string `json:"from"`
	Subject   string `json:"subject"`
}
type messageDetail struct {
	messageSummary
	Body string `json:"body"`
}

func waitForMessage(ctx context.Context, apiURL, recipient, subject string) (messageDetail, error) {
	for {
		message, found, err := findMessage(ctx, apiURL, recipient, subject)
		if err != nil {
			return messageDetail{}, err
		}
		if found {
			return message, nil
		}
		select {
		case <-ctx.Done():
			return messageDetail{}, fmt.Errorf("message did not appear in inbox before deadline: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func findMessage(ctx context.Context, apiURL, recipient, subject string) (messageDetail, bool, error) {
	target := apiURL + "/api/v1/inboxes/" + url.PathEscape(recipient) + "?limit=25"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return messageDetail{}, false, err
	}
	response, err := (&http.Client{}).Do(req)
	if err != nil {
		return messageDetail{}, false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return messageDetail{}, false, fmt.Errorf("list inbox: got HTTP %d", response.StatusCode)
	}
	var page inboxPage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		return messageDetail{}, false, fmt.Errorf("decode inbox: %w", err)
	}
	for _, summary := range page.Messages {
		if summary.Subject == subject {
			return getMessage(ctx, apiURL, summary.ID)
		}
	}
	return messageDetail{}, false, nil
}
func getMessage(ctx context.Context, apiURL, id string) (messageDetail, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/api/v1/messages/"+url.PathEscape(id), nil)
	if err != nil {
		return messageDetail{}, false, err
	}
	response, err := (&http.Client{}).Do(req)
	if err != nil {
		return messageDetail{}, false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return messageDetail{}, false, fmt.Errorf("get message: got HTTP %d", response.StatusCode)
	}
	var message messageDetail
	if err := json.NewDecoder(response.Body).Decode(&message); err != nil {
		return messageDetail{}, false, fmt.Errorf("decode message: %w", err)
	}
	return message, true, nil
}
func randomToken() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func smtpSend(ctx context.Context, address, from, to, subject, body string) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	client := newSMTPClient(conn)
	if _, err := client.response(220); err != nil {
		return err
	}
	if _, err := client.ehlo(host); err != nil {
		return err
	}
	if err := client.command(250, "MAIL FROM:<"+from+">"); err != nil {
		return err
	}
	if err := client.command(250, "RCPT TO:<"+to+">"); err != nil {
		return err
	}
	if err := client.command(354, "DATA"); err != nil {
		return err
	}
	message := "To: " + to + "\r\nFrom: " + from + "\r\nSubject: " + subject + "\r\nDate: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n\r\n" + body + "\r\n.\r\n"
	if err := client.write(message); err != nil {
		return err
	}
	if _, err := client.response(250); err != nil {
		return err
	}
	return client.command(221, "QUIT")
}

type smtpClient struct {
	conn   net.Conn
	reader *textproto.Reader
}

func newSMTPClient(conn net.Conn) *smtpClient {
	return &smtpClient{conn: conn, reader: textproto.NewReader(bufio.NewReader(conn))}
}
func (c *smtpClient) ehlo(host string) ([]string, error) {
	if err := c.write("EHLO " + host + "\r\n"); err != nil {
		return nil, err
	}
	return c.response(250)
}
func (c *smtpClient) command(expected int, command string) error {
	if err := c.write(command + "\r\n"); err != nil {
		return err
	}
	_, err := c.response(expected)
	return err
}
func (c *smtpClient) write(value string) error { _, err := c.conn.Write([]byte(value)); return err }
func (c *smtpClient) response(expected int) ([]string, error) {
	code, message, err := c.reader.ReadResponse(expected)
	if err != nil {
		return nil, err
	}
	if code != expected {
		return nil, fmt.Errorf("got SMTP %d, want %d", code, expected)
	}
	return strings.Split(message, "\n"), nil
}
