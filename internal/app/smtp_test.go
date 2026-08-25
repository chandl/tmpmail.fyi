package app

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestReadDataDotUnstuffs(t *testing.T) {
	got, err := readData(bufio.NewReader(bytes.NewBufferString("one\r\n..two\r\n.\r\n")), 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\r\n.two\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestReadSMTPLineRejectsOversizedInputWithoutNewline(t *testing.T) {
	input := bytes.Repeat([]byte("x"), maxSMTPCommandLineBytes+1)
	_, err := readSMTPLine(bufio.NewReader(bytes.NewReader(input)), maxSMTPCommandLineBytes)
	if !errors.Is(err, errSMTPLineTooLong) {
		t.Fatalf("expected errSMTPLineTooLong, got %v", err)
	}
}

func TestReadDataRejectsOversizedLine(t *testing.T) {
	input := append(bytes.Repeat([]byte("x"), maxSMTPDataLineBytes+1), '\n')
	_, err := readData(bufio.NewReader(bytes.NewReader(input)), int64(len(input)+1))
	if !errors.Is(err, errSMTPLineTooLong) {
		t.Fatalf("expected errSMTPLineTooLong, got %v", err)
	}
}

func TestReadDataRejectsOversizedMessage(t *testing.T) {
	_, err := readData(bufio.NewReader(bytes.NewBufferString("first\r\nsecond\r\n.\r\n")), 8)
	if !errors.Is(err, errMessageTooLarge) {
		t.Fatalf("expected errMessageTooLarge, got %v", err)
	}
}

func TestSMTPPerSourceConnectionLimit(t *testing.T) {
	server := NewSMTPServer(Config{}, nil)
	address := &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 2525}
	var source string
	for i := 0; i < maxSMTPConnectionsPerIP; i++ {
		var ok bool
		source, ok = server.acquireSource(address)
		if !ok {
			t.Fatalf("connection %d unexpectedly rejected", i+1)
		}
	}
	if _, ok := server.acquireSource(address); ok {
		t.Fatal("expected per-source connection limit")
	}
	for i := 0; i < maxSMTPConnectionsPerIP; i++ {
		server.releaseSource(source)
	}
	if _, ok := server.acquireSource(address); !ok {
		t.Fatal("expected released source slot to be reusable")
	}
}

func TestSMTPAcceptsOnlyConfiguredDomain(t *testing.T) {
	s := NewSMTPServer(Config{MailDomain: "mail.test"}, nil)
	if !s.accepts("inbox@mail.test") || s.accepts("inbox@elsewhere.test") {
		t.Fatal("domain validation failed")
	}
}

func TestSMTPDeliversMessageToInbox(t *testing.T) {
	store := testStore(t, time.Hour)
	server := NewSMTPServer(Config{
		MailDomain:      "mail.test",
		MaxMessageBytes: 1024 * 1024,
		MaxStorageBytes: 1024 * 1024,
	}, store)
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	go server.handle(serverConn)

	reader := bufio.NewReader(clientConn)
	readReply := func(want int) {
		t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if len(line) < 3 || line[:3] != fmt.Sprintf("%d", want) {
			t.Fatalf("wanted SMTP %d, got %q", want, line)
		}
	}
	writeCommand := func(command string, want int) {
		t.Helper()
		if _, err := fmt.Fprintf(clientConn, "%s\r\n", command); err != nil {
			t.Fatal(err)
		}
		readReply(want)
	}

	readReply(220)
	writeCommand("EHLO test-client", 250)
	writeCommand("MAIL FROM:<sender@example.org>", 250)
	writeCommand("RCPT TO:<build@mail.test>", 250)
	writeCommand("DATA", 354)
	if _, err := fmt.Fprint(clientConn, "From: sender@example.org\r\nSubject: integration works\r\n\r\nhello\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	readReply(250)
	writeCommand("QUIT", 221)

	messages, err := store.List("build@mail.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Subject != "integration works" {
		t.Fatalf("unexpected inbox contents: %#v", messages)
	}
}
