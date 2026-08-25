package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"
)

const (
	maxSMTPConnections      = 100
	maxSMTPConnectionsPerIP = 10
	maxSMTPCommandLineBytes = 8 << 10
	maxSMTPDataLineBytes    = 8 << 10
	maxSMTPRecipients       = 100
	smtpSessionTimeout      = 2 * time.Minute
)

var (
	errSMTPLineTooLong = errors.New("SMTP line too long")
	errMessageTooLarge = errors.New("message too large")
)

type SMTPServer struct {
	cfg      Config
	store    *Store
	listener net.Listener
	mu       sync.Mutex
	sessions chan struct{}
	sourceMu sync.Mutex
	sources  map[string]int
}

func NewSMTPServer(cfg Config, store *Store) *SMTPServer {
	return &SMTPServer{cfg: cfg, store: store, sessions: make(chan struct{}, maxSMTPConnections), sources: make(map[string]int)}
}

func (s *SMTPServer) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.SMTPAddr)
	if err != nil {
		return fmt.Errorf("SMTP listen %s: %w", s.cfg.SMTPAddr, err)
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	log.Printf("SMTP listening on %s for %s", s.cfg.SMTPAddr, s.cfg.MailDomain)
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		select {
		case s.sessions <- struct{}{}:
			source, ok := s.acquireSource(conn.RemoteAddr())
			if !ok {
				<-s.sessions
				s.rejectConnection(conn, "per_ip_limit")
				continue
			}
			go func(conn net.Conn, source string) {
				defer func() {
					s.releaseSource(source)
					<-s.sessions
					if s.cfg.MetricsEnabled {
						smtpConnections.Dec()
					}
				}()
				if s.cfg.MetricsEnabled {
					smtpConnections.Inc()
				}
				s.handle(conn)
			}(conn, source)
		default:
			s.rejectConnection(conn, "global_limit")
		}
	}
}

func (s *SMTPServer) rejectConnection(conn net.Conn, reason string) {
	if s.cfg.MetricsEnabled {
		smtpRejections.WithLabelValues(reason).Inc()
	}
	// Do not allow a connection flood to create unbounded goroutines.
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = io.WriteString(conn, "421 too many concurrent connections\r\n")
	_ = conn.Close()
}

