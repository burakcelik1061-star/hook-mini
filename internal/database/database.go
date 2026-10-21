// Package database owns the Postgres connection pool and schema migrations.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/burakcelik1061-star/hook-mini/db/migrations"
)

// Connect opens a pool and waits until Postgres answers a ping. Containers
// start in no guaranteed order, so a short retry window is part of the
// contract: it returns only when the database is usable or ctx is done.
func Connect(ctx context.Context, log *slog.Logger, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	delay := 250 * time.Millisecond
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return pool, nil
		}
		if ctx.Err() != nil || attempt >= 10 {
			pool.Close()
			return nil, fmt.Errorf("ping database after %d attempts: %w", attempt, err)
		}
		log.Warn("database not ready, retrying", "attempt", attempt, "delay", delay, "error", err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("waiting for database: %w", ctx.Err())
		}
		delay = min(delay*2, 3*time.Second)
	}
}

// Migrate applies every pending migration and returns how many ran.
func Migrate(ctx context.Context, log *slog.Logger, url string) (int, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return 0, fmt.Errorf("open database/sql handle: %w", err)
	}
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return 0, fmt.Errorf("create migration provider: %w", err)
	}

	results, err := provider.Up(ctx)
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "took", r.Duration)
	}
	if err != nil {
		return len(results), fmt.Errorf("apply migrations: %w", err)
	}
	return len(results), nil
}
