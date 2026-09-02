package app

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSMTPUsesConfigurableGlobalConnectionLimit(t *testing.T) {
	server := mustNewSMTPServer(t, Config{MaxSMTPConnections: 17}, nil)
	if got := cap(server.sessions); got != 17 {
		t.Fatalf("global connection limit = %d, want 17", got)
	}
}

func TestSMTPEnforcesConfigurablePerIPConnectionLimit(t *testing.T) {
	server := mustNewSMTPServer(t, Config{MaxSMTPConnectionsPerIP: 2}, nil)
	if !server.acquireSource("192.0.2.1") || !server.acquireSource("192.0.2.1") || server.acquireSource("192.0.2.1") {
		t.Fatal("expected source admission limit to be enforced")
	}
	server.releaseSource("192.0.2.1")
	if !server.acquireSource("192.0.2.1") {
		t.Fatal("expected released source slot to be reusable")
	}
}

func TestSMTPAcceptsOnlyConfiguredDomain(t *testing.T) {
	server := mustNewSMTPServer(t, Config{MailDomain: "mail.test"}, nil)
	if !server.accepts("inbox@mail.test") || server.accepts("inbox@elsewhere.test") {
		t.Fatal("domain validation failed")
	}
}

func TestSMTPDeliversMessageToInbox(t *testing.T) {
	store := testStore(t, time.Hour)
	server := mustNewSMTPServer(t, Config{MailDomain: "mail.test", MaxMessageBytes: 1024 * 1024, MaxStorageBytes: 1024 * 1024, MetricsEnabled: true}, store)
	client, reader := startSMTPServer(t, server)

	readSMTPResponse(t, reader, 220)
	writeSMTPCommand(t, client, "EHLO test-client")
	readSMTPResponse(t, reader, 250)
	writeSMTPCommand(t, client, "MAIL FROM:<sender@example.org>")
	readSMTPResponse(t, reader, 250)
	writeSMTPCommand(t, client, "RCPT TO:<build@mail.test>")
	readSMTPResponse(t, reader, 250)
	writeSMTPCommand(t, client, "DATA")
	readSMTPResponse(t, reader, 354)
	if _, err := fmt.Fprint(client, "From: sender@example.org\r\nSubject: integration works\r\n\r\nhello\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	readSMTPResponse(t, reader, 250)
	writeSMTPCommand(t, client, "QUIT")
	readSMTPResponse(t, reader, 221)

	messages, err := store.List("build@mail.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Subject != "integration works" {
		t.Fatalf("unexpected inbox contents: %#v", messages)
	}
	metrics := httptest.NewRecorder()
	NewMetricsServer().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metrics.Body.String(), "tmpmail_smtp_delivery_duration_seconds") {
		t.Fatal("expected SMTP delivery duration metric")
	}
}

