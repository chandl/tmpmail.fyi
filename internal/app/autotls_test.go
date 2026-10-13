package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

func testAutoTLS(t *testing.T) *AutoTLS {
	t.Helper()
	a, err := NewAutoTLS(Config{AutoTLSWebEnabled: true, AutoTLSSMTPEnabled: true, MailDomain: "mail.test", WebDomain: "web.test", ACMECacheDir: t.TempDir(), ACMEAcceptTOS: true})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAutoTLSRedirectAndReadiness(t *testing.T) {
	a := testAutoTLS(t)
	handler := a.HTTPHandler()
	for _, tt := range []struct {
		target   string
		status   int
		location string
	}{
		{"http://web.test/inbox?inbox=hello", http.StatusServiceUnavailable, ""},
		{"http://mail.test/", http.StatusServiceUnavailable, ""},
		{"http://evil.test/", http.StatusBadRequest, ""},
		{"http://127.0.0.1:8080/healthz", http.StatusServiceUnavailable, ""},
		{"http://web.test/.well-known/acme-challenge/missing", http.StatusNotFound, ""},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", tt.target, nil))
		if w.Code != tt.status || w.Header().Get("Location") != tt.location {
			t.Fatalf("%s: status=%d location=%q", tt.target, w.Code, w.Header().Get("Location"))
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, "http://web.test/inbox?inbox=hello", nil))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "5" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("pending %s: status=%d headers=%v", method, w.Code, w.Header())
		}
		if method == http.MethodGet && (!strings.Contains(w.Body.String(), "Waiting for a secure connection") || !strings.Contains(w.Body.String(), `http-equiv="refresh" content="5"`) || !strings.Contains(w.Body.String(), "web.test")) {
			t.Fatal("pending page missing status, domain or automatic refresh")
		}
		if method == http.MethodHead && w.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
	}
	for _, host := range a.domains {
		certPath, keyPath := writeTestCertificate(t, host, 1)
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.activate(host, &cert); err != nil {
			t.Fatal(err)
		}
	}
	if !a.Ready() {
		t.Fatal("expected ready certificates")
	}
	for _, tt := range []struct{ target, location string }{
		{"http://web.test/inbox?inbox=hello", "https://web.test/inbox?inbox=hello"},
		{"http://mail.test/", "https://web.test/"},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", tt.target, nil))
		if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != tt.location || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("ready redirect: status=%d headers=%v", w.Code, w.Header())
		}
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8080/healthz", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("health status: %d", w.Code)
	}
	if _, err := a.GetCertificate(nil); err != nil {
		t.Fatalf("SMTP without SNI: %v", err)
	}
	if _, err := a.HTTPSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.test"}); err == nil {
		t.Fatal("unconfigured HTTPS hostname accepted")
	}
	a.mu.Lock()
	a.certs[a.mailDomain].Leaf.NotAfter = time.Now().Add(-time.Minute)
	a.mu.Unlock()
	if a.Ready() {
		t.Fatal("expired certificate is ready")
	}
	if _, err := a.GetCertificate(nil); err == nil {
		t.Fatal("expired certificate served")
	}
	a.mu.Lock()
	a.certs[a.webDomain].Leaf.NotAfter = time.Now().Add(-time.Minute)
	a.mu.Unlock()
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://web.test/inbox?inbox=hello", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Location") != "" {
		t.Fatal("expired HTTPS certificate still redirects visitors")
	}

}

func TestAutoTLSReusesPersistentCacheWithoutACME(t *testing.T) {
	root := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"mail.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	// Autocert's RSA cache contains the private key followed by the chain.
	cache := autocert.DirCache(filepath.Join(root, "production"))
	if err := cache.Put(context.Background(), "mail.test+rsa", append(keyPEM, certPEM...)); err != nil {
		t.Fatal(err)
	}
	cfg := Config{AutoTLSWebEnabled: true, AutoTLSSMTPEnabled: true, MailDomain: "mail.test", WebDomain: "mail.test", ACMECacheDir: root, ACMEAcceptTOS: true}
	for i := 0; i < 2; i++ {
		a, err := NewAutoTLS(cfg)
		if err != nil {
			t.Fatal(err)
		}
		a.manager.Client.DirectoryURL = "http://127.0.0.1:1/unreachable"
		cert, err := a.manager.GetCertificate(&tls.ClientHelloInfo{ServerName: "mail.test"})
		if err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
		if err := a.activate("mail.test", cert); err != nil {
			t.Fatal(err)
		}
		if !a.Ready() {
			t.Fatal("cached certificate not ready")
		}
	}
	cfg.ACMEStaging = true
	staging, err := NewAutoTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staging.manager.Cache.Get(context.Background(), "mail.test+rsa"); err != autocert.ErrCacheMiss {
		t.Fatalf("staging shares production cache: %v", err)
	}
}

