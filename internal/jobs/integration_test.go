package jobs_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/burstman/wasatn/internal/config"
	"github.com/burstman/wasatn/internal/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// These tests exercise River against a real Postgres database. They are skipped
// unless TEST_DATABASE_URL points at a throwaway database, because they insert
// rows and elect a leader.
//
//	TEST_DATABASE_URL=postgres://... go test ./internal/jobs/ -run Integration
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig() error = %v", err)
	}
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	return pool
}

// Integration test: a job inserted by the web process must be picked up and run
// by a worker using the same queue configuration.
func TestIntegrationInsertAndRun(t *testing.T) {
	pool := testPool(t)

	registry := jobs.NewRegistry()
	done := make(chan struct{})
	if err := jobs.Register(registry, integrationArgs{}, &integrationWorker{done: done}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	riverCfg := &config.RiverConfig{
		MaxWorkers: 2,
		Queues:     map[string]int{jobs.QueueDefault: 1},
	}

	client, err := jobs.NewClient(pool, riverCfg, registry.Bundle(), nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	workerCtx, stop := context.WithCancel(context.Background())
	defer stop()

	started := make(chan struct{})
	go func() {
		close(started)
		_ = jobs.Run(workerCtx, client)
	}()
	<-started

	// Give the client a moment to register as a leader and begin fetching.
	inserted, err := client.Insert(context.Background(), integrationArgs{}, nil)
	if err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if inserted.Job == nil {
		t.Fatal("Insert() returned no job")
	}
	id := inserted.Job.ID

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		state := jobState(t, pool, id)
		t.Fatalf("job did not run within 30s (state %s)", state)
	}

	// The job must be recorded as completed.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if state := jobState(t, pool, id); state == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job state = %s, want completed", jobState(t, pool, id))
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Let the client resign before the pool closes, otherwise River logs a
	// leadership error while the test tears down.
	stop()
	time.Sleep(500 * time.Millisecond)
}

func jobState(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()

	var state string
	err := pool.QueryRow(context.Background(), `select state from river_job where id = $1`, id).Scan(&state)
	if err != nil {
		t.Fatalf("query job state: %v", err)
	}
	return state
}

type integrationArgs struct{}

func (integrationArgs) Kind() string { return "integration_probe" }

type integrationWorker struct {
	river.WorkerDefaults[integrationArgs]
	done chan struct{}
	once bool
}

func (w *integrationWorker) Work(ctx context.Context, job *river.Job[integrationArgs]) error {
	if !w.once {
		w.once = true
		close(w.done)
	}
	return nil
}
