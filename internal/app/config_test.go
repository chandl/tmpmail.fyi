package app

import "testing"

func TestLoadConfigSMTPConnectionLimit(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSMTPConnections != defaultMaxSMTPConnections {
		t.Fatalf("default limit = %d, want %d", cfg.MaxSMTPConnections, defaultMaxSMTPConnections)
	}

	t.Setenv("SMTP_MAX_CONNECTIONS", "250")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSMTPConnections != 250 {
		t.Fatalf("configured limit = %d, want 250", cfg.MaxSMTPConnections)
	}
}

func TestLoadConfigRejectsInvalidSMTPConnectionLimit(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")
	t.Setenv("SMTP_MAX_CONNECTIONS", "0")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected non-positive SMTP_MAX_CONNECTIONS to be rejected")
	}
}

func TestLoadConfigHTTPProtectionDefaults(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxHTTPRequests != 512 || cfg.HTTPAccessLogMode != "errors" {
		t.Fatalf("unexpected HTTP defaults: %#v", cfg)
	}
}
