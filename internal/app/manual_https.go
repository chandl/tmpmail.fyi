package app

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// ManualHTTPS shares SMTP's validated, last-valid-pair certificate reloader.
type ManualHTTPS struct {
	domain      string
	certificate *tlsCertificateReloader
}

func NewManualHTTPS(cfg Config) (*ManualHTTPS, error) {
	certificate, err := newTLSCertificateReloader(cfg.HTTPSTLSCertFile, cfg.HTTPSTLSKeyFile, cfg.WebDomain, nil)
	if err != nil {
		return nil, fmt.Errorf("load HTTPS TLS certificate: %w", err)
	}
	m := &ManualHTTPS{domain: cfg.WebDomain, certificate: certificate}
	if _, err := m.GetCertificate(nil); err != nil {
		return nil, err
	}
	log.Printf("manual HTTPS TLS configured: domain=%s expires=%s", cfg.WebDomain, certificate.cert.Leaf.NotAfter.UTC().Format(time.RFC3339))
	return m, nil
}

func (m *ManualHTTPS) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello != nil && hello.ServerName != "" {
		host, err := normalizeTLSHost(hello.ServerName)
		if err != nil || host != m.domain {
			return nil, errors.New("unconfigured HTTPS hostname")
		}
	}
	cert, err := m.certificate.GetCertificate(hello)
	if err != nil {
		return nil, err
	}
	if time.Now().Before(cert.Leaf.NotBefore) || !time.Now().Before(cert.Leaf.NotAfter) {
		return nil, errors.New("HTTPS TLS certificate is outside its validity period")
	}
	return cert, nil
}

func (m *ManualHTTPS) Ready() bool { _, err := m.GetCertificate(nil); return err == nil }
func (m *ManualHTTPS) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}, GetCertificate: m.GetCertificate}
}
func (m *ManualHTTPS) HTTPHandler() http.Handler {
	return webTLSHTTPFallback(m.domain, true, m.Ready, m.Ready, nil)
}
func (m *ManualHTTPS) HTTPSHandler(application http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		host, err := normalizeTLSHost(host)
		if err != nil || host != m.domain {
			http.Error(w, "unconfigured hostname", http.StatusBadRequest)
			return
		}
		if !m.Ready() {
			http.Error(w, "HTTPS certificate unavailable", http.StatusServiceUnavailable)
			return
		}
		application.ServeHTTP(w, r)
	})
}

// GuardHealth composes manual web readiness with an automatic SMTP health check.
func (m *ManualHTTPS) GuardHealth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && !m.Ready() {
			http.Error(w, "HTTPS certificate unavailable", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