func TestSMTPLibraryEnforcesMessageSizeLimit(t *testing.T) {
	store := testStore(t, time.Hour)
	server := mustNewSMTPServer(t, Config{MailDomain: "mail.test", MaxMessageBytes: 8, MaxStorageBytes: 1024}, store)
	client, reader := startSMTPServer(t, server)

	readSMTPResponse(t, reader, 220)
	writeSMTPCommand(t, client, "EHLO client")
	readSMTPResponse(t, reader, 250)
	writeSMTPCommand(t, client, "MAIL FROM:<sender@example.org>")
	readSMTPResponse(t, reader, 250)
	writeSMTPCommand(t, client, "RCPT TO:<build@mail.test>")
	readSMTPResponse(t, reader, 250)
	writeSMTPCommand(t, client, "DATA")
	readSMTPResponse(t, reader, 354)
	if _, err := fmt.Fprint(client, "subject\r\nbody\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	readSMTPResponse(t, reader, 552)
}

func TestSMTPSTARTTLS(t *testing.T) {
	certFile, keyFile := writeTestCertificate(t, "mail.test", 1)
	server := mustNewSMTPServer(t, Config{MailDomain: "mail.test", SMTPTLSCertFile: certFile, SMTPTLSKeyFile: keyFile, MaxMessageBytes: 1024}, nil)
	client, reader := startSMTPServer(t, server)

	readSMTPResponse(t, reader, 220)
	writeSMTPCommand(t, client, "EHLO test-client")
	capabilities := readSMTPResponse(t, reader, 250)
	if !containsSMTPLine(capabilities, "STARTTLS") {
		t.Fatalf("expected STARTTLS capability, got %#v", capabilities)
	}
	writeSMTPCommand(t, client, "STARTTLS")
	readSMTPResponse(t, reader, 220)

	tlsClient := tls.Client(client, &tls.Config{ServerName: "mail.test", InsecureSkipVerify: true}) // test-only certificate
	if err := tlsClient.Handshake(); err != nil {
		t.Fatal(err)
	}
	reader = bufio.NewReader(tlsClient)
	writeSMTPCommand(t, tlsClient, "MAIL FROM:<sender@example.org>")
	readSMTPResponse(t, reader, 502)
	writeSMTPCommand(t, tlsClient, "EHLO test-client")
	capabilities = readSMTPResponse(t, reader, 250)
	if containsSMTPLine(capabilities, "STARTTLS") {
		t.Fatalf("STARTTLS should not be advertised after upgrade: %#v", capabilities)
	}
	writeSMTPCommand(t, tlsClient, "MAIL FROM:<sender@example.org>")
	readSMTPResponse(t, reader, 250)
	writeSMTPCommand(t, tlsClient, "QUIT")
	readSMTPResponse(t, reader, 221)
}

func TestTLSCertificateReloaderReloadsAtomically(t *testing.T) {
	certFile, keyFile := writeTestCertificate(t, "mail.test", 1)
	reloader, err := newTLSCertificateReloader(certFile, keyFile, "mail.test")
	if err != nil {
		t.Fatal(err)
	}
	first, err := reloader.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	retained, err := reloader.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificate[0], retained.Certificate[0]) {
		t.Fatal("expected the previous certificate to survive an invalid reload")
	}
	secondCert, secondKey := writeTestCertificate(t, "mail.test", 2)
	certBytes, err := os.ReadFile(secondCert)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(secondKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, certBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := reloader.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Certificate[0], reloaded.Certificate[0]) {
		t.Fatal("expected updated certificate to be loaded")
	}
}

func TestSMTPRejectsInvalidTLSConfiguration(t *testing.T) {
	if _, err := NewSMTPServer(Config{SMTPTLSCertFile: "missing", SMTPTLSKeyFile: "missing"}, nil); err == nil {
		t.Fatal("expected missing certificate files to be rejected")
	}
}

func startSMTPServer(t *testing.T, server *SMTPServer) (net.Conn, *bufio.Reader) {
	t.Helper()
	server.server.ErrorLog = log.New(io.Discard, "", 0)
	serverConn, clientConn := net.Pipe()
	listener := &singleConnectionListener{conn: serverConn, closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- server.server.Serve(&smtpAdmissionListener{Listener: listener, server: server}) }()
	t.Cleanup(func() {
		_ = clientConn.Close()
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return clientConn, bufio.NewReader(clientConn)
}

func readSMTPResponse(t *testing.T, reader *bufio.Reader, want int) []string {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if len(line) < 4 {
		t.Fatalf("malformed SMTP response %q", line)
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil || code != want {
		t.Fatalf("wanted SMTP %d, got %q", want, line)
	}
	lines := []string{strings.TrimRight(line[4:], "\r\n")}
	for line[3] == '-' {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if len(line) < 4 || line[:3] != fmt.Sprintf("%03d", want) {
			t.Fatalf("malformed SMTP multi-line response %q", line)
		}
		lines = append(lines, strings.TrimRight(line[4:], "\r\n"))
	}
	return lines
}

func writeSMTPCommand(t *testing.T, conn net.Conn, command string) {
	t.Helper()
	if _, err := fmt.Fprintf(conn, "%s\r\n", command); err != nil {
		t.Fatal(err)
	}
}

func containsSMTPLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

type singleConnectionListener struct {
	conn     net.Conn
	accepted bool
	closed   chan struct{}
	close    sync.Once
}

func (l *singleConnectionListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *singleConnectionListener) Close() error {
	l.close.Do(func() { close(l.closed) })
	return nil
}

func (l *singleConnectionListener) Addr() net.Addr { return pipeAddr("smtp-test") }

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

func mustNewSMTPServer(t *testing.T, cfg Config, store *Store) *SMTPServer {
	t.Helper()
	server, err := NewSMTPServer(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func writeTestCertificate(t *testing.T, name string, serial int64) (string, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
