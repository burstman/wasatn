// Command server runs the WasaTN HTTP server.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/burstman/wasatn/internal/app"
	"github.com/burstman/wasatn/internal/config"
	"github.com/burstman/wasatn/internal/db"
	"github.com/burstman/wasatn/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "err", err.Error())
		os.Exit(1)
	}
}

func run() error {
	log := newLogger()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateForProduction(); err != nil {
		return err
	}
	if !cfg.Meta.MetaConfigured() {
		log.Warn("meta is not configured, so connecting a WhatsApp number is disabled",
			"hint", "set META_APP_ID and META_APP_SECRET",
		)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	// SuperKit's error handler is package-level state, so install it once.
	app.KitUseErrorHandler(web.ErrorHandler)

	application, err := app.New(cfg, log, pool)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening",
			"addr", cfg.HTTPAddr,
			"env", string(cfg.Env),
			"base_url", cfg.PublicBaseURL,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Give in-flight requests time to finish before closing the pool.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("server stopped")
	return nil
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("SUPERKIT_ENV") == "development" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
