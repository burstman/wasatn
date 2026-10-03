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
	if !cfg.Meta.SignupConfigured() {
		// Split from the webhook warning below: the webhook can be fully working
		// while signup is not, and an operator should not have to guess which
		// half of Meta is missing.
		log.Warn("embedded signup is disabled, so no WhatsApp number can be connected",
			"app_id_set", cfg.Meta.AppID != "",
			"app_secret_set", cfg.Meta.AppSecret != "",
			"config_id_set", cfg.Meta.FBConfigID != "",
			"hint", "set META_APP_ID, META_APP_SECRET and META_FB_CONFIG_ID",
		)
	}
	// Separate warning because a missing app secret and a missing verify token
	// break different halves of the webhook, and an operator debugging a
	// rejected delivery needs to know which.
	if cfg.Meta.AppSecret == "" || cfg.Meta.VerifyToken == "" {
		log.Warn("the meta webhook rejects every delivery while its credentials are unset",
			"app_secret_set", cfg.Meta.AppSecret != "",
			"verify_token_set", cfg.Meta.VerifyToken != "",
			"hint", "set META_APP_SECRET and META_VERIFY_TOKEN",
		)
	}
	if !cfg.Meta.GraphVersionSupported() {
		log.Warn("the configured cloud api version is older than embedded signup v4 requires",
			"configured", cfg.Meta.Version(),
			"recommended", config.DefaultGraphVersion,
			"hint", "raise META_GRAPH_VERSION, or signup will be refused by Meta",
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
