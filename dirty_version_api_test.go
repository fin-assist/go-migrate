package migrate_test

import (
	"errors"
	"testing"
	"testing/fstest"

	engine "github.com/golang-migrate/migrate/v4"

	migrate "github.com/fin-assist/go-migrate"
)

// TestAPIConsumerVersionCommandShowsDirtyAfterFailedFirstRollback is the operator flow of
// docs/migrations.md (FIN-314): after a failed rollback of the first migration,
// "migrate version" must say dirty, matching why "migrate up" refuses to run.
func TestAPIConsumerVersionCommandShowsDirtyAfterFailedFirstRollback(t *testing.T) {
	dsn := consumerDatabase(t)
	ctx := t.Context()
	c := migrate.DefaultConfig(dsn)
	library := migrate.Source{Name: "library", Table: "library_versions", FS: sub(t, "library")}
	service := migrate.Source{Name: "service", Table: "service_versions", FS: fstest.MapFS{
		"000001_create_note.up.sql":   {Data: []byte("CREATE TABLE note(id bigint)")},
		"000001_create_note.down.sql": {Data: []byte("DROP TABLE note_does_not_exist")},
	}}
	sources := []migrate.Source{library, service}
	if err := migrate.Up(ctx, c, sources); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Down(ctx, c, service, 1); err == nil {
		t.Fatal("failed rollback reported success")
	}
	st, err := migrate.Versions(ctx, c, sources)
	if err != nil || len(st) != 2 {
		t.Fatalf("versions %v %v", st, err)
	}
	if st[1].Source != "service" || st[1].Version != 0 || !st[1].Dirty {
		t.Fatalf("service %+v, want dirty 0", st[1])
	}
	if st[0].Source != "library" || st[0].Dirty {
		t.Fatalf("library %+v must stay clean", st[0])
	}
	var dirty engine.ErrDirty
	if err := migrate.Up(ctx, c, sources); !errors.As(err, &dirty) {
		t.Fatalf("deploy on a dirty source: %v", err)
	}
	if !exists(t, dsn, "note") {
		t.Fatal("the failed rollback must leave the schema as it was")
	}
	if _, err := pgtestExec(t, dsn, "DROP TABLE note"); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Force(ctx, c, service, 0); err != nil {
		t.Fatal(err)
	}
	st, err = migrate.Versions(ctx, c, sources)
	if err != nil || st[1].Version != 0 || st[1].Dirty {
		t.Fatalf("after force %v %v, want clean 0", st, err)
	}
	if err := migrate.Up(ctx, c, sources); err != nil || !exists(t, dsn, "note") {
		t.Fatalf("deploy after recovery: %v", err)
	}
}
