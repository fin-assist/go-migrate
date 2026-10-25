package migrate_test

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	migrate "github.com/fin-assist/go-migrate"
	migratetest "github.com/fin-assist/go-migrate/internal/pgtest"
	harness "github.com/fin-assist/go-migrate/migratetest"
)

func TestIntegrationPublicHarnessAndDeploymentGuards(t *testing.T) {
	// Check the actual shared harness against an isolated database, including a source
	// whose final remove step deliberately has no executable down migration.
	for _, name := range []string{"service", "library"} {
		t.Run(name, func(t *testing.T) {
			dsn := harness.DSN(t, migratetest.DSN(t))
			harness.CheckUpDownUp(t, dsn, migrate.Source{Name: name, Table: "versions", FS: sub(t, name)})
		})
	}
	dsn := migratetest.DSN(t)
	c := migrate.DefaultConfig(dsn)
	stable := migrate.Source{Name: "stable", Table: "stable_versions", FS: sub(t, "library")}
	if err := migrate.Up(t.Context(), c, []migrate.Source{stable}); err != nil {
		t.Fatal(err)
	}
	// Invalid deployment bundles must not mutate the already running schema.
	file := func() *fstest.MapFile { return &fstest.MapFile{Data: []byte("SELECT 1")} }
	cases := []migrate.Source{{}, {Name: "bad", Table: "bad_versions", FS: fsysError{}},
		{Name: "bad", Table: "bad_versions", FS: fstest.MapFS{"wrong.sql": file()}},
		{Name: "bad", Table: "bad_versions", FS: fstest.MapFS{"000001_a.up.sql": file()}},
		{Name: "bad", Table: "bad_versions", FS: fstest.MapFS{"000001_a.down.sql": file()}},
		{Name: "bad", Table: "bad_versions", FS: fstest.MapFS{"000001_a.up.sql": file(), "000001_b.down.sql": file()}},
	}
	for _, bad := range cases {
		if err := migrate.Up(t.Context(), c, []migrate.Source{stable, bad}); err == nil {
			t.Fatal("invalid up accepted")
		}
		if _, err := migrate.Versions(t.Context(), c, []migrate.Source{bad}); err == nil {
			t.Fatal("invalid version accepted")
		}
		if err := migrate.Down(t.Context(), c, bad, 1); err == nil {
			t.Fatal("invalid down accepted")
		}
		if err := migrate.Force(t.Context(), c, bad, 1); err == nil {
			t.Fatal("invalid force accepted")
		}
		if err := migrate.CheckUpDownUp(t.Context(), c, bad); err == nil {
			t.Fatal("invalid check accepted")
		}
	}
	for _, bundle := range [][]migrate.Source{{stable, stable}, {stable, {Name: "other", Table: stable.Table, FS: stable.FS}}} {
		if err := migrate.Up(t.Context(), c, bundle); err == nil {
			t.Fatal("duplicate sources accepted")
		}
	}
	if _, err := migrate.Find([]migrate.Source{stable}, "stable"); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Find([]migrate.Source{stable}, "other"); err == nil {
		t.Fatal("missing name accepted")
	}
	if err := migrate.Down(t.Context(), c, stable, 0); err == nil {
		t.Fatal("zero steps")
	}
	if err := migrate.Down(t.Context(), c, stable, 1); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Up(t.Context(), c, []migrate.Source{stable}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, dsn, "lib_outbox") {
		t.Fatal("stable schema lost")
	}
	st, err := migrate.Versions(t.Context(), c, []migrate.Source{stable})
	if err != nil || st[0].Version != 1 || st[0].Dirty {
		t.Fatalf("stable version %v %v", st, err)
	}
	if err := migrate.Validate(migrate.Source{Name: "unreadable", FS: fsysError{}}); err == nil || !strings.Contains(err.Error(), fs.ErrPermission.Error()) {
		t.Fatal(err)
	}
}
