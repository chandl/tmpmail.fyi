package app

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAutomaticTLSServiceOverrides(t *testing.T) {
	for _, tt := range []struct {
		name, global, web, smtp string
		wantWeb, wantSMTP       bool
	}{
		{"legacy both", "true", "", "", true, true},
		{"legacy off", "false", "", "", false, false},
		{"web only", "true", "", "false", true, false},
		{"SMTP only", "true", "false", "", false, true},
		{"both overridden off", "true", "false", "false", false, false},
		{"explicit web enabled", "false", "true", "", true, false},
		{"explicit SMTP enabled", "false", "", "true", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MAIL_DOMAIN", "mail.test")
			t.Setenv("WEB_DOMAIN", "web.test")
			t.Setenv("ACME_ACCEPT_TOS", "true")
			t.Setenv("AUTO_TLS_ENABLED", tt.global)
			t.Setenv("AUTO_TLS_WEB_ENABLED", tt.web)
			t.Setenv("AUTO_TLS_SMTP_ENABLED", tt.smtp)
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.AutoTLSWebEnabled != tt.wantWeb || cfg.AutoTLSSMTPEnabled != tt.wantSMTP || cfg.AutoTLSEnabled != (tt.wantWeb || tt.wantSMTP) {
				t.Fatalf("unexpected toggles: %+v", cfg)
			}
			if !cfg.AutoTLSEnabled {
				return
			}
			cfg.ACMECacheDir = t.TempDir()
			a, err := NewAutoTLS(cfg)
			if err != nil {
				t.Fatal(err)
			}
			expected := 0
			if tt.wantWeb {
				expected++
			}
			if tt.wantSMTP {
				expected++
			}
			if len(a.domains) != expected {
				t.Fatalf("domains=%v", a.domains)
			}
			for _, pair := range []struct {
				host    string
				enabled bool
			}{{"web.test", tt.wantWeb}, {"mail.test", tt.wantSMTP}} {
				err := a.manager.HostPolicy(context.Background(), pair.host)
				if (err == nil) != pair.enabled {
					t.Fatalf("certificate policy for %s: %v", pair.host, err)
				}
			}
			if !tt.wantSMTP {
				t.Setenv("SMTP_TLS_CERT_FILE", "/cert.pem")
				t.Setenv("SMTP_TLS_KEY_FILE", "/key.pem")
				if _, err := LoadConfig(); err != nil {
					t.Fatalf("manual SMTP with automatic web TLS: %v", err)
				}
			}
		})
	}
	for _, name := range []string{"AUTO_TLS_WEB_ENABLED", "AUTO_TLS_SMTP_ENABLED"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MAIL_DOMAIN", "mail.test")
			t.Setenv(name, "invalid")
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid toggle accepted")
			}
		})
	}
}

func activateModeCertificate(t *testing.T, a *AutoTLS, host string) {
	t.Helper()
	certPath, keyPath := writeTestCertificate(t, host, 1)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.activate(host, &cert); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticTLSIndependentReadiness(t *testing.T) {
	a := testAutoTLS(t)
	activateModeCertificate(t, a, a.webDomain)
	if a.Ready() {
		t.Fatal("SMTP unexpectedly ready")
	}
	application := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	w := httptest.NewRecorder()
	a.HTTPSHandler(application).ServeHTTP(w, httptest.NewRequest("GET", "https://web.test/", nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("SMTP blocks ready HTTPS: %d", w.Code)
	}
	w = httptest.NewRecorder()
	a.HTTPHandler().ServeHTTP(w, httptest.NewRequest("GET", "http://web.test/", nil))
	if w.Code != http.StatusPermanentRedirect {
		t.Fatalf("SMTP blocks HTTPS redirect: %d", w.Code)
	}
	b := testAutoTLS(t)
	activateModeCertificate(t, b, b.mailDomain)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.WaitSMTPReady(ctx); err != nil {
		t.Fatalf("web blocks ready SMTP: %v", err)
	}
}

func TestSMTPOnlyAutomaticTLSServesHTTPUI(t *testing.T) {
	a, err := NewAutoTLS(Config{AutoTLSSMTPEnabled: true, MailDomain: "mail.test", WebDomain: "web.test", ACMEAcceptTOS: true, ACMECacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	handler := a.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) }))
	for _, method := range []string{"GET", "POST"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, "http://web.test/", nil))
		if w.Code != http.StatusCreated || w.Header().Get("Location") != "" {
			t.Fatalf("HTTP UI %s: %d", method, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://mail.test/.well-known/acme-challenge/missing", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("SMTP challenge unavailable: %d", w.Code)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/healthz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("pending SMTP health: %d", w.Code)
	}
	activateModeCertificate(t, a, a.mailDomain)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/healthz", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("ready SMTP health: %d", w.Code)
	}
}
