package migrate_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	migrate "github.com/fin-assist/go-migrate"
	pgtest "github.com/fin-assist/go-migrate/internal/pgtest"
	harness "github.com/fin-assist/go-migrate/migratetest"
)

func consumerDatabase(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("TEST_DATABASE_URL")
	if admin == "" {
		admin = pgtest.DSN(t)
	}
	return harness.DSN(t, admin)
}
func TestAPIConsumerDeploymentAndRollback(t *testing.T) {
	dsn := consumerDatabase(t)
	ctx := t.Context()
	c := migrate.DefaultConfig(dsn)
	sources := []migrate.Source{{Name: "library", Table: "library_versions", FS: sub(t, "library")}, {Name: "service", Table: "service_versions", FS: sub(t, "service")}}
	// A deploy command discovers the selected source from its public source list.
	selected, err := migrate.Find(sources, "service")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Find(sources, "unknown"); err == nil {
		t.Fatal("unknown source")
	}
	st, err := migrate.Versions(ctx, c, sources)
	if err != nil || len(st) != 2 || st[0].Version != 0 || st[1].Version != 0 {
		t.Fatalf("fresh state %v %v", st, err)
	}
	if err := migrate.Up(ctx, c, sources); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Up(ctx, c, sources); err != nil {
		t.Fatal("repeated deploy", err)
	}
	st, err = migrate.Versions(ctx, c, sources)
	if err != nil || st[0].Version != 1 || st[1].Version != 4 || st[1].Dirty {
		t.Fatalf("deployment %v %v", st, err)
	}
	if !exists(t, dsn, "lib_outbox") || !exists(t, dsn, "note") {
		t.Fatal("schemas not applied")
	}
	// Operators cannot accidentally roll back a remove step; they explicitly acknowledge
	// the unchanged schema with Force before deploying a separately corrected source.
	if err := migrate.Down(ctx, c, selected, 1); !hasSQLState(err, "P0001") {
		t.Fatal(err)
	}
	st, err = migrate.Versions(ctx, c, sources)
	if err != nil || !st[1].Dirty {
		t.Fatalf("missing dirty state %v %v", st, err)
	}
	if err := migrate.Force(ctx, c, selected, 4); err != nil {
		t.Fatal(err)
	}
	lib := sources[0]
	if err := migrate.Down(ctx, c, lib, 1); err != nil {
		t.Fatal(err)
	}
	if exists(t, dsn, "lib_outbox") {
		t.Fatal("down left table")
	}
	if err := migrate.Up(ctx, c, []migrate.Source{lib}); err != nil {
		t.Fatal(err)
	}
	empty := migrate.Source{Name: "future", Table: "future_versions", FS: fstest.MapFS{}}
	if err := migrate.Up(ctx, c, []migrate.Source{empty}); err != nil {
		t.Fatal(err)
	}
	st, err = migrate.Versions(ctx, c, []migrate.Source{empty})
	if err != nil || st[0].Version != 0 || st[0].Dirty {
		t.Fatalf("empty %v %v", st, err)
	}
}
func TestAPIConsumerMigrationChecks(t *testing.T) {
	for _, name := range []string{"library", "service"} {
		t.Run(name, func(t *testing.T) {
			harness.CheckUpDownUp(t, consumerDatabase(t), migrate.Source{Name: name, Table: "versions", FS: sub(t, name)})
		})
	}
	dsn := consumerDatabase(t)
	src := migrate.Source{Name: "bad", Table: "versions", FS: sub(t, "broken")}
	if err := migrate.CheckUpDownUp(t.Context(), migrate.DefaultConfig(dsn), src); err == nil {
		t.Fatal("broken source accepted")
	}
	st, err := migrate.Versions(t.Context(), migrate.DefaultConfig(dsn), []migrate.Source{src})
	if err != nil || !st[0].Dirty {
		t.Fatalf("dirty check %v %v", st, err)
	}
}
func TestAPIConsumerQueueTimeoutCancellationAndSQLBounds(t *testing.T) {
	dsn := consumerDatabase(t)
	ctx := t.Context()
	src := migrate.Source{Name: "service", Table: "versions", FS: sub(t, "library")}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", "fin-assist/migrate/versions"); err != nil {
		t.Fatal(err)
	}
	c := migrate.DefaultConfig(dsn)
	c.WaitTimeout = 40 * time.Millisecond
	if err := migrate.Up(ctx, c, []migrate.Source{src}); err == nil || !strings.Contains(err.Error(), "still runs after") {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := migrate.Up(cancelled, c, []migrate.Source{src}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtext($1))", "fin-assist/migrate/versions"); err != nil {
		t.Fatal(err)
	}
	slow := migrate.Source{Name: "slow", Table: "slow_versions", FS: fstest.MapFS{"000001_slow.up.sql": {Data: []byte("SELECT pg_sleep(2)")}, "000001_slow.down.sql": {Data: []byte("SELECT 1")}}}
	c.StatementTimeout = 40 * time.Millisecond
	c.WaitTimeout = time.Second
	if err := migrate.Up(ctx, c, []migrate.Source{slow}); !hasSQLState(err, "57014") {
		t.Fatal(err)
	}
	if err := migrate.Up(ctx, migrate.DefaultConfig("://"), []migrate.Source{src}); err == nil {
		t.Fatal("bad connection accepted")
	}
}

