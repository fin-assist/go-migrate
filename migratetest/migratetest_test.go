package migratetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeConnection struct {
	sql    []string
	fail   error
	closed int
}

func (c *fakeConnection) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	c.sql = append(c.sql, sql)
	return pgconn.CommandTag{}, c.fail
}
func (c *fakeConnection) Close(context.Context) error { c.closed++; return nil }
func TestUnitDatabaseOwnership(t *testing.T) {
	for _, admin := range []string{"postgres://user:secret@localhost/admin?sslmode=disable&dbname=override", "host=localhost user=user password=secret dbname=admin sslmode=disable"} {
		t.Run(admin[:8], func(t *testing.T) {
			c := &fakeConnection{}
			calls := 0
			dial := func(ctx context.Context, dsn string) (connection, error) {
				calls++
				if dsn != admin || ctx.Err() != nil {
					t.Fatal("admin configuration/context changed")
				}
				return c, nil
			}
			dsn, cleanup, err := database(context.Background(), admin, dial)
			if err != nil {
				t.Fatal(err)
			}
			pc, err := pgx.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(pc.Database, "t_") || pc.Password != "secret" || pc.User != "user" {
				t.Fatal("database configuration lost")
			}
			if len(c.sql) != 1 || c.sql[0] != "CREATE DATABASE "+(pgx.Identifier{pc.Database}).Sanitize() {
				t.Fatal(c.sql)
			}
			if err := cleanup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || c.closed != 2 || c.sql[1] != "DROP DATABASE IF EXISTS "+(pgx.Identifier{pc.Database}).Sanitize()+" WITH (FORCE)" {
				t.Fatalf("calls=%d closed=%d sql=%v", calls, c.closed, c.sql)
			}
		})
	}
}
func TestUnitDatabaseFailuresDoNotLeakCredentials(t *testing.T) {
	secret := errors.New("credential-secret")
	for _, mode := range []string{"parse", "connect", "create", "cleanup-connect", "cleanup-drop"} {
		t.Run(mode, func(t *testing.T) {
			c := &fakeConnection{}
			calls := 0
			dial := func(context.Context, string) (connection, error) {
				calls++
				if mode == "connect" || (mode == "cleanup-connect" && calls == 2) {
					return nil, secret
				}
				return c, nil
			}
			admin := "postgres://user:credential-secret@localhost/admin"
			if mode == "parse" {
				admin = "://credential-secret"
			}
			if mode == "create" {
				c.fail = secret
			}
			_, cleanup, err := database(context.Background(), admin, dial)
			if strings.HasPrefix(mode, "cleanup") {
				if err != nil {
					t.Fatal(err)
				}
				if mode == "cleanup-drop" {
					c.fail = secret
				}
				err = cleanup(context.Background())
			}
			if err == nil || strings.Contains(err.Error(), "credential-secret") {
				t.Fatalf("unsafe/missing error: %v", err)
			}
			if (mode == "parse" && calls != 0) || (mode == "create" && c.closed != 1) {
				t.Fatal("continued after failure")
			}
		})
	}
}
