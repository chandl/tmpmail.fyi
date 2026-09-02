package app

import "testing"

func TestLoadConfigSMTPConnectionLimit(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSMTPConnections != defaultMaxSMTPConnections || cfg.MaxSMTPConnectionsPerIP != 10 {
		t.Fatalf("unexpected SMTP defaults: %#v", cfg)
	}

	t.Setenv("SMTP_MAX_CONNECTIONS", "250")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SMTP_MAX_CONNECTIONS_PER_IP", "25")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSMTPConnections != 250 || cfg.MaxSMTPConnectionsPerIP != 25 {
		t.Fatalf("unexpected configured limits: %#v", cfg)
	}
}

func TestLoadConfigRejectsInvalidSMTPConnectionLimit(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")
	t.Setenv("SMTP_MAX_CONNECTIONS", "0")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected non-positive SMTP_MAX_CONNECTIONS to be rejected")
	}
}

func TestLoadConfigRejectsInvalidSMTPPerIPConnectionLimit(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")
	t.Setenv("SMTP_MAX_CONNECTIONS_PER_IP", "-1")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected negative SMTP_MAX_CONNECTIONS_PER_IP to be rejected")
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