// Consumer validation protects a deployment command before it opens any connection.
func TestAPIConsumerRejectsMalformedSources(t *testing.T) {
	file := func() *fstest.MapFile { return &fstest.MapFile{Data: []byte("SELECT 1")} }
	for _, files := range []fstest.MapFS{
		{"bad-name.sql": file()}, {"000001_a.up.sql": file()}, {"000001_a.down.sql": file()},
		{"000001_a.up.sql": file(), "000001_b.down.sql": file()},
	} {
		if err := migrate.Validate(migrate.Source{Name: "bad", Table: "versions", FS: files}); err == nil {
			t.Fatal("invalid source accepted")
		}
	}
	unreadable := migrate.Source{Name: "unreadable", Table: "versions", FS: fsysError{}}
	if err := migrate.Validate(unreadable); !errors.Is(err, fs.ErrPermission) {
		t.Fatal(err)
	}
	c := migrate.DefaultConfig("unused")
	for _, sources := range [][]migrate.Source{{{}}, {{Name: "same", Table: "a", FS: fstest.MapFS{}}, {Name: "same", Table: "b", FS: fstest.MapFS{}}}, {{Name: "a", Table: "same", FS: fstest.MapFS{}}, {Name: "b", Table: "same", FS: fstest.MapFS{}}}} {
		if err := migrate.Up(t.Context(), c, sources); err == nil {
			t.Fatal("up")
		}
		if _, err := migrate.Versions(t.Context(), c, sources); err == nil {
			t.Fatal("versions")
		}
		if err := migrate.Down(t.Context(), c, sources[0], 0); err == nil {
			t.Fatal("down steps")
		}
	}
	if err := migrate.Down(t.Context(), c, migrate.Source{}, 1); err == nil {
		t.Fatal("down")
	}
	if err := migrate.Force(t.Context(), c, migrate.Source{}, 0); err == nil {
		t.Fatal("force")
	}
	if err := migrate.CheckUpDownUp(t.Context(), c, migrate.Source{}); err == nil {
		t.Fatal("check")
	}
}

type fsysError struct{}

func (fsysError) Open(string) (fs.File, error) { return nil, fs.ErrPermission }

func TestAPIConsumerReadOnlyConnectionRefusesSetupAndCanRetry(t *testing.T) {
	dsn := consumerDatabase(t)
	src := migrate.Source{Name: "library", Table: "versions", FS: sub(t, "library")}
	c := migrate.DefaultConfig(dsn + "&default_transaction_read_only=on")
	err := migrate.Up(t.Context(), c, []migrate.Source{src})
	if !hasSQLState(err, "25006") {
		t.Fatalf("read-only setup must fail: %v", err)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var active int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name IN ('migrate','migrate-wait')").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("failed deployment left %d connections", active)
	}
	if exists(t, dsn, "lib_outbox") {
		t.Fatal("migration ran despite setup failure")
	}
	c = migrate.DefaultConfig(dsn)
	if err := migrate.Up(t.Context(), c, []migrate.Source{src}); err != nil {
		t.Fatal("retry after corrected configuration", err)
	}
	st, err := migrate.Versions(t.Context(), c, []migrate.Source{src})
	if err != nil || st[0].Version != 1 || st[0].Dirty {
		t.Fatalf("retry state %v %v", st, err)
	}
}
