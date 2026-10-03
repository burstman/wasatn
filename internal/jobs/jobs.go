// Package jobs wires the River job queue.
//
// River is Postgres-backed, so job rows live in the same database as the rest of
// the application and can be inserted transactionally with the data they act
// on. No Redis is involved.
//
// No job kinds are registered yet. Milestone 5 adds the campaign send jobs and
// milestone 6 adds scheduling and recurring campaigns.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/burstman/wasatn/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Queue names. Send work is kept on its own queue so a burst of campaign
// messages cannot starve maintenance jobs such as token refresh.
const (
	// QueueSend handles per-recipient WhatsApp sends.
	QueueSend = "campaign_send"
	// QueueSchedule handles campaign dispatch and recurring enqueues.
	QueueSchedule = "campaign_schedule"
	// QueueDefault is River's built-in fallback queue.
	QueueDefault = river.QueueDefault
)

// Client both enqueues and executes work. The web process uses it only to
// insert; the worker process calls Start.
type Client = river.Client[pgx.Tx]

// Registry is the set of job workers the worker process can run.
//
// It wraps River's bundle because River's registry cannot be inspected, and the
// worker needs to know whether it has anything to do: River refuses to start a
// client with no workers registered.
type Registry struct {
	bundle *river.Workers
	kinds  []string
}

// NewRegistry builds the registry and registers every job kind the app has.
// Job kinds arrive with the campaign milestones; until then it is empty.
func NewRegistry() *Registry {
	return &Registry{bundle: river.NewWorkers()}
}

// Bundle is River's worker registry, for NewClient and Run.
func (r *Registry) Bundle() *river.Workers { return r.bundle }

// Kinds lists the registered job kinds, for logging.
func (r *Registry) Kinds() []string { return slices.Clone(r.kinds) }

// Empty reports whether no job kind is registered.
func (r *Registry) Empty() bool { return len(r.kinds) == 0 }

// Register adds a job worker, recording its kind so Empty stays accurate.
func Register[T river.JobArgs](r *Registry, jobArgs T, worker river.Worker[T]) error {
	if err := river.AddWorkerSafely(r.bundle, worker); err != nil {
		return fmt.Errorf("jobs: register %s: %w", jobArgs.Kind(), err)
	}
	r.kinds = append(r.kinds, jobArgs.Kind())
	return nil
}

// NewWorkers returns an empty River worker registry.
func NewWorkers() *river.Workers {
	return river.NewWorkers()
}

// NewClient builds the River client with the given workers registered.
//
// workers may be nil for the web process, which never executes jobs. River
// rejects a client that sets Queues without a worker registry, so an empty one
// is substituted in that case: the web process can then insert jobs without
// being able to execute any.
func NewClient(pool *pgxpool.Pool, cfg *config.RiverConfig, workers *river.Workers, log *slog.Logger) (*Client, error) {
	if pool == nil {
		return nil, fmt.Errorf("jobs: pool is required")
	}

	queues, err := queueConfig(cfg)
	if err != nil {
		return nil, err
	}

	if workers == nil {
		workers = NewWorkers()
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            queues,
		Workers:           workers,
		Logger:            log,
		MaxAttempts:       8,
		JobTimeout:        2 * time.Minute,
		JobStuckThreshold: 5 * time.Minute,
		SoftStopTimeout:   30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: create river client: %w", err)
	}
	return client, nil
}

// Run starts processing the job kinds registered on the client and blocks until
// ctx is cancelled. On cancellation the pool stops accepting new jobs and waits
// for in-flight work up to the configured soft stop timeout.
func Run(ctx context.Context, client *Client) error {
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("jobs: start river client: %w", err)
	}
	return nil
}

func queueConfig(cfg *config.RiverConfig) (map[string]river.QueueConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("jobs: river config is required")
	}
	if cfg.MaxWorkers <= 0 {
		return nil, fmt.Errorf("jobs: RIVER_MAX_WORKERS must be positive, got %d", cfg.MaxWorkers)
	}

	total := 0
	for _, max := range cfg.Queues {
		total += max
	}
	if total == 0 {
		return nil, fmt.Errorf("jobs: at least one queue must be configured")
	}
	if total > cfg.MaxWorkers {
		return nil, fmt.Errorf("jobs: queue worker counts (%d) exceed RIVER_MAX_WORKERS (%d)", total, cfg.MaxWorkers)
	}

	queues := make(map[string]river.QueueConfig, len(cfg.Queues))
	for name, max := range cfg.Queues {
		queues[name] = river.QueueConfig{MaxWorkers: max}
	}
	return queues, nil
}
