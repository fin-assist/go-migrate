package migrate_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	engine "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	migrate "github.com/fin-assist/go-migrate"
)

func postgresError(err error) *pgconn.PgError {
	var direct *pgconn.PgError
	if errors.As(err, &direct) {
		return direct
	}
	var value database.Error
	if errors.As(err, &value) {
		if errors.As(value.OrigErr, &direct) {
			return direct
		}
	}
	var pointer *database.Error
	if errors.As(err, &pointer) {
		if errors.As(pointer.OrigErr, &direct) {
			return direct
		}
	}
	return nil
}
func hasSQLState(err error, code string) bool {
	pgerr := postgresError(err)
	return pgerr != nil && pgerr.Code == code
}

func TestAPIConsumerValidatorRejectsNilFilesystem(t *testing.T) {
	if err := migrate.Validate(migrate.Source{}); err == nil || !strings.Contains(err.Error(), "files are required") {
		t.Fatalf("zero-value source must be rejected without a panic: %v", err)
	}
}
func TestAPIConsumerErrorsAndLogsDoNotExposeSQLOrDSN(t *testing.T) {
	dsn := consumerDatabase(t)
	var logs bytes.Buffer
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	src := migrate.Source{Name: "private", Table: "versions", FS: fstest.MapFS{
		"000001_private.up.sql":   {Data: []byte("DO $$ BEGIN RAISE EXCEPTION 'sql-secret-251' USING DETAIL='detail-secret-251'; END $$;")},
		"000001_private.down.sql": {Data: []byte("SELECT 1")},
	}}
	err := migrate.Up(t.Context(), cfg, []migrate.Source{src})
	if !hasSQLState(err, "P0001") {
		t.Fatalf("semantic SQL failure lost: %v", err)
	}
	pgerr := postgresError(err)
	if pgerr.Message != "sql-secret-251" || pgerr.Detail != "detail-secret-251" {
		t.Fatal("explicit diagnostics lost")
	}
	// Ordinary formatting and structured logging remain safe. Unwrapped diagnostics are
	// intentionally available only to code that handles them without logging/exposing them.
	slog.New(slog.NewTextHandler(&logs, nil)).Error("deploy failed", "error", err)
	for _, format := range []string{"%v", "%+v", "%s", "%q"} {
		text := fmt.Sprintf(format, err)
		for _, secret := range []string{"sql-secret-251", "detail-secret-251", "DO $$", "RAISE EXCEPTION"} {
			if strings.Contains(text, secret) || strings.Contains(logs.String(), secret) {
				t.Fatal("private SQL escaped error/log boundary")
			}
		}
	}
	var dirty engine.ErrDirty
	if err := migrate.Up(t.Context(), cfg, []migrate.Source{src}); !errors.As(err, &dirty) || dirty.Version != 1 {
		t.Fatalf("dirty diagnostic lost: %v", err)
	}
	cfg.DSN = "postgres://migration-user:dsn-secret-251@%zz/bad"
	err = migrate.Up(t.Context(), cfg, []migrate.Source{src})
	if !errors.Is(err, migrate.ErrInvalidDSN) {
		t.Fatalf("invalid DSN identity lost: %v", err)
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if strings.Contains(fmt.Sprint(cause), "dsn-secret-251") || strings.Contains(fmt.Sprint(cause), "postgres://") {
			t.Fatal("unsafe parser cause retained")
		}
	}
}

func TestAPIConsumerCancelledEmptyDeploymentCanRetry(t *testing.T) {
	dsn := consumerDatabase(t)
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
	src := migrate.Source{Name: "service", Table: "versions", FS: fstest.MapFS{}}
	deployment, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- migrate.Up(deployment, cfg, []migrate.Source{src}) }()
	waitPostgres(t, ctx, conn, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name='migrate' AND wait_event='advisory')")
	cancel()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", nativeLock); err != nil {
		t.Fatal(err)
	}
	if err := receiveRun(t, ctx, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled empty deployment returned %v", err)
	}
	// A later deployment adds its first real migration on the same version table.
	// It must acquire the queue and proceed cleanly without manual metadata repair.
	src.FS = sub(t, "library")
	if err := migrate.Up(ctx, cfg, []migrate.Source{src}); err != nil {
		t.Fatal("retry deployment", err)
	}
	st, err := migrate.Versions(ctx, cfg, []migrate.Source{src})
	if err != nil || st[0].Version != 1 || st[0].Dirty || !exists(t, dsn, "lib_outbox") {
		t.Fatalf("retry deployment state %v %v", st, err)
	}
}
