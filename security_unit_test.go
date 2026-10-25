package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestUnitEngineErrorBoundaryAndInvalidDSN(t *testing.T) {
	secret := "private-query-detail-password"
	pgerr := &pgconn.PgError{Code: "P0001", Message: secret, Detail: secret}
	cause := database.Error{Query: []byte(secret), OrigErr: pgerr}
	var log bytes.Buffer
	cfg := DefaultConfig("postgres://localhost/db")
	cfg.Logger = slog.New(slog.NewTextHandler(&log, nil))
	r := fakeRunner(&fakeQueue{}, &fakeSession{upErr: cause})
	err := r.up(context.Background(), cfg, []Source{source()})
	var diagnostic database.Error
	if !errors.As(err, &diagnostic) || !errors.Is(diagnostic.OrigErr, pgerr) {
		t.Fatal("diagnostics lost")
	}
	for _, format := range []string{"%v", "%+v", "%s", "%q"} {
		if strings.Contains(fmt.Sprintf(format, err), secret) || strings.Contains(log.String(), secret) {
			t.Fatal("private diagnostics exposed")
		}
	}
	malformed := "postgres://user:password-secret@%zz/db"
	cfg.DSN = malformed
	err = Up(context.Background(), cfg, []Source{source()})
	if !errors.Is(err, ErrInvalidDSN) || strings.Contains(err.Error(), "password-secret") {
		t.Fatal("unsafe invalid DSN error")
	}
	if _, err := open(cfg); !errors.Is(err, ErrInvalidDSN) || strings.Contains(err.Error(), "password-secret") {
		t.Fatal("unsafe migration connection configuration")
	}
}

func TestUnitValidateRejectsNilFilesWithoutIO(t *testing.T) {
	if err := Validate(Source{Name: "missing-files"}); err == nil || !strings.Contains(err.Error(), "files are required") {
		t.Fatalf("nil filesystem must return a validation error: %v", err)
	}
}
