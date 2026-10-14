package app

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

func TestManualHTTPSConfig(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "mail.test")
	t.Setenv("WEB_DOMAIN", "web.test")
	t.Setenv("AUTO_TLS_ENABLED", "false")
	t.Setenv("AUTO_TLS_WEB_ENABLED", "false")
	t.Setenv("AUTO_TLS_SMTP_ENABLED", "false")
	t.Setenv("HTTPS_TLS_CERT_FILE", "/cert")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("unpaired HTTPS cert accepted")
	}
	t.Setenv("HTTPS_TLS_KEY_FILE", "/key")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPSTLSCertFile != "/cert" || cfg.HTTPSTLSKeyFile != "/key" {
		t.Fatal("manual HTTPS configuration lost")
	}
	t.Setenv("AUTO_TLS_WEB_ENABLED", "true")
	t.Setenv("ACME_ACCEPT_TOS", "true")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("manual and automatic web TLS accepted together")
	}
	t.Setenv("AUTO_TLS_WEB_ENABLED", "false")
	t.Setenv("AUTO_TLS_SMTP_ENABLED", "true")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("manual web with automatic SMTP: %v", err)
	}
	t.Setenv("AUTO_TLS_SMTP_ENABLED", "false")
	t.Setenv("HTTPS_ADDR", ":8080")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("manual HTTPS listener collision accepted")
	}
}

func TestManualHTTPSReloadAndHandshake(t *testing.T) {
	certFile, keyFile := writeTestCertificate(t, "web.test", 1)
	cfg := Config{WebDomain: "web.test", HTTPSTLSCertFile: certFile, HTTPSTLSKeyFile: keyFile}
	m, err := NewManualHTTPS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	secondCertFile, secondKeyFile := writeTestCertificate(t, "web.test", 2)
	second, err := tls.LoadX509KeyPair(secondCertFile, secondKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(second.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(m.certificate.cert.Leaf)
	roots.AddCert(leaf)
	server := httptest.NewUnstartedServer(m.HTTPSHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	server.TLS = m.TLSConfig()
	server.StartTLS()
	defer server.Close()
	transport := &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{ServerName: "web.test", RootCAs: roots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	check := func(want int64) {
		t.Helper()
		req, err := http.NewRequest("GET", server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "web.test"
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent || response.TLS.PeerCertificates[0].SerialNumber.Int64() != want {
			t.Fatalf("unexpected HTTPS response: status=%d serial=%s", response.StatusCode, response.TLS.PeerCertificates[0].SerialNumber)
		}
	}
	check(1)
	certPEM, _ := os.ReadFile(secondCertFile)
	keyPEM, _ := os.ReadFile(secondKeyFile)
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	check(1) // Incomplete renewal keeps serving the previous key pair.
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	check(2)
	w := httptest.NewRecorder()
	m.HTTPHandler().ServeHTTP(w, httptest.NewRequest("GET", "http://web.test/?inbox=hello", nil))
	if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "https://web.test/?inbox=hello" {
		t.Fatalf("manual HTTPS redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
	cfg.WebDomain = "other.test"
	if _, err := NewManualHTTPS(cfg); err == nil {
		t.Fatal("wrong certificate hostname accepted")
	}
	m.certificate.cert.Leaf.NotAfter = time.Now().Add(-time.Minute)
	if m.Ready() {
		t.Fatal("expired manual certificate ready")
	}
	if _, err := m.GetCertificate(nil); err == nil {
		t.Fatal("expired manual certificate served")
	}
	w = httptest.NewRecorder()
	m.GuardHealth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/healthz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("expired manual certificate health check succeeded")
	}
}

func TestManualHTTPSWithAutomaticSMTP(t *testing.T) {
	certFile, keyFile := writeTestCertificate(t, "web.test", 1)
	cfg := Config{AutoTLSSMTPEnabled: true, MailDomain: "mail.test", WebDomain: "web.test", ACMEAcceptTOS: true, ACMECacheDir: t.TempDir(), HTTPSTLSCertFile: certFile, HTTPSTLSKeyFile: keyFile}
	m, err := NewManualHTTPS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAutoTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler := m.GuardHealth(a.HTTPHandler(m.HTTPHandler()))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://web.test/?inbox=hello", nil))
	if w.Code != http.StatusPermanentRedirect {
		t.Fatalf("pending SMTP blocks manual HTTPS redirect: %d", w.Code)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://mail.test/.well-known/acme-challenge/missing", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("manual HTTPS intercepted SMTP challenge: %d", w.Code)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/healthz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("pending automatic SMTP reported healthy")
	}
	activateModeCertificate(t, a, "mail.test")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1/healthz", nil))
	if w.Code != http.StatusNoContent {
		t.Fatal("mixed manual and automatic TLS did not become healthy")
	}
}

func TestManualHTTPSRetainsValidCertificateForInvalidDates(t *testing.T) {
	for _, future := range []bool{false, true} {
		name := "expired"
		if future {
			name = "future"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				certFile, keyFile := writeTestCertificate(t, "web.test", 1)
				cfg := Config{WebDomain: "web.test", HTTPSTLSCertFile: certFile, HTTPSTLSKeyFile: keyFile}
				m, err := NewManualHTTPS(cfg)
				if err != nil {
					t.Fatal(err)
				}
				original, err := m.GetCertificate(nil)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				before, after := now.Add(-2*time.Hour), now.Add(-time.Hour)
				if future {
					before, after = now.Add(time.Hour), now.Add(2*time.Hour)
				}
				replacement := statusCertificate(t, "web.test", before, after)
				writePair := func(cert *tls.Certificate) {
					t.Helper()
					key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				writePair(replacement)
				for i := 0; i < 2; i++ {
					got, err := m.GetCertificate(nil)
					if err != nil || got != original || !m.Ready() {
						t.Fatalf("invalid replacement displaced valid certificate: %v", err)
					}
				}
				if _, err := NewManualHTTPS(cfg); err == nil {
					t.Fatal("invalid initial certificate accepted")
				}
				if future {
					// The same files become usable when NotBefore is reached.
					time.Sleep(time.Hour)
					got, err := m.GetCertificate(nil)
					if err != nil || got == original || !m.Ready() {
						t.Fatalf("future replacement not adopted once valid: %v", err)
					}
				}
				valid := statusCertificate(t, "web.test", now.Add(-time.Hour), now.Add(3*time.Hour))
				writePair(valid)
				got, err := m.GetCertificate(nil)
				if err != nil || got == original || !got.Leaf.NotAfter.Equal(now.Add(3*time.Hour).Truncate(time.Second)) {
					t.Fatalf("valid replacement not adopted: %v", err)
				}
			})
		})
	}
}
