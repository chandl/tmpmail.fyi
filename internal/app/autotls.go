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
	webEnabled            bool
	smtpEnabled           bool
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
	a := &AutoTLS{mailDomain: cfg.MailDomain, webDomain: cfg.WebDomain, webEnabled: cfg.AutoTLSWebEnabled, smtpEnabled: cfg.AutoTLSSMTPEnabled, certs: make(map[string]*tls.Certificate), ready: make(chan struct{})}
	if cfg.AutoTLSWebEnabled {
		a.domains = append(a.domains, cfg.WebDomain)
	}
	if cfg.AutoTLSSMTPEnabled && (!cfg.AutoTLSWebEnabled || cfg.MailDomain != cfg.WebDomain) {
		a.domains = append(a.domains, cfg.MailDomain)
	}
	if len(a.domains) == 0 {
		return nil, errors.New("automatic TLS requires an enabled web or SMTP service")
	}
	a.manager = &autocert.Manager{Prompt: autocert.AcceptTOS, Cache: autocert.DirCache(cacheDir), Email: cfg.ACMEEmail, HostPolicy: autocert.HostWhitelist(a.domains...), Client: &acme.Client{DirectoryURL: directory, HTTPClient: &http.Client{Transport: &acmeLogTransport{base: http.DefaultTransport, debug: cfg.ACMEDebug, logf: log.Printf}}}}
	// Register HTTP-01 before the first issuance. Autocert may also use TLS-ALPN-01.
	a.manager.HTTPHandler(nil)
	log.Printf("automatic TLS configured: environment=%s web=%t smtp=%t domains=%s cache=%s debug=%t", environment, cfg.AutoTLSWebEnabled, cfg.AutoTLSSMTPEnabled, strings.Join(a.domains, ","), cacheDir, cfg.ACMEDebug)
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

func (a *AutoTLS) WebReady() bool { _, err := a.certificate(a.webDomain); return err == nil }

// SMTP startup only waits for its own certificate, independently of the UI.
func (a *AutoTLS) WaitSMTPReady(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := a.certificate(a.mailDomain); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
		var attempts sync.WaitGroup
		for _, host := range a.domains {
			if ctx.Err() != nil {
				return
			}
			attempts.Add(1)
			go func(host string) {
				defer attempts.Done()
				if err := a.refreshCertificate(ctx, host); err != nil {
					log.Printf("automatic TLS for %s: %v", host, err)
				}
			}(host)
		}
		attempts.Wait()

		if a.Ready() {
			a.once.Do(func() { log.Printf("automatic TLS certificates ready"); close(a.ready) })
			delay = time.Minute
		} else if delay < time.Hour {
			delay *= 2
			if delay > time.Hour {
				delay = time.Hour
			}
		}
		if !a.Ready() {
			log.Printf("automatic TLS not ready: retry_in=%s next_attempt=%s; check DNS and public ports 80/443 for all configured domains", delay, time.Now().Add(delay).UTC().Format(time.RFC3339))
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

// refreshCertificate reports progress while autocert waits for CA validation.
// Normal polling of an already active certificate stays quiet.
func (a *AutoTLS) refreshCertificate(ctx context.Context, host string) error {
	_, err := a.certificate(host)
	pending := err != nil
	started := time.Now()
	if pending {
		log.Printf("automatic TLS certificate lookup/issuance started: domain=%s", host)
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					log.Printf("automatic TLS still waiting for certificate: domain=%s elapsed=%s", host, time.Since(started).Round(time.Second))
				}
			}
		}()
	}
	cert, err := a.manager.GetCertificate(&tls.ClientHelloInfo{ServerName: host})
	if err == nil {
		err = a.activate(host, cert)
	}
	if pending {
		log.Printf("automatic TLS certificate attempt finished: domain=%s elapsed=%s success=%t", host, time.Since(started).Round(time.Millisecond), err == nil)
	}
	return err
}

// Trace only request metadata, never account keys, bodies, headers or queries.
// Failures are always visible, including autocert's background renewal requests.
type acmeLogTransport struct {
	base  http.RoundTripper
	debug bool
	logf  func(string, ...any)
}

func (t *acmeLogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	endpoint := req.URL.Host + req.URL.EscapedPath()
	started := time.Now()
	if t.debug {
		t.logf("ACME request started: method=%s endpoint=%s", req.Method, endpoint)
	}
	response, err := t.base.RoundTrip(req)
	elapsed := time.Since(started).Round(time.Millisecond)
	if err != nil {
		t.logf("ACME request failed: method=%s endpoint=%s elapsed=%s error=%v", req.Method, endpoint, elapsed, err)
	} else if t.debug || response.StatusCode >= http.StatusBadRequest {
		t.logf("ACME response: method=%s endpoint=%s status=%d elapsed=%s", req.Method, endpoint, response.StatusCode, elapsed)
	}
	return response, err
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
	if a.smtpEnabled && host == a.mailDomain {
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
		if !a.WebReady() {
			http.Error(w, "TLS provisioning pending or certificate expired", http.StatusServiceUnavailable)
			return
		}
		application.ServeHTTP(w, r)
	})
}

func (a *AutoTLS) HTTPHandler(application ...http.Handler) http.Handler {
	var appHandler http.Handler
	if len(application) != 0 {
		appHandler = application[0]
	}
	fallback := webTLSHTTPFallback(a.webDomain, a.webEnabled, a.WebReady, a.Ready, appHandler)
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
		if !a.webEnabled && !strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
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

// webTLSHTTPFallback presents provisioning state and redirects only when ready.
func webTLSHTTPFallback(domain string, enabled bool, ready, healthy func() bool, application http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if !healthy() {
				http.Error(w, "TLS provisioning pending or certificate expired", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !enabled {
			if application != nil {
				application.ServeHTTP(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "use HTTPS", http.StatusBadRequest)
			return
		}
		// Keep visitors on HTTP until HTTPS has a usable certificate. Health
		// checks and ACME challenge requests retain their own response paths.
		w.Header().Set("Cache-Control", "no-store")
		if !ready() {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Retry-After", "5")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
			w.WriteHeader(http.StatusServiceUnavailable)
			if r.Method != http.MethodHead {
				_ = uiTemplates.ExecuteTemplate(w, "tls-pending", struct {
					pageChrome
					Domain string
				}{newPageChrome(), domain})
			}
			return
		}
		target := &url.URL{Scheme: "https", Host: domain, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, target.String(), http.StatusPermanentRedirect)
	})
}
