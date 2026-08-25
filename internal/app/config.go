package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MailDomain      string
	DataDir         string
	SMTPAddr        string
	HTTPAddr        string
	MessageTTL      time.Duration
	MaxMessageBytes int64
	MaxStorageBytes int64
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
	domain := strings.ToLower(strings.TrimSpace(os.Getenv("MAIL_DOMAIN")))
	if domain == "" || strings.ContainsAny(domain, "@ /\\") {
		return Config{}, fmt.Errorf("MAIL_DOMAIN must be a domain name")
	}
	return Config{domain, env("DATA_DIR", "/data"), env("SMTP_ADDR", ":25"), env("HTTP_ADDR", ":8080"), ttl, maxMessage, maxStorage}, nil
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
