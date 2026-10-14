package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestAutoTLSRefreshSchedulesAreIndependent(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "failing SMTP"
		if blocked {
			name = "stalled SMTP"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				old := &tls.Certificate{Leaf: &x509.Certificate{NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}}
				renewed := &tls.Certificate{Leaf: &x509.Certificate{NotBefore: old.Leaf.NotBefore, NotAfter: old.Leaf.NotAfter.Add(time.Hour)}}
				a := &AutoTLS{domains: []string{"web.test", "mail.test"}, certs: map[string]*tls.Certificate{"web.test": old}, ready: make(chan struct{})}
				var webAttempts, mailAttempts atomic.Int64
				var available atomic.Pointer[tls.Certificate]
				available.Store(old)
				done := make(chan struct{})
				go func() {
					defer close(done)
					a.run(ctx, func(ctx context.Context, host string) error {
						if host == "mail.test" {
							mailAttempts.Add(1)
							if blocked {
								<-ctx.Done()
							}
							return errors.New("SMTP provisioning failed")
						}
						webAttempts.Add(1)
						a.mu.Lock()
						a.certs[host] = available.Load()
						a.mu.Unlock()
						return nil
					})
				}()
				synctest.Wait()
				// Allow the failing hostname's backoff to reach its one-hour cap.
				time.Sleep(3*time.Hour + 30*time.Second)
				synctest.Wait()
				before := webAttempts.Load()
				available.Store(renewed)
				time.Sleep(time.Minute)
				synctest.Wait()
				if webAttempts.Load() != before+1 {
					t.Fatalf("web refresh attempts: before=%d after=%d", before, webAttempts.Load())
				}
				if cert, err := a.certificate("web.test"); err != nil || cert != renewed {
					t.Fatalf("web renewal was not adopted within a minute: %v", err)
				}
				if a.Ready() {
					t.Fatal("pending SMTP reported healthy")
				}
				if (!blocked && mailAttempts.Load() >= 20) || (blocked && mailAttempts.Load() != 1) {
					t.Fatalf("unexpected SMTP attempts: %d", mailAttempts.Load())
				}
				cancel()
				<-done
			})
		})
	}
}