func TestAutomaticTLSConfig(t *testing.T) {
	t.Setenv("MAIL_DOMAIN", "MAIL.EXAMPLE.COM")
	t.Setenv("AUTO_TLS_ENABLED", "true")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("terms acceptance missing")
	}
	t.Setenv("ACME_ACCEPT_TOS", "true")
	t.Setenv("DATA_DIR", "/custom-data")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebDomain != "mail.example.com" || cfg.ACMECacheDir != "/custom-data/acme" {
		t.Fatalf("defaults: %+v", cfg)
	}
	for _, host := range []string{"*.example.com", "127.0.0.1", "localhost", "example.com:443", "a..com", "-a.example.com"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("WEB_DOMAIN", host)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid certificate hostname accepted")
			}
		})
	}
	t.Setenv("HTTPS_ADDR", ":8080")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("listener conflict accepted")
	}
	t.Setenv("HTTPS_ADDR", ":8443")
	t.Setenv("SMTP_TLS_CERT_FILE", "/cert")
	t.Setenv("SMTP_TLS_KEY_FILE", "/key")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("manual and automatic TLS accepted together")
	}
}

func TestAutomaticSMTPSTARTTLSWithoutSNI(t *testing.T) {
	a := testAutoTLS(t)
	certFile, keyFile := writeTestCertificate(t, "mail.test", 7)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.activate("mail.test", &cert); err != nil {
		t.Fatal(err)
	}
	server, err := NewSMTPServerWithCertificate(Config{MailDomain: "mail.test", MaxMessageBytes: 1024}, nil, a)
	if err != nil {
		t.Fatal(err)
	}
	client, reader := startSMTPServer(t, server)
	readSMTPResponse(t, reader, 220)
	writeSMTPCommand(t, client, "EHLO test-client")
	if !containsSMTPLine(readSMTPResponse(t, reader, 250), "STARTTLS") {
		t.Fatal("STARTTLS missing")
	}
	writeSMTPCommand(t, client, "STARTTLS")
	readSMTPResponse(t, reader, 220)
	tlsClient := tls.Client(client, &tls.Config{InsecureSkipVerify: true}) // Test certificate, deliberately no SNI.
	if err := tlsClient.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := tlsClient.ConnectionState().PeerCertificates[0].VerifyHostname("mail.test"); err != nil {
		t.Fatal(err)
	}
	reader = bufio.NewReader(tlsClient)
	writeSMTPCommand(t, tlsClient, "EHLO test-client")
	readSMTPResponse(t, reader, 250)
	writeSMTPCommand(t, tlsClient, "QUIT")
	readSMTPResponse(t, reader, 221)
}

func TestAutomaticHTTPSHandshakeAndHostPolicy(t *testing.T) {
	a := testAutoTLS(t)
	for _, host := range a.domains {
		certFile, keyFile := writeTestCertificate(t, host, 9)
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.activate(host, &cert); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewUnstartedServer(a.HTTPSHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	server.TLS = a.HTTPSConfig()
	server.StartTLS()
	defer server.Close()
	cert, err := a.certificate(a.webDomain)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	transport := &http.Transport{TLSClientConfig: &tls.Config{ServerName: a.webDomain, RootCAs: roots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	for _, host := range []string{a.webDomain, "evil.test"} {
		request, err := http.NewRequest("GET", server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = host
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		expected := http.StatusNoContent
		if host == "evil.test" {
			expected = http.StatusBadRequest
		}
		if response.StatusCode != expected {
			t.Fatalf("host %s: status %d", host, response.StatusCode)
		}
	}
	// An invalid replacement must leave the currently active certificate intact.
	if err := a.activate(a.webDomain, &tls.Certificate{}); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	current, err := a.certificate(a.webDomain)
	if err != nil || current != cert {
		t.Fatal("last valid certificate lost")
	}
}
