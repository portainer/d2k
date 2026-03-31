package main

import (
	"context"
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

	handler := router.New(a, cfg.Namespace, logger)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      handler,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0, // 0 = no timeout; needed for streaming log endpoints
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGTERM / SIGINT.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		logger.Infow("d2k listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalw("server error", "error", err)
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
