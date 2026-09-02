package app

import "testing"

func TestLoadConfigSMTPPerIPConnectionLimit(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSMTPConnectionsPerIP != defaultMaxSMTPConnectionsPerIP {
		t.Fatalf("default limit = %d, want %d", cfg.MaxSMTPConnectionsPerIP, defaultMaxSMTPConnectionsPerIP)
	}

	t.Setenv("SMTP_MAX_CONNECTIONS_PER_IP", "0")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSMTPConnectionsPerIP != 0 {
		t.Fatalf("disabled limit = %d, want 0", cfg.MaxSMTPConnectionsPerIP)
	}
}

func TestLoadConfigRejectsNegativeSMTPPerIPConnectionLimit(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")
	t.Setenv("SMTP_MAX_CONNECTIONS_PER_IP", "-1")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected negative SMTP_MAX_CONNECTIONS_PER_IP to be rejected")
	}
}
