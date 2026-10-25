package migrate_test

import (
	"context"
	"errors"
	"runtime/debug"
	"testing"
	"testing/fstest"
	"time"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/jackc/pgx/v5"

	migrate "github.com/fin-assist/go-migrate"
	pgtest "github.com/fin-assist/go-migrate/internal/pgtest"
)

func waitPostgres(t *testing.T, ctx context.Context, conn *pgx.Conn, query string, args ...any) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var ready bool
		if err := conn.QueryRow(ctx, query, args...).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

// waitRunnerDisconnected waits until no runner connection remains on the test database.
// A client-side close is asynchronous for the server: the backend stays visible in
// pg_stat_activity for a few milliseconds, so a single read races with it (FIN-279).
// A real leak keeps the connection open and fails the test at the deadline. GC is off
// while waiting: a finalizer of an unreachable leaked net.Conn would otherwise close the
// socket and hide the leak. The package runs no parallel tests, so the process-wide
// setting affects only this wait.
func waitRunnerDisconnected(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	waitPostgres(t, ctx, conn, "SELECT NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name IN ('migrate','migrate-wait'))")
}
func receiveRun(t *testing.T, ctx context.Context, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil
	}
}
func TestIntegrationEmptyInitializationCancellationAndErrorPriority(t *testing.T) {
	for _, operation := range []string{"up", "versions", "check", "initialization-error"} {
		t.Run(operation, func(t *testing.T) {
			dsn := pgtest.DSN(t)
			ctx, deadline := context.WithTimeout(t.Context(), 5*time.Second)
			defer deadline()
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close(context.Background()) }()
			var databaseName string
			if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
				t.Fatal(err)
			}
			nativeLock, err := database.GenerateAdvisoryLockId(databaseName, "public", "versions")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", nativeLock); err != nil {
				t.Fatal(err)
			}
			cfg := migrate.DefaultConfig(dsn)
			if operation == "initialization-error" {
				cfg.DSN += "&default_transaction_read_only=on"
			}
			src := migrate.Source{Name: "empty", Table: "versions", FS: fstest.MapFS{}}
			runctx, cancel := context.WithCancel(ctx)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				switch operation {
				case "versions":
					_, err := migrate.Versions(runctx, cfg, []migrate.Source{src})
					result <- err
				case "check":
					result <- migrate.CheckUpDownUp(runctx, cfg, src)
				default:
					result <- migrate.Up(runctx, cfg, []migrate.Source{src})
				}
			}()
			waitPostgres(t, ctx, conn, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name='migrate' AND wait_event='advisory')")
			cancel()
			if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", nativeLock); err != nil {
				t.Fatal(err)
			}
			err = receiveRun(t, ctx, result)
			if operation == "initialization-error" {
				if !hasSQLState(err, "25006") || errors.Is(err, context.Canceled) {
					t.Fatalf("real setup error replaced: %v", err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("empty source lost cancellation: %v", err)
			}
			waitRunnerDisconnected(t, conn)
			cfg = migrate.DefaultConfig(dsn)
			if err := migrate.Up(ctx, cfg, []migrate.Source{src}); err != nil {
				t.Fatal("retry", err)
			}
			st, err := migrate.Versions(ctx, cfg, []migrate.Source{src})
			if err != nil || st[0].Version != 0 || st[0].Dirty {
				t.Fatalf("retry state %v %v", st, err)
			}
		})
	}
}
func TestIntegrationQueueRemainsHeldAcrossFilesBeforeCompetingDown(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx, deadline := context.WithTimeout(t.Context(), 5*time.Second)
	defer deadline()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(251102),pg_advisory_lock(251104)"); err != nil {
		t.Fatal(err)
	}
	src := migrate.Source{Name: "service", Table: "versions", FS: fstest.MapFS{
		"000001_first.up.sql":    {Data: []byte("CREATE TABLE first_file(id bigint); SELECT pg_advisory_lock(251101); SELECT pg_advisory_lock(251102)")},
		"000001_first.down.sql":  {Data: []byte("DROP TABLE first_file")},
		"000002_second.up.sql":   {Data: []byte("CREATE TABLE second_file(id bigint); SELECT pg_advisory_lock(251103); SELECT pg_advisory_lock(251104)")},
		"000002_second.down.sql": {Data: []byte("DROP TABLE second_file")},
	}}
	cfg := migrate.DefaultConfig(dsn)
	up, down := make(chan error, 1), make(chan error, 1)
	go func() { up <- migrate.Up(ctx, cfg, []migrate.Source{src}) }()
	signal := "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND objid=$1 AND granted)"
	waitPostgres(t, ctx, conn, signal, int64(251101))
	go func() { down <- migrate.Down(ctx, cfg, src, 1) }()
	waiting := "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name='migrate-wait' AND wait_event='advisory')"
	waitPostgres(t, ctx, conn, waiting)
	var competitor int32
	if err := conn.QueryRow(ctx, "SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND application_name='migrate-wait' AND wait_event='advisory'").Scan(&competitor); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(251102)"); err != nil {
		t.Fatal(err)
	}
	waitPostgres(t, ctx, conn, signal, int64(251103))
	var stillWaiting bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND application_name='migrate-wait' AND wait_event='advisory')", competitor).Scan(&stillWaiting); err != nil {
		t.Fatal(err)
	}
	if !stillWaiting {
		t.Fatal("competing Down acquired queue between SQL files")
	}
	select {
	case err := <-down:
		t.Fatalf("competing Down completed before the source: %v", err)
	default:
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(251104)"); err != nil {
		t.Fatal(err)
	}
	if err := receiveRun(t, ctx, up); err != nil {
		t.Fatal("up", err)
	}
	if err := receiveRun(t, ctx, down); err != nil {
		t.Fatal("down", err)
	}
	st, err := migrate.Versions(ctx, cfg, []migrate.Source{src})
	if err != nil || st[0].Version != 1 || st[0].Dirty || !exists(t, dsn, "first_file") || exists(t, dsn, "second_file") {
		t.Fatalf("competitor ran in the wrong order: %v %v", st, err)
	}
}

