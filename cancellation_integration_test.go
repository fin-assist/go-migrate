package migrate_test

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/golang-migrate/migrate/v4/database"

	migrate "github.com/fin-assist/go-migrate"
	pgtest "github.com/fin-assist/go-migrate/internal/pgtest"
)

func TestIntegrationCancellationDuringFileStopsBeforeNextVersion(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx, deadline := context.WithTimeout(t.Context(), 5*time.Second)
	defer deadline()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(81251)"); err != nil {
		t.Fatal(err)
	}
	src := migrate.Source{Name: "service", Table: "versions", FS: fstest.MapFS{
		"000001_first.up.sql":    {Data: []byte("CREATE TABLE first_file(id bigint); SELECT pg_advisory_lock(81252); SELECT pg_advisory_lock(81251);")},
		"000001_first.down.sql":  {Data: []byte("DROP TABLE first_file")},
		"000002_second.up.sql":   {Data: []byte("CREATE TABLE second_file(id bigint)")},
		"000002_second.down.sql": {Data: []byte("DROP TABLE second_file")},
	}}
	cfg := migrate.DefaultConfig(dsn)
	runctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- migrate.Up(runctx, cfg, []migrate.Source{src}) }()
	// The first advisory lock is a database-visible signal that the file started; the
	// second is held by this test, so no elapsed-time assumption controls cancellation.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var started bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=81252 AND granted)").Scan(&started); err != nil {
			t.Fatal(err)
		}
		if started {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	cancel()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(81251)"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation result: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !exists(t, dsn, "first_file") || exists(t, dsn, "second_file") {
		t.Fatal("cancellation did not stop at file boundary")
	}
	st, err := migrate.Versions(ctx, cfg, []migrate.Source{src})
	if err != nil || st[0].Version != 1 || st[0].Dirty {
		t.Fatalf("post-cancel %v %v", st, err)
	}
	// Queue and engine connections must be gone; a new run proceeds without manual repair.
	if err := migrate.Up(ctx, cfg, []migrate.Source{src}); err != nil {
		t.Fatal(err)
	}
	st, err = migrate.Versions(ctx, cfg, []migrate.Source{src})
	if err != nil || st[0].Version != 2 || st[0].Dirty || !exists(t, dsn, "second_file") {
		t.Fatalf("restart %v %v", st, err)
	}
}
func TestIntegrationDDLAndStatementTimeouts(t *testing.T) {
	for _, mode := range []string{"ddl", "statement"} {
		t.Run(mode, func(t *testing.T) {
			dsn := pgtest.DSN(t)
			ctx := t.Context()
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close(context.Background()) }()
			sql := "SELECT pg_sleep(2)"
			if mode == "ddl" {
				if _, err := conn.Exec(ctx, "CREATE TABLE busy(id bigint)"); err != nil {
					t.Fatal(err)
				}
				tx, err := conn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if _, err := tx.Exec(ctx, "LOCK TABLE busy IN ACCESS EXCLUSIVE MODE"); err != nil {
					t.Fatal(err)
				}
				sql = "ALTER TABLE busy ADD COLUMN value text"
			}
			src := migrate.Source{Name: mode, Table: "versions", FS: fstest.MapFS{"000001_bounded.up.sql": {Data: []byte(sql)}, "000001_bounded.down.sql": {Data: []byte("SELECT 1")}}}
			c := migrate.DefaultConfig(dsn)
			c.LockTimeout = 30 * time.Millisecond
			c.StatementTimeout = 100 * time.Millisecond
			err = migrate.Up(ctx, c, []migrate.Source{src})
			var dbErr database.Error
			var pgerr *pgconn.PgError
			if !errors.As(err, &dbErr) || !errors.As(dbErr.OrigErr, &pgerr) {
				t.Fatalf("missing PostgreSQL timeout: %v", err)
			}
			want := "57014"
			if mode == "ddl" {
				want = "55P03"
			}
			if pgerr.Code != want {
				t.Fatalf("SQLSTATE %s want %s", pgerr.Code, want)
			}
			st, err := migrate.Versions(ctx, c, []migrate.Source{src})
			if err != nil || st[0].Version != 1 || !st[0].Dirty {
				t.Fatalf("timeout state %v %v", st, err)
			}
		})
	}
}
