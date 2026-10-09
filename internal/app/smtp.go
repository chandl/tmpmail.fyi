package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	smtp "github.com/emersion/go-smtp"
	"github.com/google/uuid"
)

const (
	defaultMaxSMTPConnections   = 100
	defaultMaxSMTPRecipients    = 5
	maxConcurrentSMTPRejections = 16
	maxSMTPCommandLineBytes     = 8 << 10
	smtpSessionTimeout          = 2 * time.Minute
)

type SMTPServer struct {
	analytics *Analytics
	cfg       Config
	store     *Store
	server    *smtp.Server
	sessions  chan struct{}
	rejects   chan struct{}
	sourceMu  sync.Mutex
	sources   map[string]int
	tlsCert   *tlsCertificateReloader
}

func NewSMTPServer(cfg Config, store *Store, analytics ...*Analytics) (*SMTPServer, error) {
	limit := cfg.MaxSMTPConnections
	if limit == 0 {
		limit = defaultMaxSMTPConnections
	}
	maxRecipients := cfg.MaxSMTPRecipients
	if maxRecipients == 0 {
		maxRecipients = defaultMaxSMTPRecipients
	}
	server := &SMTPServer{cfg: cfg, store: store, sessions: make(chan struct{}, limit), rejects: make(chan struct{}, maxConcurrentSMTPRejections), sources: make(map[string]int)}
	if len(analytics) > 0 && analytics[0] != nil && analytics[0].Snapshot().Enabled {
		server.analytics = analytics[0]
	}
	if cfg.SMTPTLSCertFile != "" {
		reloader, err := newTLSCertificateReloader(cfg.SMTPTLSCertFile, cfg.SMTPTLSKeyFile, cfg.MailDomain, setSMTPTLSCertificateExpiry)
		if err != nil {
			return nil, fmt.Errorf("load SMTP TLS certificate: %w", err)
		}
		server.tlsCert = reloader
	} else {
		smtpTLSCertificateNotAfter.Set(0)
	}
	protocolServer := smtp.NewServer(smtp.BackendFunc(func(conn *smtp.Conn) (smtp.Session, error) {
		if server.cfg.MetricsEnabled {
			_, tlsActive := conn.TLSConnectionState()
			smtpSessionsTotal.WithLabelValues(strconv.FormatBool(tlsActive)).Inc()
		}
		return &smtpSession{server: server, sourceIP: sourceAddress(conn.Conn().RemoteAddr())}, nil
	}))
	protocolServer.Domain = cfg.MailDomain
	protocolServer.MaxRecipients = maxRecipients
	protocolServer.MaxMessageBytes = cfg.MaxMessageBytes
	protocolServer.MaxLineLength = maxSMTPCommandLineBytes
	protocolServer.ReadTimeout = smtpSessionTimeout
	protocolServer.WriteTimeout = smtpSessionTimeout
	protocolServer.TLSConfig = server.tlsConfig()
	server.server = protocolServer
	return server, nil
}

// ListenAndServe delegates SMTP parsing, command ordering, DATA decoding, and
// STARTTLS state handling to emersion/go-smtp. Admission remains local so the
// per-source connection limit applies before the protocol server allocates a
// session.
func (s *SMTPServer) ListenAndServe(ctx context.Context) error {
	_ = ctx // Shutdown is driven by Close so go-smtp can close active sessions.
	listener, err := net.Listen("tcp", s.cfg.SMTPAddr)
	if err != nil {
		return fmt.Errorf("SMTP listen %s: %w", s.cfg.SMTPAddr, err)
	}
	log.Printf("SMTP listening on %s for %s (global_connection_limit=%d)", s.cfg.SMTPAddr, s.cfg.MailDomain, cap(s.sessions))
	return s.server.Serve(&smtpAdmissionListener{Listener: listener, server: s})
}

func (s *SMTPServer) Close() error {
	err := s.server.Close()
	if errors.Is(err, smtp.ErrServerClosed) {
		return nil
	}
	return err
}

type smtpAdmissionListener struct {
	net.Listener
	server *SMTPServer
}

func (l *smtpAdmissionListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.server.sessions <- struct{}{}:
			source := sourceAddress(conn.RemoteAddr())
			if !l.server.acquireSource(source) {
				<-l.server.sessions
				l.server.scheduleRejection(conn, "per_ip_limit")
				continue
			}
			if l.server.cfg.MetricsEnabled {
				smtpConnectionsTotal.WithLabelValues("accepted").Inc()
				smtpConnections.Inc()
			}
			started := time.Now()
			return &admittedSMTPConn{Conn: conn, release: func() {
				l.server.releaseSource(source)
				<-l.server.sessions
				if l.server.cfg.MetricsEnabled {
					smtpConnections.Dec()
					smtpSessionDuration.WithLabelValues("handled").Observe(time.Since(started).Seconds())
				}
			}}, nil
		default:
			l.server.scheduleRejection(conn, "global_limit")
		}
	}
}

type admittedSMTPConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *admittedSMTPConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func (s *SMTPServer) scheduleRejection(conn net.Conn, reason string) {
	if s.cfg.MetricsEnabled {
		smtpRejections.WithLabelValues(reason).Inc()
		smtpConnectionsTotal.WithLabelValues("rejected").Inc()
	}
	select {
	case s.rejects <- struct{}{}:
		go func() {
			defer func() { <-s.rejects }()
			started := time.Now()
			_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			if _, err := io.WriteString(conn, "421 4.3.2 service temporarily overloaded\r\n"); err == nil {
				s.recordRejection(sourceAddress(conn.RemoteAddr()), "", "", "CONNECT", 421, started)
			}
			_ = conn.Close()
		}()
	default:
		_ = conn.Close()
	}
}

func (s *SMTPServer) acquireSource(source string) bool {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	if s.cfg.MaxSMTPConnectionsPerIP > 0 && s.sources[source] >= s.cfg.MaxSMTPConnectionsPerIP {
		return false
	}
	s.sources[source]++
	return true
}

func (s *SMTPServer) releaseSource(source string) {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	if s.sources[source] <= 1 {
		delete(s.sources, source)
		return
	}
	s.sources[source]--
}

func sourceAddress(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err == nil {
		return host
	}
	return addr.String()
}

type smtpSession struct {
	server     *SMTPServer
	sourceIP   string
	sender     string
	recipients []string
}

func (s *SMTPServer) recordRejection(ip, sender, recipient, stage string, code int, started time.Time) {
	if s.analytics == nil {
		return
	}
	_, domain, _ := strings.Cut(sender, "@")
	s.analytics.Record(AnalyticsEvent{ID: uuid.NewString(), Kind: "smtpRejected", Timestamp: time.Now(), IP: ip, Sender: sender, SenderDomain: strings.ToLower(domain), Recipient: recipient, Route: stage, Status: code, DurationMS: float64(time.Since(started).Microseconds()) / 1000})
}

func (s *smtpSession) rejectData(code int, started time.Time) {
	for _, recipient := range s.recipients {
		s.server.recordRejection(s.sourceIP, s.sender, recipient, "DATA", code, started)
	}
}

func (s *smtpSession) Reset() { s.sender, s.recipients = "", nil }

func (s *smtpSession) Logout() error { return nil }

func (s *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	s.sender, s.recipients = from, nil
	return nil
}

func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	started := time.Now()
	if !s.server.accepts(to) {
		if s.server.cfg.MetricsEnabled {
			smtpRejections.WithLabelValues("recipient_domain").Inc()
		}
		s.server.recordRejection(s.sourceIP, s.sender, to, "RCPT TO", 550, started)
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "unknown recipient domain"}
	}
	if !containsRecipient(s.recipients, to) {
		s.recipients = append(s.recipients, to)
	}
	return nil
}

func (s *smtpSession) Data(r io.Reader) error {
	deliveryStarted := time.Now()
	observeDelivery := func(err error) {
		if s.server.cfg.MetricsEnabled {
			smtpDeliveryDuration.WithLabelValues(metricResult(err)).Observe(time.Since(deliveryStarted).Seconds())
		}
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		if s.server.cfg.MetricsEnabled {
			smtpMessages.WithLabelValues("rejected").Inc()
			if errors.Is(err, smtp.ErrDataTooLarge) {
				smtpRejections.WithLabelValues("message_too_large").Inc()
			} else {
				smtpRejections.WithLabelValues("message_read_failed").Inc()
			}
		}
		observeDelivery(err)
		if errors.Is(err, smtp.ErrDataTooLarge) {
			s.rejectData(552, deliveryStarted)
			return smtp.ErrDataTooLarge
		}
		s.rejectData(554, deliveryStarted)
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 0, 0}, Message: "message read failed"}
	}
	messages, err := s.server.store.Save(s.recipients, s.sender, raw)
	if err != nil {
		log.Printf("store message: %v", err)
		if s.server.cfg.MetricsEnabled {
			smtpMessages.WithLabelValues("failed").Inc()
		}
		observeDelivery(err)
		s.rejectData(451, deliveryStarted)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "temporary storage failure"}
	}
	deliveryMS := float64(time.Since(deliveryStarted).Microseconds()) / 1000
	for _, message := range messages {
		if s.server.analytics != nil {
			sender := boundedEventString(s.sender, 320)
			_, domain, _ := strings.Cut(sender, "@")
			s.server.analytics.Record(AnalyticsEvent{ID: uuid.NewString(), Kind: "delivery", Timestamp: message.Received, Recipient: boundedEventString(message.Recipient, 320), Sender: sender, SenderDomain: boundedEventString(strings.ToLower(domain), 253), IP: boundedEventString(s.sourceIP, 64), Size: message.Size, MessageID: message.ID, DurationMS: deliveryMS, DurationKnown: true})
		}
		log.Printf("[smtp receive] id=%s recipient=%s sender=%s source_ip=%s bytes=%d", message.ID, message.Recipient, s.sender, s.sourceIP, message.Size)
		if s.server.cfg.MetricsEnabled {
			smtpMessages.WithLabelValues("accepted").Inc()
			smtpMessageBytes.Add(float64(message.Size))
		}
	}
	observeDelivery(nil)
	return nil
}