// TestIntegrationEmptyRecordedVersionPreservesCancellation: an empty source whose table
// records a version must still report cancellation that happened during initialization.
func TestIntegrationEmptyRecordedVersionPreservesCancellation(t *testing.T) {
	for _, operation := range []string{"versions", "up", "down"} {
		t.Run(operation, func(t *testing.T) {
			dsn := pgtest.DSN(t)
			ctx, deadline := context.WithTimeout(t.Context(), 10*time.Second)
			defer deadline()
			newer := migrate.Source{Name: "service", Table: "versions", FS: fstest.MapFS{
				"1_a.up.sql": {Data: []byte("CREATE TABLE a(id bigint)")}, "1_a.down.sql": {Data: []byte("DROP TABLE a")},
			}}
			if err := migrate.Up(ctx, migrate.DefaultConfig(dsn), []migrate.Source{newer}); err != nil {
				t.Fatal(err)
			}
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close(context.Background()) }()
			var databaseName string
			if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
				t.Fatal(err)
			}
			nativeLock, err := database.GenerateAdvisoryLockId(databaseName, "public", "versions")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", nativeLock); err != nil {
				t.Fatal(err)
			}
			older := migrate.Source{Name: "service", Table: "versions", FS: fstest.MapFS{}}
			cfg := migrate.DefaultConfig(dsn)
			runctx, cancel := context.WithCancel(ctx)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				switch operation {
				case "versions":
					st, err := migrate.Versions(runctx, cfg, []migrate.Source{older})
					if err == nil && st != nil {
						err = errors.New("status returned after cancellation")
					}
					result <- err
				case "down":
					result <- migrate.Down(runctx, cfg, older, 1)
				default:
					result <- migrate.Up(runctx, cfg, []migrate.Source{older})
				}
			}()
			waitPostgres(t, ctx, conn, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name='migrate' AND wait_event='advisory')")
			cancel()
			if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", nativeLock); err != nil {
				t.Fatal(err)
			}
			if err := receiveRun(t, ctx, result); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			st, err := migrate.Versions(ctx, cfg, []migrate.Source{older})
			if err != nil || st[0].Version != 1 || st[0].Dirty {
				t.Fatalf("recorded state changed: %v %v", st, err)
			}
		})
	}
}
