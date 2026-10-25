package migrate_test

import (
	"context"
	"fmt"
	"testing"

	migrate "github.com/fin-assist/go-migrate"
	"github.com/fin-assist/go-migrate/migratetest"

	"github.com/jackc/pgx/v5"
)

func TestAPIConsumerHarnessSupportsShortAndLongMigrationNames(t *testing.T) {
	for _, width := range []int{3, 6} {
		t.Run(fmt.Sprintf("width%d", width), func(t *testing.T) {
			dsn := consumerDatabase(t)
			src := numberedConsumerSource(width)
			migratetest.CheckUpDownUp(t, dsn, src)
			cfg := migrate.DefaultConfig(dsn)
			state, err := migrate.Versions(t.Context(), cfg, []migrate.Source{src})
			if err != nil || len(state) != 1 || state[0].Version != 2 || state[0].Dirty {
				t.Fatalf("checked source: %v %v", state, err)
			}
			conn, err := pgx.Connect(t.Context(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close(context.Background()) }()
			var rows int
			if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM consumer_items").Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("consumer effect: %d %v", rows, err)
			}
			// Test helpers may skip an irreversible down; an operator's Down must still fail.
			if err := migrate.Down(t.Context(), cfg, src, 1); !hasSQLState(err, "P0001") {
				t.Fatalf("irreversible rollback accepted: %v", err)
			}
		})
	}
}
