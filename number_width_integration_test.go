package migrate_test

import (
	"fmt"
	"testing"
	"testing/fstest"

	migrate "github.com/fin-assist/go-migrate"
	pgtest "github.com/fin-assist/go-migrate/internal/pgtest"
)

// Published consumers use both 001_* and 000001_* names. The irreversible step
// deliberately raises an error if a checker tries to roll it back.
func numberedConsumerSource(width int) migrate.Source {
	return migrate.Source{Name: "consumer", Table: "consumer_versions", FS: fstest.MapFS{
		fmt.Sprintf("%0*d_items.up.sql", width, 1):   {Data: []byte("CREATE TABLE consumer_items (id bigint PRIMARY KEY)")},
		fmt.Sprintf("%0*d_items.down.sql", width, 1): {Data: []byte("DROP TABLE consumer_items")},
		fmt.Sprintf("%0*d_seed.up.sql", width, 2):    {Data: []byte("INSERT INTO consumer_items VALUES (1)")},
		fmt.Sprintf("%0*d_seed.down.sql", width, 2):  {Data: []byte("-- irreversible\nDO $$ BEGIN RAISE EXCEPTION 'seed cannot be undone'; END $$;")},
	}}
}

func TestIntegrationCheckAcceptsPublishedConsumerNumberWidths(t *testing.T) {
	for _, width := range []int{3, 6} {
		t.Run(fmt.Sprintf("width%d", width), func(t *testing.T) {
			dsn := pgtest.DSN(t)
			src := numberedConsumerSource(width)
			cfg := migrate.DefaultConfig(dsn)
			if err := migrate.CheckUpDownUp(t.Context(), cfg, src); err != nil {
				t.Fatal(err)
			}
			state, err := migrate.Versions(t.Context(), cfg, []migrate.Source{src})
			if err != nil || len(state) != 1 || state[0].Version != 2 || state[0].Dirty {
				t.Fatalf("consumer state: %v %v", state, err)
			}
			if !exists(t, dsn, "consumer_items") {
				t.Fatal("consumer schema lost")
			}
		})
	}
}
