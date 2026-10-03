// Command worker runs the River job worker.
//
// It is a separate process from the web server so a slow or stuck send does not
// affect request latency, and so the two can be scaled independently.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/burstman/wasatn/internal/config"
	"github.com/burstman/wasatn/internal/db"
	"github.com/burstman/wasatn/internal/jobs"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil {
		log.Error("worker exited with error", "err", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	registry := jobs.NewRegistry()

	client, err := jobs.NewClient(pool, &cfg.River, registry.Bundle(), log.With("component", "river"))
	if err != nil {
		return err
	}

	// River refuses to start a client with no job kinds registered, and the job
	// kinds arrive with the campaign milestones. Say so plainly instead of
	// exiting with a River error that looks like a misconfiguration.
	if registry.Empty() {
		log.Info("no job kinds are registered yet, so there is nothing for the worker to run",
			"note", "campaign send and scheduling workers land in a later milestone",
		)
		return nil
	}

	log.Info("worker starting",
		"max_workers", cfg.River.MaxWorkers,
		"queues", cfg.River.Queues,
		"kinds", registry.Kinds(),
	)

	// Start blocks until ctx is cancelled, then drains in-flight jobs.
	if err := jobs.Run(ctx, client); err != nil {
		return err
	}

	log.Info("worker stopped")
	return nil
}
