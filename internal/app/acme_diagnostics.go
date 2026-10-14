package app

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
)

const acmeDiagnosticLimit = 64 << 10

// Capture an allowlisted summary as the client consumes the response. The
// original bytes and read/close behavior are preserved for the ACME client.
type acmeDiagnosticBody struct {
	io.ReadCloser
	mu        sync.Mutex
	captured  []byte
	truncated bool
	once      sync.Once
	endpoint  string
	logf      func(string, ...any)
}

func (b *acmeDiagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	remaining := acmeDiagnosticLimit - len(b.captured)
	if n > remaining {
		b.truncated = true
		remainingBytes := p[:remaining]
		b.captured = append(b.captured, remainingBytes...)
	} else {
		b.captured = append(b.captured, p[:n]...)
	}
	b.mu.Unlock()
	if err == io.EOF {
		b.report()
	}
	return n, err
}
func (b *acmeDiagnosticBody) Close() error { err := b.ReadCloser.Close(); b.report(); return err }

type acmeDiagnosticProblem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

func (b *acmeDiagnosticBody) report() {
	b.once.Do(func() {
		b.mu.Lock()
		captured := append([]byte(nil), b.captured...)
		truncated := b.truncated
		b.mu.Unlock()
		if truncated {
			return
		}
		var data struct {
			Status     string `json:"status"`
			Type       string `json:"type"`
			Detail     string `json:"detail"`
			Identifier struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"identifier"`
			Error      *acmeDiagnosticProblem `json:"error"`
			Challenges []struct {
				Type   string                 `json:"type"`
				Status string                 `json:"status"`
				Error  *acmeDiagnosticProblem `json:"error"`
			} `json:"challenges"`
		}
		if json.Unmarshal(captured, &data) != nil {
			return
		}
		if data.Status != "" {
			b.logf("ACME state: endpoint=%s identifier=%q status=%q", b.endpoint, diagnosticText(data.Identifier.Value), diagnosticText(data.Status))
		}
		problem := func(p *acmeDiagnosticProblem, challenge string) {
			if p != nil {
				b.logf("ACME validation/error: endpoint=%s challenge=%q type=%q detail=%q", b.endpoint, diagnosticText(challenge), diagnosticText(p.Type), diagnosticText(p.Detail))
			}
		}
		if strings.HasPrefix(data.Type, "urn:ietf:params:acme:error:") {
			problem(&acmeDiagnosticProblem{Type: data.Type, Detail: data.Detail}, "")
		}
		problem(data.Error, data.Type)
		for _, challenge := range data.Challenges {
			b.logf("ACME challenge: endpoint=%s type=%q status=%q", b.endpoint, diagnosticText(challenge.Type), diagnosticText(challenge.Status))
			problem(challenge.Error, challenge.Type)
		}
	})
}
func diagnosticText(value string) string {
	text := []rune(value)
	if len(text) > 1000 {
		return string(text[:1000]) + "…"
	}
	return value
}
