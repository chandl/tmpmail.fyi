package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/chandl/tmpmail.fyi/internal/app"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:8080", 2*time.Second)
		if err != nil {
			log.Fatal(err)
		}
		_ = conn.Close()
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

	smtpServer, err := app.NewSMTPServer(cfg, store)
	if err != nil {
		log.Fatalf("configure SMTP server: %v", err)
	}
	httpServer := newHTTPServer(cfg.HTTPAddr, app.NewHTTPServer(cfg, store))

	errCh := make(chan error, 3)
	go func() { errCh <- smtpServer.ListenAndServe(ctx) }()
	go func() {
		log.Printf("HTTP listening on %s", cfg.HTTPAddr)
		err := httpServer.ListenAndServe()
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

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
	case err := <-errCh:
		if err != nil {
			log.Fatal(err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
	if metricsServer != nil {
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("metrics shutdown: %v", err)
		}
	}
	if err := smtpServer.Close(); err != nil {
		log.Printf("SMTP shutdown: %v", err)
	}
	fmt.Println("bye")
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
