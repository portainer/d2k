package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sethvargo/go-envconfig"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/config"
	"github.com/portainer/d2k/internal/logging"
	"github.com/portainer/d2k/internal/router"
)

// fileExists returns true if the file at path exists and is readable.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func main() {
	ctx := context.Background()

	var cfg config.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		log.Fatalf("unable to parse configuration: %s", err)
	}

	logger, err := logging.NewLogger(cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		log.Fatalf("unable to initialise logger: %s", err)
	}
	defer logger.Sync()

	logger.Infow("starting d2k",
		"namespace", cfg.Namespace,
		"port", cfg.Port,
		"swarm_mode", cfg.SwarmMode,
	)

	adapterOpts := &adapter.Options{
		Config: &cfg,
		Logger: logger,
	}

	a, err := adapter.NewKubernetesDockerAdapter(adapterOpts)
	if err != nil {
		logger.Fatalw("unable to create Kubernetes adapter", "error", err)
	}

	logger.Infow("connected to Kubernetes namespace", "namespace", cfg.Namespace)

	a.LogNFSStorageClasses(context.Background())

	handler := router.New(a, cfg.Namespace, cfg.SwarmMode, logger)

	// Detect TLS: if both cert and key files exist, listen on TLSPort with TLS.
	// The files are typically mounted from a Kubernetes Secret named d2k-tls.
	useTLS := fileExists(cfg.TLSCertFile) && fileExists(cfg.TLSKeyFile)

	var listenAddr string
	if useTLS {
		listenAddr = fmt.Sprintf(":%d", cfg.TLSPort)
	} else {
		listenAddr = fmt.Sprintf(":%d", cfg.Port)
	}

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      handler,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0, // 0 = no timeout; needed for streaming log endpoints
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGTERM / SIGINT.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		if useTLS {
			cert, tlsErr := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
			if tlsErr != nil {
				logger.Fatalw("unable to load TLS certificate", "error", tlsErr)
			}
			srv.TLSConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			}
			logger.Infow("d2k listening with TLS", "addr", srv.Addr, "cert", cfg.TLSCertFile)
			if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				logger.Fatalw("server error", "error", err)
			}
		} else {
			logger.Warnw("TLS not configured — listening without TLS", "addr", srv.Addr,
				"hint", "mount a Kubernetes Secret named d2k-tls with tls.crt and tls.key to enable TLS")
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Fatalw("server error", "error", err)
			}
		}
	}()

	<-quit
	logger.Info("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Errorw("graceful shutdown failed", "error", err)
	}

	logger.Info("d2k stopped")
}
