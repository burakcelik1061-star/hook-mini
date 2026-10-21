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
	"github.com/pressly/goose/v3/lock"

	"github.com/burakcelik1061-star/hook-mini/db/migrations"
)

// sessionDefaults are server-side limits set on every pooled connection
// unless the connection string sets them itself. HTTP request contexts carry
// no deadline, so without them one query stuck behind a lock (or a
// transaction left open by a bug) would hold its pool connection, and
// eventually all of them, indefinitely. Every query in this service takes
// milliseconds; the limits only end the pathological case, which then
// surfaces as an error instead of a hang. Migrations use their own
// connection and are not affected.
var sessionDefaults = map[string]string{
	"statement_timeout":                   "30s",
	"lock_timeout":                        "10s",
	"idle_in_transaction_session_timeout": "60s",
}

// Connect opens a pool and waits until Postgres answers a ping. Containers
// start in no guaranteed order, so a short retry window is part of the
// contract: it returns only when the database is usable or ctx is done.
func Connect(ctx context.Context, log *slog.Logger, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	for k, v := range sessionDefaults {
		if _, set := cfg.ConnConfig.RuntimeParams[k]; !set {
			cfg.ConnConfig.RuntimeParams[k] = v
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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

	// A Postgres advisory lock makes concurrent migrators take turns: two
	// replicas starting together must not both try to create the same table.
	// A waiting migrator polls every second (the library's minimum) for up to
	// five minutes instead of its 5-second default.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return 0, fmt.Errorf("create migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSessionLocker(locker))
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
