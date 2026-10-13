package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the real autocert/ACME flow against a local CA. The CA validates
// ownership through tmpmail's HTTP handler and signs the actual submitted CSR.
func TestAutoTLSHTTPValidationAndRenewal(t *testing.T) {
	cfg := Config{AutoTLSWebEnabled: false, AutoTLSSMTPEnabled: true, MailDomain: "mail.test", WebDomain: "mail.test", ACMECacheDir: t.TempDir(), ACMEAcceptTOS: true}
	a, err := NewAutoTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	challengeServer := httptest.NewServer(a.HTTPHandler())
	defer challengeServer.Close()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var caURL string
	challengeURL := challengeServer.URL
	var mu sync.Mutex
	var validated bool
	var certificate []byte
	var serial atomic.Int64
	var validations atomic.Int64
	var nonce atomic.Int64
	ca := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", nonce.Add(1)))
		w.Header().Set("Content-Type", "application/json")
		reply := func(status int, value any) { w.WriteHeader(status); json.NewEncoder(w).Encode(value) }
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/directory":
			reply(200, map[string]any{"newNonce": caURL + "/nonce", "newAccount": caURL + "/account", "newOrder": caURL + "/new-order"})
		case "/nonce":
			w.WriteHeader(200)
		case "/account":
			w.Header().Set("Location", caURL+"/account/1")
			reply(201, map[string]any{"status": "valid", "orders": caURL + "/orders"})
		case "/new-order":
			validated = false
			certificate = nil
			w.Header().Set("Location", caURL+"/order")
			reply(201, map[string]any{"status": "pending", "identifiers": []any{map[string]string{"type": "dns", "value": "mail.test"}}, "authorizations": []string{caURL + "/auth"}, "finalize": caURL + "/finalize"})
		case "/auth":
			status := "pending"
			if validated {
				status = "valid"
			}
			reply(200, map[string]any{"status": status, "identifier": map[string]string{"type": "dns", "value": "mail.test"}, "challenges": []any{map[string]string{"type": "http-01", "url": caURL + "/challenge", "status": status, "token": "test-token"}}})
		case "/challenge":
			req, _ := http.NewRequest("GET", challengeURL+"/.well-known/acme-challenge/test-token", nil)
			req.Host = "mail.test"
			response, fetchErr := http.DefaultClient.Do(req)
			if fetchErr != nil {
				t.Errorf("fetch challenge: %v", fetchErr)
				reply(400, map[string]string{"detail": "challenge failed"})
				return
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 || len(body) <= len("test-token.") || string(body[:len("test-token.")]) != "test-token." {
				t.Errorf("challenge response: status=%d body=%s", response.StatusCode, body)
				reply(400, map[string]string{"detail": "challenge failed"})
				return
			}
			validated = true
			validations.Add(1)
			reply(200, map[string]string{"type": "http-01", "url": caURL + "/challenge", "status": "valid", "token": "test-token"})
		case "/order":
			status := "ready"
			if len(certificate) > 0 {
				status = "valid"
			}
			reply(200, map[string]any{"status": status, "authorizations": []string{caURL + "/auth"}, "finalize": caURL + "/finalize", "certificate": caURL + "/certificate"})
		case "/finalize":
			var jws struct{ Payload string }
			json.NewDecoder(r.Body).Decode(&jws)
			payload, decodeErr := base64.RawURLEncoding.DecodeString(jws.Payload)
			if decodeErr != nil {
				t.Errorf("decode JWS: %v", decodeErr)
				w.WriteHeader(400)
				return
			}
			var data struct{ CSR string }
			json.Unmarshal(payload, &data)
			csrDER, _ := base64.RawURLEncoding.DecodeString(data.CSR)
			csr, parseErr := x509.ParseCertificateRequest(csrDER)
			if parseErr != nil {
				t.Errorf("parse CSR: %v", parseErr)
				w.WriteHeader(400)
				return
			}
			if !validated || csr.CheckSignature() != nil {
				t.Error("invalid issuance request")
				w.WriteHeader(400)
				return
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(serial.Add(1)), DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			der, signErr := x509.CreateCertificate(rand.Reader, template, template, csr.PublicKey, caKey)
			if signErr != nil {
				t.Errorf("sign certificate: %v", signErr)
				w.WriteHeader(500)
				return
			}
			certificate = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
			reply(200, map[string]any{"status": "valid", "certificate": caURL + "/certificate", "finalize": caURL + "/finalize"})
		case "/certificate":
			w.Header().Set("Content-Type", "application/pem-certificate-chain")
			w.Write(certificate)
		default:
			t.Errorf("unexpected ACME endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer ca.Close()
	caURL = ca.URL
	a.manager.Client.DirectoryURL = ca.URL + "/directory"
	if err := a.refreshCertificate(context.Background(), "mail.test"); err != nil {
		t.Fatal(err)
	}
	cert, err := a.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.activate("mail.test", cert); err != nil {
		t.Fatal(err)
	}
	if validations.Load() != 1 || !a.Ready() {
		t.Fatal("HTTP validation did not activate certificate")
	}

	// Replace the persisted certificate with one nearing expiry. A fresh manager
	// should renew it automatically while continuing to serve the valid old pair.
	key := cert.PrivateKey.(*rsa.PrivateKey)
	oldTemplate := &x509.Certificate{SerialNumber: big.NewInt(99), DNSNames: []string{"mail.test"}, NotBefore: time.Now().Add(-89 * 24 * time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, oldTemplate, oldTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cached := append(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	if err := a.manager.Cache.Put(context.Background(), "mail.test+rsa", cached); err != nil {
		t.Fatal(err)
	}
	b, err := NewAutoTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b.manager.Client.DirectoryURL = ca.URL + "/directory"
	// The new manager must serve its own HTTP challenge tokens.
	renewalChallenge := httptest.NewServer(b.HTTPHandler())
	defer renewalChallenge.Close()
	mu.Lock()
	challengeURL = renewalChallenge.URL
	mu.Unlock()
	cachedCert, err := b.manager.GetCertificate(&tls.ClientHelloInfo{ServerName: "mail.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.activate("mail.test", cachedCert); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		next, err := b.manager.GetCertificate(&tls.ClientHelloInfo{ServerName: "mail.test"})
		if err != nil {
			t.Fatal(err)
		}
		if next.Leaf.SerialNumber.Int64() != 99 {
			if err := b.activate("mail.test", next); err != nil {
				t.Fatal(err)
			}
			if validations.Load() != 2 {
				t.Fatalf("renewal validations=%d", validations.Load())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("certificate was not renewed")
}
