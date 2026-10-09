package app

import "testing"

func TestLoadConfigSMTPConnectionLimit(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSMTPConnections != defaultMaxSMTPConnections || cfg.MaxSMTPConnectionsPerIP != 10 || cfg.MaxSMTPRecipients != defaultMaxSMTPRecipients {
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
	t.Setenv("SMTP_MAX_RECIPIENTS", "5")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSMTPConnections != 250 || cfg.MaxSMTPConnectionsPerIP != 25 || cfg.MaxSMTPRecipients != 5 {
		t.Fatalf("unexpected configured limits: %#v", cfg)
	}
}

func TestLoadConfigRejectsInvalidSMTPRecipientLimit(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")
	t.Setenv("SMTP_MAX_RECIPIENTS", "0")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected non-positive SMTP_MAX_RECIPIENTS to be rejected")
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

func TestLoadConfigRequiresSMTPTLSCertificateAndKeyTogether(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")
	t.Setenv("SMTP_TLS_CERT_FILE", "/cert.pem")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected unpaired SMTP TLS certificate setting to be rejected")
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

func TestLoadConfigAnalytics(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminEnabled || cfg.AnalyticsEnabled || cfg.AdminAddr != "127.0.0.1:8081" || cfg.AnalyticsEventTTL.Hours() != 720 || cfg.AnalyticsMaxStorageBytes != 1073741824 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	for _, setting := range []struct{ key, value string }{{"ANALYTICS_EVENT_TTL", "721h"}, {"ANALYTICS_EVENT_TTL", "0h"}, {"ANALYTICS_MAX_STORAGE_BYTES", "0"}, {"ANALYTICS_ENABLED", "perhaps"}, {"ADMIN_ENABLED", "perhaps"}} {
		t.Run(setting.key+setting.value, func(t *testing.T) {
			t.Setenv(setting.key, setting.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid analytics setting accepted")
			}
		})
	}
	t.Setenv("ANALYTICS_EVENT_TTL", "24h")
	t.Setenv("ADMIN_ENABLED", "true")
	t.Setenv("ANALYTICS_ENABLED", "true")
	cfg, err = LoadConfig()
	if err != nil || !cfg.AdminEnabled || !cfg.AnalyticsEnabled || cfg.AnalyticsEventTTL.Hours() != 24 {
		t.Fatalf("override: %+v %v", cfg, err)
	}
}
