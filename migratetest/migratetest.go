// Package migratetest checks migration sources against an explicitly supplied PostgreSQL.
// It never selects a server, reads environment variables, or starts containers.
package migratetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	appmigrate "github.com/fin-assist/go-migrate"
	"github.com/fin-assist/go-migrate/internal/safeerror"
)

// IrreversibleMarker starts a down file whose SQL deliberately refuses rollback.
const IrreversibleMarker = appmigrate.IrreversibleMarker

type connection interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Close(context.Context) error
}
type connector func(context.Context, string) (connection, error)

func connect(ctx context.Context, dsn string) (connection, error) { return pgx.Connect(ctx, dsn) }

// DSN creates a fresh database on adminDSN and registers bounded cleanup on t.
// adminDSN must name a PostgreSQL 18 database whose user can create databases.
// Connection strings are never included in failure messages.
func DSN(t testing.TB, adminDSN string) string {
	t.Helper()
	dsn, cleanup, err := database(t.Context(), adminDSN, connect)
	if err != nil {
		t.Fatalf("create test database: %v", err)
		return ""
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := cleanup(ctx); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})
	return dsn
}

func database(ctx context.Context, adminDSN string, dial connector) (string, func(context.Context) error, error) {
	_, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		return "", nil, fmt.Errorf("invalid admin connection configuration")
	}
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", nil, fmt.Errorf("generate database name: %w", err)
	}
	name := "t_" + hex.EncodeToString(bytes)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := dial(ctx, adminDSN)
	if err != nil {
		return "", nil, safeerror.Wrap("connect to admin database failed", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", nil, safeerror.Wrap("create database failed", err)
	}
	dsn := adminDSN + " dbname=" + name
	if strings.HasPrefix(adminDSN, "postgres://") || strings.HasPrefix(adminDSN, "postgresql://") {
		u, _ := url.Parse(adminDSN)
		u.Path = "/" + name
		query := u.Query()
		query.Set("dbname", name)
		u.RawQuery = query.Encode()
		dsn = u.String()
	}
	cleanup := func(ctx context.Context) error {
		conn, err := dial(ctx, adminDSN)
		if err != nil {
			return safeerror.Wrap("connect for cleanup failed", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		return safeerror.Wrap("drop database failed", err)
	}
	// pgx reconstructs both URL and keyword DSNs without leaking the admin database.
	return dsn, cleanup, nil
}

// CheckUpDownUp walks an initially empty caller-owned database through each up/down/up.
// The caller owns database cleanup. Irreversible down files are skipped, then the walk
// continues forward. A dirty version or an ineffective down fails the test.
func CheckUpDownUp(t testing.TB, dsn string, src appmigrate.Source) {
	t.Helper()
	cfg := appmigrate.DefaultConfig(dsn)
	cfg.WaitTimeout = 2 * time.Minute
	cfg.StatementTimeout = 2 * time.Minute
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := appmigrate.CheckUpDownUp(ctx, cfg, src); err != nil {
		// Errors from migration SQL describe the failed version; DSNs are not logged.
		t.Fatal(err)
	}
}
