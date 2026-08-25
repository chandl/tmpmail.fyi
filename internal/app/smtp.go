package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"
)

type SMTPServer struct {
	cfg      Config
	store    *Store
	listener net.Listener
	mu       sync.Mutex
}

func NewSMTPServer(cfg Config, store *Store) *SMTPServer { return &SMTPServer{cfg: cfg, store: store} }

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
		go s.handle(conn)
	}
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
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	defer w.Flush()
	write := func(code int, message string) { fmt.Fprintf(w, "%d %s\r\n", code, message); w.Flush() }
	write(220, "tmpmail ESMTP ready")
	var sender string
	var hasMail bool
	var recipients []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("SMTP read: %v", err)
			}
			return
		}
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
			recipients = append(recipients, address)
			write(250, "recipient accepted")
		case "DATA":
			if len(recipients) == 0 {
				write(503, "need RCPT TO first")
				continue
			}
			write(354, "end data with <CR><LF>.<CR><LF>")
			raw, err := readData(r, s.cfg.MaxMessageBytes)
			if err != nil {
				if strings.Contains(err.Error(), "too large") {
					_ = drainData(r)
				}
				write(552, "message too large or malformed")
				sender, hasMail, recipients = "", false, nil
				continue
			}
			stored := true
			for _, recipient := range recipients {
				message, err := s.store.Save(recipient, sender, raw)
				if err != nil {
					log.Printf("store message: %v", err)
					write(451, "temporary storage failure")
					stored = false
					break
				}
				log.Printf("received message id=%s recipient=%s bytes=%d", message.ID, message.Recipient, message.Size)
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
	var result []byte
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == ".\r\n" || line == ".\n" {
			return result, nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		if int64(len(result)+len(line)) > max {
			return nil, fmt.Errorf("too large")
		}
		result = append(result, line...)
	}
}

func drainData(r *bufio.Reader) error {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		if line == ".\r\n" || line == ".\n" {
			return nil
		}
	}
}
