package migrate_test

import (
	"strings"
	"testing"
	"testing/fstest"

	migrate "github.com/fin-assist/go-migrate"
)

// TestAPIConsumerDeployFailsWhenAppliedFileIsGone is the deploy Job of a service whose
// applied file was renamed in a later release (FIN-278): the Job must fail, not report
// "up to date" and let the Deployment roll out on the old schema.
func TestAPIConsumerDeployFailsWhenAppliedFileIsGone(t *testing.T) {
	dsn := consumerDatabase(t)
	ctx := t.Context()
	c := migrate.DefaultConfig(dsn)
	library := migrate.Source{Name: "library", Table: "library_versions", FS: sub(t, "library")}
	release1 := []migrate.Source{library, {Name: "service", Table: "service_versions", FS: sub(t, "service")}}
	if err := migrate.Up(ctx, c, release1); err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{}
	for _, name := range []string{"000001_create_note", "000002_add_note_created_at", "000003_index_note_created_at"} {
		for _, kind := range []string{".up.sql", ".down.sql"} {
			files[name+kind] = &fstest.MapFile{Data: []byte("SELECT 1")}
		}
	}
	files["000005_next.up.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE next_step(id bigint)")}
	files["000005_next.down.sql"] = &fstest.MapFile{Data: []byte("DROP TABLE next_step")}
	release2 := []migrate.Source{library, {Name: "service", Table: "service_versions", FS: files}}
	if err := migrate.Validate(release2[1]); err != nil {
		t.Fatalf("files alone are valid, only the applied state is not: %v", err)
	}
	err := migrate.Up(ctx, c, release2)
	if err == nil || !strings.Contains(err.Error(), "source service: applied version 4 of source service has no migration file") {
		t.Fatalf("deploy must fail: %v", err)
	}
	if strings.Contains(err.Error(), dsn) || exists(t, dsn, "next_step") {
		t.Fatal("refusal leaked the DSN or applied the next file")
	}
	if err := migrate.Force(ctx, c, release2[1], 4); err == nil {
		t.Fatal("operator can force a version the release does not have")
	}
	if err := migrate.Validate(migrate.Source{Name: "dup", FS: fstest.MapFS{
		"1_a.up.sql": {}, "001_a.up.sql": {}, "1_a.down.sql": {},
	}}); err == nil {
		t.Fatal("two files of one version accepted")
	}
}
