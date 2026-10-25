package migrate_test

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"

	engine "github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"

	migrate "github.com/fin-assist/go-migrate"
	pgtest "github.com/fin-assist/go-migrate/internal/pgtest"
)

// TestIntegrationFailedFirstRollbackIsReportedDirty reproduces FIN-314 on PostgreSQL: a
// failed Down of the first migration leaves the (-1, dirty) row, which Versions used to
// report as a clean version 0.
func TestIntegrationFailedFirstRollbackIsReportedDirty(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	c := migrate.DefaultConfig(dsn)
	src := migrate.Source{Name: "service", Table: "versions", FS: fstest.MapFS{
		"1_a.up.sql":   sqlFile("CREATE TABLE a(id bigint)"),
		"1_a.down.sql": sqlFile("DROP TABLE missing_table_314"),
	}}
	if err := migrate.Up(ctx, c, []migrate.Source{src}); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Down(ctx, c, src, 1); err == nil {
		t.Fatal("rollback with a failing down file succeeded")
	}
	var recorded int64
	var dirtyRow bool
	conn := pgtestConn(t, dsn)
	if err := conn.QueryRow(ctx, "SELECT version, dirty FROM versions").Scan(&recorded, &dirtyRow); err != nil || recorded != -1 || !dirtyRow {
		t.Fatalf("version table (%d, %v) %v, want the engine's (-1, dirty)", recorded, dirtyRow, err)
	}
	st, err := migrate.Versions(ctx, c, []migrate.Source{src})
	if err != nil || len(st) != 1 || st[0].Version != 0 || !st[0].Dirty {
		t.Fatalf("versions %v %v, want dirty 0", st, err)
	}
	var dirty engine.ErrDirty
	if err := migrate.Up(ctx, c, []migrate.Source{src}); !errors.As(err, &dirty) {
		t.Fatalf("up on a dirty source: %v", err)
	}
	if err := migrate.CheckUpDownUp(ctx, c, src); err == nil {
		t.Fatal("up/down/up walk started on a dirty source")
	}
	// Recovery by the documented path: the schema still has table a, drop it and force 0.
	if _, err := conn.Exec(ctx, "DROP TABLE a"); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Force(ctx, c, src, 0); err != nil {
		t.Fatal(err)
	}
	st, err = migrate.Versions(ctx, c, []migrate.Source{src})
	if err != nil || st[0].Version != 0 || st[0].Dirty {
		t.Fatalf("after force %v %v, want clean 0", st, err)
	}
	if err := migrate.Up(ctx, c, []migrate.Source{src}); err != nil || !exists(t, dsn, "a") {
		t.Fatalf("up after recovery: %v", err)
	}
}

func pgtestConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}
