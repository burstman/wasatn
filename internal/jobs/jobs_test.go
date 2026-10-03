package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/burstman/wasatn/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// newPool returns a lazily-connected pool. Nothing dials out until a query
// runs, so these tests never touch a database.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	cfg, err := pgxpool.ParseConfig("postgres://user:pass@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig() error = %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func validRiverConfig() *config.RiverConfig {
	return &config.RiverConfig{
		MaxWorkers: 11,
		Queues:     map[string]int{QueueSend: 8, QueueSchedule: 2, QueueDefault: 1},
	}
}

func TestQueueConfig(t *testing.T) {
	t.Parallel()

	got, err := queueConfig(validRiverConfig())
	if err != nil {
		t.Fatalf("queueConfig() error = %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d queues, want 3", len(got))
	}
	for _, name := range []string{QueueSend, QueueSchedule, QueueDefault} {
		qc, ok := got[name]
		if !ok {
			t.Errorf("queue %q is missing", name)
			continue
		}
		if qc.MaxWorkers < 1 {
			t.Errorf("queue %q MaxWorkers = %d, want at least 1", name, qc.MaxWorkers)
		}
	}

	// The send queue must keep its own budget so a burst of sends cannot starve
	// maintenance jobs.
	if got[QueueSend].MaxWorkers <= got[QueueSchedule].MaxWorkers {
		t.Error("the send queue does not have more workers than the schedule queue")
	}
}

func TestQueueConfigRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     *config.RiverConfig
		wantErr string
	}{
		{name: "nil config", cfg: nil, wantErr: "river config is required"},
		{
			name:    "no workers",
			cfg:     &config.RiverConfig{MaxWorkers: 0, Queues: map[string]int{QueueDefault: 1}},
			wantErr: "RIVER_MAX_WORKERS must be positive",
		},
		{
			name:    "no queues",
			cfg:     &config.RiverConfig{MaxWorkers: 5, Queues: map[string]int{}},
			wantErr: "at least one queue",
		},
		{
			name:    "queues exceed the worker budget",
			cfg:     &config.RiverConfig{MaxWorkers: 2, Queues: map[string]int{QueueSend: 8, QueueDefault: 1}},
			wantErr: "exceed RIVER_MAX_WORKERS",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := queueConfig(tc.cfg)
			if err == nil {
				t.Fatalf("queueConfig() error = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// Regression guard: River refuses to build a client that has Queues set but no
// worker registry, which broke server startup until the nil case was handled.
func TestNewClientWithoutWorkers(t *testing.T) {
	t.Parallel()

	client, err := NewClient(newPool(t), validRiverConfig(), nil, nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewClient() returned no client")
	}
}

func TestNewClientWithWorkers(t *testing.T) {
	t.Parallel()

	workers := river.NewWorkers()
	client, err := NewClient(newPool(t), validRiverConfig(), workers, nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewClient() returned no client")
	}
}

func TestNewClientRequiresPool(t *testing.T) {
	t.Parallel()

	if _, err := NewClient(nil, validRiverConfig(), nil, nil); err == nil {
		t.Error("NewClient(nil pool) error = nil, want error")
	}
}

func TestNewClientRejectsBadConfig(t *testing.T) {
	t.Parallel()

	_, err := NewClient(newPool(t), &config.RiverConfig{MaxWorkers: 1, Queues: map[string]int{QueueDefault: 5}}, nil, nil)
	if err == nil {
		t.Error("NewClient() error = nil, want error for an invalid config")
	}
}

func TestNewWorkersStartsEmpty(t *testing.T) {
	t.Parallel()

	// No job kinds are registered until the campaign milestones land.
	if got := NewWorkers(); got == nil {
		t.Fatal("NewWorkers() = nil, want an empty registry")
	}
}

func TestRegistryStartsEmpty(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if !r.Empty() {
		t.Error("Empty() = false for a fresh registry")
	}
	if got := r.Kinds(); len(got) != 0 {
		t.Errorf("Kinds() = %v, want empty", got)
	}
	if r.Bundle() == nil {
		t.Fatal("Bundle() = nil, want River's registry")
	}
}

func TestRegisterRecordsKind(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	worker := &noopWorker{}

	if err := Register(r, noopArgs{}, worker); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if r.Empty() {
		t.Error("Empty() = true after registering a job kind")
	}
	want := noopArgs{}.Kind()
	if got := r.Kinds(); len(got) != 1 || got[0] != want {
		t.Errorf("Kinds() = %v, want [%q]", got, want)
	}
}

func TestRegisterRejectsDuplicateKind(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := Register(r, noopArgs{}, &noopWorker{}); err != nil {
		t.Fatalf("first Register() error = %v", err)
	}
	if err := Register(r, noopArgs{}, &noopWorker{}); err == nil {
		t.Error("Register() error = nil for a duplicate kind, want error")
	}
}

// noopArgs is a throwaway job kind used to exercise registration.
type noopArgs struct{}

func (noopArgs) Kind() string { return "test_noop" }

type noopWorker struct {
	river.WorkerDefaults[noopArgs]
}

func (w *noopWorker) Work(context.Context, *river.Job[noopArgs]) error { return nil }
