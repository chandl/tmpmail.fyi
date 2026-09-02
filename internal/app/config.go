package app

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MailDomain              string
	DataDir                 string
	SMTPAddr                string
	SMTPTLSCertFile         string
	SMTPTLSKeyFile          string
	HTTPAddr                string
	MetricsAddr             string
	MessageTTL              time.Duration
	MaxMessageBytes         int64
	MaxStorageBytes         int64
	MaxSMTPConnections      int
	MaxSMTPConnectionsPerIP int
	MaxSMTPRecipients       int
	MaxHTTPRequests         int
	MetricsEnabled          bool
	HTTPAccessLogMode       string
	HTTPLogHeaders          []string
}

func LoadConfig() (Config, error) {
	ttl, err := time.ParseDuration(env("MESSAGE_TTL", "1h"))
	if err != nil || ttl <= 0 {
		return Config{}, fmt.Errorf("MESSAGE_TTL must be a positive duration")
	}
	maxMessage, err := positiveInt(env("MAX_MESSAGE_BYTES", "2097152"))
	if err != nil {
		return Config{}, fmt.Errorf("MAX_MESSAGE_BYTES: %w", err)
	}
	maxStorage, err := positiveInt(env("MAX_STORAGE_BYTES", "21474836480"))
	if err != nil {
		return Config{}, fmt.Errorf("MAX_STORAGE_BYTES: %w", err)
	}
	maxSMTPConnections, err := positiveInt(env("SMTP_MAX_CONNECTIONS", strconv.Itoa(defaultMaxSMTPConnections)))
	if err != nil {
		return Config{}, fmt.Errorf("SMTP_MAX_CONNECTIONS: %w", err)
	}
	maxSMTPConnectionsPerIP, err := nonNegativeInt(env("SMTP_MAX_CONNECTIONS_PER_IP", "10"))
	if err != nil {
		return Config{}, fmt.Errorf("SMTP_MAX_CONNECTIONS_PER_IP: %w", err)
	}
	maxSMTPRecipients, err := positiveInt(env("SMTP_MAX_RECIPIENTS", strconv.Itoa(defaultMaxSMTPRecipients)))
	if err != nil {
		return Config{}, fmt.Errorf("SMTP_MAX_RECIPIENTS: %w", err)
	}
	tlsCertFile := strings.TrimSpace(os.Getenv("SMTP_TLS_CERT_FILE"))
	tlsKeyFile := strings.TrimSpace(os.Getenv("SMTP_TLS_KEY_FILE"))
	if (tlsCertFile == "") != (tlsKeyFile == "") {
		return Config{}, fmt.Errorf("SMTP_TLS_CERT_FILE and SMTP_TLS_KEY_FILE must be set together")
	}
	maxHTTPRequests, err := nonNegativeInt(env("HTTP_MAX_CONCURRENT_REQUESTS", "512"))
	if err != nil {
		return Config{}, fmt.Errorf("HTTP_MAX_CONCURRENT_REQUESTS: %w", err)
	}
	domain := strings.ToLower(strings.TrimSpace(os.Getenv("MAIL_DOMAIN")))
	if domain == "" || strings.ContainsAny(domain, "@ /\\") {
		return Config{}, fmt.Errorf("MAIL_DOMAIN must be a domain name")
	}
	logHeaders, err := parseHTTPLogHeaders(env("HTTP_LOG_HEADERS", "User-Agent"))
	if err != nil {
		return Config{}, fmt.Errorf("HTTP_LOG_HEADERS: %w", err)
	}
	accessLogMode := strings.ToLower(env("HTTP_ACCESS_LOG_MODE", "errors"))
	if accessLogMode != "all" && accessLogMode != "errors" && accessLogMode != "off" {
		return Config{}, fmt.Errorf("HTTP_ACCESS_LOG_MODE must be all, errors, or off")
	}
	return Config{MailDomain: domain, DataDir: env("DATA_DIR", "/data"), SMTPAddr: env("SMTP_ADDR", ":25"), SMTPTLSCertFile: tlsCertFile, SMTPTLSKeyFile: tlsKeyFile, HTTPAddr: env("HTTP_ADDR", ":8080"), MetricsAddr: env("METRICS_ADDR", "127.0.0.1:9090"), MessageTTL: ttl, MaxMessageBytes: maxMessage, MaxStorageBytes: maxStorage, MaxSMTPConnections: int(maxSMTPConnections), MaxSMTPConnectionsPerIP: maxSMTPConnectionsPerIP, MaxSMTPRecipients: int(maxSMTPRecipients), MaxHTTPRequests: maxHTTPRequests, MetricsEnabled: env("METRICS_ENABLED", "false") == "true", HTTPAccessLogMode: accessLogMode, HTTPLogHeaders: logHeaders}, nil
}

func parseHTTPLogHeaders(value string) ([]string, error) {
	var headers []string
	seen := make(map[string]struct{})
	for _, header := range strings.Split(value, ",") {
		header = strings.TrimSpace(header)
		if !validHeaderName(header) {
			return nil, fmt.Errorf("%q is not a valid HTTP header name", header)
		}
		canonical := http.CanonicalHeaderKey(header)
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		headers = append(headers, canonical)
	}
	return headers, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if !strings.ContainsRune("!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", character) {
			return false
		}
	}
	return true
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func positiveInt(value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("must be a positive integer")
	}
	return n, nil
}

func nonNegativeInt(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("must be a non-negative integer")
	}
	return n, nil
}
