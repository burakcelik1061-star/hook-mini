package database

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/burakcelik1061-star/hook-mini/db/migrations"
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

// scratchDatabase creates an empty database next to TEST_DATABASE_URL's and
// returns its URL; it is dropped when the test ends.
func scratchDatabase(ctx context.Context, t *testing.T) string {
	t.Helper()
	base := testURL(t)

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	name := "hookmini_scratch_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// TestMigrateWaitsForAdvisoryLock is the deterministic proof that migrators
// take turns: while another session holds the migration lock, Migrate must
// block and change nothing; once the lock is released it must proceed.
func TestMigrateWaitsForAdvisoryLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	scratch := scratchDatabase(ctx, t)

	holder, err := pgx.Connect(ctx, scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(context.Background())
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", lock.DefaultLockID); err != nil {
		t.Fatalf("take the migration lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Migrate(ctx, quiet(), scratch)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Migrate finished (err=%v) while another session held the lock", err)
	case <-time.After(1500 * time.Millisecond):
	}

	var tables int
	if err := holder.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'goose_db_version'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("Migrate changed the schema (%d tables) without holding the lock", tables)
	}

	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", lock.DefaultLockID); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Migrate after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Migrate did not proceed after the lock was released")
	}
}

// TestConcurrentMigratorsAllSucceed is a smoke test: several migrators started
// together against an empty database must all succeed and apply every
// migration exactly once between them. The race it guards against is timing
// dependent, so TestMigrateWaitsForAdvisoryLock is the real proof of the lock.
func TestConcurrentMigratorsAllSucceed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	scratch := scratchDatabase(ctx, t)

	const migrators = 6
	var (
		wg      sync.WaitGroup
		applied atomic.Int32
		errs    = make(chan error, migrators)
	)
	for range migrators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := Migrate(ctx, quiet(), scratch)
			applied.Add(int32(n))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent Migrate failed: %v", err)
		}
	}
	// Each migration must have been applied exactly once in total.
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(files))
	}
	if got := int(applied.Load()); got != len(files) {
		t.Errorf("migrations applied across all migrators = %d, want %d", got, len(files))
	}
}

// TestMigrationsRoundTrip proves every migration's Down really undoes its Up:
// up, all the way down, up again. It runs in a scratch database it creates and
// drops itself, so it cannot disturb the database the other tests share.
func TestMigrationsRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	scratch := scratchDatabase(ctx, t)

	db, err := sql.Open("pgx", scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}

	userTables := func() []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT tablename FROM pg_tables
			WHERE schemaname = 'public' AND tablename <> 'goose_db_version' ORDER BY tablename`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	functions := func() int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	upTables := userTables()
	if len(upTables) == 0 || functions() == 0 {
		t.Fatalf("up created no schema: tables=%v", upTables)
	}

	if _, err := provider.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if left := userTables(); len(left) != 0 || functions() != 0 {
		t.Fatalf("down left objects behind: tables=%v functions=%d", left, functions())
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if again := userTables(); !slices.Equal(again, upTables) {
		t.Fatalf("schema after down+up differs:\n got %v\nwant %v", again, upTables)
	}
}

// Every pooled connection carries the server-side limits, and a query stuck
// behind a lock fails instead of holding its connection for ever. A value in
// the connection string wins over the default.
func TestConnectSetsSessionLimits(t *testing.T) {
	base := testURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := Connect(ctx, quiet(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for setting, want := range map[string]string{
		"statement_timeout": "30s", "lock_timeout": "10s", "idle_in_transaction_session_timeout": "1min",
	} {
		var got string
		if err := pool.QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q (err %v), want %q", setting, got, err, want)
		}
	}

	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("lock_timeout", "200")
	u.RawQuery = q.Encode()
	short, err := Connect(ctx, quiet(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()

	var got string
	if err := short.QueryRow(ctx, "SELECT current_setting('lock_timeout')").Scan(&got); err != nil || got != "200ms" {
		t.Errorf("lock_timeout from the URL = %q (err %v), want 200ms", got, err)
	}

	// One session holds a lock; a session waiting for it gives up instead of
	// waiting for as long as the holder likes.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(4242)`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = short.Exec(ctx, `SELECT pg_advisory_xact_lock(4242)`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" { // lock_not_available
		t.Fatalf("err = %v, want a lock timeout", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("waited %s for the lock despite lock_timeout=200ms", waited)
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
