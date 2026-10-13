package app

import (
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"
)

func statusCertificate(t *testing.T, domain string, before, after time.Time) *tls.Certificate {
	t.Helper()
	certPath, keyPath := writeTestCertificate(t, domain, 5)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(6), Subject: pkix.Name{CommonName: "Test issuer"}, DNSNames: []string{domain}, NotBefore: before, NotAfter: after}
	key := cert.PrivateKey.(crypto.Signer)
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert.Certificate = [][]byte{der}
	cert.Leaf = nil
	return &cert
}

func TestTLSServiceValidityStatus(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name          string
		before, after time.Time
		state         string
		warn          bool
	}{
		{"valid", now.Add(-time.Hour), now.Add(30 * 24 * time.Hour), "ready", false},
		{"near expiry", now.Add(-time.Hour), now.Add(2 * 24 * time.Hour), "ready", true},
		{"expired", now.Add(-time.Hour), now.Add(-time.Minute), "expired", false},
		{"future", now.Add(time.Hour), now.Add(30 * 24 * time.Hour), "not_yet_valid", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status := tlsServiceStatus("manual", "web.test", statusCertificate(t, "web.test", tt.before, tt.after), true)
			if status.State != tt.state || status.ExpiringSoon != tt.warn || status.Environment != "" {
				t.Fatalf("status=%+v", status)
			}
			if status.Issuer == "" || len(status.DNSNames) != 1 || len(status.FingerprintSHA256) != 64 || status.NotAfter == nil {
				t.Fatal("certificate metadata missing")
			}
		})
	}
	if status := tlsServiceStatus("off", "web.test", nil, false); status.State != "off" || status.NotAfter != nil {
		t.Fatalf("off status=%+v", status)
	}
}

func TestAdminAutomaticTLSStateAndRedaction(t *testing.T) {
	cfg := Config{AutoTLSWebEnabled: true, AutoTLSSMTPEnabled: true, WebDomain: "web.test", MailDomain: "mail.test", ACMEAcceptTOS: true, ACMEStaging: true, ACMECacheDir: t.TempDir(), ACMEEmail: "contact-secret", SMTPTLSKeyFile: "key-path-secret"}
	a, err := NewAutoTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A status request must remain local even if the ACME endpoint is unreachable.
	a.manager.Client.DirectoryURL = "http://127.0.0.1:1/unreachable"
	h := NewAdminServer(cfg, nil, nil, nil, TLSRuntimeSources{Automatic: a})
	var out struct {
		TLS TLSOverviewStatus `json:"tls"`
	}
	w := adminGet(t, h, "/api/status", &out)
	if out.TLS.Web.State != "pending" || out.TLS.SMTP.State != "pending" || out.TLS.Web.Environment != "staging" {
		t.Fatalf("initial state=%+v", out.TLS)
	}
	activateModeCertificate(t, a, "web.test")
	w = adminGet(t, h, "/api/status", &out)
	if out.TLS.Web.State != "ready" || out.TLS.SMTP.State != "pending" {
		t.Fatalf("independent state=%+v", out.TLS)
	}
	expired := statusCertificate(t, "mail.test", time.Now().Add(-time.Hour), time.Now().Add(-time.Minute))
	a.mu.Lock()
	a.certs["mail.test"] = expired
	a.mu.Unlock()
	w = adminGet(t, h, "/api/status", &out)
	if out.TLS.SMTP.State != "expired" || out.TLS.SMTP.NotAfter == nil {
		t.Fatalf("expiry metadata lost: %+v", out.TLS.SMTP)
	}
	for _, secret := range []string{"contact-secret", "key-path-secret", cfg.ACMECacheDir, "PRIVATE KEY"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("TLS status exposed %s", secret)
		}
	}
}

func TestAdminManualWebAndPlaintextSMTPStatus(t *testing.T) {
	certFile, keyFile := writeTestCertificate(t, "web.test", 1)
	cfg := Config{MailDomain: "mail.test", WebDomain: "web.test", HTTPSTLSCertFile: certFile, HTTPSTLSKeyFile: keyFile}
	m, err := NewManualHTTPS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := NewAdminServer(cfg, nil, nil, nil, TLSRuntimeSources{ManualWeb: m})
	var out struct {
		TLS TLSOverviewStatus `json:"tls"`
	}
	w := adminGet(t, h, "/api/status", &out)
	if out.TLS.Web.Mode != "manual" || out.TLS.Web.State != "ready" || out.TLS.SMTP.State != "off" {
		t.Fatalf("manual status=%+v", out.TLS)
	}
	if strings.Contains(w.Body.String(), certFile) || strings.Contains(w.Body.String(), keyFile) {
		t.Fatal("manual file paths exposed")
	}
	server, err := NewSMTPServer(Config{MailDomain: "web.test", SMTPTLSCertFile: certFile, SMTPTLSKeyFile: keyFile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MailDomain = "web.test"
	cfg.SMTPTLSCertFile = certFile
	cfg.SMTPTLSKeyFile = keyFile
	h = NewAdminServer(cfg, nil, server, nil, TLSRuntimeSources{ManualWeb: m})
	adminGet(t, h, "/api/status", &out)
	if out.TLS.SMTP.Mode != "manual" || out.TLS.SMTP.State != "ready" {
		t.Fatalf("manual SMTP status=%+v", out.TLS.SMTP)
	}
}
