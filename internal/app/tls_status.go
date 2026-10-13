package app

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"time"
)

type TLSRuntimeSources struct {
	Automatic *AutoTLS
	ManualWeb *ManualHTTPS
}

type TLSServiceStatus struct {
	Mode              string     `json:"mode"`
	State             string     `json:"state"`
	Domain            string     `json:"domain"`
	Environment       string     `json:"environment,omitempty"`
	NotBefore         *time.Time `json:"notBefore,omitempty"`
	NotAfter          *time.Time `json:"notAfter,omitempty"`
	Issuer            string     `json:"issuer,omitempty"`
	DNSNames          []string   `json:"dnsNames,omitempty"`
	FingerprintSHA256 string     `json:"fingerprintSHA256,omitempty"`
	ExpiringSoon      bool       `json:"expiringSoon"`
}

type TLSOverviewStatus struct {
	Web  TLSServiceStatus `json:"web"`
	SMTP TLSServiceStatus `json:"smtp"`
}

// certificateSnapshot reads active state, including expired certificates, without
// invoking autocert's issuance or renewal API from an admin request.
func (a *AutoTLS) certificateSnapshot(domain string) *tls.Certificate {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.certs[domain]
}

func tlsServiceStatus(mode, domain string, cert *tls.Certificate, staging bool) TLSServiceStatus {
	status := TLSServiceStatus{Mode: mode, Domain: domain, State: "off"}
	if mode == "off" {
		return status
	}
	status.State = "pending"
	if mode == "automatic" {
		status.Environment = "production"
		if staging {
			status.Environment = "staging"
		}
	}
	if cert == nil || len(cert.Certificate) == 0 {
		return status
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		status.State = "unavailable"
		return status
	}
	before, after := leaf.NotBefore.UTC(), leaf.NotAfter.UTC()
	status.NotBefore = &before
	status.NotAfter = &after
	status.Issuer = leaf.Issuer.String()
	status.DNSNames = append([]string(nil), leaf.DNSNames...)
	fingerprint := sha256.Sum256(leaf.Raw)
	status.FingerprintSHA256 = hex.EncodeToString(fingerprint[:])
	now := time.Now()
	switch {
	case now.Before(before):
		status.State = "not_yet_valid"
	case !now.Before(after):
		status.State = "expired"
	default:
		status.State = "ready"
		status.ExpiringSoon = after.Sub(now) <= 7*24*time.Hour
	}
	return status
}

func tlsOverview(cfg Config, smtp *SMTPServer, sources TLSRuntimeSources) TLSOverviewStatus {
	webMode, smtpMode := "off", "off"
	var webCert, smtpCert *tls.Certificate
	if cfg.AutoTLSWebEnabled {
		webMode = "automatic"
		if sources.Automatic != nil {
			webCert = sources.Automatic.certificateSnapshot(cfg.WebDomain)
		}
	} else if cfg.HTTPSTLSCertFile != "" {
		webMode = "manual"
		if sources.ManualWeb != nil {
			webCert, _ = sources.ManualWeb.certificate.GetCertificate(nil)
		}
	}
	if cfg.AutoTLSSMTPEnabled {
		smtpMode = "automatic"
		if sources.Automatic != nil {
			smtpCert = sources.Automatic.certificateSnapshot(cfg.MailDomain)
		}
	} else if cfg.SMTPTLSCertFile != "" || smtp != nil && smtp.tlsCert != nil {
		smtpMode = "manual"
		if smtp != nil && smtp.tlsCert != nil {
			smtpCert, _ = smtp.tlsCert.GetCertificate(nil)
		}
	}
	return TLSOverviewStatus{Web: tlsServiceStatus(webMode, cfg.WebDomain, webCert, cfg.ACMEStaging), SMTP: tlsServiceStatus(smtpMode, cfg.MailDomain, smtpCert, cfg.ACMEStaging)}
}
