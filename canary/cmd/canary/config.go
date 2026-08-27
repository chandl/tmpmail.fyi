package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type config struct {
	HTTPAddr   string
	Interval   time.Duration
	Timeout    time.Duration
	SMTPAddr   string
	APIURL     string
	MailDomain string
	From       string
}

func loadConfig() (config, error) {
	interval, err := positiveDuration("CANARY_INTERVAL", "1m")
	if err != nil {
		return config{}, err
	}
	timeout, err := positiveDuration("CANARY_TIMEOUT", "10s")
	if err != nil {
		return config{}, err
	}
	domain := strings.ToLower(strings.TrimSpace(os.Getenv("CANARY_MAIL_DOMAIN")))
	if domain == "" || strings.ContainsAny(domain, "@ /\\") {
		return config{}, fmt.Errorf("CANARY_MAIL_DOMAIN must be a domain name")
	}
	return config{
		HTTPAddr:   env("CANARY_HTTP_ADDR", ":8081"),
		Interval:   interval,
		Timeout:    timeout,
		SMTPAddr:   env("CANARY_SMTP_ADDR", "tmpmail:2525"),
		APIURL:     strings.TrimRight(env("CANARY_API_URL", "http://tmpmail:8080"), "/"),
		MailDomain: domain,
		From:       env("CANARY_FROM", "canary@monitor.invalid"),
	}, nil
}

func positiveDuration(key, fallback string) (time.Duration, error) {
	d, err := time.ParseDuration(env(key, fallback))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return d, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
