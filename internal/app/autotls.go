package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/net/idna"
)

// CertificateProvider supplies certificates without doing network IO in a handshake.
type CertificateProvider interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

type AutoTLS struct {
	manager               *autocert.Manager
	webDomain, mailDomain string
	domains               []string
	mu                    sync.RWMutex
	certs                 map[string]*tls.Certificate
	ready                 chan struct{}
	once                  sync.Once
}

func normalizeTLSHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || net.ParseIP(ascii) != nil || !strings.Contains(ascii, ".") || len(ascii) > 253 {
		return "", fmt.Errorf("invalid certificate hostname %q", host)
	}
	for _, label := range strings.Split(ascii, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid certificate hostname %q", host)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("invalid certificate hostname %q", host)
			}
		}
	}
	return ascii, nil
}

func NewAutoTLS(cfg Config) (*AutoTLS, error) {
	if !cfg.ACMEAcceptTOS {
		return nil, errors.New("automatic TLS requires ACME_ACCEPT_TOS=true")
	}
	environment := "production"
	directory := autocert.DefaultACMEDirectory
	if cfg.ACMEStaging {
		environment = "staging"
		directory = "https://acme-staging-v02.api.letsencrypt.org/directory"
	}
	cacheDir := filepath.Join(cfg.ACMECacheDir, environment)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create ACME cache: %w", err)
	}
	probe, err := os.CreateTemp(cacheDir, ".write-check-")
	if err != nil {
		return nil, fmt.Errorf("ACME cache is not writable: %w", err)
	}
	probe.Close()
	os.Remove(probe.Name())
	a := &AutoTLS{mailDomain: cfg.MailDomain, webDomain: cfg.WebDomain, certs: make(map[string]*tls.Certificate), ready: make(chan struct{})}
	a.domains = []string{cfg.MailDomain}
	if cfg.WebDomain != cfg.MailDomain {
		a.domains = append(a.domains, cfg.WebDomain)
	}
	a.manager = &autocert.Manager{Prompt: autocert.AcceptTOS, Cache: autocert.DirCache(cacheDir), Email: cfg.ACMEEmail, HostPolicy: autocert.HostWhitelist(a.domains...), Client: &acme.Client{DirectoryURL: directory}}
	// Register HTTP-01 before the first issuance. Autocert may also use TLS-ALPN-01.
	a.manager.HTTPHandler(nil)
	return a, nil
}

func (a *AutoTLS) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, host := range a.domains {
		cert := a.certs[host]
		if cert == nil || cert.Leaf == nil || time.Now().Before(cert.Leaf.NotBefore) || !time.Now().Before(cert.Leaf.NotAfter) {
			return false
		}
	}
	return true
}

func (a *AutoTLS) WaitReady(ctx context.Context) error {
	select {
	case <-a.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run activates cached certificates and starts autocert's renewal timers even on
// an idle deployment. RSA certificates are provisioned for broad SMTP support.
func (a *AutoTLS) Run(ctx context.Context) {
	delay := time.Minute
	for {
		for _, host := range a.domains {
			if ctx.Err() != nil {
				return
			}
			cert, err := a.manager.GetCertificate(&tls.ClientHelloInfo{ServerName: host})
			if err == nil {
				err = a.activate(host, cert)
			}
			if err != nil {
				log.Printf("automatic TLS for %s: %v", host, err)
			}
		}
		if a.Ready() {
			a.once.Do(func() { log.Printf("automatic TLS certificates ready"); close(a.ready) })
			delay = time.Minute
		} else if delay < time.Hour {
			delay *= 2
			if delay > time.Hour {
				delay = time.Hour
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (a *AutoTLS) activate(host string, cert *tls.Certificate) error {
	if cert == nil || len(cert.Certificate) == 0 {
		return errors.New("empty certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	if err := leaf.VerifyHostname(host); err != nil {
		return err
	}
	if time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return errors.New("certificate is outside its validity period")
	}
	copyCert := *cert
	copyCert.Leaf = leaf
	a.mu.Lock()
	previous := a.certs[host]
	a.certs[host] = &copyCert
	a.mu.Unlock()
	if host == a.mailDomain {
		setSMTPTLSCertificateExpiry(&copyCert)
	}
	if previous == nil || !previous.Leaf.NotAfter.Equal(leaf.NotAfter) {
		log.Printf("automatic TLS certificate active: domain=%s expires=%s", host, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return nil
}

func (a *AutoTLS) certificate(host string) (*tls.Certificate, error) {
	a.mu.RLock()
	cert := a.certs[host]
	a.mu.RUnlock()
	if cert == nil {
		return nil, errors.New("automatic TLS certificate is pending")
	}
	if time.Now().Before(cert.Leaf.NotBefore) || !time.Now().Before(cert.Leaf.NotAfter) {
		return nil, errors.New("automatic TLS certificate has expired or is not yet valid")
	}
	return cert, nil
}

// GetCertificate always selects the mail hostname, including clients without SNI.
func (a *AutoTLS) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return a.certificate(a.mailDomain)
}

func (a *AutoTLS) HTTPSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1", acme.ALPNProto}, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		host, err := normalizeTLSHost(hello.ServerName)
		if err != nil {
			return nil, err
		}
		allowed := false
		for _, domain := range a.domains {
			if host == domain {
				allowed = true
			}
		}
		if !allowed {
			return nil, errors.New("unconfigured TLS hostname")
		}
		if len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == acme.ALPNProto {
			return a.manager.GetCertificate(hello)
		}
		if host != a.webDomain {
			return nil, errors.New("hostname is not configured for HTTPS")
		}
		return a.certificate(host)
	}}
}

func (a *AutoTLS) HTTPSHandler(application http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		host, err := normalizeTLSHost(host)
		if err != nil || host != a.webDomain {
			http.Error(w, "unconfigured hostname", http.StatusBadRequest)
			return
		}
		if !a.Ready() {
			http.Error(w, "TLS provisioning pending or certificate expired", http.StatusServiceUnavailable)
			return
		}
		application.ServeHTTP(w, r)
	})
}

func (a *AutoTLS) HTTPHandler() http.Handler {
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if !a.Ready() {
				http.Error(w, "TLS provisioning pending or certificate expired", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "use HTTPS", http.StatusBadRequest)
			return
		}
		target := &url.URL{Scheme: "https", Host: a.webDomain, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, target.String(), http.StatusPermanentRedirect)
	})
	handler := a.manager.HTTPHandler(fallback)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		// Local container health checks use the listener's IP rather than a public name.
		if r.URL.Path == "/healthz" {
			fallback.ServeHTTP(w, r)
			return
		}
		host, err := normalizeTLSHost(host)
		allowed := false
		for _, domain := range a.domains {
			if host == domain {
				allowed = true
			}
		}
		if err != nil || !allowed {
			http.Error(w, "unconfigured hostname", http.StatusBadRequest)
			return
		}
		handler.ServeHTTP(w, r)
	})
}
