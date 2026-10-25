package library_test

import (
	"io/fs"
	"testing"

	library "github.com/fin-assist/go-library-template"
)

// A consumer can discover the source without pulling in an application's state.
// The template has no business API or SQL to invent just for coverage.
func TestAPIEmptyMigrationSourceHasStableConsumerMetadata(t *testing.T) {
	if library.MigrationsName != "library" || library.MigrationsTable != library.MigrationsName+"_schema_migrations" {
		t.Fatal("migration source identity changed")
	}
	files, err := fs.Glob(library.Migrations, "*.sql")
	if err != nil || len(files) != 0 {
		t.Fatal("the template unexpectedly ships library SQL", files, err)
	}
	if _, err := fs.ReadDir(library.Migrations, "."); err != nil {
		t.Fatal("consumer cannot read the embedded source", err)
	}
}
