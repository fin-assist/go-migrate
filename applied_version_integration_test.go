package migrate_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	migrate "github.com/fin-assist/go-migrate"
	pgtest "github.com/fin-assist/go-migrate/internal/pgtest"
)

func sqlFile(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// TestIntegrationAppliedVersionWithoutFileFailsInsteadOfFalseSuccess reproduces FIN-278 on
// PostgreSQL: an applied file renamed away (a gap in numbering) must not end in "up to date".
func TestIntegrationAppliedVersionWithoutFileFailsInsteadOfFalseSuccess(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	c := migrate.DefaultConfig(dsn)
	applied := migrate.Source{Name: "service", Table: "versions", FS: fstest.MapFS{
		"1_a.up.sql": sqlFile("CREATE TABLE a(id bigint)"), "1_a.down.sql": sqlFile("DROP TABLE a"),
		"2_b.up.sql": sqlFile("CREATE TABLE b(id bigint)"), "2_b.down.sql": sqlFile("DROP TABLE b"),
	}}
	if err := migrate.Up(ctx, c, []migrate.Source{applied}); err != nil {
		t.Fatal(err)
	}
	renamed := migrate.Source{Name: "service", Table: "versions", FS: fstest.MapFS{
		"1_a.up.sql": applied.FS.(fstest.MapFS)["1_a.up.sql"], "1_a.down.sql": applied.FS.(fstest.MapFS)["1_a.down.sql"],
		"3_b.up.sql": sqlFile("CREATE TABLE b3(id bigint)"), "3_b.down.sql": sqlFile("DROP TABLE b3"),
		"4_c.up.sql": sqlFile("CREATE TABLE c(id bigint)"), "4_c.down.sql": sqlFile("DROP TABLE c"),
	}}
	for name, run := range map[string]func() error{
		"up":   func() error { return migrate.Up(ctx, c, []migrate.Source{renamed}) },
		"down": func() error { return migrate.Down(ctx, c, renamed, 1) },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "applied version 2 of source service has no migration file") {
			t.Fatalf("%s: false success or wrong error: %v", name, err)
		}
	}
	if exists(t, dsn, "b3") || exists(t, dsn, "c") || !exists(t, dsn, "b") {
		t.Fatal("schema changed after the refusal")
	}
	st, err := migrate.Versions(ctx, c, []migrate.Source{renamed})
	if err != nil || st[0].Version != 2 || st[0].Dirty {
		t.Fatalf("state %v %v", st, err)
	}
	// Recovery by the documented path: force an existing version after checking the schema.
	if err := migrate.Force(ctx, c, renamed, 2); err == nil {
		t.Fatal("force to a version without a file accepted")
	}
	if _, err := pgtestExec(t, dsn, "DROP TABLE b"); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Force(ctx, c, renamed, 1); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Up(ctx, c, []migrate.Source{renamed}); err != nil {
		t.Fatal(err)
	}
	st, err = migrate.Versions(ctx, c, []migrate.Source{renamed})
	if err != nil || st[0].Version != 4 || st[0].Dirty || !exists(t, dsn, "b3") || !exists(t, dsn, "c") {
		t.Fatalf("recovered state %v %v", st, err)
	}
}

// TestIntegrationForceZeroMeansNothingApplied: "force <source> 0" used to record version 0
// without a file, after which every Up silently applied nothing.
func TestIntegrationForceZeroMeansNothingApplied(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	c := migrate.DefaultConfig(dsn)
	src := migrate.Source{Name: "service", Table: "versions", FS: fstest.MapFS{
		"1_a.up.sql": sqlFile("CREATE TABLE a(id bigint)"), "1_a.down.sql": sqlFile("DROP TABLE a"),
	}}
	for _, version := range []int{-1, 0} {
		if err := migrate.Force(ctx, c, src, version); err != nil {
			t.Fatal(version, err)
		}
		st, err := migrate.Versions(ctx, c, []migrate.Source{src})
		if err != nil || st[0].Version != 0 || st[0].Dirty {
			t.Fatalf("force %d: %v %v", version, st, err)
		}
	}
	if err := migrate.Up(ctx, c, []migrate.Source{src}); err != nil || !exists(t, dsn, "a") {
		t.Fatalf("up after force 0 applied nothing: %v", err)
	}
	if err := migrate.Force(ctx, c, src, 2); err == nil {
		t.Fatal("force beyond the source accepted")
	}
	st, err := migrate.Versions(ctx, c, []migrate.Source{src})
	if err != nil || st[0].Version != 1 || st[0].Dirty {
		t.Fatalf("refused force changed state: %v %v", st, err)
	}
	// A source whose first file is version 0 keeps 0 as a real version.
	zero := migrate.Source{Name: "zero", Table: "zero_versions", FS: fstest.MapFS{
		"0_z.up.sql": sqlFile("CREATE TABLE z(id bigint)"), "0_z.down.sql": sqlFile("DROP TABLE z"),
	}}
	if err := migrate.Force(ctx, c, zero, 0); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Up(ctx, c, []migrate.Source{zero}); err != nil || exists(t, dsn, "z") {
		t.Fatalf("version 0 must count as applied: %v", err)
	}
}

func pgtestExec(t *testing.T, dsn, sql string) (pgconn.CommandTag, error) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return conn.Exec(t.Context(), sql)
}
