// Command migrate applies database migrations.
//
// It runs two sets: WasaTN's own SQL files via golang-migrate, and River's
// embedded migrations via the River migrator.
//
// Usage:
//
//	migrate up           apply everything
//	migrate down         roll back WasaTN's migrations one step
//	migrate version      print the current WasaTN migration version
//	migrate sql <name>   print the path of a new migration to write
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/burstman/wasatn/internal/config"
	"github.com/burstman/wasatn/internal/db"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	switch command {
	case "up":
		if err := migrateApp(cfg.DatabaseURL, func(m *migrate.Migrate) error { return m.Up() }); err != nil {
			return err
		}
		return migrateRiver(ctx, pool)
	case "down":
		return migrateApp(cfg.DatabaseURL, func(m *migrate.Migrate) error { return m.Down() })
	case "version":
		version, dirty, err := appVersion(cfg.DatabaseURL)
		if err != nil {
			return err
		}
		fmt.Printf("app version: %d (dirty: %v)\n", version, dirty)
		return riverVersion(ctx, pool)
	case "sql":
		if len(os.Args) < 3 {
			return errors.New("usage: migrate sql <name>")
		}
		return newMigration(os.Args[2])
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

// migrateURL adapts a Postgres connection string to the form golang-migrate
// expects.
//
// The pgx driver registers itself as "pgx5", so migrate cannot be given a bare
// postgres:// URL. DATABASE_URL is a plain Postgres URL (Neon issues
// postgresql:// these days), so the scheme is swapped here rather than making
// operators maintain a second, migrate-specific connection string.
//
// Note the driver receives the whole URL and rewrites the scheme to postgres
// itself, so only the scheme is replaced and nothing is nested.
func migrateURL(databaseURL string) string {
	for _, scheme := range []string{"postgres://", "postgresql://"} {
		if rest, ok := strings.CutPrefix(databaseURL, scheme); ok {
			return "pgx5://" + rest
		}
	}
	return databaseURL
}

// migrateApp applies WasaTN's own migrations from the migrations/ directory.
func migrateApp(databaseURL string, direction func(*migrate.Migrate) error) error {
	path, err := migrationsDir()
	if err != nil {
		return err
	}

	m, err := migrate.New("file://"+path, migrateURL(databaseURL))
	if err != nil {
		return fmt.Errorf("initialise migrations: %w", err)
	}
	defer func() {
		// Best effort: the source and database handles are closed on success
		// anyway, and on failure the process exits.
		_, _ = m.Close()
	}()

	if err := direction(m); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	fmt.Println("app migrations: up to date")
	return nil
}

// migrateRiver applies River's embedded job queue migrations.
func migrateRiver(ctx context.Context, pool *pgxpool.Pool) error {
	driver := riverpgxv5.New(pool)
	migrator, err := rivermigrate.New[pgx.Tx](driver, &rivermigrate.Config{
		Schema: "",
	})
	if err != nil {
		return fmt.Errorf("initialise river migrator: %w", err)
	}

	result, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return fmt.Errorf("apply river migrations: %w", err)
	}
	fmt.Printf("river migrations: %d applied\n", len(result.Versions))
	return nil
}

func riverVersion(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New[pgx.Tx](riverpgxv5.New(pool), &rivermigrate.Config{})
	if err != nil {
		return fmt.Errorf("initialise river migrator: %w", err)
	}
	result, err := migrator.Validate(ctx, nil)
	if err != nil {
		return fmt.Errorf("read river version: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("river schema invalid: %s", strings.Join(result.Messages, "; "))
	}
	fmt.Println("river schema: valid")
	return nil
}

func appVersion(databaseURL string) (version uint, dirty bool, err error) {
	path, err := migrationsDir()
	if err != nil {
		return 0, false, err
	}
	m, err := migrate.New("file://"+path, migrateURL(databaseURL))
	if err != nil {
		return 0, false, fmt.Errorf("initialise migrations: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	return m.Version()
}

func migrationsDir() (string, error) {
	// Works when run from the repo root (make migrate) and from anywhere via an
	// explicit override, since the migrations live outside the binary.
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		return dir, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	path := filepath.Join(wd, "migrations")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("migrations directory not found at %s: %w", path, err)
	}
	return path, nil
}

// newMigration creates a paired up/down migration.
//
// golang-migrate's file source expects <version>_<name>.up.sql and
// <version>_<name>.down.sql. Both files are written up front so a migration is
// never applied without a way to roll it back.
func newMigration(name string) error {
	path, err := migrationsDir()
	if err != nil {
		return err
	}

	safe, err := migrationName(name)
	if err != nil {
		return err
	}

	version := time.Now().UTC().Format("20060102150405")
	// Plain comments, not "-- +migrate Up": that directive belongs to the
	// migrate CLI's own SQL format and is ignored by golang-migrate's file
	// source, which uses the filename to decide direction.
	files := map[string]string{
		version + "_" + safe + ".up.sql":   "-- Write the schema change here.\n",
		version + "_" + safe + ".down.sql": "-- Reverse the schema change here.\n",
	}

	for filename, content := range files {
		full := filepath.Join(path, filename)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write migration: %w", err)
		}
		fmt.Println("created", full)
	}
	return nil
}

// migrationName reduces a free-form description to the characters
// golang-migrate accepts in a filename. Path separators become underscores and
// everything else is dropped, so a name can never escape the migrations
// directory or smuggle SQL through a shell.
func migrationName(name string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '_', r == '.', r == '/', r == '\\':
			b.WriteRune('_')
		}
	}

	clean := strings.Trim(b.String(), "_")
	if clean == "" {
		return "", fmt.Errorf("migration name %q has no usable characters", name)
	}
	return clean, nil
}
