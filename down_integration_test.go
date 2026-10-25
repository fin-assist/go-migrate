package migrate

import (
	"context"
	"errors"
	"os"
	"testing"
	"testing/fstest"

	engine "github.com/golang-migrate/migrate/v4"

	"github.com/fin-assist/go-migrate/internal/pgtest"
)

func TestIntegrationDownMatchesNativeShortLimitAndState(t *testing.T) {
	for _, test := range []struct {
		name           string
		applied, steps int
	}{{"exact", 2, 2}, {"short-one", 2, 3}, {"short-two", 2, 4}, {"already-empty", 0, 1}} {
		t.Run(test.name, func(t *testing.T) {
			src := Source{Name: "service", Table: "versions", FS: fstest.MapFS{
				"000001_a.up.sql": {Data: []byte("CREATE TABLE a(id bigint)")}, "000001_a.down.sql": {Data: []byte("DROP TABLE a")},
				"000002_b.up.sql": {Data: []byte("CREATE TABLE b(id bigint)")}, "000002_b.down.sql": {Data: []byte("DROP TABLE b")},
			}}
			type outcome struct {
				err    error
				status Status
			}
			outcomes := make([]outcome, 2)
			for i := range outcomes {
				cfg := DefaultConfig(pgtest.DSN(t))
				ctx := context.Background()
				if test.applied > 0 {
					if err := Up(ctx, cfg, []Source{src}); err != nil {
						t.Fatal(err)
					}
				}
				if i == 0 {
					outcomes[i].err = Down(ctx, cfg, src, test.steps)
				} else {
					m, err := openMigration(cfg, src)
					if err != nil {
						t.Fatal(err)
					}
					outcomes[i].err = m.Steps(-test.steps)
					if a, b := m.Close(); a != nil || b != nil {
						t.Fatalf("close: %v %v", a, b)
					}
				}
				st, err := Versions(ctx, cfg, []Source{src})
				if err != nil {
					t.Fatal(err)
				}
				outcomes[i].status = st[0]
			}
			if outcomes[0].status != outcomes[1].status {
				t.Fatalf("runner/native state differ: %+v", outcomes)
			}
			for _, got := range outcomes {
				if test.steps > test.applied && test.applied > 0 {
					var short engine.ErrShortLimit
					if !errors.As(got.err, &short) || short.Short != uint(test.steps-test.applied) {
						t.Fatalf("short limit %v want %d", got.err, test.steps-test.applied)
					}
				} else if test.applied == 0 {
					if !errors.Is(got.err, os.ErrNotExist) {
						t.Fatalf("empty down: %v", got.err)
					}
				} else if got.err != nil {
					t.Fatal(got.err)
				}
				if got.status.Version != 0 || got.status.Dirty {
					t.Fatalf("wrong state: %+v", got.status)
				}
			}
		})
	}
}
