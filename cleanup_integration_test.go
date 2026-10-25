package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	migrate "github.com/fin-assist/go-migrate"
	pgtest "github.com/fin-assist/go-migrate/internal/pgtest"
)

func TestIntegrationFailedDriverInitializationClosesOwnedConnections(t *testing.T) {
	dsn := pgtest.DSN(t)
	src := migrate.Source{Name: "service", Table: "versions", FS: sub(t, "library")}
	err := migrate.Up(t.Context(), migrate.DefaultConfig(dsn+"&default_transaction_read_only=on"), []migrate.Source{src})
	if err == nil || !strings.Contains(err.Error(), "database driver") {
		t.Fatalf("initialization refusal: %v", err)
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
		t.Fatalf("failed initialization leaked %d migration connections", active)
	}
	if err := migrate.Up(t.Context(), migrate.DefaultConfig(dsn), []migrate.Source{src}); err != nil {
		t.Fatal("corrected run blocked", err)
	}
	st, err := migrate.Versions(t.Context(), migrate.DefaultConfig(dsn), []migrate.Source{src})
	if err != nil || st[0].Version != 1 || st[0].Dirty {
		t.Fatalf("restart %v %v", st, err)
	}
}
