package database

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

// These tests need a real Postgres. Point TEST_DATABASE_URL at a throwaway
// database, for example the one docker compose starts:
//
//	TEST_DATABASE_URL=postgres://hookmini:hookmini@127.0.0.1:5432/hookmini?sslmode=disable
//
// Without it they are skipped, so `go test ./...` stays green on a bare machine.
func testURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return url
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestConnectAndMigrate(t *testing.T) {
	url := testURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := Connect(ctx, quiet(), url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	if _, err := Migrate(ctx, quiet(), url); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	// Running again must be a no-op, not an error.
	n, err := Migrate(ctx, quiet(), url)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if n != 0 {
		t.Errorf("second Migrate applied %d migrations, want 0 (idempotent)", n)
	}

	var exists bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'set_updated_at')`).Scan(&exists)
	if err != nil || !exists {
		t.Fatalf("baseline function set_updated_at missing (err=%v)", err)
	}
}

func TestConnectGivesUpWhenDatabaseIsUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Port 1 is never a Postgres; the context deadline must end the retries.
	_, err := Connect(ctx, quiet(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	if err == nil {
		t.Fatal("expected an error for an unreachable database")
	}
}
