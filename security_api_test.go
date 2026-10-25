package migrate_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	engine "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
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