func (s *SMTPServer) acquireSource(addr net.Addr) (string, bool) {
	source := sourceAddress(addr)
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	if s.sources[source] >= maxSMTPConnectionsPerIP {
		return source, false
	}
	s.sources[source]++
	return source, true
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

func (s *SMTPServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *SMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	refreshDeadline := func() { _ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout)) }
	refreshDeadline()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	defer w.Flush()
	write := func(code int, message string) { fmt.Fprintf(w, "%d %s\r\n", code, message); w.Flush() }
	write(220, "tmpmail ESMTP ready")
	var sender string
	var hasMail bool
	var recipients []string
	for {
		line, err := readSMTPLine(r, maxSMTPCommandLineBytes)
		if err != nil {
			if errors.Is(err, errSMTPLineTooLong) {
				write(500, "command line too long")
				if s.cfg.MetricsEnabled {
					smtpRejections.WithLabelValues("command_line_limit").Inc()
				}
			}
			if err != io.EOF {
				log.Printf("SMTP read: %v", err)
			}
			return
		}
		refreshDeadline()
		line = strings.TrimRight(line, "\r\n")
		parts := strings.SplitN(line, " ", 2)
		command := strings.ToUpper(parts[0])
		arg := ""
		if len(parts) == 2 {
			arg = parts[1]
		}
		switch command {
		case "EHLO", "HELO":
			write(250, "tmpmail")
		case "NOOP":
			write(250, "ok")
		case "RSET":
			sender, hasMail, recipients = "", false, nil
			write(250, "reset")
		case "QUIT":
			write(221, "bye")
			return
		case "MAIL":
			address, ok := smtpAddress(arg, "FROM:")
			if !ok {
				write(501, "invalid MAIL FROM")
				continue
			}
			sender, hasMail, recipients = address, true, nil
			write(250, "sender accepted")
		case "RCPT":
			if !hasMail {
				write(503, "need MAIL FROM first")
				continue
			}
			address, ok := smtpAddress(arg, "TO:")
			if !ok || !s.accepts(address) {
				write(550, "unknown recipient domain")
				continue
			}
			if len(recipients) >= maxSMTPRecipients {
				write(452, "too many recipients")
				if s.cfg.MetricsEnabled {
					smtpRejections.WithLabelValues("recipient_limit").Inc()
				}
				continue
			}
			if !containsRecipient(recipients, address) {
				recipients = append(recipients, address)
			}
			write(250, "recipient accepted")
		case "DATA":
			if len(recipients) == 0 {
				write(503, "need RCPT TO first")
				continue
			}
			write(354, "end data with <CR><LF>.<CR><LF>")
			raw, err := readDataWithLimit(r, s.cfg.MaxMessageBytes, refreshDeadline)
			if err != nil {
				if errors.Is(err, errSMTPLineTooLong) {
					write(552, "message line too long")
					if s.cfg.MetricsEnabled {
						smtpRejections.WithLabelValues("data_line_limit").Inc()
					}
				} else {
					write(552, "message too large or malformed")
					if s.cfg.MetricsEnabled {
						smtpRejections.WithLabelValues("message_limit_or_malformed").Inc()
					}
				}
				if s.cfg.MetricsEnabled {
					smtpMessages.WithLabelValues("rejected").Inc()
				}
				// The remaining DATA stream is untrusted and unbounded; close rather
				// than draining it and allowing the client to retain this session.
				return
			}
			stored := true
			for _, recipient := range recipients {
				message, err := s.store.Save(recipient, sender, raw)
				if err != nil {
					log.Printf("store message: %v", err)
					write(451, "temporary storage failure")
					if s.cfg.MetricsEnabled {
						smtpMessages.WithLabelValues("failed").Inc()
					}
					stored = false
					break
				}
				log.Printf("[smtp receive] id=%s recipient=%s sender=%s bytes=%d", message.ID, message.Recipient, sender, message.Size)
				if s.cfg.MetricsEnabled {
					smtpMessages.WithLabelValues("accepted").Inc()
					smtpMessageBytes.Add(float64(message.Size))
				}
			}
			if stored {
				write(250, "message accepted")
			}
			sender, hasMail, recipients = "", false, nil
		default:
			write(500, "command not recognized")
		}
	}
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

func smtpAddress(arg, prefix string) (string, bool) {
	if !strings.HasPrefix(strings.ToUpper(arg), prefix) {
		return "", false
	}
	value := strings.TrimSpace(arg[len(prefix):])
	if !strings.HasPrefix(value, "<") || !strings.HasSuffix(value, ">") {
		return "", false
	}
	value = strings.TrimSpace(value[1 : len(value)-1])
	if value == "" && prefix == "FROM:" {
		return "", true
	}
	parsed, err := mail.ParseAddress(value)
	return value, err == nil && parsed.Address == value
}

func readData(r *bufio.Reader, max int64) ([]byte, error) {
	return readDataWithLimit(r, max, nil)
}

func readDataWithLimit(r *bufio.Reader, max int64, onRead func()) ([]byte, error) {
	var result []byte
	for {
		line, err := readSMTPLine(r, maxSMTPDataLineBytes)
		if err != nil {
			return nil, err
		}
		if onRead != nil {
			onRead()
		}
		if line == ".\r\n" || line == ".\n" {
			return result, nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		if int64(len(result)+len(line)) > max {
			return nil, errMessageTooLarge
		}
		result = append(result, line...)
	}
}

// readSMTPLine bounds allocation while consuming an SMTP line. A line that
// exceeds the limit is fatal to the connection because the remainder is not a
// valid command boundary.
func readSMTPLine(r *bufio.Reader, max int) (string, error) {
	line := make([]byte, 0, min(max, 4096))
	for {
		fragment, err := r.ReadSlice('\n')
		if len(line)+len(fragment) > max {
			return "", errSMTPLineTooLong
		}
		line = append(line, fragment...)
		if err == nil {
			return string(line), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return "", err
	}
}
