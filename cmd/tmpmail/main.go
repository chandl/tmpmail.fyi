package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/chandl/tmpmail.fyi/internal/app"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(os.Getenv("HTTP_ADDR")); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, err := app.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		log.Fatalf("create data directory: %v", err)
	}

	store, err := app.OpenStore(filepath.Join(cfg.DataDir, "mail.db"), filepath.Join(cfg.DataDir, "messages"), cfg)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.Cleanup(); err != nil {
		log.Fatalf("initial cleanup: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go store.RunCleanup(ctx, time.Minute)

	analytics := app.OpenAnalytics(cfg)
	defer analytics.Close()
	var automaticTLS *app.AutoTLS
	var provider app.CertificateProvider
	if cfg.AutoTLSEnabled {
		automaticTLS, err = app.NewAutoTLS(cfg)
		if err != nil {
			log.Fatalf("configure automatic TLS: %v", err)
		}
		provider = automaticTLS
	}
	smtpServer, err := app.NewSMTPServerWithCertificate(cfg, store, provider, analytics)
	if err != nil {
		log.Fatalf("configure SMTP server: %v", err)
	}
	applicationHandler := app.NewHTTPServer(cfg, store, analytics)
	httpServer := newHTTPServer(cfg.HTTPAddr, applicationHandler)
	var httpsServer *http.Server
	var httpListener, httpsListener net.Listener
	if automaticTLS != nil {
		httpServer.Handler = automaticTLS.HTTPHandler()
		httpsServer = newHTTPServer(cfg.HTTPSAddr, automaticTLS.HTTPSHandler(applicationHandler))
		httpsServer.TLSConfig = automaticTLS.HTTPSConfig()
		httpListener, err = net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			log.Fatalf("bind HTTP challenge listener: %v", err)
		}
		httpsListener, err = net.Listen("tcp", cfg.HTTPSAddr)
		if err != nil {
			httpListener.Close()
			log.Fatalf("bind HTTPS listener: %v", err)
		}
	}

	errCh := make(chan error, 5)
	go func() {
		if automaticTLS != nil {
			if err := automaticTLS.WaitReady(ctx); err != nil {
				return
			}
		}
		errCh <- smtpServer.ListenAndServe(ctx)
	}()
	if httpsServer != nil {
		go func() {
			log.Printf("HTTPS listening on %s", cfg.HTTPSAddr)
			err := httpsServer.ServeTLS(httpsListener, "", "")
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errCh <- err
		}()
		go automaticTLS.Run(ctx)
	}
	go func() {
		log.Printf("HTTP listening on %s", cfg.HTTPAddr)
		var err error
		if httpListener != nil {
			err = httpServer.Serve(httpListener)
		} else {
			err = httpServer.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	var metricsServer *http.Server
	if cfg.MetricsEnabled {
		metricsServer = newHTTPServer(cfg.MetricsAddr, app.NewMetricsServer())
		go func() {
			log.Printf("metrics listening on %s", cfg.MetricsAddr)
			err := metricsServer.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errCh <- err
		}()
	}

	var adminServer *http.Server
	if cfg.AdminEnabled {
		adminServer = newHTTPServer(cfg.AdminAddr, app.NewAdminServer(cfg, store, smtpServer, analytics))
		go func() {
			log.Printf("private admin listening on %s (no application authentication)", cfg.AdminAddr)
			err := adminServer.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errCh <- err
		}()
	}

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
	case err := <-errCh:
		if err != nil {
			log.Fatal(err)
		}
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
	if httpsServer != nil {
		if err := httpsServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTPS shutdown: %v", err)
		}
	}
	if metricsServer != nil {
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("metrics shutdown: %v", err)
		}
	}
	if adminServer != nil {
		if err := adminServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("admin shutdown: %v", err)
		}
	}
	if err := smtpServer.Close(); err != nil {
		log.Printf("SMTP shutdown: %v", err)
	}
	fmt.Println("bye")
}

func healthcheck(addr string) error {
	target, err := healthcheckURL(addr)
	if err != nil {
		return err
	}
	// Probe the local listener directly, even when the container has proxy
	// environment variables. A redirect is not a successful health response.
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Get(target)
	if err != nil {
		return fmt.Errorf("HTTP healthcheck: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("HTTP healthcheck: got status %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	return nil
}

func healthcheckURL(addr string) (string, error) {
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("healthcheck HTTP_ADDR: %w", err)
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/healthz"}).String(), nil
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}