func containsRecipient(recipients []string, address string) bool {
	for _, recipient := range recipients {
		if recipient == address {
			return true
		}
	}
	return false
}

func (s *SMTPServer) accepts(address string) bool {
	at := strings.LastIndex(address, "@")
	return at > 0 && strings.EqualFold(address[at+1:], s.cfg.MailDomain)
}

// tlsCertificateReloader retains the last valid certificate while a certificate
// manager atomically replaces the files in a shared read-only volume.
type tlsCertificateReloader struct {
	certFile   string
	keyFile    string
	serverName string
	onReload   func(*tls.Certificate)

	mu                sync.Mutex
	cert              *tls.Certificate
	state             tlsCertificateState
	failedReloadState tlsCertificateState
	hasFailedReload   bool
}

type tlsCertificateState struct {
	cert fileState
	key  fileState
}

type fileState struct {
	modTime time.Time
	size    int64
}

func newTLSCertificateReloader(certFile, keyFile, serverName string, onReload func(*tls.Certificate)) (*tlsCertificateReloader, error) {
	cert, state, err := loadTLSCertificate(certFile, keyFile, serverName)
	if err != nil {
		return nil, err
	}
	reloader := &tlsCertificateReloader{certFile: certFile, keyFile: keyFile, serverName: serverName, onReload: onReload, cert: cert, state: state}
	if reloader.onReload != nil {
		reloader.onReload(cert)
	}
	return reloader, nil
}

func (r *tlsCertificateReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := tlsCertificateFilesState(r.certFile, r.keyFile)
	if err != nil {
		return r.cert, nil
	}
	if state == r.state || (r.hasFailedReload && state == r.failedReloadState) {
		return r.cert, nil
	}
	cert, loadedState, err := loadTLSCertificate(r.certFile, r.keyFile, r.serverName)
	if err != nil {
		r.failedReloadState, r.hasFailedReload = state, true
		log.Printf("SMTP TLS certificate reload failed: %v", err)
		return r.cert, nil
	}
	r.cert, r.state, r.hasFailedReload = cert, loadedState, false
	if r.onReload != nil {
		r.onReload(cert)
	}
	log.Printf("SMTP TLS certificate reloaded")
	return r.cert, nil
}

func setSMTPTLSCertificateExpiry(cert *tls.Certificate) {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return
	}
	smtpTLSCertificateNotAfter.Set(float64(leaf.NotAfter.Unix()))
}

func loadTLSCertificate(certFile, keyFile, serverName string) (*tls.Certificate, tlsCertificateState, error) {
	before, err := tlsCertificateFilesState(certFile, keyFile)
	if err != nil {
		return nil, tlsCertificateState{}, err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, tlsCertificateState{}, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, tlsCertificateState{}, err
	}
	if err := leaf.VerifyHostname(serverName); err != nil {
		return nil, tlsCertificateState{}, fmt.Errorf("certificate does not cover %q: %w", serverName, err)
	}
	after, err := tlsCertificateFilesState(certFile, keyFile)
	if err != nil {
		return nil, tlsCertificateState{}, err
	}
	if before != after {
		return nil, tlsCertificateState{}, errors.New("certificate files changed while loading")
	}
	return &cert, after, nil
}

func tlsCertificateFilesState(certFile, keyFile string) (tlsCertificateState, error) {
	certInfo, err := os.Stat(certFile)
	if err != nil {
		return tlsCertificateState{}, err
	}
	keyInfo, err := os.Stat(keyFile)
	if err != nil {
		return tlsCertificateState{}, err
	}
	return tlsCertificateState{cert: fileState{modTime: certInfo.ModTime(), size: certInfo.Size()}, key: fileState{modTime: keyInfo.ModTime(), size: keyInfo.Size()}}, nil
}

func (s *SMTPServer) tlsConfig() *tls.Config {
	if s.tlsCert == nil {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.tlsCert.GetCertificate}
}

// SMTPRuntimeStatus reads live admission and certificate state independently of Prometheus.
type SMTPRuntimeStatus struct {
	ActiveConnections int        `json:"activeConnections"`
	ConnectionLimit   int        `json:"connectionLimit"`
	TLSEnabled        bool       `json:"tlsEnabled"`
	TLSNotAfter       *time.Time `json:"tlsNotAfter,omitempty"`
}

func (s *SMTPServer) RuntimeStatus() SMTPRuntimeStatus {
	status := SMTPRuntimeStatus{ActiveConnections: len(s.sessions), ConnectionLimit: cap(s.sessions), TLSEnabled: s.tlsCert != nil}
	if s.tlsCert != nil {
		// Use the same reload path as STARTTLS, retaining the last valid certificate.
		cert, _ := s.tlsCert.GetCertificate(nil)
		if cert != nil && len(cert.Certificate) > 0 {
			if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
				expiry := leaf.NotAfter.UTC()
				status.TLSNotAfter = &expiry
			}
		}
	}
	return status
}
